package repository

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/servertiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const codexTicketEgressTraceTimeout = 10 * time.Second

// Only the known Codex endpoint opts into this additional same-origin request.
func canTraceCodexTicketEgress(req *http.Request) bool {
	return req != nil && req.URL != nil && req.Method == http.MethodPost &&
		req.URL.Scheme == "https" && req.URL.Hostname() == "chatgpt.com" &&
		(req.URL.Port() == "" || req.URL.Port() == "443") &&
		(req.Host == "" || req.Host == "chatgpt.com") &&
		req.URL.Path == "/backend-api/codex/responses"
}

type codexTicketConnection struct {
	mu   sync.Mutex
	conn net.Conn
}

// gotConn 记录本次请求实际使用的连接以核对出口归属
func (c *codexTicketConnection) gotConn(info httptrace.GotConnInfo) {
	c.mu.Lock()
	c.conn = info.Conn // A retry must replace the previous connection identity.
	c.mu.Unlock()
}

// last 在锁保护下读取最近一次请求连接
func (c *codexTicketConnection) last() net.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn
}

// Each attempt owns its transport: trace and POST may share one TLS tunnel,
// but the next attempt still opens a fresh tunnel through the rotating proxy.
// GotConn only observes identity; it must never read or write Transport's socket.
func doCodexTicketWithEgress(client *http.Client, req *http.Request, observe func(service.OpenAICodexTicketEgressResult), traceTimeout time.Duration) (*http.Response, error) {
	base, ok := client.Transport.(*http.Transport)
	if !ok {
		observe(codexTicketEgressFailure("unsupported_transport"))
		return servertiming.Do(client, req)
	}
	transport := base.Clone()
	transport.DisableKeepAlives = false
	transport.MaxIdleConns = 1
	transport.MaxIdleConnsPerHost = 1
	transport.MaxConnsPerHost = 1
	transport.IdleConnTimeout = 5 * time.Second
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	if transport.TLSClientConfig != nil {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	defer transport.CloseIdleConnections()
	attemptClient := *client
	attemptClient.Transport = transport

	result, traceConn := traceCodexTicketEgress(&attemptClient, req, traceTimeout)
	if result.Error != nil {
		// Make failures visible while the independent ticket request is still
		// running. The diagnostic failure must never suppress the ticket.
		observe(result)
	}
	var ticketConn codexTicketConnection
	ctx := httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: ticketConn.gotConn})
	ticketReq := req.Clone(ctx)
	ticketReq.Close = true
	resp, err := servertiming.Do(&attemptClient, ticketReq)
	if result.Error == nil {
		switch current := ticketConn.last(); {
		case current == nil || traceConn == nil:
			observe(codexTicketEgressFailure("ticket_connection_unavailable"))
		case current != traceConn:
			observe(codexTicketEgressFailure("connection_changed"))
		default:
			observe(result)
		}
	}
	return resp, err
}

// traceCodexTicketEgress 通过同源匿名请求检测出口地址并预留实际采票时间
func traceCodexTicketEgress(client *http.Client, req *http.Request, timeout time.Duration) (result service.OpenAICodexTicketEgressResult, connection net.Conn) {
	if err := req.Context().Err(); err != nil {
		return codexTicketEgressFailure(codexTicketEgressErrorReason(err, "request_error")), nil
	}
	// Leave short configured attempt deadlines to the actual ticket request.
	// For longer attempts diagnostics may use at most half the remaining budget.
	if deadline, ok := req.Context().Deadline(); ok && time.Until(deadline) <= 2*timeout {
		return codexTicketEgressFailure("insufficient_time"), nil
	}
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	defer cancel()
	var conn codexTicketConnection
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: conn.gotConn})
	target := *req.URL
	target.Path, target.RawPath, target.RawQuery, target.Fragment = "/cdn-cgi/trace", "", "", ""
	target.ForceQuery = false
	target.User = nil
	traceReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return codexTicketEgressFailure("request_error"), nil
	}
	traceReq.Header.Set("Accept", "text/plain")
	traceReq.Header.Set("User-Agent", "Sub2API-Egress-Diagnostics/1.0")
	// This is an anonymous request. In particular, no account headers, cookies,
	// session identifiers, or bearer token from the ticket request are copied.
	traceClient := *client
	traceClient.Jar = nil
	traceClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	logHTTP := service.BeginOpenAICodexTicketHTTPLog(ctx, traceReq.Method, traceReq.URL.String(), map[string]any{"anonymous": true})
	status := 0
	defer func() {
		logHTTP(status, map[string]any{"ip": result.IP, "diagnostic": result.Error}, err)
	}()
	resp, err := traceClient.Do(traceReq)
	if err != nil {
		return codexTicketEgressFailure(codexTicketEgressErrorReason(err, "network_error")), nil
	}
	defer func() { _ = resp.Body.Close() }()
	status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		result := codexTicketEgressFailure("http_error")
		result.Error.HTTPStatus = resp.StatusCode
		return result, nil
	}
	if resp.Close {
		return codexTicketEgressFailure("connection_closed"), nil
	}
	if resp.ProtoMajor != 1 {
		return codexTicketEgressFailure("unsupported_protocol"), nil
	}
	const maxBody = 4096
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return codexTicketEgressFailure(codexTicketEgressErrorReason(err, "response_read_error")), nil
	}
	if len(body) > maxBody {
		return codexTicketEgressFailure("response_too_large"), nil
	}
	var ip string
	for _, line := range strings.Split(string(body), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || key != "ip" {
			continue
		}
		if ip != "" { // Ambiguous trace data must not be attributed to a ticket.
			return codexTicketEgressFailure("ambiguous_ip"), nil
		}
		addr, parseErr := netip.ParseAddr(strings.TrimSpace(value))
		if parseErr != nil || addr.Zone() != "" || !addr.IsGlobalUnicast() || addr.IsPrivate() {
			return codexTicketEgressFailure("invalid_ip"), nil
		}
		ip = addr.Unmap().String()
	}
	if ip == "" {
		return codexTicketEgressFailure("missing_ip"), nil
	}
	return service.OpenAICodexTicketEgressResult{IP: ip}, conn.last()
}

// codexTicketEgressFailure 构造不包含敏感信息的出口检测失败结果
func codexTicketEgressFailure(reason string) service.OpenAICodexTicketEgressResult {
	return service.OpenAICodexTicketEgressResult{Error: &service.OpenAICodexTicketEgressError{Reason: reason}}
}

// codexTicketEgressErrorReason 将出口检测错误归一化为可展示的原因
func codexTicketEgressErrorReason(err error, fallback string) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "timeout"
	}
	return fallback
}
