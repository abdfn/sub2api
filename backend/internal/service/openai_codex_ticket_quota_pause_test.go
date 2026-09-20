package service

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// codexTicketQuotaPauseHeaders 为打票回归测试提供 codex Ticket Quota Pause Headers 辅助实现
func codexTicketQuotaPauseHeaders(used5h, used7d float64, reset5h, reset7d int) http.Header {
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", strconv.FormatFloat(used7d, 'f', -1, 64))
	headers.Set("x-codex-primary-window-minutes", "10080")
	headers.Set("x-codex-primary-reset-after-seconds", strconv.Itoa(reset7d))
	headers.Set("x-codex-secondary-used-percent", strconv.FormatFloat(used5h, 'f', -1, 64))
	headers.Set("x-codex-secondary-window-minutes", "300")
	headers.Set("x-codex-secondary-reset-after-seconds", strconv.Itoa(reset5h))
	return headers
}

// codexTicketQuotaPauseAccount 为打票回归测试提供 codex Ticket Quota Pause Account 辅助实现
func codexTicketQuotaPauseAccount(id int64, now time.Time, used5h, used7d float64) *Account {
	account := ticketTestAccount(id)
	account.Status = StatusActive
	account.Extra = map[string]any{
		"codex_5h_used_percent":   used5h,
		"codex_7d_used_percent":   used7d,
		"codex_5h_reset_at":       now.Add(time.Hour).Format(time.RFC3339),
		"codex_7d_reset_at":       now.Add(24 * time.Hour).Format(time.RFC3339),
		"codex_usage_updated_at":  now.Add(-time.Minute).Format(time.RFC3339),
		"auto_pause_5h_threshold": 0.8,
		"auto_pause_7d_threshold": 0.8,
	}
	return account
}

// Reload through JSON, as the repository does, so persisted behavior does not
// depend on sharing an in-memory pause object with the service that created it.
func codexTicketQuotaPauseReload(t *testing.T, account *Account, updates map[string]any) *Account {
	t.Helper()
	reloaded := *account
	extra := maps.Clone(account.Extra)
	if extra == nil {
		extra = make(map[string]any)
	}
	maps.Copy(extra, updates)
	encoded, err := json.Marshal(extra)
	require.NoError(t, err)
	reloaded.Extra = nil
	require.NoError(t, json.Unmarshal(encoded, &reloaded.Extra))
	return &reloaded
}

// TestCodexTicketQuotaPause_StopsEveryModelWithoutBlockingUse 验证 Codex Ticket Quota Pause Stops Every Model Without Blocking Use 场景
func TestCodexTicketQuotaPause_StopsEveryModelWithoutBlockingUse(t *testing.T) {
	for i, tt := range []struct {
		name   string
		used5h float64
		used7d float64
	}{
		{name: "5h exhausted", used5h: 100, used7d: 20},
		{name: "7d exhausted", used5h: 20, used7d: 100},
		{name: "over quota", used5h: 105, used7d: 101},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now().UTC()
			account := codexTicketQuotaPauseAccount(int64(4100+i), now, tt.used5h, tt.used7d)
			oldTicket := &openAICodexTicket{
				AccountID: account.ID, Model: openAICodexTicketDefaultModel,
				State: fakeCodexTicketState(292), Length: 292,
				CapturedAt: now.Add(-55 * time.Minute), ExpiresAt: now.Add(5 * time.Minute), Attempts: 2,
			}
			account.Extra[openAICodexTicketExtraKey(oldTicket.Model)] = oldTicket
			originalExtra := maps.Clone(account.Extra)
			repo := &codexTicketRefreshRepo{accounts: []Account{*account}}
			var requests atomic.Int64
			upstream := &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}, nil
			}}
			svc := ticketTestService(t, config.OpenAICodexTicketConfig{
				Enabled: true, FailClosed: true, HarvestProxyURL: "http://proxy.example.com:8080",
			}, upstream)
			svc.accountRepo = repo
			svc.setOpenAICodexTicketNextHarvest(now.Add(time.Minute))

			paused, _ := shouldAutoPauseOpenAIAccountByQuota(context.Background(), account)
			require.True(t, paused, "the quota would normally trigger proactive account pausing")
			require.True(t, EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformOpenAI: 80}, now).ShouldPause)
			svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultSolModel)
			require.Equal(t, int64(1), requests.Load())
			require.Nil(t, account.RateLimitedAt)
			require.Nil(t, account.RateLimitResetAt)
			require.Equal(t, StatusActive, account.Status)
			for key, value := range originalExtra {
				require.Equal(t, value, account.Extra[key], "harvest errors must preserve existing account metadata: %s", key)
			}
			require.Len(t, repo.updates, 1, "a harvest 429 persists only its pause marker, not quota or model limits")
			require.NotNil(t, repo.updates[OpenAICodexTicketHarvestPauseExtraKey])

			statuses := svc.OpenAICodexTicketStatuses(context.Background(), account, now)
			require.Len(t, statuses, 2)
			require.True(t, statuses[0].Ready, "the existing valid ticket must survive")
			require.False(t, statuses[1].Ready)
			for _, status := range statuses {
				require.True(t, status.HarvestEnabled)
				require.True(t, status.HarvestPaused)
				require.False(t, status.Blocked)
				require.False(t, status.Harvesting)
				require.Nil(t, status.NextHarvestAt)
				require.False(t, svc.openAICodexTicketBlocksAccount(account, status.Model))
				headers := http.Header{}
				require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, status.Model, headers))
				if status.Ready {
					require.Equal(t, oldTicket.State, headers.Get(openAICodexTurnStateHeader))
				} else {
					require.Empty(t, headers.Get(openAICodexTurnStateHeader))
				}
				svc.probeOnceOpenAICodexTicket(context.Background(), account, status.Model)
			}
			svc.refreshOpenAICodexTickets(context.Background())
			require.Equal(t, int64(1), requests.Load(), "all subsequent model probes stop, even with an old repository snapshot")
			require.Equal(t, statuses, svc.OpenAICodexTicketStatuses(context.Background(), account, now), "skipped probes must not change attempt counters")

			reloaded := codexTicketQuotaPauseReload(t, account, repo.updates)
			restarted := ticketTestService(t, svc.openAICodexTicketConfig(), upstream)
			restarted.probeOnceOpenAICodexTicket(context.Background(), reloaded, openAICodexTicketDefaultModel)
			require.Equal(t, int64(1), requests.Load(), "the pause must survive a process restart")
			paused, _ = shouldAutoPauseOpenAIAccountByQuota(context.Background(), reloaded)
			require.False(t, paused)
			require.False(t, EvaluateAccountSchedulingThreshold(reloaded, map[string]int{PlatformOpenAI: 80}, now).ShouldPause)
			for _, status := range OpenAICodexTicketStatuses(reloaded, svc.openAICodexTicketConfig(), now) {
				require.True(t, status.HarvestPaused)
				require.False(t, status.Blocked)
			}
			payload, err := json.Marshal(restarted.OpenAICodexTicketStatuses(context.Background(), reloaded, now))
			require.NoError(t, err)
			require.Contains(t, string(payload), `"harvest_paused":true`)
			require.NotContains(t, string(payload), oldTicket.State)
		})
	}
}

// TestCodexTicketQuotaPause_Requires429AndCurrentExhaustion 验证 Codex Ticket Quota Pause Requires429 And Current Exhaustion 场景
func TestCodexTicketQuotaPause_Requires429AndCurrentExhaustion(t *testing.T) {
	for i, tt := range []struct {
		name       string
		status     int
		used5h     float64
		used7d     float64
		headers    http.Header
		expired    bool
		wantPaused bool
	}{
		{name: "ordinary 429 retries", status: 429, used5h: 99.9, used7d: 40},
		{name: "missing quota retries", status: 429},
		{name: "503 with exhausted cache retries", status: 503, used5h: 100},
		{name: "503 with exhausted headers retries", status: 503, headers: codexTicketQuotaPauseHeaders(100, 20, 3600, 86400)},
		{name: "expired cached quota retries", status: 429, used5h: 100, expired: true},
		{name: "expired header window retries", status: 429, headers: codexTicketQuotaPauseHeaders(100, 20, 0, 86400)},
		{name: "fresh low headers supersede exhausted cache", status: 429, used5h: 100, used7d: 100, headers: codexTicketQuotaPauseHeaders(30, 40, 3600, 86400)},
		{name: "fresh exhausted 5h supersedes low cache", status: 429, used5h: 10, headers: codexTicketQuotaPauseHeaders(100, 20, 3600, 86400), wantPaused: true},
		{name: "fresh exhausted 7d supersedes low cache", status: 429, used5h: 10, headers: codexTicketQuotaPauseHeaders(20, 101, 3600, 86400), wantPaused: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now().UTC()
			account := codexTicketQuotaPauseAccount(int64(4200+i), now, tt.used5h, tt.used7d)
			if tt.expired {
				account.Extra["codex_5h_reset_at"] = now.Add(-time.Minute).Format(time.RFC3339)
			}
			repo := &codexTicketRefreshRepo{}
			var requests atomic.Int64
			svc := ticketTestService(t, config.OpenAICodexTicketConfig{
				Enabled: true, FailClosed: true, HarvestProxyURL: "http://proxy.example.com:8080",
			}, &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return &http.Response{StatusCode: tt.status, Header: tt.headers.Clone()}, nil
			}})
			svc.accountRepo = repo
			for range 2 {
				svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
			}
			wantRequests := int64(2)
			if tt.wantPaused {
				wantRequests = 1
			}
			require.Equal(t, wantRequests, requests.Load())
			for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), account, now) {
				require.Equal(t, tt.wantPaused, status.HarvestPaused)
				require.Equal(t, !tt.wantPaused, status.Blocked)
			}
			if !tt.wantPaused {
				require.Empty(t, repo.updates)
			} else {
				require.Len(t, repo.updates, 1)
			}
			require.Equal(t, tt.used5h, account.Extra["codex_5h_used_percent"], "probe headers must not rewrite global usage")
			require.Equal(t, tt.used7d, account.Extra["codex_7d_used_percent"])
		})
	}
}

// TestCodexTicketQuotaPause_RefreshKeepsOtherAccountsRunning 验证 Codex Ticket Quota Pause Refresh Keeps Other Accounts Running 场景
func TestCodexTicketQuotaPause_RefreshKeepsOtherAccountsRunning(t *testing.T) {
	now := time.Now().UTC()
	exhausted := codexTicketQuotaPauseAccount(4301, now, 100, 20)
	healthy := codexTicketQuotaPauseAccount(4302, now, 20, 30)
	repo := &codexTicketRefreshRepo{accounts: []Account{*exhausted, *healthy}}
	var requests atomic.Int64
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, FailClosed: true, HarvestProxyURL: "http://proxy.example.com:8080",
	}, &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: http.StatusTooManyRequests}, nil
	}})
	svc.accountRepo = repo
	svc.probeOnceOpenAICodexTicket(context.Background(), exhausted, openAICodexTicketDefaultModel)
	for range 2 {
		svc.refreshOpenAICodexTickets(context.Background())
	}
	require.Equal(t, int64(5), requests.Load(), "only the healthy account's two models continue each cycle")
	for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), exhausted, now) {
		require.True(t, status.HarvestPaused)
	}
	for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), healthy, now) {
		require.False(t, status.HarvestPaused)
		require.Equal(t, 2, status.Attempts)
		require.True(t, svc.openAICodexTicketBlocksAccount(healthy, status.Model))
		require.ErrorIs(t, svc.applyOpenAICodexTicket(context.Background(), healthy, status.Model, http.Header{}), ErrOpenAICodexTicketUnavailable)
	}
}

// TestCodexTicketQuotaPause_RecoversOnlyFromNewUsageOrOriginalReset 验证 Codex Ticket Quota Pause Recovers Only From New Usage Or Original Reset 场景
func TestCodexTicketQuotaPause_RecoversOnlyFromNewUsageOrOriginalReset(t *testing.T) {
	now := time.Now().UTC()
	account := codexTicketQuotaPauseAccount(4401, now, 10, 20)
	repo := &codexTicketRefreshRepo{}
	var requests atomic.Int64
	upstream := &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			return &http.Response{StatusCode: 429, Header: codexTicketQuotaPauseHeaders(100, 20, 3600, 86400)}, nil
		}
		return codexTicketResponse(), nil
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, FailClosed: true, HarvestProxyURL: "http://proxy.example.com:8080",
	}, upstream)
	svc.accountRepo = repo
	svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	reloaded := codexTicketQuotaPauseReload(t, account, repo.updates)
	restarted := ticketTestService(t, svc.openAICodexTicketConfig(), upstream)
	restarted.accountRepo = repo
	for _, status := range restarted.OpenAICodexTicketStatuses(context.Background(), reloaded, now) {
		require.True(t, status.HarvestPaused, "old low cached usage must not erase a pause observed in fresh response headers")
	}
	restarted.probeOnceOpenAICodexTicket(context.Background(), reloaded, openAICodexTicketDefaultSolModel)
	require.Equal(t, int64(1), requests.Load())

	for _, status := range OpenAICodexTicketStatuses(reloaded, svc.openAICodexTicketConfig(), now.Add(2*time.Hour)) {
		require.False(t, status.HarvestPaused, "the triggering 5h window resets before the unrelated 7d window")
		require.True(t, status.Blocked)
	}

	// A trusted newer observation ends the pause even before the original reset.
	// A value of 90 also verifies that ordinary proactive threshold rules resume.
	reloaded.Extra["codex_5h_used_percent"] = 90.0
	reloaded.Extra["codex_usage_updated_at"] = now.Add(time.Minute).Format(time.RFC3339)
	for _, status := range restarted.OpenAICodexTicketStatuses(context.Background(), reloaded, time.Now()) {
		require.False(t, status.HarvestPaused)
		require.True(t, status.Blocked)
	}
	paused, _ := shouldAutoPauseOpenAIAccountByQuota(context.Background(), reloaded)
	require.True(t, paused, "quota exemption ends with the harvest pause")
	require.True(t, EvaluateAccountSchedulingThreshold(reloaded, map[string]int{PlatformOpenAI: 80}, time.Now()).ShouldPause)
	restarted.probeOnceOpenAICodexTicket(context.Background(), reloaded, openAICodexTicketDefaultSolModel)
	require.Equal(t, int64(2), requests.Load())
	statuses := restarted.OpenAICodexTicketStatuses(context.Background(), reloaded, time.Now())
	require.True(t, statuses[1].Ready)
	require.False(t, statuses[1].HarvestPaused)
}

type codexTicketQuotaPauseFailingRepo struct {
	codexTicketRefreshRepo
}

// UpdateExtra 为打票回归测试提供 Update Extra 辅助实现
func (*codexTicketQuotaPauseFailingRepo) UpdateExtra(context.Context, int64, map[string]any) error {
	return errors.New("storage temporarily unavailable")
}

// GetByID 为打票回归测试提供 Get By ID 辅助实现
func (r *codexTicketQuotaPauseFailingRepo) GetByID(_ context.Context, accountID int64) (*Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == accountID {
			return &r.accounts[i], nil
		}
	}
	return nil, errors.New("account not found")
}

// TestCodexTicketQuotaPause_ImmediateEvenWhenPersistenceFails 验证 Codex Ticket Quota Pause Immediate Even When Persistence Fails 场景
func TestCodexTicketQuotaPause_ImmediateEvenWhenPersistenceFails(t *testing.T) {
	now := time.Now().UTC()
	account := codexTicketQuotaPauseAccount(4501, now, 100, 20)
	account.Schedulable = true
	var requests atomic.Int64
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, FailClosed: true, HarvestProxyURL: "http://proxy.example.com:8080",
	}, &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: 429}, nil
	}})
	svc.accountRepo = &codexTicketQuotaPauseFailingRepo{}
	svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultSolModel)
	require.Equal(t, int64(1), requests.Load())
	for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), account, time.Now()) {
		require.True(t, status.HarvestPaused)
		require.False(t, status.Blocked)
	}
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultSolModel, http.Header{}))
	require.NotContains(t, account.Extra, OpenAICodexTicketHarvestPauseExtraKey, "the scheduler still sees the original snapshot")
	paused, _ := shouldAutoPauseOpenAIAccountByQuota(context.Background(), account)
	require.True(t, paused, "the unchanged persisted snapshot alone remains over the configured quota")
	ctx := svc.withOpenAIQuotaAutoPauseContext(context.Background())
	paused, _ = shouldAutoPauseOpenAIAccountByQuota(ctx, account)
	require.False(t, paused, "the gateway context must expose its immediate in-memory harvest pause")
	otherService := ticketTestService(t, svc.openAICodexTicketConfig(), nil)
	paused, _ = shouldAutoPauseOpenAIAccountByQuota(otherService.withOpenAIQuotaAutoPauseContext(context.Background()), account)
	require.True(t, paused, "in-memory pauses must stay isolated to the service that observed them")

	// Both scheduling engines receive the same context as normal gateway requests.
	scheduler := &defaultOpenAIAccountScheduler{service: svc}
	for _, model := range svc.openAICodexTicketConfig().Models {
		require.True(t, isOpenAICompatibleAccountEligibleForRequest(ctx, account, PlatformOpenAI, model, false, ""))
		require.True(t, scheduler.isAccountRequestCompatible(ctx, account, OpenAIAccountScheduleRequest{RequestedModel: model}))
	}
	delete(account.Extra, "auto_pause_5h_threshold")
	delete(account.Extra, "auto_pause_7d_threshold")
	globalCtx := withOpenAIQuotaAutoPauseSettings(context.Background(), OpsOpenAIAccountQuotaAutoPauseSettings{
		DefaultThreshold5h: 0.8, DefaultThreshold7d: 0.8,
	})
	paused, _ = shouldAutoPauseOpenAIAccountByQuota(globalCtx, account)
	require.True(t, paused, "global quota settings would otherwise pause this exhausted account")
	ctx = svc.withOpenAIQuotaAutoPauseContext(globalCtx)
	paused, _ = shouldAutoPauseOpenAIAccountByQuota(ctx, account)
	require.False(t, paused, "global quota settings must also honor the live harvest pause")

	resetAt := now.Add(time.Hour)
	account.RateLimitResetAt = &resetAt
	for _, model := range svc.openAICodexTicketConfig().Models {
		require.False(t, isOpenAICompatibleAccountEligibleForRequest(ctx, account, PlatformOpenAI, model, false, ""),
			"a real account cooldown must continue to exclude every model")
	}
	account.RateLimitResetAt = nil
	account.Extra[modelRateLimitsKey] = map[string]any{
		openAICodexTicketDefaultModel: map[string]any{
			"rate_limit_reset_at": resetAt.Format(time.RFC3339),
		},
	}
	require.False(t, isOpenAICompatibleAccountEligibleForRequest(ctx, account, PlatformOpenAI, openAICodexTicketDefaultModel, false, ""),
		"a real model cooldown must remain effective")
	require.True(t, isOpenAICompatibleAccountEligibleForRequest(ctx, account, PlatformOpenAI, openAICodexTicketDefaultSolModel, false, ""),
		"a model cooldown must preserve the other model's eligibility")
}

// TestCodexTicketQuotaPause_FreshHeadersOverrideMismatchedCachedIdentity 验证 Codex Ticket Quota Pause Fresh Headers Override Mismatched Cached Identity 场景
func TestCodexTicketQuotaPause_FreshHeadersOverrideMismatchedCachedIdentity(t *testing.T) {
	for i, freshHeaders := range []bool{false, true} {
		t.Run(strconv.FormatBool(freshHeaders), func(t *testing.T) {
			now := time.Now().UTC()
			account := codexTicketQuotaPauseAccount(int64(4700+i), now, 100, 20)
			account.Extra["chatgpt_account_id"] = "old-account-identity"
			repo := &codexTicketRefreshRepo{}
			svc := ticketTestService(t, config.OpenAICodexTicketConfig{
				Enabled: true, FailClosed: true, HarvestProxyURL: "http://proxy.example.com:8080",
			}, &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
				headers := http.Header{}
				if freshHeaders {
					headers = codexTicketQuotaPauseHeaders(100, 20, 3600, 86400)
				}
				return &http.Response{StatusCode: 429, Header: headers}, nil
			}})
			svc.accountRepo = repo
			svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
			for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), account, time.Now()) {
				require.Equal(t, freshHeaders, status.HarvestPaused, "response headers belong to this request, while mismatched cached usage does not")
			}
			if !freshHeaders {
				require.Empty(t, repo.updates)
				return
			}
			reloaded := codexTicketQuotaPauseReload(t, account, repo.updates)
			reloaded.Extra["codex_5h_used_percent"] = 20.0
			reloaded.Extra["codex_usage_updated_at"] = now.Add(time.Minute).Format(time.RFC3339)
			restarted := ticketTestService(t, svc.openAICodexTicketConfig(), nil)
			require.True(t, restarted.OpenAICodexTicketStatuses(context.Background(), reloaded, time.Now())[0].HarvestPaused,
				"a newer snapshot from a mismatched identity must not clear the observed pause")
			reloaded.Extra["chatgpt_account_id"] = reloaded.Credentials["chatgpt_account_id"]
			require.False(t, restarted.OpenAICodexTicketStatuses(context.Background(), reloaded, time.Now())[0].HarvestPaused,
				"trusted newer usage can clear the observed pause")
		})
	}
}

// TestCodexTicketQuotaPause_PartialHeadersPreserveCachedResetDeadline 验证 Codex Ticket Quota Pause Partial Headers Preserve Cached Reset Deadline 场景
func TestCodexTicketQuotaPause_PartialHeadersPreserveCachedResetDeadline(t *testing.T) {
	for i, tt := range []struct {
		name       string
		resetAfter int
		wantPaused bool
	}{
		{name: "expired countdown stays expired", resetAfter: 3600},
		{name: "active countdown keeps its original deadline", resetAfter: 10800, wantPaused: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now().UTC()
			account := codexTicketQuotaPauseAccount(int64(4800+i), now, 20, 100)
			delete(account.Extra, "codex_7d_reset_at")
			account.Extra["codex_7d_reset_after_seconds"] = tt.resetAfter
			account.Extra["codex_usage_updated_at"] = now.Add(-2 * time.Hour).Format(time.RFC3339)
			repo := &codexTicketRefreshRepo{}
			svc := ticketTestService(t, config.OpenAICodexTicketConfig{
				Enabled: true, FailClosed: true, HarvestProxyURL: "http://proxy.example.com:8080",
			}, &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
				// This response updates only the 5h window. Its new observation time
				// must not restart the 7d countdown inherited from the cached snapshot.
				headers := http.Header{}
				headers.Set("x-codex-secondary-used-percent", "20")
				headers.Set("x-codex-secondary-window-minutes", "300")
				headers.Set("x-codex-secondary-reset-after-seconds", "3600")
				return &http.Response{StatusCode: 429, Header: headers}, nil
			}})
			svc.accountRepo = repo
			svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
			for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), account, time.Now()) {
				require.Equal(t, tt.wantPaused, status.HarvestPaused)
			}
			if !tt.wantPaused {
				require.Empty(t, repo.updates)
				return
			}
			reloaded := codexTicketQuotaPauseReload(t, account, repo.updates)
			for _, status := range OpenAICodexTicketStatuses(reloaded, svc.openAICodexTicketConfig(), now.Add(2*time.Hour)) {
				require.False(t, status.HarvestPaused, "the inherited 7d countdown has elapsed at its original reset time")
			}
		})
	}
}

// TestCodexTicketQuotaPause_PreviousResponseIDHonorsUnpersistedPause 验证 Codex Ticket Quota Pause Previous Response IDHonors Unpersisted Pause 场景
func TestCodexTicketQuotaPause_PreviousResponseIDHonorsUnpersistedPause(t *testing.T) {
	ctx := context.Background()
	groupID := int64(23)
	account := codexTicketQuotaPauseAccount(5001, time.Now().UTC(), 100, 20)
	account.Schedulable = true
	account.Concurrency = 2
	account.GroupIDs = []int64{groupID}
	account.Extra["openai_oauth_responses_websockets_v2_enabled"] = true
	cache := &stubGatewayCache{}
	store := NewOpenAIWSStateStore(cache)
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, FailClosed: true, HarvestProxyURL: "http://proxy.example.com:8080",
	}, &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429}, nil
	}})
	svc.cfg.Gateway.OpenAIWS = newOpenAIWSV2TestConfig().Gateway.OpenAIWS
	svc.accountRepo = &codexTicketQuotaPauseFailingRepo{
		codexTicketRefreshRepo: codexTicketRefreshRepo{accounts: []Account{*account}},
	}
	svc.cache = cache
	svc.openaiWSStateStore = store
	svc.concurrencyService = NewConcurrencyService(stubConcurrencyCache{})
	svc.schedulerSnapshot = &SchedulerSnapshotService{cache: &openAISnapshotCacheStub{
		accountsByID: map[int64]*Account{account.ID: account},
	}}
	require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_quota_pause", account.ID, time.Hour))
	require.Zero(t, svc.ResolveAccountIDByPreviousResponseIDForScheduler(
		ctx, &groupID, "resp_quota_pause", openAICodexTicketDefaultModel, nil, "", false),
		"the unchanged quota snapshot initially blocks continuation selection")

	svc.probeOnceOpenAICodexTicket(ctx, account, openAICodexTicketDefaultModel)
	require.NotContains(t, account.Extra, OpenAICodexTicketHarvestPauseExtraKey)
	require.True(t, svc.OpenAICodexTicketStatuses(ctx, account, time.Now())[0].HarvestPaused)
	// Public callers supply an ordinary context. Each entry point must arrange
	// access to the live pause for both the cached account and the DB recheck.
	require.Equal(t, account.ID, svc.ResolveAccountIDByPreviousResponseIDForScheduler(
		context.Background(), &groupID, "resp_quota_pause", openAICodexTicketDefaultModel, nil, "", false))
	selection, err := svc.SelectAccountByPreviousResponseID(
		context.Background(), &groupID, "resp_quota_pause", openAICodexTicketDefaultModel, nil, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, account.ID, selection.Account.ID)
	require.True(t, selection.Acquired)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}
