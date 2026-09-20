package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type codexTicketTokenRepo struct {
	codexTicketRefreshRepo
	extraErr    error
	setErrorErr error
	errorCalls  map[int64]string
	extraCalls  int
}

// SetError 为打票回归测试提供 Set Error 辅助实现
func (r *codexTicketTokenRepo) SetError(_ context.Context, accountID int64, message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.errorCalls == nil {
		r.errorCalls = make(map[int64]string)
	}
	r.errorCalls[accountID] = message
	return r.setErrorErr
}

// UpdateExtra 为打票回归测试提供 Update Extra 辅助实现
func (r *codexTicketTokenRepo) UpdateExtra(ctx context.Context, accountID int64, updates map[string]any) error {
	r.mu.Lock()
	r.extraCalls++
	r.mu.Unlock()
	if r.extraErr != nil {
		return r.extraErr
	}
	return r.codexTicketRefreshRepo.UpdateExtra(ctx, accountID, updates)
}

type codexTicketTokenCache struct {
	OpenAITokenCache
	token string
}

// GetAccessToken 为打票回归测试提供 Get Access Token 辅助实现
func (c *codexTicketTokenCache) GetAccessToken(context.Context, string) (string, error) {
	return c.token, nil
}

type codexTicketConditionalTokenRepo struct {
	codexTicketTokenRepo
	currentToken   string
	conditionalErr error
	conditionalID  int64
	accountToken   string
	rejectedToken  string
	errorMessage   string
	status         string
}

// SetOpenAICodexTicketErrorIfTokenMatches 为打票回归测试提供 Set Open AICodex Ticket Error If Token Matches 辅助实现
func (r *codexTicketConditionalTokenRepo) SetOpenAICodexTicketErrorIfTokenMatches(_ context.Context, id int64, accountToken, rejectedToken, errorMsg string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conditionalID, r.accountToken, r.rejectedToken, r.errorMessage = id, accountToken, rejectedToken, errorMsg
	if r.conditionalErr != nil {
		return false, r.conditionalErr
	}
	if r.currentToken != accountToken && r.currentToken != rejectedToken {
		return false, nil
	}
	r.status = StatusError
	return true, nil
}

// TestCodexTicketTokenInvalid_StopsAllModelsAndIsolatesOtherAccounts 验证 Codex Ticket Token Invalid Stops All Models And Isolates Other Accounts 场景
func TestCodexTicketTokenInvalid_StopsAllModelsAndIsolatesOtherAccounts(t *testing.T) {
	account, healthy := ticketTestAccount(5101), ticketTestAccount(5102)
	account.Status, healthy.Status = StatusActive, StatusActive
	// An identical token on another account must not share this account's stop.
	account.Credentials["chatgpt_account_id"] = "rejected-account"
	repo := &codexTicketTokenRepo{codexTicketRefreshRepo: codexTicketRefreshRepo{accounts: []Account{*account, *healthy}}}
	var rejectedCalls, healthyCalls atomic.Int64
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, FailClosed: true, HarvestProxyURL: "http://proxy.example.com:8080",
	}, &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("chatgpt-account-id") == "rejected-account" {
			rejectedCalls.Add(1)
			return &http.Response{StatusCode: http.StatusUnauthorized}, nil
		}
		healthyCalls.Add(1)
		return &http.Response{StatusCode: http.StatusServiceUnavailable}, nil
	}})
	svc.accountRepo = repo
	svc.setOpenAICodexTicketNextHarvest(time.Now().Add(time.Minute))
	svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	stopped := svc.OpenAICodexTicketStatuses(context.Background(), account, time.Now())
	require.Len(t, stopped, 2)
	for _, status := range stopped {
		require.True(t, status.TokenInvalid)
		require.False(t, status.Harvesting)
		require.Nil(t, status.NextHarvestAt)
		svc.probeOnceOpenAICodexTicket(context.Background(), account, status.Model)
	}
	for range 2 {
		svc.refreshOpenAICodexTickets(context.Background())
	}
	require.Equal(t, int64(1), rejectedCalls.Load(), "a stale active repository snapshot must not restart either model")
	require.Equal(t, int64(4), healthyCalls.Load(), "both models of the other account continue each cycle")
	require.Equal(t, stopped, svc.OpenAICodexTicketStatuses(context.Background(), account, time.Now()))
	require.Equal(t, map[int64]string{account.ID: "令牌失效，已停止打票"}, repo.errorCalls)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(healthy))
	require.Equal(t, StatusActive, account.Status, "shared account snapshots must not be mutated")
	for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), healthy, time.Now()) {
		require.False(t, status.TokenInvalid)
		require.Equal(t, 2, status.Attempts)
		require.NotNil(t, status.NextHarvestAt)
	}

	logs, err := svc.OpenAICodexTicketLogs(context.Background(), account, openAICodexTicketDefaultModel, time.Now())
	require.NoError(t, err)
	require.Len(t, logs.Entries, 2, "skipped probes must not generate new request logs")
	require.Equal(t, "token_invalid", logs.Entries[1].Reason)
	require.Equal(t, http.StatusUnauthorized, logs.Entries[1].HTTPStatus)
}

// TestCodexTicketTokenInvalid_SurvivesRestartAndNewTokenRecovers 验证 Codex Ticket Token Invalid Survives Restart And New Token Recovers 场景
func TestCodexTicketTokenInvalid_SurvivesRestartAndNewTokenRecovers(t *testing.T) {
	account := ticketTestAccount(5201)
	account.Status = StatusActive
	account.Credentials["access_token"] = "rejected-private-token"
	repo := &codexTicketTokenRepo{}
	var requests atomic.Int64
	upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		if req.Header.Get("Authorization") == "Bearer rejected-private-token" {
			return &http.Response{StatusCode: http.StatusUnauthorized}, nil
		}
		return codexTicketResponse(), nil
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, FailClosed: true, HarvestProxyURL: "http://proxy.example.com:8080",
	}, upstream)
	svc.accountRepo = repo
	svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	require.Contains(t, repo.updates, OpenAICodexTicketTokenInvalidExtraKey)
	encoded, err := json.Marshal(repo.updates)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "rejected-private-token", "persist only credential fingerprints")

	reloaded := codexTicketQuotaPauseReload(t, account, repo.updates)
	restarted := ticketTestService(t, svc.openAICodexTicketConfig(), upstream)
	for _, status := range OpenAICodexTicketStatuses(reloaded, svc.openAICodexTicketConfig(), time.Now().Add(24*time.Hour)) {
		require.True(t, status.TokenInvalid, "an invalid token must not recover merely with time")
		require.False(t, status.Harvesting)
		require.Nil(t, status.NextHarvestAt)
		restarted.probeOnceOpenAICodexTicket(context.Background(), reloaded, status.Model)
	}
	require.Equal(t, int64(1), requests.Load(), "the persisted marker alone stops both models after restart")

	// Restoring the account after changing access_token allows a fresh probe.
	reloaded.Credentials = map[string]any{"access_token": "replacement-token", "chatgpt_account_id": "acc-1"}
	reloaded.Status = StatusActive
	restarted.probeOnceOpenAICodexTicket(context.Background(), reloaded, openAICodexTicketDefaultModel)
	require.Equal(t, int64(2), requests.Load())
	statuses := restarted.OpenAICodexTicketStatuses(context.Background(), reloaded, time.Now())
	require.True(t, statuses[0].Ready)
	for _, status := range statuses {
		require.False(t, status.TokenInvalid)
	}
}

// TestCodexTicketTokenInvalid_TracksProviderTokenAndAccountSnapshot 验证 Codex Ticket Token Invalid Tracks Provider Token And Account Snapshot 场景
func TestCodexTicketTokenInvalid_TracksProviderTokenAndAccountSnapshot(t *testing.T) {
	account := ticketTestAccount(5301)
	account.Status = StatusActive
	account.Credentials["access_token"] = "snapshot-token"
	cache := &codexTicketTokenCache{token: "rejected-cache-token"}
	var requests atomic.Int64
	upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		if req.Header.Get("Authorization") == "Bearer rejected-cache-token" {
			return &http.Response{StatusCode: http.StatusUnauthorized}, nil
		}
		return codexTicketResponse(), nil
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, HarvestProxyURL: "http://proxy.example.com:8080",
	}, upstream)
	svc.openAITokenProvider = NewOpenAITokenProvider(nil, cache, nil)
	repo := &codexTicketTokenRepo{}
	svc.accountRepo = repo
	svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), account, time.Now()) {
		require.True(t, status.TokenInvalid, "the account snapshot differs from the rejected provider token")
		svc.probeOnceOpenAICodexTicket(context.Background(), account, status.Model)
	}
	require.Equal(t, int64(1), requests.Load())

	reloaded := codexTicketQuotaPauseReload(t, account, repo.updates)
	restarted := ticketTestService(t, svc.openAICodexTicketConfig(), upstream)
	restarted.openAITokenProvider = NewOpenAITokenProvider(nil, cache, nil)
	for _, status := range restarted.OpenAICodexTicketStatuses(context.Background(), reloaded, time.Now()) {
		require.True(t, status.TokenInvalid)
	}
	reloaded.Credentials = map[string]any{"access_token": "replacement-token", "chatgpt_account_id": "acc-1"}
	for range 2 {
		restarted.probeOnceOpenAICodexTicket(context.Background(), reloaded, openAICodexTicketDefaultSolModel)
	}
	require.Equal(t, int64(1), requests.Load(), "a stale provider cache must never resend the rejected token after credential replacement")
	logs, err := restarted.OpenAICodexTicketLogs(context.Background(), reloaded, openAICodexTicketDefaultSolModel, time.Now())
	require.NoError(t, err)
	require.Empty(t, logs.Entries)
	require.Zero(t, logs.Status.Attempts)
	cache.token = "replacement-token"
	restarted.probeOnceOpenAICodexTicket(context.Background(), reloaded, openAICodexTicketDefaultSolModel)
	require.Equal(t, int64(2), requests.Load())
	require.True(t, restarted.OpenAICodexTicketStatuses(context.Background(), reloaded, time.Now())[1].Ready)
}

// TestCodexTicketTokenInvalid_PersistenceFailuresStillStopHarvesting 验证 Codex Ticket Token Invalid Persistence Failures Still Stop Harvesting 场景
func TestCodexTicketTokenInvalid_PersistenceFailuresStillStopHarvesting(t *testing.T) {
	for _, tt := range []struct {
		name      string
		extraErr  error
		statusErr error
	}{
		{name: "marker write fails", extraErr: errors.New("extra unavailable")},
		{name: "account status write fails", statusErr: errors.New("status unavailable")},
		{name: "both writes fail", extraErr: errors.New("extra unavailable"), statusErr: errors.New("status unavailable")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			account := ticketTestAccount(5401)
			account.Status = StatusActive
			repo := &codexTicketTokenRepo{extraErr: tt.extraErr, setErrorErr: tt.statusErr}
			var requests atomic.Int64
			svc := ticketTestService(t, config.OpenAICodexTicketConfig{
				Enabled: true, HarvestProxyURL: "http://proxy.example.com:8080",
			}, &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return &http.Response{StatusCode: http.StatusUnauthorized}, nil
			}})
			svc.accountRepo = repo
			svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
			svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultSolModel)
			require.Equal(t, int64(1), requests.Load())
			require.Equal(t, map[int64]string{account.ID: "令牌失效，已停止打票"}, repo.errorCalls)
			require.Equal(t, 1, repo.extraCalls, "either storage failure must not skip the other write")
			require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
			for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), account, time.Now()) {
				require.True(t, status.TokenInvalid)
				require.False(t, status.Harvesting)
				require.Nil(t, status.NextHarvestAt)
			}
			if tt.extraErr == nil {
				require.Contains(t, repo.updates, OpenAICodexTicketTokenInvalidExtraKey)
			}
		})
	}
}

// TestCodexTicketTokenInvalid_OtherHTTPFailuresContinue 验证 Codex Ticket Token Invalid Other HTTPFailures Continue 场景
func TestCodexTicketTokenInvalid_OtherHTTPFailuresContinue(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			account := ticketTestAccount(5501)
			account.Status = StatusActive
			repo := &codexTicketTokenRepo{}
			var requests atomic.Int64
			svc := ticketTestService(t, config.OpenAICodexTicketConfig{
				Enabled: true, HarvestProxyURL: "http://proxy.example.com:8080",
			}, &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return &http.Response{StatusCode: code}, nil
			}})
			svc.accountRepo = repo
			for range 2 {
				svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
			}
			require.Equal(t, int64(2), requests.Load())
			require.Empty(t, repo.errorCalls)
			require.Empty(t, repo.updates)
			require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
			for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), account, time.Now()) {
				require.False(t, status.TokenInvalid)
			}
		})
	}
}

// TestCodexTicketTokenInvalid_LateConcurrentSuccessKeepsStop 验证 Codex Ticket Token Invalid Late Concurrent Success Keeps Stop 场景
func TestCodexTicketTokenInvalid_LateConcurrentSuccessKeepsStop(t *testing.T) {
	account := ticketTestAccount(5601)
	account.Status = StatusActive
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var requests atomic.Int64
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, HarvestProxyURL: "http://proxy.example.com:8080",
	}, &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			close(started)
			select {
			case <-release:
				return codexTicketResponse(), nil
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		return &http.Response{StatusCode: http.StatusUnauthorized}, nil
	}})
	repo := &codexTicketTokenRepo{}
	svc.accountRepo = repo
	go func() {
		defer close(done)
		svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultSolModel)
	}()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-done
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("the concurrent request did not start")
	}
	require.True(t, svc.OpenAICodexTicketStatuses(context.Background(), account, time.Now())[1].Harvesting)
	svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), account, time.Now()) {
		require.True(t, status.TokenInvalid)
		require.False(t, status.Harvesting, "401 suppresses live progress even before the other in-flight request completes")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the successful request did not complete")
	}
	for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), account, time.Now()) {
		require.True(t, status.TokenInvalid, "a ticket arriving after the 401 must not resume harvesting")
		require.False(t, status.Harvesting)
		require.Nil(t, status.NextHarvestAt)
		svc.probeOnceOpenAICodexTicket(context.Background(), account, status.Model)
	}
	require.Equal(t, int64(2), requests.Load())
	reloaded := codexTicketQuotaPauseReload(t, account, repo.updates)
	for _, status := range OpenAICodexTicketStatuses(reloaded, svc.openAICodexTicketConfig(), time.Now()) {
		require.True(t, status.TokenInvalid, "the late ticket persistence must preserve the invalid-token marker")
	}
}

// TestCodexTicketTokenInvalid_UsesConditionalAccountErrorUpdate 验证 Codex Ticket Token Invalid Uses Conditional Account Error Update 场景
func TestCodexTicketTokenInvalid_UsesConditionalAccountErrorUpdate(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "matching token"},
		{name: "conditional status write fails", err: errors.New("status unavailable")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			account := ticketTestAccount(5701)
			account.Status = StatusActive
			repo := &codexTicketConditionalTokenRepo{currentToken: "rejected-cache-token", conditionalErr: tt.err, status: StatusActive}
			svc := ticketTestService(t, config.OpenAICodexTicketConfig{
				Enabled: true, HarvestProxyURL: "http://proxy.example.com:8080",
			}, &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusUnauthorized}, nil
			}})
			svc.accountRepo = repo
			svc.openAITokenProvider = NewOpenAITokenProvider(nil, &codexTicketTokenCache{token: "rejected-cache-token"}, nil)
			svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
			require.Equal(t, account.ID, repo.conditionalID)
			require.Equal(t, account.GetOpenAIAccessToken(), repo.accountToken)
			require.Equal(t, "rejected-cache-token", repo.rejectedToken)
			require.Equal(t, "令牌失效，已停止打票", repo.errorMessage)
			require.Empty(t, repo.errorCalls, "a conditional repository must never use the unconditional status update")
			require.Contains(t, repo.updates, OpenAICodexTicketTokenInvalidExtraKey)
			require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
			require.True(t, svc.openAICodexTicketTokenInvalid(account))
			if tt.err == nil {
				require.Equal(t, StatusError, repo.status)
			}
		})
	}
}

// TestCodexTicketTokenInvalid_Late401DoesNotDisableReplacedCredentials 验证 Codex Ticket Token Invalid Late401 Does Not Disable Replaced Credentials 场景
func TestCodexTicketTokenInvalid_Late401DoesNotDisableReplacedCredentials(t *testing.T) {
	account := ticketTestAccount(5801)
	account.Status = StatusActive
	repo := &codexTicketConditionalTokenRepo{currentToken: account.GetOpenAIAccessToken(), status: StatusActive}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, HarvestProxyURL: "http://proxy.example.com:8080",
	}, &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-release:
			return &http.Response{StatusCode: http.StatusUnauthorized}, nil
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}})
	svc.accountRepo = repo
	go func() {
		defer close(done)
		svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	}()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-done
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("the old-token request did not start")
	}
	// Simulate an administrator replacing credentials and restoring the account
	// while a probe still holds its original immutable account snapshot.
	repo.mu.Lock()
	repo.currentToken = "replacement-token"
	repo.mu.Unlock()
	replaced := *account
	replaced.Credentials = map[string]any{"access_token": "replacement-token", "chatgpt_account_id": "acc-1"}
	svc.ClearAccountSchedulingBlock(account.ID)
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the late unauthorized response did not complete")
	}
	require.Equal(t, account.ID, repo.conditionalID)
	require.Empty(t, repo.errorCalls)
	require.Zero(t, repo.extraCalls, "a late 401 must not overwrite metadata belonging to new credentials")
	require.Equal(t, StatusActive, repo.status)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(&replaced))
	for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), &replaced, time.Now()) {
		require.False(t, status.TokenInvalid)
	}
}

// TestCodexTicketTokenInvalid_Late401PreservesNewTokenStop 验证 Codex Ticket Token Invalid Late401 Preserves New Token Stop 场景
func TestCodexTicketTokenInvalid_Late401PreservesNewTokenStop(t *testing.T) {
	account := ticketTestAccount(5901)
	account.Status = StatusActive
	repo := &codexTicketConditionalTokenRepo{currentToken: account.GetOpenAIAccessToken(), status: StatusActive}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var requests atomic.Int64
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, HarvestProxyURL: "http://proxy.example.com:8080",
	}, &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		if req.Header.Get("Authorization") == "Bearer "+account.GetOpenAIAccessToken() {
			close(started)
			select {
			case <-release:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		return &http.Response{StatusCode: http.StatusUnauthorized}, nil
	}})
	svc.accountRepo = repo
	go func() {
		defer close(done)
		svc.probeOnceOpenAICodexTicket(context.Background(), account, openAICodexTicketDefaultModel)
	}()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-done
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("the old-token request did not start")
	}
	repo.mu.Lock()
	repo.currentToken = "replacement-token"
	repo.mu.Unlock()
	replaced := *account
	replaced.Credentials = map[string]any{"access_token": "replacement-token", "chatgpt_account_id": "acc-1"}
	svc.ClearAccountSchedulingBlock(account.ID)
	// The replacement token also fails while the original model's probe is in
	// flight. Its newer stop must survive the original request's late 401.
	svc.probeOnceOpenAICodexTicket(context.Background(), &replaced, openAICodexTicketDefaultSolModel)
	require.True(t, svc.openAICodexTicketTokenInvalid(&replaced))
	require.Equal(t, 1, repo.extraCalls)
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the old-token unauthorized response did not complete")
	}
	require.Equal(t, account.GetOpenAIAccessToken(), repo.rejectedToken, "the final conditional update belongs to the old request")
	require.Equal(t, 1, repo.extraCalls, "the rejected old update must not overwrite the newer persisted marker")
	require.Equal(t, StatusError, repo.status)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(&replaced))
	for _, status := range svc.OpenAICodexTicketStatuses(context.Background(), &replaced, time.Now()) {
		require.True(t, status.TokenInvalid, "the late old-token 401 must preserve the replacement token's stop")
		require.False(t, status.Harvesting)
		require.Nil(t, status.NextHarvestAt)
		svc.probeOnceOpenAICodexTicket(context.Background(), &replaced, status.Model)
	}
	require.Equal(t, int64(2), requests.Load())
	reloaded := codexTicketQuotaPauseReload(t, &replaced, repo.updates)
	for _, status := range OpenAICodexTicketStatuses(reloaded, svc.openAICodexTicketConfig(), time.Now()) {
		require.True(t, status.TokenInvalid)
	}
}
