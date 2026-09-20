package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// BeginOpenAICodexTicketHTTPLog 记录外部请求入参，返回记录响应摘要的回调
// 调用方只传入允许记录的字段，令牌、代理密码和原始门票不进入日志
func BeginOpenAICodexTicketHTTPLog(ctx context.Context, method, target string, request any) func(int, any, error) {
	started := time.Now()
	log := logger.FromContext(ctx).With(
		zap.String("trace_id", uuid.NewString()),
		zap.String("url", target),
		zap.String("method", method),
	)
	log.Info("openai_codex_ticket external request", zap.Any("request", request))
	return func(status int, response any, err error) {
		fields := []zap.Field{
			zap.Int("status_code", status),
			zap.Int64("duration_ms", time.Since(started).Milliseconds()),
			zap.Any("response", response),
		}
		if err != nil {
			// 网络错误可能包含代理凭据，只记录规范化后的错误类别
			fields = append(fields, zap.String("error_category", openAICodexTicketProbeErrorReason(err, 1)))
		}
		log.Info("openai_codex_ticket external response", fields...)
	}
}
