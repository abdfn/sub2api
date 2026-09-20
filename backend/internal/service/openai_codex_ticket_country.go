package service

import (
	"container/list"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/language"
	"golang.org/x/time/rate"
)

const (
	openAICodexTicketCountryURL           = "https://api.country.is/"
	openAICodexTicketCountryCacheLimit    = 2048
	openAICodexTicketCountryMaxConcurrent = 4
	openAICodexTicketCountryTTL           = 24 * time.Hour
	openAICodexTicketCountryFailureTTL    = time.Minute
	openAICodexTicketCountryTimeout       = 3 * time.Second
	openAICodexTicketCountryMaxBody       = 4096
)

type openAICodexTicketCountryCacheEntry struct {
	ip      string
	country string
	expires time.Time
}

// A lookup only uses the confirmed IP recorded on the ticket's connection.
// Lookup traffic uses its own direct client, never the harvest proxy or account
// credentials. Its zero value is ready for use; client is injectable in tests.
// Its LRU list only contains *openAICodexTicketCountryCacheEntry values.
type openAICodexTicketCountryResolver struct {
	mu           sync.Mutex
	cache        map[string]*list.Element
	lru          list.List
	inflight     map[string]struct{}
	client       *http.Client
	limiter      *rate.Limiter
	backoffUntil time.Time
}

// normalizeOpenAICodexTicketEgressIP 仅接受可用于出口归属查询的公网地址
func normalizeOpenAICodexTicketEgressIP(value string) string {
	ip, err := netip.ParseAddr(value)
	if err != nil || ip.Zone() != "" {
		return ""
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return ""
	}
	return ip.String()
}

// newOpenAICodexTicketCountryHTTPClient 创建不继承代理和重定向的国家查询客户端
func newOpenAICodexTicketCountryHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			// Deliberately ignore HTTP(S)_PROXY and the ticket harvest proxy.
			Proxy:                  nil,
			DialContext:            (&net.Dialer{Timeout: openAICodexTicketCountryTimeout}).DialContext,
			ForceAttemptHTTP2:      true,
			TLSHandshakeTimeout:    openAICodexTicketCountryTimeout,
			ResponseHeaderTimeout:  openAICodexTicketCountryTimeout,
			MaxResponseHeaderBytes: 8192,
			MaxConnsPerHost:        openAICodexTicketCountryMaxConcurrent,
			MaxIdleConns:           openAICodexTicketCountryMaxConcurrent,
			MaxIdleConnsPerHost:    openAICodexTicketCountryMaxConcurrent,
			IdleConnTimeout:        30 * time.Second,
		},
		Timeout: openAICodexTicketCountryTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// enrich never waits on network I/O and never changes stored log entries. Misses
// are resolved in bounded background work for the next dialog poll.
func (r *openAICodexTicketCountryResolver) enrich(ctx context.Context, entries []OpenAICodexTicketLogEntry) {
	if ctx.Err() != nil {
		return
	}
	seen := make(map[string]string)
	// Start with the newest events so a long history cannot delay the current
	// attempt behind older rotating addresses when all lookup slots are busy.
	for i := len(entries) - 1; i >= 0; i-- {
		ip := entries[i].EgressIP
		if ip == "" {
			continue
		}
		country, ok := seen[ip]
		if !ok {
			country = r.lookup(ip, time.Now())
			seen[ip] = country
		}
		entries[i].EgressCountryCode = country
	}
}

// lookup 优先读取国家缓存并去重提交受限的后台查询
func (r *openAICodexTicketCountryResolver) lookup(ip string, now time.Time) string {
	ip = normalizeOpenAICodexTicketEgressIP(ip)
	if ip == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if element := r.cache[ip]; element != nil {
		entry, _ := element.Value.(*openAICodexTicketCountryCacheEntry)
		if now.Before(entry.expires) {
			r.lru.MoveToFront(element)
			return entry.country
		}
		delete(r.cache, ip)
		r.lru.Remove(element)
	}
	if _, busy := r.inflight[ip]; busy || len(r.inflight) >= openAICodexTicketCountryMaxConcurrent || now.Before(r.backoffUntil) {
		return ""
	}
	if r.inflight == nil {
		r.inflight = make(map[string]struct{})
	}
	if r.client == nil {
		r.client = newOpenAICodexTicketCountryHTTPClient()
	}
	if r.limiter == nil {
		// Country.is permits 10 requests/second. Leave headroom and avoid bursts.
		r.limiter = rate.NewLimiter(rate.Every(time.Second/8), 1)
	}
	r.inflight[ip] = struct{}{}
	go r.resolve(ip, r.client, r.limiter)
	return ""
}

// resolve 查询出口国家并更新有界缓存和服务退避状态
func (r *openAICodexTicketCountryResolver) resolve(ip string, client *http.Client, limiter *rate.Limiter) {
	// Closing a dialog never cancels a ticket; only this independent lookup has
	// a short deadline. No request context, proxy settings or credentials survive.
	ctx, cancel := context.WithTimeout(context.Background(), openAICodexTicketCountryTimeout)
	defer cancel()
	var country string
	var backoff time.Time
	if err := limiter.Wait(ctx); err == nil {
		r.mu.Lock()
		backingOff := time.Now().Before(r.backoffUntil)
		r.mu.Unlock()
		if !backingOff {
			country, backoff = lookupOpenAICodexTicketCountry(ctx, client, ip)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inflight, ip)
	if backoff.After(r.backoffUntil) {
		r.backoffUntil = backoff
	}
	if r.cache == nil {
		r.cache = make(map[string]*list.Element)
	}
	if len(r.cache) >= openAICodexTicketCountryCacheLimit {
		oldest := r.lru.Back()
		oldestEntry, _ := oldest.Value.(*openAICodexTicketCountryCacheEntry)
		delete(r.cache, oldestEntry.ip)
		r.lru.Remove(oldest)
	}
	ttl := openAICodexTicketCountryTTL
	if country == "" {
		ttl = openAICodexTicketCountryFailureTTL
	}
	entry := &openAICodexTicketCountryCacheEntry{ip: ip, country: country, expires: time.Now().Add(ttl)}
	r.cache[ip] = r.lru.PushFront(entry)
}

// lookupOpenAICodexTicketCountry 查询并验证出口地址对应的国家代码
func lookupOpenAICodexTicketCountry(ctx context.Context, client *http.Client, ip string) (country string, retryAt time.Time) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openAICodexTicketCountryURL+ip, nil)
	if err != nil {
		return "", time.Time{}
	}
	req.Header.Set("Accept", "application/json")
	logHTTP := BeginOpenAICodexTicketHTTPLog(ctx, req.Method, req.URL.String(), map[string]any{"ip": ip})
	status := 0
	defer func() {
		logHTTP(status, map[string]any{"country": country, "retry_at": retryAt}, err)
	}()
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}
	}
	defer func() { _ = resp.Body.Close() }()
	status = resp.StatusCode
	if resp.StatusCode == http.StatusTooManyRequests {
		return "", openAICodexTicketCountryBackoff(resp.Header.Get("Retry-After"), time.Now())
	}
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, openAICodexTicketCountryMaxBody+1))
	if err != nil || len(body) > openAICodexTicketCountryMaxBody {
		return "", time.Time{}
	}
	var result struct {
		IP      string `json:"ip"`
		Country string `json:"country"`
	}
	if json.Unmarshal(body, &result) != nil || normalizeOpenAICodexTicketEgressIP(result.IP) != ip {
		return "", time.Time{}
	}
	country = strings.ToUpper(result.Country)
	if len(country) != 2 {
		return "", time.Time{}
	}
	region, err := language.ParseRegion(country)
	if err != nil || !region.IsCountry() {
		return "", time.Time{}
	}
	return country, time.Time{}
}

// openAICodexTicketCountryBackoff 解析并限制国家查询服务的重试等待时间
func openAICodexTicketCountryBackoff(retryAfter string, now time.Time) time.Time {
	delay := time.Minute
	if seconds, err := strconv.ParseInt(retryAfter, 10, 32); err == nil {
		delay = time.Duration(seconds) * time.Second
	} else if retryAt, err := http.ParseTime(retryAfter); err == nil {
		delay = retryAt.Sub(now)
	}
	if delay < time.Minute {
		delay = time.Minute
	} else if delay > 15*time.Minute {
		delay = 15 * time.Minute
	}
	return now.Add(delay)
}
