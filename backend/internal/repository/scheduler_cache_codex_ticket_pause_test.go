package repository

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestSchedulerCachePreservesCodexTicketQuotaPause 验证 Scheduler Cache Preserves Codex Ticket Quota Pause 场景
func TestSchedulerCachePreservesCodexTicketQuotaPause(t *testing.T) {
	now := time.Now().UTC()
	account := service.Account{
		ID: 42, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true,
		Extra: map[string]any{
			"codex_5h_used_percent": 100,
			"codex_5h_reset_at":     now.Add(time.Hour).Format(time.RFC3339),
			service.OpenAICodexTicketHarvestPauseExtraKey: map[string]any{
				"observed_at": now.Format(time.RFC3339Nano),
				"usage": map[string]any{
					"codex_5h_used_percent": 100,
					"codex_5h_reset_at":     now.Add(time.Minute).Format(time.RFC3339),
				},
			},
		},
	}
	full, metadata, err := marshalSchedulerCacheAccount(account)
	require.NoError(t, err)
	for name, payload := range map[string][]byte{"full": full, "metadata": metadata} {
		t.Run(name, func(t *testing.T) {
			decoded, err := decodeCachedAccount(payload)
			require.NoError(t, err)
			require.Contains(t, decoded.Extra, service.OpenAICodexTicketHarvestPauseExtraKey)
			thresholds := map[string]int{service.PlatformOpenAI: 90}
			require.False(t, service.EvaluateAccountSchedulingThreshold(decoded, thresholds, now).ShouldPause)
			statuses := service.OpenAICodexTicketStatuses(decoded, config.OpenAICodexTicketConfig{Enabled: true, FailClosed: true}, now)
			require.Len(t, statuses, 2)
			for _, status := range statuses {
				require.True(t, status.HarvestPaused)
				require.False(t, status.Blocked)
			}
			require.True(t, service.EvaluateAccountSchedulingThreshold(decoded, thresholds, now.Add(2*time.Minute)).ShouldPause,
				"the probe exemption must expire with its original quota window")
			resetAt := now.Add(time.Hour)
			decoded.RateLimitResetAt = &resetAt
			require.False(t, decoded.IsSchedulable(), "real upstream rate limits still prevent scheduling")
		})
	}
}
