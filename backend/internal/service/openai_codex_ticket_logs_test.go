package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestCodexTicketLogsRecordProbeResultsWithoutSensitiveData 验证 Codex Ticket Logs Record Probe Results Without Sensitive Data 场景
func TestCodexTicketLogsRecordProbeResultsWithoutSensitiveData(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		state       string
		err         error
		nilResponse bool
		reason      string
		event       string
	}{
		{name: "HTTP failure", status: 503, state: fakeCodexTicketState(292), reason: "http_error", event: "miss"},
		{name: "invalid token", status: 401, reason: "token_invalid", event: "miss"},
		{name: "missing header", status: 200, reason: "missing_state", event: "miss"},
		{name: "wrong length", status: 200, state: fakeCodexTicketState(332), reason: "length_mismatch", event: "miss"},
		{name: "invalid prefix", status: 200, state: strings.Repeat("S", 292), reason: "invalid_state", event: "miss"},
		{name: "success", status: 200, state: fakeCodexTicketState(292), reason: "harvested", event: "success"},
		{name: "network", err: errors.New("proxy socks5h://user:proxy-secret@private-host:1080 token bearer-secret"), reason: "network_error", event: "error"},
		{name: "timeout", err: context.DeadlineExceeded, reason: "timeout", event: "error"},
		{name: "canceled", err: context.Canceled, reason: "canceled", event: "error"},
		{name: "no response", nilResponse: true, reason: "request_error", event: "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
				if tt.err != nil || tt.nilResponse {
					return nil, tt.err
				}
				return &http.Response{
					StatusCode: tt.status,
					Header:     http.Header{http.CanonicalHeaderKey(openAICodexTurnStateHeader): []string{tt.state}, "X-Private": []string{"private-header"}},
					Body:       io.NopCloser(strings.NewReader("private-response-body")),
				}, nil
			}}
			svc := ticketTestService(t, config.OpenAICodexTicketConfig{
				Enabled: true, FailClosed: true, HarvestProxyURL: "socks5h://user:proxy-secret@private-host:1080",
			}, upstream)
			account := ticketTestAccount(41)
			account.Status = StatusActive
			account.Credentials["access_token"] = "bearer-secret"
			svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
			logs, err := svc.OpenAICodexTicketLogs(context.Background(), account, openAICodexTicketDefaultModel, time.Now())
			require.NoError(t, err)
			require.Len(t, logs.Entries, 2)
			require.Equal(t, "started", logs.Entries[0].Event)
			require.Equal(t, "request_started", logs.Entries[0].Reason)
			result := logs.Entries[1]
			require.Equal(t, tt.event, result.Event)
			require.Equal(t, tt.reason, result.Reason)
			require.Greater(t, result.ID, logs.Entries[0].ID)
			require.False(t, result.Time.Before(logs.Entries[0].Time))
			for _, entry := range logs.Entries {
				require.Equal(t, 1, entry.Attempt)
				require.Equal(t, 292, entry.TargetLength)
			}
			require.NotNil(t, result.DurationMS)
			require.GreaterOrEqual(t, *result.DurationMS, int64(0))
			if tt.err == nil && !tt.nilResponse {
				require.Equal(t, tt.status, result.HTTPStatus)
				require.NotNil(t, result.TicketLength)
				require.Equal(t, len(tt.state), *result.TicketLength)
			} else {
				require.Nil(t, result.TicketLength)
				require.Zero(t, result.HTTPStatus)
			}
			require.NotNil(t, logs.Status)
			require.Equal(t, tt.event == "success", logs.Status.Ready)
			require.False(t, logs.Status.Harvesting)
			require.Equal(t, 1, logs.Status.Attempts)
			payload, err := json.Marshal(logs)
			require.NoError(t, err)
			for _, secret := range []string{"proxy-secret", "private-host", "bearer-secret", "private-header", "private-response-body", fakeCodexTicketState(292), fakeCodexTicketState(332), strings.Repeat("S", 292)} {
				require.NotContains(t, string(payload), secret)
			}
		})
	}
}

// TestCodexTicketLogsTokenFailureDoesNotCountRequest 验证 Codex Ticket Logs Token Failure Does Not Count Request 场景
func TestCodexTicketLogsTokenFailureDoesNotCountRequest(t *testing.T) {
	upstream := &httpUpstreamRecorder{}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestProxyURL: "http://proxy.example.com"}, upstream)
	account := ticketTestAccount(41)
	account.Credentials = nil
	svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	logs, err := svc.OpenAICodexTicketLogs(context.Background(), account, openAICodexTicketDefaultModel, time.Now())
	require.NoError(t, err)
	require.Empty(t, upstream.requests)
	require.Len(t, logs.Entries, 1)
	require.Equal(t, "skipped", logs.Entries[0].Event)
	require.Equal(t, "token_error", logs.Entries[0].Reason)
	require.Equal(t, "ticket_connection_unavailable", logs.Entries[0].EgressError.Reason)
	require.Zero(t, logs.Entries[0].Attempt)
	require.Zero(t, logs.Status.Attempts)
}

// TestCodexTicketLogsExposeInFlightRequestWithoutStartingAnother 验证 Codex Ticket Logs Expose In Flight Request Without Starting Another 场景
func TestCodexTicketLogsExposeInFlightRequestWithoutStartingAnother(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	upstream := &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		close(started)
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
	defer func() { close(release); <-done }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	for range 3 {
		logs, err := svc.OpenAICodexTicketLogs(context.Background(), account, openAICodexTicketDefaultModel, time.Now())
		require.NoError(t, err)
		require.Len(t, logs.Entries, 1)
		require.Equal(t, "started", logs.Entries[0].Event)
		require.True(t, logs.Status.Harvesting)
		require.Equal(t, 1, logs.Status.Attempts)
	}
	require.Equal(t, int64(1), calls.Load())
}

// TestCodexTicketLogsPreserveAttemptRoundsAndTeamLength 验证 Codex Ticket Logs Preserve Attempt Rounds And Team Length 场景
func TestCodexTicketLogsPreserveAttemptRoundsAndTeamLength(t *testing.T) {
	var calls atomic.Int64
	upstream := &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
		resp := codexTicketResponse()
		if calls.Add(1) > 1 {
			resp.Header.Set(openAICodexTurnStateHeader, fakeCodexTicketState(332))
		}
		return resp, nil
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestProxyURL: "http://proxy.example.com"}, upstream)
	account := ticketTestAccount(41)
	account.Credentials["plan_type"] = "team"
	for range 3 {
		svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	}
	logs, err := svc.OpenAICodexTicketLogs(context.Background(), account, openAICodexTicketDefaultModel, time.Now())
	require.NoError(t, err)
	require.Len(t, logs.Entries, 6)
	for i, attempt := range []int{1, 1, 2, 2, 1, 1} {
		require.Equal(t, attempt, logs.Entries[i].Attempt)
		require.Equal(t, 332, logs.Entries[i].TargetLength)
	}
	require.Equal(t, "length_mismatch", logs.Entries[1].Reason)
	require.Equal(t, "harvested", logs.Entries[3].Reason)
	require.Equal(t, "harvested", logs.Entries[5].Reason)
}

// TestCodexTicketLogSnapshotsValidateModelAndIsolateAccount 验证 Codex Ticket Log Snapshots Validate Model And Isolate Account 场景
func TestCodexTicketLogSnapshotsValidateModelAndIsolateAccount(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Models: []string{"custom-model", "another-model"}}, nil)
	account := ticketTestAccount(41)
	for _, target := range []struct {
		id     int64
		model  string
		reason string
	}{
		{41, "custom-model", "first"}, {41, "another-model", "second"}, {42, "custom-model", "third"},
	} {
		svc.openaiCodexTicketLogs.append(target.id, target.model, OpenAICodexTicketLogEntry{Reason: target.reason})
	}
	logs, err := svc.OpenAICodexTicketLogs(context.Background(), account, " custom-model ", time.Now())
	require.NoError(t, err)
	require.Equal(t, "custom-model", logs.Model)
	require.Len(t, logs.Entries, 1)
	require.Equal(t, "first", logs.Entries[0].Reason)
	require.Nil(t, logs.Status, "retained diagnostics remain readable while harvesting is disabled")
	require.Equal(t, OpenAICodexTicketLogLimit, logs.Limit)
	_, err = svc.OpenAICodexTicketLogs(context.Background(), account, openAICodexTicketDefaultModel, time.Now())
	require.ErrorIs(t, err, ErrOpenAICodexTicketLogModel)
	_, err = svc.OpenAICodexTicketLogs(context.Background(), account, "", time.Now())
	require.ErrorIs(t, err, ErrOpenAICodexTicketLogModel)
	logs, err = svc.OpenAICodexTicketLogs(context.Background(), ticketTestAccount(43), "custom-model", time.Now())
	require.NoError(t, err)
	require.NotNil(t, logs.Entries)
	require.Empty(t, logs.Entries)
}

// TestCodexTicketLogStoreBoundsHistoryAndCopiesSnapshots 验证 Codex Ticket Log Store Bounds History And Copies Snapshots 场景
func TestCodexTicketLogStoreBoundsHistoryAndCopiesSnapshots(t *testing.T) {
	var store openAICodexTicketLogStore
	length, duration := 292, int64(12)
	for i := 1; i <= OpenAICodexTicketLogLimit+25; i++ {
		store.append(41, "model", OpenAICodexTicketLogEntry{Attempt: i, TicketLength: &length, DurationMS: &duration})
	}
	length, duration = 1, 1
	entries := store.snapshot(41, "model")
	require.Len(t, entries, OpenAICodexTicketLogLimit)
	require.Equal(t, 26, entries[0].Attempt)
	require.Equal(t, OpenAICodexTicketLogLimit+25, entries[len(entries)-1].Attempt)
	for i, entry := range entries {
		require.Equal(t, 292, *entry.TicketLength)
		require.Equal(t, int64(12), *entry.DurationMS)
		if i > 0 {
			require.Greater(t, entry.ID, entries[i-1].ID)
		}
	}
	entries[0].Reason, *entries[0].TicketLength, *entries[0].DurationMS = "changed", 3, 4
	fresh := store.snapshot(41, "model")
	require.Empty(t, fresh[0].Reason)
	require.Equal(t, 292, *fresh[0].TicketLength)
	require.Equal(t, int64(12), *fresh[0].DurationMS)
}

// TestCodexTicketLogStoreEvictsLeastRecentlyUsedStream 验证 Codex Ticket Log Store Evicts Least Recently Used Stream 场景
func TestCodexTicketLogStoreEvictsLeastRecentlyUsedStream(t *testing.T) {
	var store openAICodexTicketLogStore
	for id := 1; id <= openAICodexTicketLogMaxStreams; id++ {
		store.append(int64(id), "model", OpenAICodexTicketLogEntry{})
	}
	require.Len(t, store.snapshot(1, "model"), 1)
	store.append(openAICodexTicketLogMaxStreams+1, "model", OpenAICodexTicketLogEntry{})
	require.Len(t, store.streams, openAICodexTicketLogMaxStreams)
	require.Len(t, store.snapshot(1, "model"), 1)
	require.Empty(t, store.snapshot(2, "model"))
	require.Len(t, store.snapshot(openAICodexTicketLogMaxStreams+1, "model"), 1)
}

// TestCodexTicketLogStoreConcurrentSnapshotsRemainOrdered 验证 Codex Ticket Log Store Concurrent Snapshots Remain Ordered 场景
func TestCodexTicketLogStoreConcurrentSnapshotsRemainOrdered(t *testing.T) {
	var store openAICodexTicketLogStore
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				store.append(41, "model", OpenAICodexTicketLogEntry{Attempt: i})
				entries := store.snapshot(41, "model")
				for j := 1; j < len(entries); j++ {
					if entries[j].ID <= entries[j-1].ID {
						t.Error("snapshot is not ordered by increasing ID")
					}
				}
			}
		}()
	}
	wg.Wait()
	require.Len(t, store.snapshot(41, "model"), OpenAICodexTicketLogLimit)
}

// TestCodexTicketLogsBindObservedIPToEachAttemptRound 验证 Codex Ticket Logs Bind Observed IPTo Each Attempt Round 场景
func TestCodexTicketLogsBindObservedIPToEachAttemptRound(t *testing.T) {
	ips := []string{"8.8.8.8", "1.1.1.1", "", "192.168.1.1", "proxy-secret@private-host:1080"}
	calls := 0
	upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		observer := OpenAICodexTicketEgressObserverFromContext(req.Context())
		require.NotNil(t, observer)
		if ip := ips[calls]; ip != "" {
			observer(OpenAICodexTicketEgressResult{IP: ip})
		}
		calls++
		return codexTicketResponse(), nil
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestProxyURL: "http://proxy.example.com"}, upstream)
	account := ticketTestAccount(41)
	for range ips {
		svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	}
	entries := svc.openaiCodexTicketLogs.snapshot(account.ID, openAICodexTicketDefaultModel)
	require.Len(t, entries, 2*len(ips))
	for round, wantIP := range []string{"8.8.8.8", "1.1.1.1", "", "", ""} {
		for _, entry := range entries[round*2 : round*2+2] {
			require.Equal(t, 1, entry.Attempt, "each successful round resets the counter")
			require.Equal(t, wantIP, entry.EgressIP)
			require.Empty(t, entry.EgressCountryCode)
			payload, err := json.Marshal(entry)
			require.NoError(t, err)
			if wantIP == "" {
				require.NotContains(t, string(payload), "egress_ip")
			}
		}
	}
	require.Nil(t, svc.openaiCodexTicketCountries.client, "harvesting does not start a country lookup")
}

// TestCodexTicketLogsExposeConfirmedIPWhileRequestIsInFlight 验证 Codex Ticket Logs Expose Confirmed IPWhile Request Is In Flight 场景
func TestCodexTicketLogsExposeConfirmedIPWhileRequestIsInFlight(t *testing.T) {
	observed, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		OpenAICodexTicketEgressObserverFromContext(req.Context())(OpenAICodexTicketEgressResult{IP: "8.8.8.8"})
		close(observed)
		<-release
		return nil, errors.New("private upstream error")
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestProxyURL: "http://proxy.example.com"}, upstream)
	account := ticketTestAccount(41)
	go func() {
		defer close(done)
		svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	}()
	defer func() { close(release); <-done }()
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("request did not observe its connection")
	}
	entries := svc.openaiCodexTicketLogs.snapshot(account.ID, openAICodexTicketDefaultModel)
	require.Len(t, entries, 1)
	require.Equal(t, "started", entries[0].Event)
	require.Equal(t, "8.8.8.8", entries[0].EgressIP)
}

// TestCodexTicketLogsKeepConfirmedIPOnProbeError 验证 Codex Ticket Logs Keep Confirmed IPOn Probe Error 场景
func TestCodexTicketLogsKeepConfirmedIPOnProbeError(t *testing.T) {
	upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		OpenAICodexTicketEgressObserverFromContext(req.Context())(OpenAICodexTicketEgressResult{IP: "8.8.8.8"})
		return nil, errors.New("private upstream error")
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestProxyURL: "http://proxy.example.com"}, upstream)
	account := ticketTestAccount(41)
	svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	entries := svc.openaiCodexTicketLogs.snapshot(account.ID, openAICodexTicketDefaultModel)
	require.Len(t, entries, 2)
	require.Equal(t, "network_error", entries[1].Reason)
	for _, entry := range entries {
		require.Equal(t, "8.8.8.8", entry.EgressIP)
	}
}

// TestCodexTicketLogEgressUpdatesUseIDsAndStayInTheirStream 验证 Codex Ticket Log Egress Updates Use IDs And Stay In Their Stream 场景
func TestCodexTicketLogEgressUpdatesUseIDsAndStayInTheirStream(t *testing.T) {
	var store openAICodexTicketLogStore
	first := store.append(41, "first", OpenAICodexTicketLogEntry{Attempt: 1})
	second := store.append(41, "first", OpenAICodexTicketLogEntry{Attempt: 1})
	store.append(41, "other", OpenAICodexTicketLogEntry{Attempt: 1})
	store.append(42, "first", OpenAICodexTicketLogEntry{Attempt: 1})
	store.setEgressResult(41, "first", second, OpenAICodexTicketEgressResult{IP: "1.1.1.1"})
	store.setEgressResult(41, "other", first, OpenAICodexTicketEgressResult{IP: "8.8.8.8"})
	store.setEgressResult(42, "first", first, OpenAICodexTicketEgressResult{IP: "8.8.8.8"})
	entries := store.snapshot(41, "first")
	require.Empty(t, entries[0].EgressIP)
	require.Equal(t, "1.1.1.1", entries[1].EgressIP)
	require.Empty(t, store.snapshot(41, "other")[0].EgressIP)
	require.Empty(t, store.snapshot(42, "first")[0].EgressIP)
	for range OpenAICodexTicketLogLimit {
		store.append(41, "first", OpenAICodexTicketLogEntry{Attempt: 1})
	}
	store.setEgressResult(41, "first", first, OpenAICodexTicketEgressResult{IP: "8.8.8.8"})
	for _, entry := range store.snapshot(41, "first") {
		require.Empty(t, entry.EgressIP, "an evicted log must not update a newer attempt")
	}
}
