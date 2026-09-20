//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestCodexTicketQuotaPause_Business429StillLimitsAccount 验证 Codex Ticket Quota Pause Business429 Still Limits Account 场景
func TestCodexTicketQuotaPause_Business429StillLimitsAccount(t *testing.T) {
	account := codexTicketQuotaPauseAccount(4601, time.Now().UTC(), 100, 20)
	probeRepo := &codexTicketRefreshRepo{}
	harvester := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, FailClosed: true, HarvestProxyURL: "http://proxy.example.com:8080",
	}, &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429}, nil
	}})
	harvester.accountRepo = probeRepo
	harvester.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	account = codexTicketQuotaPauseReload(t, account, probeRepo.updates)
	require.True(t, harvester.OpenAICodexTicketStatuses(context.Background(), account, time.Now())[0].HarvestPaused)

	repo := &openAI429SnapshotRepo{}
	limiter := NewRateLimitService(repo, nil, nil, nil, nil)
	limiter.handle429(context.Background(), account, codexTicketQuotaPauseHeaders(100, 20, 3600, 86400), nil)
	require.Equal(t, account.ID, repo.rateLimitedID, "the harvest-only pause must not disable ordinary upstream 429 handling")
	require.Equal(t, 100.0, repo.updatedExtra["codex_5h_used_percent"])
	require.Equal(t, 20.0, repo.updatedExtra["codex_7d_used_percent"])
}

// TestCodexTicketQuotaPause_GlobalSchedulingThresholdHonorsUnpersistedPause 验证 Codex Ticket Quota Pause Global Scheduling Threshold Honors Unpersisted Pause 场景
func TestCodexTicketQuotaPause_GlobalSchedulingThresholdHonorsUnpersistedPause(t *testing.T) {
	settings := newSettingServiceForPlatformThresholdTest(map[string]string{
		SettingKeyAccountSchedulingThresholds: `{"openai":80}`,
	})
	t.Cleanup(func() {
		accountSchedulingThresholdsSF.Forget(SettingKeyAccountSchedulingThresholds)
		accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{})
	})
	limitRepo := &rateLimitAccountRepoStub{}
	limiter := NewRateLimitService(limitRepo, nil, &config.Config{}, nil, nil)
	limiter.SetSettingService(settings)
	account := codexTicketQuotaPauseAccount(4901, time.Now().UTC(), 100, 20)
	account.Schedulable = true
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, FailClosed: true, HarvestProxyURL: "http://proxy.example.com:8080",
	}, &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429}, nil
	}})
	svc.rateLimitService = limiter
	svc.accountRepo = &codexTicketQuotaPauseFailingRepo{}
	ctx := svc.withOpenAIQuotaAutoPauseContext(context.Background())

	before := *account
	require.True(t, svc.isOpenAIAccountBlockedBySchedulingThreshold(ctx, &before),
		"the configured global percentage threshold must be active for this test")
	require.Equal(t, 1, limitRepo.tempCalls)
	svc.probeOnceOpenAICodexTicket(ctx, account, openAICodexTicketDefaultModel)
	require.NotContains(t, account.Extra, OpenAICodexTicketHarvestPauseExtraKey)
	require.True(t, svc.OpenAICodexTicketStatuses(ctx, account, time.Now())[0].HarvestPaused)
	require.False(t, svc.isOpenAIAccountBlockedBySchedulingThreshold(ctx, account),
		"the live pause must bypass threshold pausing before storage or scheduler snapshots catch up")
	require.Equal(t, 1, limitRepo.tempCalls, "harvest pausing must not persist an additional account cooldown")
	require.Nil(t, account.TempUnschedulableUntil)
	require.Nil(t, account.RateLimitResetAt)
	for _, model := range svc.openAICodexTicketConfig().Models {
		require.False(t, svc.openAICodexTicketBlocksAccount(account, model))
		require.NoError(t, svc.applyOpenAICodexTicket(ctx, account, model, http.Header{}))
	}
}
