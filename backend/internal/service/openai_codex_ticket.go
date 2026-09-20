package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	openAICodexTicketExtraKeyPrefix  = "codex_turn_ticket:"
	openAICodexAstraMinVersion       = "0.153.4"
	openAICodexTicketStatePrefix     = "gAAAAA"
	openAICodexTicketDefaultModel    = "gpt-6-astra"
	openAICodexTicketDefaultSolModel = "gpt-5.6-sol"
	openAICodexTicketDefaultLength   = 292
	openAICodexTicketTeamLength      = 332
)

// ErrOpenAICodexTicketUnavailable 表示该号该模型没有可用的 turn-state 门票，
// 且 fail_closed 禁止裸打业务请求。
var ErrOpenAICodexTicketUnavailable = errors.New("codex turn-state ticket unavailable")

type openAICodexTicket struct {
	AccountID  int64     `json:"account_id"`
	Model      string    `json:"model"`
	State      string    `json:"state"`
	Length     int       `json:"length"`
	CapturedAt time.Time `json:"captured_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Attempts   int       `json:"attempts"`
}

// openAICodexTicketKey 构造账号和模型隔离的内存缓存键
func openAICodexTicketKey(accountID int64, model string) string {
	return fmt.Sprintf("%d\x00%s", accountID, strings.TrimSpace(model))
}

// openAICodexTicketExtraKey 构造门票在账号扩展字段中的持久化键
func openAICodexTicketExtraKey(model string) string {
	return openAICodexTicketExtraKeyPrefix + strings.TrimSpace(model)
}

// normalizeOpenAICodexTicketModel 清理模型名称首尾空白
func normalizeOpenAICodexTicketModel(model string) string {
	return strings.TrimSpace(model)
}

// normalizeOpenAICodexTicketPlanType follows the frontend OpenAI plan_type
// normalization: trim, lowercase and remove whitespace, underscores and
// hyphens. Upstream has emitted all of those spellings for the same tier.
func normalizeOpenAICodexTicketPlanType(planType string) string {
	planType = strings.ToLower(strings.TrimSpace(planType))
	var b strings.Builder
	b.Grow(len(planType))
	for _, r := range planType {
		switch r {
		case ' ', '\t', '\n', '\r', '_', '-':
			continue
		default:
			_, _ = b.WriteRune(r)
		}
	}
	return b.String()
}

// isOpenAICodexTicketTeamAccount 识别使用团队门票长度的订阅类型
func isOpenAICodexTicketTeamAccount(account *Account) bool {
	if account == nil {
		return false
	}
	switch normalizeOpenAICodexTicketPlanType(account.GetCredential("plan_type")) {
	case "team", "selfservebusinessprolite":
		return true
	default:
		return false
	}
}

// openAICodexTicketTargetLength returns the length required for one account.
// Team/Business accounts always use the dedicated 332-byte ticket; personal
// accounts retain the configured target length for backwards compatibility.
func openAICodexTicketTargetLength(account *Account, configured int) int {
	if isOpenAICodexTicketTeamAccount(account) {
		return openAICodexTicketTeamLength
	}
	if configured <= 0 {
		return openAICodexTicketDefaultLength
	}
	return configured
}

// extractOpenAICodexTicketModel 从最终出站请求体提取门票模型
func extractOpenAICodexTicketModel(body []byte) string {
	return normalizeOpenAICodexTicketModel(gjson.GetBytes(body, "model").String())
}

// openAICodexTicketConfig 读取打票配置并补齐运行时默认值
func (s *OpenAIGatewayService) openAICodexTicketConfig() config.OpenAICodexTicketConfig {
	cfg := config.OpenAICodexTicketConfig{}
	if s != nil && s.cfg != nil {
		cfg = s.cfg.Gateway.OpenAICodexTicket
	}
	if cfg.TargetLength <= 0 {
		cfg.TargetLength = openAICodexTicketDefaultLength
	}
	if cfg.TTLSeconds <= 0 {
		cfg.TTLSeconds = 3600
	}
	if cfg.RefreshBeforeSeconds <= 0 {
		cfg.RefreshBeforeSeconds = 600
	}
	if cfg.HarvestProbeIntervalSeconds <= 0 {
		cfg.HarvestProbeIntervalSeconds = 6
	}
	if cfg.HarvestAttemptTimeoutSeconds <= 0 {
		cfg.HarvestAttemptTimeoutSeconds = 25
	}
	if len(cfg.Models) == 0 {
		cfg.Models = []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel}
	}
	return cfg
}

// openAICodexTicketGatedModel 判断模型是否启用门票注入和缺票门控
func (s *OpenAIGatewayService) openAICodexTicketGatedModel(model string) bool {
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" || !s.openAICodexTicketEnabled() {
		return false
	}
	for _, item := range s.openAICodexTicketConfig().Models {
		if normalizeOpenAICodexTicketModel(item) == model {
			return true
		}
	}
	return false
}

// OpenAICodexTicketStatus 是给管理端看的门票摘要，不含 state blob。
type OpenAICodexTicketStatus struct {
	Model            string     `json:"model"`
	Length           int        `json:"length,omitempty"`
	Ready            bool       `json:"ready"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	Blocked          bool       `json:"blocked"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	HarvestEnabled   bool       `json:"harvest_enabled"`
	HarvestPaused    bool       `json:"harvest_paused"`
	TokenInvalid     bool       `json:"token_invalid"`
	Attempts         int        `json:"attempts"`
	Harvesting       bool       `json:"harvesting"`
	NextHarvestAt    *time.Time `json:"next_harvest_at,omitempty"`
}

// Progress is local to this harvester. Unfinished rounds restart at zero after
// a process restart; completed round counts are saved with the successful ticket.
type openAICodexTicketProgress struct {
	attempts   int
	harvesting bool
	completed  bool
}

// OpenAICodexTicketStatuses 根据持久化门票生成不含原始票据的管理端摘要
func OpenAICodexTicketStatuses(account *Account, cfg config.OpenAICodexTicketConfig, now time.Time) []OpenAICodexTicketStatus {
	if !cfg.Enabled || !isOpenAICodexTicketAccount(account) {
		return nil
	}
	models := cfg.Models
	if len(models) == 0 {
		models = []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel}
	}
	targetLen := openAICodexTicketTargetLength(account, cfg.TargetLength)
	harvestPaused := openAICodexTicketHarvestPaused(account, now)
	tokenInvalid := openAICodexTicketTokenInvalid(account)
	out := make([]OpenAICodexTicketStatus, 0, len(models))
	for _, model := range models {
		model = normalizeOpenAICodexTicketModel(model)
		if model == "" {
			continue
		}
		ticket := parseOpenAICodexTicketFromAny(0, model, nil)
		if account != nil && account.Extra != nil {
			ticket = parseOpenAICodexTicketFromAny(account.ID, model, account.Extra[openAICodexTicketExtraKey(model)])
		}
		status := openAICodexTicketStatus(model, ticket, cfg, now, targetLen)
		status.HarvestPaused = harvestPaused
		status.TokenInvalid = tokenInvalid
		status.Blocked = status.Blocked && !harvestPaused
		out = append(out, status)
	}
	return out
}

// openAICodexTicketStatus 计算单模型门票就绪状态和有效期
func openAICodexTicketStatus(model string, ticket *openAICodexTicket, cfg config.OpenAICodexTicketConfig, now time.Time, targetLen int) OpenAICodexTicketStatus {
	status := OpenAICodexTicketStatus{
		Model:          model,
		HarvestEnabled: cfg.Enabled && validOpenAICodexTicketHarvestProxy(cfg.HarvestProxyURL),
	}
	if ticket.valid(now, targetLen) {
		status.Ready = true
		status.Length = ticket.Length
		status.RemainingSeconds = int64(ticket.ExpiresAt.Sub(now) / time.Second)
		if status.RemainingSeconds < 0 {
			status.RemainingSeconds = 0
		}
		exp := ticket.ExpiresAt
		status.ExpiresAt = &exp
	}
	if status.HarvestEnabled && ticket != nil {
		status.Attempts = max(0, ticket.Attempts)
	}
	status.Blocked = cfg.FailClosed && !status.Ready
	return status
}

// OpenAICodexTicketStatuses combines persisted tickets with live harvest progress.
// The next attempt time is the actual shared loop deadline, never a deadline
// invented at read time. No status read writes account metadata.
func (s *OpenAIGatewayService) OpenAICodexTicketStatuses(ctx context.Context, account *Account, now time.Time) []OpenAICodexTicketStatus {
	if s == nil || !isOpenAICodexTicketAccount(account) || !s.openAICodexTicketEnabledContext(ctx) {
		return nil
	}
	cfg := s.openAICodexTicketConfig()
	cfg.Enabled = true
	cfg.HarvestProxyURL = s.openAICodexTicketHarvestProxyURLContext(ctx)
	targetLen := openAICodexTicketTargetLength(account, cfg.TargetLength)
	harvestPaused := s.openAICodexTicketHarvestPaused(account, now)
	tokenInvalid := s.openAICodexTicketTokenInvalid(account)
	out := make([]OpenAICodexTicketStatus, 0, len(cfg.Models))
	for _, model := range cfg.Models {
		model = normalizeOpenAICodexTicketModel(model)
		if model == "" {
			continue
		}
		ticket := s.lookupOpenAICodexTicket(account, model)
		status := openAICodexTicketStatus(model, ticket, cfg, now, targetLen)
		status.HarvestPaused = harvestPaused
		status.TokenInvalid = tokenInvalid
		status.Blocked = status.Blocked && !harvestPaused
		if status.HarvestEnabled {
			s.openaiCodexTicketStatusMu.RLock()
			progress, tracked := s.openaiCodexTicketProgress[openAICodexTicketKey(account.ID, model)]
			next := s.openaiCodexTicketNextHarvest
			s.openaiCodexTicketStatusMu.RUnlock()
			if tracked {
				status.Attempts = progress.attempts
			}
			if account.Status == StatusActive && !account.IsRateLimited() && !harvestPaused && !tokenInvalid {
				status.Harvesting = progress.harvesting
				if !status.Harvesting && next.After(now) && (!status.Ready || ticket.needsRefresh(now, time.Duration(cfg.RefreshBeforeSeconds)*time.Second)) {
					status.NextHarvestAt = &next
				}
			}
		}
		out = append(out, status)
	}
	return out
}

// validOpenAICodexTicketHarvestProxy 判断打票代理是否非空且格式有效
func validOpenAICodexTicketHarvestProxy(proxyURL string) bool {
	return strings.TrimSpace(proxyURL) != "" && ValidateOpenAICodexTicketHarvestProxyURL(proxyURL) == nil
}

// beginOpenAICodexTicketAttempt 记录本轮真正发出的采票尝试
func (s *OpenAIGatewayService) beginOpenAICodexTicketAttempt(accountID int64, model string) int {
	s.openaiCodexTicketStatusMu.Lock()
	defer s.openaiCodexTicketStatusMu.Unlock()
	if s.openaiCodexTicketProgress == nil {
		s.openaiCodexTicketProgress = make(map[string]openAICodexTicketProgress)
	}
	key := openAICodexTicketKey(accountID, model)
	progress := s.openaiCodexTicketProgress[key]
	if progress.completed {
		progress.attempts = 0
		progress.completed = false
	}
	progress.attempts++
	progress.harvesting = true
	s.openaiCodexTicketProgress[key] = progress
	return progress.attempts
}

// finishOpenAICodexTicketAttempt 结束当前采票尝试并记录轮次是否成功
func (s *OpenAIGatewayService) finishOpenAICodexTicketAttempt(accountID int64, model string, completed bool) int {
	s.openaiCodexTicketStatusMu.Lock()
	defer s.openaiCodexTicketStatusMu.Unlock()
	key := openAICodexTicketKey(accountID, model)
	progress, tracked := s.openaiCodexTicketProgress[key]
	if !tracked {
		return 0
	}
	progress.harvesting = false
	progress.completed = completed
	s.openaiCodexTicketProgress[key] = progress
	return progress.attempts
}

// setOpenAICodexTicketNextHarvest 同步下一轮后台采集的实际触发时间
func (s *OpenAIGatewayService) setOpenAICodexTicketNextHarvest(next time.Time) {
	s.openaiCodexTicketStatusMu.Lock()
	s.openaiCodexTicketNextHarvest = next
	s.openaiCodexTicketStatusMu.Unlock()
}

// openAICodexTicketEnabled 读取当前生效的打票总开关
func (s *OpenAIGatewayService) openAICodexTicketEnabled() bool {
	return s.openAICodexTicketEnabledContext(context.Background())
}

// openAICodexTicketEnabledContext 在请求上下文中读取后台设置优先的打票开关
func (s *OpenAIGatewayService) openAICodexTicketEnabledContext(ctx context.Context) bool {
	if s == nil {
		return false
	}
	fallback := s.cfg != nil && s.cfg.Gateway.OpenAICodexTicket.Enabled
	if s.settingService != nil {
		return s.settingService.GetOpenAICodexTicketEnabled(ctx, fallback)
	}
	return fallback
}

// openAICodexTicketHarvestProxyURL 获取当前生效的独立打票代理
func (s *OpenAIGatewayService) openAICodexTicketHarvestProxyURL() string {
	return s.openAICodexTicketHarvestProxyURLContext(context.Background())
}

// openAICodexTicketHarvestProxyURLContext 读取后台代理配置，缺失时回退到启动配置
func (s *OpenAIGatewayService) openAICodexTicketHarvestProxyURLContext(ctx context.Context) string {
	if s.settingService != nil {
		if proxy := s.settingService.GetOpenAICodexTicketHarvestProxyURL(ctx); proxy != "" {
			return proxy
		}
	}
	return strings.TrimSpace(s.openAICodexTicketConfig().HarvestProxyURL)
}

// valid 校验门票的前缀、实际长度和本地有效期
func (t *openAICodexTicket) valid(now time.Time, targetLen int) bool {
	if t == nil {
		return false
	}
	state := strings.TrimSpace(t.State)
	if len(state) != targetLen || t.Length != targetLen || !strings.HasPrefix(state, openAICodexTicketStatePrefix) {
		return false
	}
	if t.ExpiresAt.IsZero() || !now.Before(t.ExpiresAt) {
		return false
	}
	return true
}

// needsRefresh 判断门票是否已进入提前续期窗口
func (t *openAICodexTicket) needsRefresh(now time.Time, refreshBefore time.Duration) bool {
	if t == nil || t.ExpiresAt.IsZero() {
		return true
	}
	return !t.ExpiresAt.After(now.Add(refreshBefore))
}

// lookupOpenAICodexTicket 从内存与持久化快照选择更新且有效的门票
func (s *OpenAIGatewayService) lookupOpenAICodexTicket(account *Account, model string) *openAICodexTicket {
	if s == nil || account == nil || account.ID <= 0 {
		return nil
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" {
		return nil
	}
	key := openAICodexTicketKey(account.ID, model)
	targetLen := openAICodexTicketDefaultLength
	if s != nil {
		targetLen = openAICodexTicketTargetLength(account, s.openAICodexTicketConfig().TargetLength)
	}
	now := time.Now()
	var mem *openAICodexTicket
	if raw, ok := s.openaiCodexTickets.Load(key); ok {
		mem, _ = raw.(*openAICodexTicket)
	}
	var extra *openAICodexTicket
	if account.Extra != nil {
		extra = parseOpenAICodexTicketFromAny(account.ID, model, account.Extra[openAICodexTicketExtraKey(model)])
	}
	if extra.valid(now, targetLen) && (mem == nil || extra.CapturedAt.After(mem.CapturedAt)) {
		s.openaiCodexTickets.Store(key, extra)
		return extra
	}
	if mem.valid(now, targetLen) {
		return mem
	}
	if extra != nil {
		s.openaiCodexTickets.Store(key, extra)
		return extra
	}
	if mem != nil {
		s.openaiCodexTickets.Delete(key)
	}
	return nil
}

// parseOpenAICodexTicketFromAny 将账号扩展字段转换为标准门票结构
func parseOpenAICodexTicketFromAny(accountID int64, model string, raw any) *openAICodexTicket {
	if raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var ticket openAICodexTicket
	if err := json.Unmarshal(b, &ticket); err != nil {
		return nil
	}
	ticket.AccountID = accountID
	if strings.TrimSpace(model) != "" {
		ticket.Model = model
	}
	ticket.State = strings.TrimSpace(ticket.State)
	if ticket.Length == 0 {
		ticket.Length = len(ticket.State)
	}
	if ticket.State == "" {
		return nil
	}
	return &ticket
}

// storeOpenAICodexTicket 更新内存门票并以键级合并方式持久化到账号
func (s *OpenAIGatewayService) storeOpenAICodexTicket(ctx context.Context, account *Account, ticket *openAICodexTicket) {
	if s == nil || account == nil || ticket == nil || account.ID <= 0 {
		return
	}
	model := normalizeOpenAICodexTicketModel(ticket.Model)
	ticket.Model = model
	ticket.AccountID = account.ID
	s.openaiCodexTickets.Store(openAICodexTicketKey(account.ID, model), ticket)
	if s.accountRepo == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		openAICodexTicketExtraKey(model): ticket,
	}); err != nil {
		logger.L().Warn("openai_codex_ticket persist failed",
			zap.Int64("account_id", account.ID),
			zap.String("model", model),
			zap.Error(err),
		)
	}
}

// applyOpenAICodexTicket 在出站请求上覆盖 x-codex-turn-state。
// 请求路径只注入已捕获的有效门票，不现场打票；缺票拦截启用时返回
// ErrOpenAICodexTicketUnavailable。因额度耗尽停止打票的账号仍放行业务请求。
func (s *OpenAIGatewayService) applyOpenAICodexTicket(ctx context.Context, account *Account, model string, h http.Header) error {
	if s == nil || h == nil || !isOpenAICodexTicketAccount(account) || !s.openAICodexTicketEnabledContext(ctx) {
		return nil
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" || !s.openAICodexTicketGatedModel(model) {
		return nil
	}
	cfg := s.openAICodexTicketConfig()
	ticket := s.lookupOpenAICodexTicket(account, model)
	if ticket.valid(time.Now(), openAICodexTicketTargetLength(account, cfg.TargetLength)) {
		h.Set(openAICodexTurnStateHeader, ticket.State)
		return nil
	}
	if !cfg.FailClosed || s.openAICodexTicketHarvestPaused(account, time.Now()) {
		return nil
	}
	return ErrOpenAICodexTicketUnavailable
}

// openAICodexTicketOutboundModel 预测本请求真正出站的模型名，也就是
// applyOpenAICodexTicket 注入时读到的 body.model。
//
// 调度门控与注入必须按同一个模型名判定门票。普通请求下二者同源：Forward 的
// upstreamModel 与本函数都走 resolveOpenAIAccountUpstreamModelForRequest，且
// Forward 会把 body.model 改写成该值后才注入。但 /responses/compact 例外——
// Forward 会把出站模型进一步改写为 compact 映射或 gateway.openai_compact_model
// （默认非空），此时若门控仍按客户端原始模型判定，就会把「实际出站是非门控
// 模型、根本不需要票」的 compact 请求整片误拦成不可调度。
func (s *OpenAIGatewayService) openAICodexTicketOutboundModel(account *Account, requestedModel string, requireCompact bool) string {
	model := strings.TrimSpace(requestedModel)
	if account == nil || model == "" {
		return model
	}
	if !account.IsOpenAI() {
		return canonicalOpenAIAccountSchedulingModel(account, model)
	}
	_, upstreamModel := resolveOpenAIForwardMappedModels(account, model, requireCompact)
	if requireCompact {
		// 与 Forward 同序：compact 兜底模型优先于普通/compact 映射结果。
		if compactModel := strings.TrimSpace(s.resolveOpenAICompactFallbackModel(account, model)); compactModel != "" {
			upstreamModel = compactModel
		}
	}
	if upstreamModel = strings.TrimSpace(upstreamModel); upstreamModel != "" {
		return upstreamModel
	}
	return model
}

// outboundModel 必须是真正会发给上游的模型名（openAICodexTicketOutboundModel），
// 不是客户端原始模型：注入侧读的是出站 body.model，两侧口径必须一致。
func (s *OpenAIGatewayService) openAICodexTicketBlocksAccount(account *Account, outboundModel string) bool {
	if s == nil || !isOpenAICodexTicketAccount(account) || !s.openAICodexTicketEnabled() {
		return false
	}
	cfg := s.openAICodexTicketConfig()
	if !cfg.FailClosed || s.openAICodexTicketHarvestPaused(account, time.Now()) {
		return false
	}
	model := normalizeOpenAICodexTicketModel(outboundModel)
	if !s.openAICodexTicketGatedModel(model) {
		return false
	}
	ticket := s.lookupOpenAICodexTicket(account, model)
	return !ticket.valid(time.Now(), openAICodexTicketTargetLength(account, cfg.TargetLength))
}

// fireOpenAICodexTicketProbe 通过专用代理发送采票请求并读取响应头和出口诊断
func (s *OpenAIGatewayService) fireOpenAICodexTicketProbe(ctx context.Context, account *Account, token, model, proxyURL string, attemptTimeout time.Duration) (state string, status, attempt int, egress OpenAICodexTicketEgressResult, err error) {
	// Preparation failures have no trace result. Do not attribute the main
	// request's token, header or connection error to the diagnostic endpoint.
	egress.Error = &OpenAICodexTicketEgressError{Reason: "ticket_connection_unavailable"}
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()

	body := []byte(`{"model":` + jsonString(model) + `,"store":false,"stream":true,"instructions":"Reply with exactly: pong","input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, chatgptCodexURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, 0, egress, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAIHarvest))
	req.Close = true
	req.Host = "chatgpt.com"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("session_id", uuid.NewString())
	if err := resolveAndSetOpenAIChatGPTAccountHeaders(attemptCtx, s.accountRepo, req.Header, account); err != nil {
		return "", 0, 0, egress, err
	}
	applyOpenAICodexTicketHarvestIdentity(req.Header, model)
	if err := attemptCtx.Err(); err != nil {
		return "", 0, 0, egress, err
	}

	// Synthetic probes must use the dedicated no-reuse transport even when the
	// production account is bound to a plugin. This also avoids reading pluginManager
	// while handlers are still wiring it during gateway construction.
	// Another model may have received a 401 while token/header preparation ran.
	if s.openAICodexTicketTokenInvalid(account) || s.lookupOpenAICodexTicketTokenInvalidation(account).matches(token) {
		return "", 0, 0, egress, errOpenAICodexTicketTokenInvalid
	}
	attempt = s.beginOpenAICodexTicketAttempt(account.ID, model)
	logID := s.openaiCodexTicketLogs.append(account.ID, model, OpenAICodexTicketLogEntry{
		Attempt: attempt, Event: "started", Reason: "request_started",
		TargetLength: openAICodexTicketTargetLength(account, s.openAICodexTicketConfig().TargetLength),
	})
	egress = OpenAICodexTicketEgressResult{}
	req = req.WithContext(WithOpenAICodexTicketEgressObserver(req.Context(), func(result OpenAICodexTicketEgressResult) {
		result = normalizeOpenAICodexTicketEgressResult(result)
		if egress.IP == "" || result.IP != "" {
			egress = result
			s.openaiCodexTicketLogs.setEgressResult(account.ID, model, logID, result)
		}
	}))
	defer s.finishOpenAICodexTicketAttempt(account.ID, model, false)
	logHTTP := BeginOpenAICodexTicketHTTPLog(attemptCtx, req.Method, req.URL.String(), map[string]any{
		"account_id": account.ID, "model": model, "body": json.RawMessage(body),
	})
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	responseSummary := map[string]any{}
	responseStatus := 0
	if resp != nil {
		responseStatus = resp.StatusCode
		responseSummary["ticket_length"] = len(extractOpenAICodexTurnState(resp.Header))
		responseSummary["request_id"] = resp.Header.Get("x-request-id")
	}
	logHTTP(responseStatus, responseSummary, err)
	if egress.IP == "" && egress.Error == nil {
		reason := "unknown"
		if err != nil || resp == nil {
			reason = "ticket_connection_unavailable"
		}
		egress.Error = &OpenAICodexTicketEgressError{Reason: reason}
		s.openaiCodexTicketLogs.setEgressResult(account.ID, model, logID, egress)
	}
	if err != nil {
		return "", 0, attempt, egress, err
	}
	if resp == nil {
		return "", 0, attempt, egress, errOpenAICodexTicketEmptyResponse
	}
	// Only the response header is needed; no connection will be reused.
	defer func() {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	if resp.StatusCode == http.StatusUnauthorized {
		s.stopOpenAICodexTicketHarvestOnUnauthorized(ctx, account, token)
	} else if resp.StatusCode == http.StatusTooManyRequests {
		s.pauseOpenAICodexTicketHarvestOnQuota(ctx, account, resp.Header)
	}
	return extractOpenAICodexTurnState(resp.Header), resp.StatusCode, attempt, egress, nil
}

// jsonString 将字符串编码为合法的 JSON 字面量
func jsonString(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `""`
	}
	return string(b)
}

// applyOpenAICodexTicketHarvestIdentity 设置采票请求身份并满足目标模型的客户端版本要求
func applyOpenAICodexTicketHarvestIdentity(h http.Header, model string) {
	ensureCodexIdentityHeaders(h)
	enforceCodexIdentityHeaders(h)
	version := strings.TrimSpace(h.Get("version"))
	if needsOpenAICodexAstraVersion(model) && (version == "" || CompareVersions(version, openAICodexAstraMinVersion) < 0) {
		h.Set("version", openAICodexAstraMinVersion)
		h.Set("user-agent", buildCodexCLIUserAgent(openAICodexAstraMinVersion))
		h.Set("originator", openai.CodexDefaultOriginator)
	}
}

// needsOpenAICodexAstraVersion 判断目标模型是否需要 Astra 最低客户端版本
func needsOpenAICodexAstraVersion(model string) bool {
	m := strings.ToLower(normalizeOpenAICodexTicketModel(model))
	return strings.Contains(m, "gpt-6") || strings.Contains(m, "astra")
}

// StartOpenAICodexTicketHarvester 幂等启动可取消的后台采票循环
func (s *OpenAIGatewayService) StartOpenAICodexTicketHarvester() {
	if s == nil {
		return
	}
	s.openaiCodexTicketLifecycleMu.Lock()
	defer s.openaiCodexTicketLifecycleMu.Unlock()
	if s.openaiCodexTicketStopped || s.openaiCodexTicketDone != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.openaiCodexTicketCancel = cancel
	s.openaiCodexTicketDone = done
	go func() {
		defer close(done)
		s.openAICodexTicketHarvestLoop(ctx)
	}()
	logger.L().Info("openai_codex_ticket harvester started",
		zap.Int("ttl_seconds", s.openAICodexTicketConfig().TTLSeconds),
		zap.Int("target_length", s.openAICodexTicketConfig().TargetLength),
		zap.Strings("models", s.openAICodexTicketConfig().Models),
	)
}

// StopOpenAICodexTicketHarvester 取消后台采票并等待在途任务退出
func (s *OpenAIGatewayService) StopOpenAICodexTicketHarvester() {
	if s == nil {
		return
	}
	s.openaiCodexTicketLifecycleMu.Lock()
	s.openaiCodexTicketStopped = true
	cancel, done := s.openaiCodexTicketCancel, s.openaiCodexTicketDone
	s.openaiCodexTicketLifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// openAICodexTicketHarvestLoop 逐轮采集待续期门票并公布下一次执行时间
func (s *OpenAIGatewayService) openAICodexTicketHarvestLoop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	defer s.setOpenAICodexTicketNextHarvest(time.Time{})
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.setOpenAICodexTicketNextHarvest(time.Time{})
			s.refreshOpenAICodexTickets(ctx)
			interval := time.Duration(s.openAICodexTicketConfig().HarvestProbeIntervalSeconds) * time.Second
			s.setOpenAICodexTicketNextHarvest(time.Now().Add(interval))
			timer.Reset(interval)
		}
	}
}

// refreshOpenAICodexTickets probes each account/model with a missing or soon-to-expire
// ticket once, skipping rate-limited, quota-paused or token-invalid accounts until recovery. The loop waits
// for all probes, then waits the configured interval before starting the next cycle.
func (s *OpenAIGatewayService) refreshOpenAICodexTickets(ctx context.Context) {
	if s == nil || s.accountRepo == nil || ctx.Err() != nil || !s.openAICodexTicketEnabledContext(ctx) {
		return
	}
	if !validOpenAICodexTicketHarvestProxy(s.openAICodexTicketHarvestProxyURLContext(ctx)) || s.httpUpstream == nil {
		return
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		logger.L().Warn("openai_codex_ticket list accounts failed", zap.Error(err))
		return
	}
	cfg := s.openAICodexTicketConfig()
	now := time.Now()
	refreshBefore := time.Duration(cfg.RefreshBeforeSeconds) * time.Second
	var wg sync.WaitGroup
	probed := 0
	for i := range accounts {
		account := accounts[i]
		if account.Status != StatusActive || !isOpenAICodexTicketAccount(&account) || account.IsRateLimited() || s.openAICodexTicketHarvestPaused(&account, now) || s.openAICodexTicketTokenInvalid(&account) {
			continue
		}
		targetLen := openAICodexTicketTargetLength(&account, cfg.TargetLength)
		for _, model := range cfg.Models {
			model := normalizeOpenAICodexTicketModel(model)
			if model == "" {
				continue
			}
			// 已有一张有效且未临近过期的票 → 本周期不打，省得白刷。
			if t := s.lookupOpenAICodexTicket(&account, model); t.valid(now, targetLen) && !t.needsRefresh(now, refreshBefore) {
				continue
			}
			acc := account
			// Token/header helpers may update account metadata; each model owns its maps.
			acc.Extra = maps.Clone(account.Extra)
			acc.Credentials = maps.Clone(account.Credentials)
			probed++
			wg.Add(1)
			go func(acc Account, model string) {
				defer wg.Done()
				s.probeOnceOpenAICodexTicket(ctx, &acc, model)
			}(acc, model)
		}
	}
	wg.Wait()
	if probed > 0 {
		logger.L().Info("openai_codex_ticket probe cycle", zap.Int("probed", probed))
	}
}

// probeOnceOpenAICodexTicket 走打票代理打一发。命中合格目标长度（HTTP 200、长度==target、
// gAAAAA 前缀）就落库；401 或 429 且额度耗尽时停止打票，其他 miss 交给下个周期重试。
// 同一 key 并发去重，避免上一发还没回来又叠一发。
func (s *OpenAIGatewayService) probeOnceOpenAICodexTicket(ctx context.Context, account *Account, model string) {
	if s == nil || !isOpenAICodexTicketAccount(account) || account.IsRateLimited() || ctx.Err() != nil || !s.openAICodexTicketEnabledContext(ctx) {
		return
	}
	cfg := s.openAICodexTicketConfig()
	targetLen := openAICodexTicketTargetLength(account, cfg.TargetLength)
	proxyURL := s.openAICodexTicketHarvestProxyURLContext(ctx)
	if !validOpenAICodexTicketHarvestProxy(proxyURL) || s.httpUpstream == nil || ctx.Err() != nil {
		return
	}
	key := openAICodexTicketKey(account.ID, model)
	_, _, _ = s.openaiCodexTicketFlight.Do(key, func() (any, error) {
		if s.openAICodexTicketHarvestPaused(account, time.Now()) || s.openAICodexTicketTokenInvalid(account) {
			return nil, nil
		}
		startedAt := time.Now()
		token, _, err := s.GetAccessToken(ctx, account)
		if err != nil || strings.TrimSpace(token) == "" {
			duration := time.Since(startedAt).Milliseconds()
			s.openaiCodexTicketLogs.append(account.ID, model, OpenAICodexTicketLogEntry{
				Event: "skipped", Reason: "token_error", TargetLength: targetLen, DurationMS: &duration,
				EgressError: &OpenAICodexTicketEgressError{Reason: "ticket_connection_unavailable"},
			})
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.String("reason", "token"), zap.Error(err))
			return nil, nil
		}
		if s.openAICodexTicketHarvestPaused(account, time.Now()) || s.openAICodexTicketTokenInvalid(account) {
			return nil, nil
		}
		startedAt = time.Now()
		state, status, attempt, egress, perr := s.fireOpenAICodexTicketProbe(ctx, account, token, model, proxyURL, time.Duration(cfg.HarvestAttemptTimeoutSeconds)*time.Second)
		duration := time.Since(startedAt).Milliseconds()
		entry := OpenAICodexTicketLogEntry{Attempt: attempt, TargetLength: targetLen, DurationMS: &duration, EgressIP: egress.IP, EgressError: egress.Error}
		if errors.Is(perr, errOpenAICodexTicketTokenInvalid) {
			return nil, nil
		}
		if perr != nil {
			entry.Event, entry.Reason = "error", openAICodexTicketProbeErrorReason(perr, attempt)
			if attempt == 0 {
				entry.Event = "skipped"
			}
			s.openaiCodexTicketLogs.append(account.ID, model, entry)
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.String("reason", "error"), zap.Error(perr))
			return nil, nil
		}
		if status != http.StatusOK || state == "" || len(state) != targetLen || !strings.HasPrefix(state, openAICodexTicketStatePrefix) {
			length := len(state)
			entry.Event, entry.HTTPStatus, entry.TicketLength = "miss", status, &length
			switch {
			case status == http.StatusUnauthorized:
				entry.Reason = "token_invalid"
			case status == http.StatusTooManyRequests && s.openAICodexTicketHarvestPaused(account, time.Now()):
				entry.Reason = "quota_exhausted"
			case status != http.StatusOK:
				entry.Reason = "http_error"
			case state == "":
				entry.Reason = "missing_state"
			case len(state) != targetLen:
				entry.Reason = "length_mismatch"
			default:
				entry.Reason = "invalid_state"
			}
			s.openaiCodexTicketLogs.append(account.ID, model, entry)
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.Int("http", status), zap.Int("len", len(state)))
			return nil, nil
		}
		now := time.Now()
		ticket := &openAICodexTicket{
			AccountID:  account.ID,
			Model:      model,
			State:      state,
			Length:     len(state),
			CapturedAt: now,
			ExpiresAt:  now.Add(time.Duration(cfg.TTLSeconds) * time.Second),
			Attempts:   s.finishOpenAICodexTicketAttempt(account.ID, model, true),
		}
		s.storeOpenAICodexTicket(ctx, account, ticket)
		entry.Event, entry.Reason, entry.HTTPStatus, entry.TicketLength = "success", "harvested", status, &ticket.Length
		s.openaiCodexTicketLogs.append(account.ID, model, entry)
		logger.L().Info("openai_codex_ticket harvested",
			zap.Int64("account_id", account.ID), zap.String("model", model),
			zap.Int("length", ticket.Length), zap.String("mode", "continuous"))
		return nil, nil
	})
}

// IsOpenAICodexTicketExtraKey identifies server-managed ticket material.
func IsOpenAICodexTicketExtraKey(key string) bool {
	return strings.HasPrefix(key, openAICodexTicketExtraKeyPrefix)
}

// MergeOpenAICodexTicketExtra preserves only persisted tickets, never summaries or
// blobs supplied by an account edit. The repository repeats this under the row
// lock so a concurrent harvest cannot be overwritten by a stale admin snapshot.
func MergeOpenAICodexTicketExtra(extra, current map[string]any) map[string]any {
	result := maps.Clone(extra)
	for key := range result {
		if IsOpenAICodexTicketExtraKey(key) {
			delete(result, key)
		}
	}
	for key, value := range current {
		if IsOpenAICodexTicketExtraKey(key) {
			if result == nil {
				result = make(map[string]any)
			}
			result[key] = value
		}
	}
	return result
}

// ValidateOpenAICodexTicketHarvestProxyURL validates only syntax, without making
// a network request or including credentials in validation errors.
func ValidateOpenAICodexTicketHarvestProxyURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return errors.New("harvest proxy must be an HTTP(S) or SOCKS5(h) URL with a host and no path, query or fragment")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return errors.New("harvest proxy scheme must be http, https, socks5 or socks5h")
	}
	if port := parsed.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("harvest proxy port must be between 1 and 65535")
		}
	}
	return nil
}

// MaskProxyURL never returns a stored proxy password, even for invalid legacy data.
func MaskProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || ValidateOpenAICodexTicketHarvestProxyURL(raw) != nil {
		return ""
	}
	parsed, _ := url.Parse(raw)
	if parsed.User != nil {
		if _, ok := parsed.User.Password(); ok {
			parsed.User = url.UserPassword(parsed.User.Username(), "***")
		}
	}
	return parsed.String()
}

// IsMaskedProxyURL recognizes the exact password placeholder emitted by the API.
func IsMaskedProxyURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return false
	}
	password, ok := parsed.User.Password()
	return ok && password == "***"
}

// Credential shadows do not own tickets. Keep their existing forwarding policy
// instead of imposing a gate for a key the harvester never populates.
func isOpenAICodexTicketAccount(account *Account) bool {
	return account != nil && account.IsOpenAIOAuthLike() && !account.IsShadow()
}

// IsOpenAICodexTicketPrivateExtraKey also covers the retired account-level proxy
// override, whose credentials may remain in older account records.
func IsOpenAICodexTicketPrivateExtraKey(key string) bool {
	return IsOpenAICodexTicketExtraKey(key) || key == "codex_harvest_proxy_url"
}

// RedactOpenAICodexTicketExtra strips ephemeral ticket material from exports
// without changing the source account or unrelated backup fields.
func RedactOpenAICodexTicketExtra(extra map[string]any) map[string]any {
	redacted := maps.Clone(extra)
	for key := range redacted {
		if IsOpenAICodexTicketPrivateExtraKey(key) {
			delete(redacted, key)
		}
	}
	return redacted
}
