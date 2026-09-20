package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestCodexTicketHarvestStatusTracksMissRetrySuccessAndNewRound 验证 Codex Ticket Harvest Status Tracks Miss Retry Success And New Round 场景
func TestCodexTicketHarvestStatusTracksMissRetrySuccessAndNewRound(t *testing.T) {
	var calls atomic.Int64
	upstream := &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return codexTicketResponse(), nil
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, HarvestProxyURL: "socks5h://user:secret@proxy.example.com:1080", Models: []string{openAICodexTicketDefaultModel}, FailClosed: true,
	}, upstream)
	account := ticketTestAccount(41)
	account.Status = StatusActive
	status := func() OpenAICodexTicketStatus {
		return svc.OpenAICodexTicketStatuses(context.Background(), account, time.Now())[0]
	}

	svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	miss := status()
	require.True(t, miss.HarvestEnabled)
	require.Equal(t, 1, miss.Attempts)
	require.True(t, miss.Blocked)
	require.False(t, miss.Harvesting)
	require.Nil(t, miss.NextHarvestAt, "a standalone probe must not invent a loop deadline")

	svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	success := status()
	require.Equal(t, 2, success.Attempts)
	require.True(t, success.Ready, "a live ticket must appear before the account snapshot reloads")
	require.False(t, success.Blocked)
	require.False(t, success.Harvesting)
	require.Nil(t, success.NextHarvestAt)
	ticket := svc.lookupOpenAICodexTicket(account, openAICodexTicketDefaultModel)
	require.Equal(t, 2, ticket.Attempts)

	// Only successful tickets persist their round count, including across restart.
	account.Extra = map[string]any{openAICodexTicketExtraKey(ticket.Model): ticket}
	restarted := ticketTestService(t, svc.openAICodexTicketConfig(), nil)
	require.Equal(t, 2, restarted.OpenAICodexTicketStatuses(context.Background(), account, time.Now())[0].Attempts)

	svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	require.Equal(t, 1, status().Attempts, "a new refresh round starts its own count")
	payload, err := json.Marshal(status())
	require.NoError(t, err)
	require.NotContains(t, string(payload), "secret")
	require.NotContains(t, string(payload), ticket.State)
}

// TestCodexTicketHarvestStatusUsesSharedLoopDeadline 验证 Codex Ticket Harvest Status Uses Shared Loop Deadline 场景
func TestCodexTicketHarvestStatusUsesSharedLoopDeadline(t *testing.T) {
	account := ticketTestAccount(41)
	account.Status = StatusActive
	started := make(chan string, 2)
	releaseAstra := make(chan struct{})
	releaseSol := make(chan struct{})
	var mu sync.Mutex
	calls := make(map[string]int)
	upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		model := gjson.GetBytes(body, "model").String()
		mu.Lock()
		calls[model]++
		attempt := calls[model]
		mu.Unlock()
		if attempt > 1 {
			return codexTicketResponse(), nil
		}
		started <- model
		release := releaseAstra
		if model == openAICodexTicketDefaultSolModel {
			release = releaseSol
		}
		select {
		case <-release:
			return &http.Response{StatusCode: http.StatusServiceUnavailable}, nil
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, HarvestProxyURL: "http://proxy.example.com:8080", HarvestProbeIntervalSeconds: 1,
	}, upstream)
	svc.accountRepo = &codexTicketRefreshRepo{accounts: []Account{*account}}
	svc.StartOpenAICodexTicketHarvester()
	t.Cleanup(svc.StopOpenAICodexTicketHarvester)
	for range 2 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("both model probes did not start")
		}
	}
	statuses := func() []OpenAICodexTicketStatus {
		return svc.OpenAICodexTicketStatuses(context.Background(), account, time.Now())
	}
	for _, status := range statuses() {
		require.Equal(t, 1, status.Attempts)
		require.True(t, status.Harvesting)
		require.Nil(t, status.NextHarvestAt)
	}
	close(releaseAstra)
	require.Eventually(t, func() bool { return !statuses()[0].Harvesting }, time.Second, time.Millisecond)
	waiting := statuses()
	require.Nil(t, waiting[0].NextHarvestAt, "the next cycle is unknown while another model still runs")
	require.True(t, waiting[1].Harvesting)
	completedAt := time.Now()
	close(releaseSol)
	var scheduled []OpenAICodexTicketStatus
	require.Eventually(t, func() bool {
		scheduled = statuses()
		return scheduled[0].NextHarvestAt != nil && scheduled[1].NextHarvestAt != nil
	}, time.Second, time.Millisecond)
	require.Equal(t, scheduled[0].NextHarvestAt, scheduled[1].NextHarvestAt)
	require.False(t, scheduled[0].NextHarvestAt.Before(completedAt.Add(time.Second)))
	require.Equal(t, scheduled[0].NextHarvestAt, statuses()[0].NextHarvestAt, "reading status must not push back the deadline")
	inactive := *account
	inactive.Status = StatusError
	for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), &inactive, time.Now()) {
		require.Nil(t, status.NextHarvestAt)
		require.False(t, status.Harvesting)
	}
	require.Eventually(t, func() bool {
		got := statuses()
		return got[0].Ready && got[1].Ready
	}, 3*time.Second, time.Millisecond)
	for _, status := range statuses() {
		require.Equal(t, 2, status.Attempts)
		require.False(t, status.Harvesting)
		require.Nil(t, status.NextHarvestAt)
	}
	svc.StopOpenAICodexTicketHarvester()
	require.True(t, svc.openaiCodexTicketNextHarvest.IsZero())
}

// TestCodexTicketHarvestStatusSkipsUnsentAttempts 验证 Codex Ticket Harvest Status Skips Unsent Attempts 场景
func TestCodexTicketHarvestStatusSkipsUnsentAttempts(t *testing.T) {
	for _, proxy := range []string{"", "ftp://proxy.example.com", "http://proxy.example.com:99999", "http://proxy.example.com/path", "http://proxy.example.com:8080"} {
		t.Run(proxy, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{}
			svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestProxyURL: proxy}, upstream)
			account := ticketTestAccount(41)
			account.Status = StatusActive
			// The valid-proxy case stops at token resolution, before an outbound request.
			account.Credentials = nil
			svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
			require.Empty(t, upstream.requests)
			for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), account, time.Now()) {
				require.Equal(t, proxy == "http://proxy.example.com:8080", status.HarvestEnabled)
				require.Zero(t, status.Attempts)
				require.False(t, status.Harvesting)
				require.Nil(t, status.NextHarvestAt)
			}
		})
	}
}

// TestCodexTicketHarvestStatusUsesLiveSettings 验证 Codex Ticket Harvest Status Uses Live Settings 场景
func TestCodexTicketHarvestStatusUsesLiveSettings(t *testing.T) {
	repo := &codexTicketSettingRepo{codexPolicyMigrationRepoStub: &codexPolicyMigrationRepoStub{values: map[string]string{}}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: false, HarvestProxyURL: "http://fallback.example.com:8080"}, nil)
	svc.settingService = NewSettingService(repo, svc.cfg)
	account := ticketTestAccount(41)
	account.Status = StatusActive
	statuses := func() []OpenAICodexTicketStatus {
		return svc.OpenAICodexTicketStatuses(context.Background(), account, time.Now())
	}
	require.Empty(t, statuses())
	repo.values[SettingKeyOpenAICodexTicketEnabled] = "true"
	svc.settingService.InvalidateOpenAICodexTicketEnabledCache()
	require.True(t, statuses()[0].HarvestEnabled, "valid YAML proxy remains a fallback")
	repo.values[SettingKeyOpenAICodexTicketHarvestProxyURL] = "ftp://invalid.example.com"
	svc.settingService.InvalidateOpenAICodexTicketHarvestProxyCache()
	require.False(t, statuses()[0].HarvestEnabled)
	repo.values[SettingKeyOpenAICodexTicketHarvestProxyURL] = "socks5://live.example.com:1080"
	svc.settingService.InvalidateOpenAICodexTicketHarvestProxyCache()
	require.True(t, statuses()[0].HarvestEnabled)
	repo.values[SettingKeyOpenAICodexTicketEnabled] = "false"
	svc.settingService.InvalidateOpenAICodexTicketEnabledCache()
	require.Empty(t, statuses())
}
