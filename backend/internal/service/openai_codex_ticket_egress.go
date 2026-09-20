package service

import "context"

type openAICodexTicketEgressObserverKey struct{}

// OpenAICodexTicketEgressError contains only allowlisted diagnostics. HTTPStatus
// belongs to the anonymous IP trace response, never the ticket response.
type OpenAICodexTicketEgressError struct {
	Reason     string `json:"reason"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type OpenAICodexTicketEgressResult struct {
	IP    string
	Error *OpenAICodexTicketEgressError
}

// WithOpenAICodexTicketEgressObserver opts a harvest request into observing the
// public IP of its own connection. The transport calls observe before returning
// from Do. A result contains either a same-connection confirmed address or the
// specific diagnostic failure; it never contains raw upstream errors.
func WithOpenAICodexTicketEgressObserver(ctx context.Context, observe func(OpenAICodexTicketEgressResult)) context.Context {
	return context.WithValue(ctx, openAICodexTicketEgressObserverKey{}, observe)
}

// OpenAICodexTicketEgressObserverFromContext 读取当前请求的出口诊断回调
func OpenAICodexTicketEgressObserverFromContext(ctx context.Context) func(OpenAICodexTicketEgressResult) {
	observe, _ := ctx.Value(openAICodexTicketEgressObserverKey{}).(func(OpenAICodexTicketEgressResult))
	return observe
}

// normalizeOpenAICodexTicketEgressResult 校验出口地址并仅保留允许展示的诊断字段
func normalizeOpenAICodexTicketEgressResult(result OpenAICodexTicketEgressResult) OpenAICodexTicketEgressResult {
	if result.IP != "" {
		if ip := normalizeOpenAICodexTicketEgressIP(result.IP); ip != "" {
			return OpenAICodexTicketEgressResult{IP: ip}
		}
		return OpenAICodexTicketEgressResult{Error: &OpenAICodexTicketEgressError{Reason: "invalid_ip"}}
	}
	diagnostic := OpenAICodexTicketEgressError{Reason: "unknown"}
	if result.Error != nil {
		switch result.Error.Reason {
		case "timeout", "canceled", "network_error", "http_error", "connection_closed", "unsupported_protocol",
			"response_too_large", "response_read_error", "missing_ip", "invalid_ip", "ambiguous_ip", "connection_changed",
			"ticket_connection_unavailable", "insufficient_time", "unsupported_transport", "request_error", "unknown":
			diagnostic.Reason = result.Error.Reason
		}
		if diagnostic.Reason == "http_error" && result.Error.HTTPStatus >= 100 && result.Error.HTTPStatus <= 599 {
			diagnostic.HTTPStatus = result.Error.HTTPStatus
		}
	}
	return OpenAICodexTicketEgressResult{Error: &diagnostic}
}
