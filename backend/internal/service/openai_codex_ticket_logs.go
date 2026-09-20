package service

import (
	"container/list"
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

const (
	OpenAICodexTicketLogLimit      = 200
	openAICodexTicketLogMaxStreams = 512
)

var (
	ErrOpenAICodexTicketLogModel      = errors.New("model is not configured for Codex tickets")
	errOpenAICodexTicketEmptyResponse = errors.New("nil upstream response")
)

// OpenAICodexTicketLogEntry deliberately contains only diagnostics, never raw
// upstream errors, headers, bodies, credentials, proxy addresses, or ticket state.
type OpenAICodexTicketLogEntry struct {
	ID                int64                         `json:"id"`
	Time              time.Time                     `json:"time"`
	Attempt           int                           `json:"attempt"`
	Event             string                        `json:"event"`
	Reason            string                        `json:"reason"`
	HTTPStatus        int                           `json:"http_status,omitempty"`
	TicketLength      *int                          `json:"ticket_length,omitempty"`
	TargetLength      int                           `json:"target_length"`
	DurationMS        *int64                        `json:"duration_ms,omitempty"`
	EgressIP          string                        `json:"egress_ip,omitempty"`
	EgressCountryCode string                        `json:"egress_country_code,omitempty"`
	EgressError       *OpenAICodexTicketEgressError `json:"egress_error,omitempty"`
}

type OpenAICodexTicketLogs struct {
	Model   string                      `json:"model"`
	Entries []OpenAICodexTicketLogEntry `json:"entries"`
	Status  *OpenAICodexTicketStatus    `json:"status"`
	Limit   int                         `json:"limit"`
}

type openAICodexTicketLogStream struct {
	key     string
	entries [OpenAICodexTicketLogLimit]OpenAICodexTicketLogEntry
	next    int
	size    int
}

// Each stream is a ring; LRU eviction also bounds memory when accounts are
// deleted or model configuration changes. Its zero value is ready for use.
// The LRU list only contains *openAICodexTicketLogStream values.
type openAICodexTicketLogStore struct {
	mu      sync.Mutex
	streams map[string]*list.Element
	lru     list.List
	nextID  int64
}

// cloneOpenAICodexTicketLogEntry 复制日志条目中的指针字段以隔离并发快照
func cloneOpenAICodexTicketLogEntry(entry OpenAICodexTicketLogEntry) OpenAICodexTicketLogEntry {
	if entry.TicketLength != nil {
		length := *entry.TicketLength
		entry.TicketLength = &length
	}
	if entry.DurationMS != nil {
		duration := *entry.DurationMS
		entry.DurationMS = &duration
	}
	if entry.EgressError != nil {
		diagnostic := *entry.EgressError
		entry.EgressError = &diagnostic
	}
	return entry
}

// append 向有界日志环追加条目并淘汰最久未使用的日志流
func (store *openAICodexTicketLogStore) append(accountID int64, model string, entry OpenAICodexTicketLogEntry) int64 {
	key := openAICodexTicketKey(accountID, model)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.streams == nil {
		store.streams = make(map[string]*list.Element)
	}
	element := store.streams[key]
	if element == nil {
		if len(store.streams) >= openAICodexTicketLogMaxStreams {
			oldest := store.lru.Back()
			oldestStream, _ := oldest.Value.(*openAICodexTicketLogStream)
			delete(store.streams, oldestStream.key)
			store.lru.Remove(oldest)
		}
		element = store.lru.PushFront(&openAICodexTicketLogStream{key: key})
		store.streams[key] = element
	} else {
		store.lru.MoveToFront(element)
	}
	store.nextID++
	entry.ID = store.nextID
	entry.Time = time.Now().UTC()
	stream, _ := element.Value.(*openAICodexTicketLogStream)
	stream.entries[stream.next] = cloneOpenAICodexTicketLogEntry(entry)
	stream.next = (stream.next + 1) % OpenAICodexTicketLogLimit
	stream.size = min(stream.size+1, OpenAICodexTicketLogLimit)
	return entry.ID
}

// Attempt numbers restart after a successful harvest. Match the immutable log
// ID so a late observation never changes an earlier round or another stream.
func (store *openAICodexTicketLogStore) setEgressResult(accountID int64, model string, logID int64, result OpenAICodexTicketEgressResult) {
	result = normalizeOpenAICodexTicketEgressResult(result)
	store.mu.Lock()
	defer store.mu.Unlock()
	element := store.streams[openAICodexTicketKey(accountID, model)]
	if element == nil {
		return
	}
	stream, _ := element.Value.(*openAICodexTicketLogStream)
	for i := range stream.entries {
		if stream.entries[i].ID == logID {
			entry := &stream.entries[i]
			if entry.EgressIP == "" || result.IP != "" {
				entry.EgressIP, entry.EgressError = result.IP, result.Error
			}
			return
		}
	}
}

// snapshot 返回指定账号和模型的独立有序日志快照
func (store *openAICodexTicketLogStore) snapshot(accountID int64, model string) []OpenAICodexTicketLogEntry {
	store.mu.Lock()
	defer store.mu.Unlock()
	entries := make([]OpenAICodexTicketLogEntry, 0)
	element := store.streams[openAICodexTicketKey(accountID, model)]
	if element == nil {
		return entries
	}
	store.lru.MoveToFront(element)
	stream, _ := element.Value.(*openAICodexTicketLogStream)
	entries = make([]OpenAICodexTicketLogEntry, stream.size)
	start := (stream.next - stream.size + OpenAICodexTicketLogLimit) % OpenAICodexTicketLogLimit
	for i := range entries {
		entries[i] = cloneOpenAICodexTicketLogEntry(stream.entries[(start+i)%OpenAICodexTicketLogLimit])
	}
	return entries
}

// OpenAICodexTicketLogs returns a bounded local snapshot. Reading never starts a
// probe, increments an attempt, or writes account metadata.
func (s *OpenAIGatewayService) OpenAICodexTicketLogs(ctx context.Context, account *Account, model string, now time.Time) (*OpenAICodexTicketLogs, error) {
	model = normalizeOpenAICodexTicketModel(model)
	configured := false
	for _, candidate := range s.openAICodexTicketConfig().Models {
		if model != "" && normalizeOpenAICodexTicketModel(candidate) == model {
			configured = true
			break
		}
	}
	if !configured {
		return nil, ErrOpenAICodexTicketLogModel
	}
	result := &OpenAICodexTicketLogs{Model: model, Entries: []OpenAICodexTicketLogEntry{}, Limit: OpenAICodexTicketLogLimit}
	if s == nil || !isOpenAICodexTicketAccount(account) {
		return result, nil
	}
	result.Entries = s.openaiCodexTicketLogs.snapshot(account.ID, model)
	s.openaiCodexTicketCountries.enrich(ctx, result.Entries)
	for _, status := range s.OpenAICodexTicketStatuses(ctx, account, now) {
		if status.Model == model {
			result.Status = &status
			break
		}
	}
	return result, nil
}

// openAICodexTicketProbeErrorReason 将采票错误归一化为不含敏感信息的原因
func openAICodexTicketProbeErrorReason(err error, attempt int) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, errOpenAICodexTicketEmptyResponse):
		return "request_error"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "timeout"
	}
	if attempt > 0 {
		return "network_error"
	}
	return "request_error"
}
