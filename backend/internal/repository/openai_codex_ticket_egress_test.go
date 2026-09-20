package repository

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// startCodexEgressConnectProxy 为打票回归测试提供 start Codex Egress Connect Proxy 辅助实现
func startCodexEgressConnectProxy(t *testing.T, target string) (string, *atomic.Int64) {
	t.Helper()
	var connects atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusBadRequest)
			return
		}
		upstream, err := net.Dial("tcp", target)
		if err != nil {
			http.Error(w, "target unavailable", http.StatusBadGateway)
			return
		}
		defer func() { _ = upstream.Close() }()
		hijacker, _ := w.(http.Hijacker)
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		connects.Add(1)
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(upstream, rw)
			_ = upstream.Close()
			close(done)
		}()
		_, _ = io.Copy(conn, upstream)
		_ = conn.Close()
		<-done
	}))
	t.Cleanup(server.Close)
	return server.URL, &connects
}

// TestCodexTicketEgressSharesOnlyTheCurrentAttemptConnection 验证 Codex Ticket Egress Shares Only The Current Attempt Connection 场景
func TestCodexTicketEgressSharesOnlyTheCurrentAttemptConnection(t *testing.T) {
	for _, proxyKind := range []string{"direct", "http", "socks5"} {
		t.Run(proxyKind, func(t *testing.T) {
			type seenRequest struct {
				path, address, method string
				header                http.Header
				close                 bool
			}
			var mu sync.Mutex
			var seen []seenRequest
			target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = append(seen, seenRequest{r.URL.Path, r.RemoteAddr, r.Method, r.Header.Clone(), r.Close})
				mu.Unlock()
				if r.URL.Path == "/cdn-cgi/trace" {
					_, _ = io.WriteString(w, "ip=8.8.8.8\nloc=US\n")
					return
				}
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			t.Cleanup(target.Close)
			client := target.Client()
			base, _ := client.Transport.(*http.Transport)
			base.DisableKeepAlives = true
			base.ForceAttemptHTTP2 = false
			base.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
			var calls *atomic.Int64
			if proxyKind != "direct" {
				var proxy string
				if proxyKind == "http" {
					proxy, calls = startCodexEgressConnectProxy(t, target.Listener.Addr().String())
				} else {
					proxy, calls = startTestSOCKS5Proxy(t)
				}
				proxyURL, err := url.Parse(proxy)
				require.NoError(t, err)
				proxied, err := buildUpstreamTransport(poolSettings{}, proxyURL, upstreamProtocolModeOpenAIH1NoReuse)
				require.NoError(t, err)
				proxied.TLSClientConfig = base.TLSClientConfig.Clone()
				base, client.Transport = proxied, proxied
			}
			for range 2 {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target.URL+"/backend-api/codex/responses", strings.NewReader(`{"model":"gpt-6-astra"}`))
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer test-account-secret")
				req.Header.Set("Cookie", "account-cookie")
				req.Header.Set("ChatGPT-Account-ID", "account-private-id")
				req.Header.Set("session_id", "private-session")
				var result service.OpenAICodexTicketEgressResult
				resp, err := doCodexTicketWithEgress(client, req, func(value service.OpenAICodexTicketEgressResult) { result = value }, time.Second)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
				require.Equal(t, "8.8.8.8", result.IP)
				require.Nil(t, result.Error)
			}
			mu.Lock()
			defer mu.Unlock()
			require.Len(t, seen, 4)
			for _, i := range []int{0, 2} {
				require.Equal(t, "/cdn-cgi/trace", seen[i].path)
				require.Equal(t, http.MethodGet, seen[i].method)
				for _, name := range []string{"Authorization", "Cookie", "ChatGPT-Account-ID", "session_id"} {
					require.Empty(t, seen[i].header.Get(name), name)
				}
				require.Equal(t, seen[i].address, seen[i+1].address, "trace and ticket must share the real TLS connection")
				require.Equal(t, "Bearer test-account-secret", seen[i+1].header.Get("Authorization"))
				require.True(t, seen[i+1].close)
			}
			require.NotEqual(t, seen[0].address, seen[2].address, "a new attempt must allow proxy rotation")
			require.True(t, base.DisableKeepAlives, "the shared upstream transport must remain unchanged")
			if calls != nil {
				require.Equal(t, int64(2), calls.Load(), "one proxy tunnel per complete attempt")
			}
		})
	}
}

// TestCodexTicketEgressTraceFailuresDoNotPreventTickets 验证 Codex Ticket Egress Trace Failures Do Not Prevent Tickets 场景
func TestCodexTicketEgressTraceFailuresDoNotPreventTickets(t *testing.T) {
	for _, scenario := range []string{"close", "reconnect", "redirect", "status", "oversize", "private", "invalid", "duplicate", "missing", "read_error", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			var traceCalls, ticketCalls atomic.Int64
			target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/cdn-cgi/trace" {
					ticketCalls.Add(1)
					w.WriteHeader(http.StatusOK)
					return
				}
				traceCalls.Add(1)
				switch scenario {
				case "close":
					w.Header().Set("Connection", "close")
				case "reconnect":
					hijacker, _ := w.(http.Hijacker)
					conn, rw, err := hijacker.Hijack()
					if err == nil {
						body := "ip=8.8.8.8\n"
						_, _ = fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
						_ = rw.Flush()
						_ = conn.Close()
					}
					return
				case "redirect":
					http.Redirect(w, r, "/must-not-follow", http.StatusFound)
					return
				case "status":
					w.WriteHeader(http.StatusForbidden)
					return
				case "oversize":
					_, _ = io.WriteString(w, "ip=8.8.8.8\n"+strings.Repeat("x", 4096))
					return
				case "private":
					_, _ = io.WriteString(w, "ip=10.1.2.3\n")
					return
				case "invalid":
					_, _ = io.WriteString(w, "ip=not-an-address\n")
					return
				case "duplicate":
					_, _ = io.WriteString(w, "ip=8.8.8.8\nip=9.9.9.9\n")
					return
				case "missing":
					_, _ = io.WriteString(w, "loc=US\n")
					return
				case "read_error":
					w.Header().Set("Content-Length", "100")
					_, _ = io.WriteString(w, "ip=8.8.8.8\n")
					return
				case "timeout":
					<-r.Context().Done()
					return
				}
				_, _ = io.WriteString(w, "ip=8.8.8.8\n")
			}))
			t.Cleanup(target.Close)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target.URL+"/backend-api/codex/responses", strings.NewReader("ticket"))
			require.NoError(t, err)
			var result service.OpenAICodexTicketEgressResult
			resp, err := doCodexTicketWithEgress(target.Client(), req, func(value service.OpenAICodexTicketEgressResult) { result = value }, 100*time.Millisecond)
			// A peer can close after claiming keepalive; Go may return the write
			// error or redial. Neither outcome may inherit the trace connection IP.
			if scenario != "reconnect" {
				require.NoError(t, err)
				require.Equal(t, int64(1), ticketCalls.Load())
			}
			if resp != nil {
				require.NoError(t, resp.Body.Close())
			}
			require.Equal(t, int64(1), traceCalls.Load())
			if err == nil || scenario != "reconnect" {
				require.Empty(t, result.IP)
				require.NotNil(t, result.Error)
			}
			if scenario == "reconnect" {
				if result.Error != nil {
					require.Contains(t, []string{"connection_changed", "ticket_connection_unavailable"}, result.Error.Reason)
				}
				return
			}
			reasons := map[string]string{
				"close": "connection_closed", "redirect": "http_error", "status": "http_error",
				"oversize": "response_too_large", "private": "invalid_ip", "invalid": "invalid_ip",
				"duplicate": "ambiguous_ip", "missing": "missing_ip", "read_error": "response_read_error", "timeout": "timeout",
			}
			require.Equal(t, reasons[scenario], result.Error.Reason)
			if scenario == "redirect" {
				require.Equal(t, http.StatusFound, result.Error.HTTPStatus)
			} else if scenario == "status" {
				require.Equal(t, http.StatusForbidden, result.Error.HTTPStatus)
			} else {
				require.Zero(t, result.Error.HTTPStatus)
			}
		})
	}
}

// TestCodexTicketEgressDoOptInPreservesTracking 验证 Codex Ticket Egress Do Opt In Preserves Tracking 场景
func TestCodexTicketEgressDoOptInPreservesTracking(t *testing.T) {
	var traceCalls, ticketCalls atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cdn-cgi/trace" {
			traceCalls.Add(1)
			_, _ = io.WriteString(w, "ip=2606:4700:4700::1111\n")
			return
		}
		ticketCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	svc, _ := NewHTTPUpstream(nil).(*httpUpstreamService)
	entry, err := svc.getClientEntry("", 41, 1, service.HTTPUpstreamProfileOpenAIHarvest, false, false)
	require.NoError(t, err)
	transport, _ := entry.client.Transport.(*http.Transport)
	targetTransport, _ := target.Client().Transport.(*http.Transport)
	transport.TLSClientConfig = targetTransport.TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, target.Listener.Addr().String())
	}
	for _, observe := range []bool{false, true} {
		ctx := service.WithHTTPUpstreamProfile(t.Context(), service.HTTPUpstreamProfileOpenAIHarvest)
		var result service.OpenAICodexTicketEgressResult
		if observe {
			ctx = service.WithOpenAICodexTicketEgressObserver(ctx, func(value service.OpenAICodexTicketEgressResult) { result = value })
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader("ticket"))
		require.NoError(t, err)
		resp, err := svc.Do(req, "", 41, 1)
		require.NoError(t, err)
		require.Equal(t, int64(1), atomic.LoadInt64(&entry.inFlight))
		require.NoError(t, resp.Body.Close())
		require.Zero(t, atomic.LoadInt64(&entry.inFlight))
		if observe {
			require.Equal(t, "2606:4700:4700::1111", result.IP)
		} else {
			require.Empty(t, result.IP)
		}
		require.Nil(t, result.Error)
	}
	require.Equal(t, int64(1), traceCalls.Load())
	require.Equal(t, int64(2), ticketCalls.Load())
}

// TestCodexTicketEgressCancelledParentSkipsAllRequests 验证 Codex Ticket Egress Cancelled Parent Skips All Requests 场景
func TestCodexTicketEgressCancelledParentSkipsAllRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var calls atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(target.Close)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, nil)
	require.NoError(t, err)
	var result service.OpenAICodexTicketEgressResult
	_, err = doCodexTicketWithEgress(target.Client(), req, func(value service.OpenAICodexTicketEgressResult) { result = value }, time.Second)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, "canceled", result.Error.Reason)
	require.Zero(t, calls.Load())
}

// TestCodexTicketEgressReservesShortDeadlineForTicket 验证 Codex Ticket Egress Reserves Short Deadline For Ticket 场景
func TestCodexTicketEgressReservesShortDeadlineForTicket(t *testing.T) {
	var traceCalls, ticketCalls atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cdn-cgi/trace" {
			traceCalls.Add(1)
			<-r.Context().Done()
			return
		}
		ticketCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL+"/backend-api/codex/responses", nil)
	require.NoError(t, err)
	var result service.OpenAICodexTicketEgressResult
	resp, err := doCodexTicketWithEgress(target.Client(), req, func(value service.OpenAICodexTicketEgressResult) { result = value }, codexTicketEgressTraceTimeout)
	require.NoError(t, err, "optional diagnostics must leave short attempt deadlines to the ticket")
	require.NoError(t, resp.Body.Close())
	require.Equal(t, int64(1), ticketCalls.Load())
	require.Zero(t, traceCalls.Load())
	require.Equal(t, "insufficient_time", result.Error.Reason)
}
