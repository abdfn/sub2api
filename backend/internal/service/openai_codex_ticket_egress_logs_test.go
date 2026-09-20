package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestCodexTicketLogsExposeEgressFailureBeforeTicketCompletes 验证 Codex Ticket Logs Expose Egress Failure Before Ticket Completes 场景
func TestCodexTicketLogsExposeEgressFailureBeforeTicketCompletes(t *testing.T) {
	observed, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		OpenAICodexTicketEgressObserverFromContext(req.Context())(OpenAICodexTicketEgressResult{
			Error: &OpenAICodexTicketEgressError{Reason: "http_error", HTTPStatus: http.StatusForbidden},
		})
		close(observed)
		<-release
		return codexTicketResponse(), nil
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestProxyURL: "http://proxy.example.com"}, upstream)
	account := ticketTestAccount(41)
	account.Status = StatusActive
	go func() {
		defer close(done)
		svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	}()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-done
	}()
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("diagnostic was not reported")
	}
	logs, err := svc.OpenAICodexTicketLogs(context.Background(), account, openAICodexTicketDefaultModel, time.Now())
	require.NoError(t, err)
	require.Len(t, logs.Entries, 1)
	require.Equal(t, "started", logs.Entries[0].Event)
	require.True(t, logs.Status.Harvesting)
	require.Equal(t, &OpenAICodexTicketEgressError{Reason: "http_error", HTTPStatus: 403}, logs.Entries[0].EgressError)
	close(release)
	<-done
	entries := svc.openaiCodexTicketLogs.snapshot(account.ID, openAICodexTicketDefaultModel)
	require.Len(t, entries, 2)
	require.Equal(t, entries[0].EgressError, entries[1].EgressError)
	require.Equal(t, "success", entries[1].Event)
	require.Equal(t, http.StatusOK, entries[1].HTTPStatus, "ticket HTTP status is independent from trace HTTP status")
}

// TestCodexTicketLogsKeepEgressFailuresSeparateAcrossAttemptRounds 验证 Codex Ticket Logs Keep Egress Failures Separate Across Attempt Rounds 场景
func TestCodexTicketLogsKeepEgressFailuresSeparateAcrossAttemptRounds(t *testing.T) {
	reasons := []string{"timeout", "missing_ip", "connection_changed"}
	calls := 0
	upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		OpenAICodexTicketEgressObserverFromContext(req.Context())(OpenAICodexTicketEgressResult{
			Error: &OpenAICodexTicketEgressError{Reason: reasons[calls]},
		})
		calls++
		return codexTicketResponse(), nil
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestProxyURL: "http://proxy.example.com"}, upstream)
	account := ticketTestAccount(41)
	for range reasons {
		svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	}
	entries := svc.openaiCodexTicketLogs.snapshot(account.ID, openAICodexTicketDefaultModel)
	require.Len(t, entries, 6)
	for i, entry := range entries {
		require.Equal(t, 1, entry.Attempt)
		require.Equal(t, reasons[i/2], entry.EgressError.Reason)
		require.Empty(t, entry.EgressIP)
	}
}

// TestCodexTicketLogsConfirmedIPClearsAndTakesPrecedenceOverFailure 验证 Codex Ticket Logs Confirmed IPClears And Takes Precedence Over Failure 场景
func TestCodexTicketLogsConfirmedIPClearsAndTakesPrecedenceOverFailure(t *testing.T) {
	upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		observe := OpenAICodexTicketEgressObserverFromContext(req.Context())
		observe(OpenAICodexTicketEgressResult{Error: &OpenAICodexTicketEgressError{Reason: "timeout"}})
		observe(OpenAICodexTicketEgressResult{IP: "8.8.8.8", Error: &OpenAICodexTicketEgressError{Reason: "http_error", HTTPStatus: 503}})
		observe(OpenAICodexTicketEgressResult{Error: &OpenAICodexTicketEgressError{Reason: "unknown"}})
		return codexTicketResponse(), nil
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestProxyURL: "http://proxy.example.com"}, upstream)
	account := ticketTestAccount(41)
	svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	entries := svc.openaiCodexTicketLogs.snapshot(account.ID, openAICodexTicketDefaultModel)
	require.Len(t, entries, 2)
	for _, entry := range entries {
		require.Equal(t, "8.8.8.8", entry.EgressIP)
		require.Nil(t, entry.EgressError)
		payload, err := json.Marshal(entry)
		require.NoError(t, err)
		require.NotContains(t, string(payload), "egress_error")
	}
}

// TestCodexTicketLogsFallbackDoesNotInferTraceFailureFromTicketError 验证 Codex Ticket Logs Fallback Does Not Infer Trace Failure From Ticket Error 场景
func TestCodexTicketLogsFallbackDoesNotInferTraceFailureFromTicketError(t *testing.T) {
	for _, tt := range []struct {
		name, reason string
		err          error
		nilResponse  bool
	}{
		{name: "success without observation", reason: "unknown"},
		{name: "main request timeout", reason: "ticket_connection_unavailable", err: context.DeadlineExceeded},
		{name: "main request network", reason: "ticket_connection_unavailable", err: errors.New("proxy-user:proxy-secret@private-host")},
		{name: "empty response", reason: "ticket_connection_unavailable", nilResponse: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
				if tt.err != nil || tt.nilResponse {
					return nil, tt.err
				}
				return codexTicketResponse(), nil
			}}
			svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestProxyURL: "http://proxy.example.com"}, upstream)
			account := ticketTestAccount(41)
			svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
			entries := svc.openaiCodexTicketLogs.snapshot(account.ID, openAICodexTicketDefaultModel)
			require.Len(t, entries, 2)
			for _, entry := range entries {
				require.Equal(t, tt.reason, entry.EgressError.Reason)
				require.Zero(t, entry.EgressError.HTTPStatus)
			}
		})
	}
}

// TestCodexTicketLogsEgressDiagnosticsAreCopiedAndScopedToLogID 验证 Codex Ticket Logs Egress Diagnostics Are Copied And Scoped To Log ID 场景
func TestCodexTicketLogsEgressDiagnosticsAreCopiedAndScopedToLogID(t *testing.T) {
	var store openAICodexTicketLogStore
	diagnostic := &OpenAICodexTicketEgressError{Reason: "http_error", HTTPStatus: 403}
	first := store.append(41, "model", OpenAICodexTicketLogEntry{Attempt: 1, EgressError: diagnostic})
	second := store.append(41, "model", OpenAICodexTicketLogEntry{Attempt: 1})
	store.append(41, "other-model", OpenAICodexTicketLogEntry{Attempt: 1})
	store.append(42, "model", OpenAICodexTicketLogEntry{Attempt: 1})
	diagnostic.Reason, diagnostic.HTTPStatus = "changed-after-append", 500
	entries := store.snapshot(41, "model")
	require.Equal(t, &OpenAICodexTicketEgressError{Reason: "http_error", HTTPStatus: 403}, entries[0].EgressError)
	entries[0].EgressError.Reason = "changed-in-snapshot"
	diagnostic = &OpenAICodexTicketEgressError{Reason: "timeout"}
	result := OpenAICodexTicketEgressResult{Error: diagnostic}
	store.setEgressResult(41, "model", second, result)
	store.setEgressResult(41, "other-model", second, result)
	store.setEgressResult(42, "model", second, result)
	diagnostic.Reason = "changed-after-update"
	entries = store.snapshot(41, "model")
	require.Equal(t, "http_error", entries[0].EgressError.Reason)
	require.Equal(t, "timeout", entries[1].EgressError.Reason)
	require.Nil(t, store.snapshot(41, "other-model")[0].EgressError)
	require.Nil(t, store.snapshot(42, "model")[0].EgressError)
	for range OpenAICodexTicketLogLimit {
		store.append(41, "model", OpenAICodexTicketLogEntry{Attempt: 1})
	}
	store.setEgressResult(41, "model", first, OpenAICodexTicketEgressResult{Error: &OpenAICodexTicketEgressError{Reason: "missing_ip"}})
	for _, entry := range store.snapshot(41, "model") {
		require.Nil(t, entry.EgressError, "an evicted ID must not update a reused attempt number")
	}
}

// TestCodexTicketLogsEgressDiagnosticsNeverExposeUnrecognizedText 验证 Codex Ticket Logs Egress Diagnostics Never Expose Unrecognized Text 场景
func TestCodexTicketLogsEgressDiagnosticsNeverExposeUnrecognizedText(t *testing.T) {
	for _, tt := range []struct {
		input OpenAICodexTicketEgressResult
		want  OpenAICodexTicketEgressError
	}{
		{OpenAICodexTicketEgressResult{Error: &OpenAICodexTicketEgressError{Reason: "proxy-secret@private-host", HTTPStatus: 403}}, OpenAICodexTicketEgressError{Reason: "unknown"}},
		{OpenAICodexTicketEgressResult{Error: &OpenAICodexTicketEgressError{Reason: "network_error", HTTPStatus: 500}}, OpenAICodexTicketEgressError{Reason: "network_error"}},
		{OpenAICodexTicketEgressResult{Error: &OpenAICodexTicketEgressError{Reason: "http_error", HTTPStatus: 9999}}, OpenAICodexTicketEgressError{Reason: "http_error"}},
		{OpenAICodexTicketEgressResult{IP: "proxy-secret@private-host"}, OpenAICodexTicketEgressError{Reason: "invalid_ip"}},
		{OpenAICodexTicketEgressResult{}, OpenAICodexTicketEgressError{Reason: "unknown"}},
	} {
		upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
			OpenAICodexTicketEgressObserverFromContext(req.Context())(tt.input)
			return codexTicketResponse(), nil
		}}
		svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestProxyURL: "http://proxy.example.com"}, upstream)
		account := ticketTestAccount(41)
		svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
		entries := svc.openaiCodexTicketLogs.snapshot(account.ID, openAICodexTicketDefaultModel)
		for _, entry := range entries {
			require.Equal(t, &tt.want, entry.EgressError)
		}
		payload, err := json.Marshal(entries)
		require.NoError(t, err)
		require.NotContains(t, string(payload), "proxy-secret")
		require.NotContains(t, string(payload), "private-host")
	}
}
