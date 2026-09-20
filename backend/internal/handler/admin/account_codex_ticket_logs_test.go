package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type codexTicketLogsAdminStub struct {
	service.AdminService
	account *service.Account
	err     error
	lookups int
}

// GetAccount 为打票回归测试提供 Get Account 辅助实现
func (s *codexTicketLogsAdminStub) GetAccount(context.Context, int64) (*service.Account, error) {
	s.lookups++
	return s.account, s.err
}

type codexTicketLogsStub struct {
	codexTicketStatusStub
	logs      *service.OpenAICodexTicketLogs
	err       error
	calls     int
	accountID int64
	model     string
}

// OpenAICodexTicketLogs 为打票回归测试提供 Open AICodex Ticket Logs 辅助实现
func (s *codexTicketLogsStub) OpenAICodexTicketLogs(_ context.Context, account *service.Account, model string, _ time.Time) (*service.OpenAICodexTicketLogs, error) {
	s.calls++
	s.accountID, s.model = account.ID, model
	return s.logs, s.err
}

// codexTicketLogsRequest 为打票回归测试提供 codex Ticket Logs Request 辅助实现
func codexTicketLogsRequest(handler *AccountHandler, path string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/admin/accounts/:id/codex-ticket-logs", handler.GetCodexTicketLogs)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

// TestAccountCodexTicketLogsReturnsScopedUncachedSnapshot 验证 Account Codex Ticket Logs Returns Scoped Uncached Snapshot 场景
func TestAccountCodexTicketLogsReturnsScopedUncachedSnapshot(t *testing.T) {
	admin := &codexTicketLogsAdminStub{account: &service.Account{ID: 41, Platform: service.PlatformOpenAI, Type: service.AccountTypeSetupToken}}
	provider := &codexTicketLogsStub{logs: &service.OpenAICodexTicketLogs{
		Model: "gpt-6-astra", Entries: []service.OpenAICodexTicketLogEntry{{ID: 7, Event: "miss", Reason: "missing_state", Attempt: 3, TargetLength: 292}},
		Status: &service.OpenAICodexTicketStatus{Model: "gpt-6-astra", Attempts: 3}, Limit: 200,
	}}
	handler := &AccountHandler{adminService: admin}
	handler.SetCodexTicketStatusProvider(provider)
	response := codexTicketLogsRequest(handler, "/api/v1/admin/accounts/41/codex-ticket-logs?model=%20gpt-6-astra%20")
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	require.Equal(t, int64(41), provider.accountID)
	require.Equal(t, "gpt-6-astra", provider.model)
	require.Equal(t, 1, provider.calls)
	var envelope struct {
		Data service.OpenAICodexTicketLogs `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	require.Equal(t, *provider.logs, envelope.Data)
}

// TestAccountCodexTicketLogsValidatesRequestAndAccount 验证 Account Codex Ticket Logs Validates Request And Account 场景
func TestAccountCodexTicketLogsValidatesRequestAndAccount(t *testing.T) {
	parentID := int64(40)
	tests := []struct {
		name        string
		path        string
		account     *service.Account
		accountErr  error
		providerErr error
		noProvider  bool
		status      int
		wantLookups int
		wantCalls   int
	}{
		{name: "invalid ID", path: "oops?model=gpt-6-astra", status: 400},
		{name: "zero ID", path: "0?model=gpt-6-astra", status: 400},
		{name: "negative ID", path: "-1?model=gpt-6-astra", status: 400},
		{name: "missing model", path: "41", status: 400},
		{name: "blank model", path: "41?model=%20", status: 400},
		{name: "missing account", path: "41?model=gpt-6-astra", accountErr: service.ErrAccountNotFound, status: 404, wantLookups: 1},
		{name: "nil account", path: "41?model=gpt-6-astra", status: 404, wantLookups: 1},
		{name: "other platform", path: "41?model=gpt-6-astra", account: &service.Account{ID: 41, Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth}, status: 400, wantLookups: 1},
		{name: "API key", path: "41?model=gpt-6-astra", account: &service.Account{ID: 41, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}, status: 400, wantLookups: 1},
		{name: "shadow", path: "41?model=gpt-6-astra", account: &service.Account{ID: 41, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, ParentAccountID: &parentID}, status: 400, wantLookups: 1},
		{name: "unsupported model", path: "41?model=other", account: &service.Account{ID: 41, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}, providerErr: service.ErrOpenAICodexTicketLogModel, status: 400, wantLookups: 1, wantCalls: 1},
		{name: "provider unavailable", path: "41?model=gpt-6-astra", account: &service.Account{ID: 41, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}, noProvider: true, status: 503, wantLookups: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			admin := &codexTicketLogsAdminStub{account: tt.account, err: tt.accountErr}
			handler := &AccountHandler{adminService: admin}
			provider := &codexTicketLogsStub{err: tt.providerErr}
			if tt.noProvider {
				handler.SetCodexTicketStatusProvider(&codexTicketStatusStub{})
			} else {
				handler.SetCodexTicketStatusProvider(provider)
			}
			id, query := tt.path, ""
			for i, char := range tt.path {
				if char == '?' {
					id, query = tt.path[:i], tt.path[i:]
					break
				}
			}
			response := codexTicketLogsRequest(handler, "/api/v1/admin/accounts/"+id+"/codex-ticket-logs"+query)
			require.Equal(t, tt.status, response.Code)
			require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			require.Equal(t, tt.wantLookups, admin.lookups)
			require.Equal(t, tt.wantCalls, provider.calls)
		})
	}
}
