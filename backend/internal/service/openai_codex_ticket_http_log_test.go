package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestCodexTicketHTTPLogCorrelatesAndRedacts 验证请求响应日志可关联且不泄露网络错误中的代理凭据
func TestCodexTicketHTTPLogCorrelatesAndRedacts(t *testing.T) {
	core, entries := observer.New(zap.InfoLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	complete := BeginOpenAICodexTicketHTTPLog(ctx, "POST", chatgptCodexURL, map[string]any{"model": "gpt-6-astra"})
	complete(502, map[string]any{"ticket_length": 0}, errors.New("proxy http://user:private-password@proxy.example failed"))
	logs := entries.All()
	require.Len(t, logs, 2)
	request, response := logs[0].ContextMap(), logs[1].ContextMap()
	require.NotEmpty(t, request["trace_id"])
	require.Equal(t, request["trace_id"], response["trace_id"])
	require.Equal(t, chatgptCodexURL, response["url"])
	require.Equal(t, "POST", response["method"])
	require.EqualValues(t, 502, response["status_code"])
	require.Contains(t, response, "duration_ms")
	require.Contains(t, request, "request")
	require.Contains(t, response, "response")
	require.Equal(t, "network_error", response["error_category"])
	require.NotContains(t, response, "error")
}
