package service

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestCodexTicketHarvestSkipsRateLimitedAccountsAndResumes 验证 Codex Ticket Harvest Skips Rate Limited Accounts And Resumes 场景
func TestCodexTicketHarvestSkipsRateLimitedAccountsAndResumes(t *testing.T) {
	for _, tt := range []struct {
		name     string
		planType string
		length   int
		refresh  bool
		clear    bool
	}{
		{name: "refresh personal after expiry", length: 292, refresh: true},
		{name: "refresh team after manual clear", planType: "team", length: 332, refresh: true, clear: true},
		{name: "direct personal after manual clear", length: 292, clear: true},
		{name: "direct premium after expiry", planType: "self_serve_business_prolite", length: 332},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now().UTC()
			resetAt := now.Add(time.Hour)
			limitedAt := now.Add(-time.Minute)
			account := ticketTestAccount(41)
			account.Status = StatusActive
			account.Credentials["plan_type"] = tt.planType
			account.RateLimitedAt = &limitedAt
			account.RateLimitResetAt = &resetAt
			previousTicket := &openAICodexTicket{
				AccountID: account.ID, Model: openAICodexTicketDefaultModel,
				State: fakeCodexTicketState(tt.length), Length: tt.length,
				CapturedAt: now.Add(-55 * time.Minute), ExpiresAt: now.Add(5 * time.Minute), Attempts: 2,
			}
			account.Extra = map[string]any{openAICodexTicketExtraKey(previousTicket.Model): previousTicket}
			repo := &codexTicketRefreshRepo{accounts: []Account{*account}}
			var requests atomic.Int64
			upstream := &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				response := codexTicketResponse()
				response.Header.Set(openAICodexTurnStateHeader, fakeCodexTicketState(tt.length))
				return response, nil
			}}
			svc := ticketTestService(t, config.OpenAICodexTicketConfig{
				Enabled: true, HarvestProxyURL: "http://proxy.example.com:8080",
			}, upstream)
			svc.accountRepo = repo
			next := now.Add(time.Minute)
			svc.setOpenAICodexTicketNextHarvest(next)
			run := func() {
				if tt.refresh {
					svc.refreshOpenAICodexTickets(context.Background())
					return
				}
				for _, model := range svc.openAICodexTicketConfig().Models {
					svc.probeOnceOpenAICodexTicket(context.Background(), account, model)
				}
			}

			run()
			require.Zero(t, requests.Load(), "limited accounts must neither refresh existing tickets nor fetch missing tickets")
			require.Empty(t, repo.updates)
			require.Empty(t, svc.openaiCodexTicketProgress, "skipped probes must not increment attempts")
			require.Equal(t, previousTicket, svc.lookupOpenAICodexTicket(account, previousTicket.Model))
			statuses := svc.OpenAICodexTicketStatuses(context.Background(), account, now)
			require.Len(t, statuses, 2)
			require.True(t, statuses[0].Ready, "rate limits must preserve an existing valid ticket")
			require.Equal(t, 2, statuses[0].Attempts)
			require.False(t, statuses[1].Ready)
			for _, status := range statuses {
				require.True(t, status.HarvestEnabled, "the global feature remains enabled during a rate limit")
				require.False(t, status.Harvesting)
				require.Nil(t, status.NextHarvestAt, "limited accounts must not show a pending probe countdown")
			}

			if tt.clear {
				account.RateLimitResetAt = nil
			} else {
				expired := now.Add(-time.Minute)
				account.RateLimitResetAt = &expired
			}
			repo.accounts[0] = *account
			for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), account, now) {
				require.Equal(t, &next, status.NextHarvestAt, "recovery must rejoin the next shared probe cycle")
			}
			run()
			require.Equal(t, int64(2), requests.Load())
			require.Len(t, repo.updates, 2)
			for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), account, time.Now()) {
				require.True(t, status.Ready)
				require.Equal(t, tt.length, status.Length)
				require.Equal(t, 1, status.Attempts)
			}
		})
	}
}
