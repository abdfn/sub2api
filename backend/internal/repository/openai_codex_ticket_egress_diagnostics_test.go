package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type codexTicketEgressRoundTripper func(*http.Request) (*http.Response, error)

// RoundTrip 为打票回归测试提供 Round Trip 辅助实现
func (fn codexTicketEgressRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

// TestCodexTicketEgressReportsUnsupportedTransportBeforeTicket 验证 Codex Ticket Egress Reports Unsupported Transport Before Ticket 场景
func TestCodexTicketEgressReportsUnsupportedTransportBeforeTicket(t *testing.T) {
	var result service.OpenAICodexTicketEgressResult
	calls := 0
	client := &http.Client{Transport: codexTicketEgressRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, http.MethodPost, req.Method)
		require.Equal(t, "unsupported_transport", result.Error.Reason)
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	require.NoError(t, err)
	resp, err := doCodexTicketWithEgress(client, req, func(value service.OpenAICodexTicketEgressResult) { result = value }, time.Second)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 1, calls)
	require.Empty(t, result.IP)
	require.Zero(t, result.Error.HTTPStatus)
}

// TestCodexTicketEgressReportsFailureBeforeTicketCompletes 验证 Codex Ticket Egress Reports Failure Before Ticket Completes 场景
func TestCodexTicketEgressReportsFailureBeforeTicketCompletes(t *testing.T) {
	release, ticketStarted, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	observations := make(chan service.OpenAICodexTicketEgressResult, 1)
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cdn-cgi/trace" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		close(ticketStarted)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target.URL+"/backend-api/codex/responses", nil)
	require.NoError(t, err)
	go func() {
		defer close(done)
		resp, requestErr := doCodexTicketWithEgress(target.Client(), req, func(result service.OpenAICodexTicketEgressResult) { observations <- result }, time.Second)
		if requestErr != nil {
			t.Errorf("ticket request failed: %v", requestErr)
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
	}()
	defer func() { close(release); <-done }()
	select {
	case result := <-observations:
		require.Equal(t, "http_error", result.Error.Reason)
		require.Equal(t, http.StatusForbidden, result.Error.HTTPStatus)
	case <-time.After(time.Second):
		t.Fatal("diagnostics waited for the ticket request")
	}
	select {
	case <-ticketStarted:
	case <-time.After(time.Second):
		t.Fatal("diagnostic failure prevented the ticket request")
	}
	select {
	case <-done:
		t.Fatal("ticket should still be waiting for its response")
	default:
	}
}

// TestCodexTicketEgressReportsChangedConnectionAfterTicketRedirect 验证 Codex Ticket Egress Reports Changed Connection After Ticket Redirect 场景
func TestCodexTicketEgressReportsChangedConnectionAfterTicketRedirect(t *testing.T) {
	var ticketCalls atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cdn-cgi/trace":
			_, _ = io.WriteString(w, "ip=8.8.8.8\n")
		case "/backend-api/codex/responses":
			ticketCalls.Add(1)
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
		default:
			ticketCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(target.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target.URL+"/backend-api/codex/responses", strings.NewReader("ticket"))
	require.NoError(t, err)
	var result service.OpenAICodexTicketEgressResult
	resp, err := doCodexTicketWithEgress(target.Client(), req, func(value service.OpenAICodexTicketEgressResult) { result = value }, time.Second)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, int64(2), ticketCalls.Load())
	require.Empty(t, result.IP)
	require.Equal(t, "connection_changed", result.Error.Reason)
}

// TestCodexTicketEgressDoesNotMislabelTicketPreparationFailure 验证 Codex Ticket Egress Does Not Mislabel Ticket Preparation Failure 场景
func TestCodexTicketEgressDoesNotMislabelTicketPreparationFailure(t *testing.T) {
	var traceCalls, ticketCalls atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cdn-cgi/trace" {
			traceCalls.Add(1)
			_, _ = io.WriteString(w, "ip=8.8.8.8\n")
			return
		}
		ticketCalls.Add(1)
	}))
	t.Cleanup(target.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target.URL+"/backend-api/codex/responses", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer invalid\nprivate-secret")
	var result service.OpenAICodexTicketEgressResult
	_, err = doCodexTicketWithEgress(target.Client(), req, func(value service.OpenAICodexTicketEgressResult) { result = value }, time.Second)
	require.Error(t, err)
	require.Equal(t, int64(1), traceCalls.Load(), "the anonymous trace succeeds without the invalid ticket header")
	require.Zero(t, ticketCalls.Load())
	require.Empty(t, result.IP)
	require.Equal(t, "ticket_connection_unavailable", result.Error.Reason)
	require.Zero(t, result.Error.HTTPStatus)
}

// TestCodexTicketEgressTraceReportsProtocolAndRequestFailures 验证 Codex Ticket Egress Trace Reports Protocol And Request Failures 场景
func TestCodexTicketEgressTraceReportsProtocolAndRequestFailures(t *testing.T) {
	for _, scenario := range []string{"protocol", "request", "network"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: codexTicketEgressRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				if scenario == "network" {
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("proxy-user:proxy-secret@private-host")}
				}
				return &http.Response{StatusCode: http.StatusOK, ProtoMajor: 2, Body: io.NopCloser(strings.NewReader("ip=8.8.8.8\n"))}, nil
			})}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
			require.NoError(t, err)
			if scenario == "request" {
				req.URL.Host = "[invalid-host"
			}
			result, conn := traceCodexTicketEgress(client, req, time.Second)
			require.Empty(t, result.IP)
			require.Nil(t, conn)
			require.NotNil(t, result.Error)
			want := map[string]string{"protocol": "unsupported_protocol", "request": "request_error", "network": "network_error"}
			require.Equal(t, want[scenario], result.Error.Reason)
			require.Zero(t, result.Error.HTTPStatus)
			if scenario == "request" {
				require.Zero(t, calls)
			} else {
				require.Equal(t, 1, calls)
			}
		})
	}
}

// TestCodexTicketEgressClassifiesWrappedContextAndNetworkTimeouts 验证 Codex Ticket Egress Classifies Wrapped Context And Network Timeouts 场景
func TestCodexTicketEgressClassifiesWrappedContextAndNetworkTimeouts(t *testing.T) {
	for _, tt := range []struct {
		err      error
		fallback string
		want     string
	}{
		{fmt.Errorf("private detail: %w", context.Canceled), "network_error", "canceled"},
		{fmt.Errorf("private detail: %w", context.DeadlineExceeded), "network_error", "timeout"},
		{&net.OpError{Op: "read", Err: &net.DNSError{IsTimeout: true}}, "response_read_error", "timeout"},
		{io.ErrUnexpectedEOF, "response_read_error", "response_read_error"},
		{errors.New("private-token"), "network_error", "network_error"},
	} {
		require.Equal(t, tt.want, codexTicketEgressErrorReason(tt.err, tt.fallback))
	}
}
