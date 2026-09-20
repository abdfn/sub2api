package service

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

type codexTicketCountryRoundTripper func(*http.Request) (*http.Response, error)

// RoundTrip 为打票回归测试提供 Round Trip 辅助实现
func (f codexTicketCountryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// codexTicketCountryTestClient 为打票回归测试提供 codex Ticket Country Test Client 辅助实现
func codexTicketCountryTestClient(fn codexTicketCountryRoundTripper) *http.Client {
	client := newOpenAICodexTicketCountryHTTPClient()
	client.Transport = fn
	return client
}

// codexTicketCountryResponse 为打票回归测试提供 codex Ticket Country Response 辅助实现
func codexTicketCountryResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

// waitForCodexTicketCountryLookups 为打票回归测试提供 wait For Codex Ticket Country Lookups 辅助实现
func waitForCodexTicketCountryLookups(t *testing.T, resolver *openAICodexTicketCountryResolver) {
	t.Helper()
	require.Eventually(t, func() bool {
		resolver.mu.Lock()
		defer resolver.mu.Unlock()
		return len(resolver.inflight) == 0
	}, time.Second, time.Millisecond)
}

// TestCodexTicketCountryLookupValidatesProviderResponse 验证 Codex Ticket Country Lookup Validates Provider Response 场景
func TestCodexTicketCountryLookupValidatesProviderResponse(t *testing.T) {
	for _, tt := range []struct {
		name, ip, body, want string
		status               int
	}{
		{name: "country", ip: "8.8.8.8", body: `{"ip":"8.8.8.8","country":"US"}`, want: "US"},
		{name: "IPv6 canonical form", ip: "2606:4700:4700::1111", body: `{"ip":"2606:4700:4700:0:0:0:0:1111","country":"AU"}`, want: "AU"},
		{name: "wrong IP", body: `{"ip":"1.1.1.1","country":"US"}`},
		{name: "missing IP", body: `{"country":"US"}`},
		{name: "private IP", body: `{"ip":"192.168.1.1","country":"US"}`},
		{name: "unknown country", body: `{"ip":"8.8.8.8","country":"ZZ"}`},
		{name: "region is not a country", body: `{"ip":"8.8.8.8","country":"EU"}`},
		{name: "invalid country", body: `{"ip":"8.8.8.8","country":"USA"}`},
		{name: "malformed", body: `invalid`},
		{name: "multiple values", body: `{"ip":"8.8.8.8","country":"US"}{}`},
		{name: "oversized", body: `{"ip":"8.8.8.8","country":"US","extra":"` + strings.Repeat("x", openAICodexTicketCountryMaxBody) + `"}`},
		{name: "HTTP error", body: `{"ip":"8.8.8.8","country":"US"}`, status: http.StatusServiceUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ip, status := tt.ip, tt.status
			if ip == "" {
				ip = "8.8.8.8"
			}
			if status == 0 {
				status = http.StatusOK
			}
			client := codexTicketCountryTestClient(func(req *http.Request) (*http.Response, error) {
				require.Equal(t, "https", req.URL.Scheme)
				require.Equal(t, "api.country.is", req.URL.Host)
				require.Equal(t, "/"+ip, req.URL.Path)
				require.Empty(t, req.URL.RawQuery)
				require.Nil(t, req.URL.User)
				require.Equal(t, http.MethodGet, req.Method)
				require.Nil(t, req.Body)
				require.Equal(t, http.Header{"Accept": []string{"application/json"}}, req.Header)
				return codexTicketCountryResponse(status, tt.body), nil
			})
			got, backoff := lookupOpenAICodexTicketCountry(context.Background(), client, ip)
			require.Equal(t, tt.want, got)
			require.True(t, backoff.IsZero())
		})
	}
}

// TestCodexTicketCountryClientIsDirectAndDoesNotFollowRedirects 验证 Codex Ticket Country Client Is Direct And Does Not Follow Redirects 场景
func TestCodexTicketCountryClientIsDirectAndDoesNotFollowRedirects(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy-user:proxy-secret@proxy.invalid:8080")
	t.Setenv("HTTPS_PROXY", "http://proxy-user:proxy-secret@proxy.invalid:8080")
	client := newOpenAICodexTicketCountryHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.Nil(t, transport.Proxy, "environment proxies must not change the local lookup route")
	require.Nil(t, client.Jar)
	require.Equal(t, openAICodexTicketCountryTimeout, client.Timeout)
	calls := 0
	client.Transport = codexTicketCountryRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "api.country.is", req.URL.Host)
		response := codexTicketCountryResponse(http.StatusFound, "")
		response.Header.Set("Location", "https://unrelated.example/private")
		return response, nil
	})
	country, backoff := lookupOpenAICodexTicketCountry(context.Background(), client, "8.8.8.8")
	require.Empty(t, country)
	require.True(t, backoff.IsZero())
	require.Equal(t, 1, calls)
}

// TestCodexTicketCountryResolverDeduplicatesAndBoundsBackgroundWork 验证 Codex Ticket Country Resolver Deduplicates And Bounds Background Work 场景
func TestCodexTicketCountryResolverDeduplicatesAndBoundsBackgroundWork(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	resolver := openAICodexTicketCountryResolver{
		limiter: rate.NewLimiter(rate.Inf, 1),
		client: codexTicketCountryTestClient(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			select {
			case <-release:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			return codexTicketCountryResponse(http.StatusOK, fmt.Sprintf(`{"ip":%q,"country":"US"}`, strings.TrimPrefix(req.URL.Path, "/"))), nil
		}),
	}
	defer func() { close(release); waitForCodexTicketCountryLookups(t, &resolver) }()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resolver.lookup("8.8.8.8", time.Now())
		}()
	}
	wg.Wait()
	for _, ip := range []string{"1.1.1.1", "9.9.9.9", "8.8.4.4", "1.0.0.1"} {
		require.Empty(t, resolver.lookup(ip, time.Now()))
	}
	require.Eventually(t, func() bool { return calls.Load() == 4 }, time.Second, time.Millisecond)
	resolver.mu.Lock()
	_, queued := resolver.inflight["1.0.0.1"]
	active := len(resolver.inflight)
	resolver.mu.Unlock()
	require.False(t, queued, "the next poll can retry; there is no unbounded work queue")
	require.Equal(t, openAICodexTicketCountryMaxConcurrent, active)
}

// TestCodexTicketCountryResolverCachesByIPAndExpires 验证 Codex Ticket Country Resolver Caches By IPAnd Expires 场景
func TestCodexTicketCountryResolverCachesByIPAndExpires(t *testing.T) {
	var calls atomic.Int64
	resolver := openAICodexTicketCountryResolver{
		limiter: rate.NewLimiter(rate.Inf, 1),
		client: codexTicketCountryTestClient(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			country := "US"
			if req.URL.Path == "/1.1.1.1" {
				country = "AU"
			}
			return codexTicketCountryResponse(http.StatusOK, fmt.Sprintf(`{"ip":%q,"country":%q}`, strings.TrimPrefix(req.URL.Path, "/"), country)), nil
		}),
	}
	for _, ip := range []string{"8.8.8.8", "1.1.1.1"} {
		require.Empty(t, resolver.lookup(ip, time.Now()))
	}
	waitForCodexTicketCountryLookups(t, &resolver)
	for range 3 {
		require.Equal(t, "US", resolver.lookup("8.8.8.8", time.Now()))
		require.Equal(t, "AU", resolver.lookup("1.1.1.1", time.Now()))
	}
	require.Equal(t, int64(2), calls.Load())
	resolver.mu.Lock()
	entry, _ := resolver.cache["8.8.8.8"].Value.(*openAICodexTicketCountryCacheEntry)
	entry.expires = time.Now().Add(-time.Second)
	resolver.mu.Unlock()
	require.Empty(t, resolver.lookup("8.8.8.8", time.Now()))
	waitForCodexTicketCountryLookups(t, &resolver)
	require.Equal(t, "US", resolver.lookup("8.8.8.8", time.Now()))
	require.Equal(t, int64(3), calls.Load())
}

// TestCodexTicketCountryResolverBoundsCacheAndKeepsRecentlyUsedIP 验证 Codex Ticket Country Resolver Bounds Cache And Keeps Recently Used IP 场景
func TestCodexTicketCountryResolverBoundsCacheAndKeepsRecentlyUsedIP(t *testing.T) {
	resolver := openAICodexTicketCountryResolver{
		cache:   make(map[string]*list.Element),
		limiter: rate.NewLimiter(rate.Inf, 1),
		client: codexTicketCountryTestClient(func(*http.Request) (*http.Response, error) {
			return codexTicketCountryResponse(http.StatusOK, `{"ip":"8.8.8.8","country":"US"}`), nil
		}),
	}
	for i := range openAICodexTicketCountryCacheLimit {
		ip := fmt.Sprintf("11.0.%d.%d", i/256, i%256)
		entry := &openAICodexTicketCountryCacheEntry{ip: ip, country: "AU", expires: time.Now().Add(time.Hour)}
		resolver.cache[ip] = resolver.lru.PushFront(entry)
	}
	require.Equal(t, "AU", resolver.lookup("11.0.0.0", time.Now()))
	require.Empty(t, resolver.lookup("8.8.8.8", time.Now()))
	waitForCodexTicketCountryLookups(t, &resolver)
	resolver.mu.Lock()
	cacheSize, listSize := len(resolver.cache), resolver.lru.Len()
	_, recentRetained := resolver.cache["11.0.0.0"]
	_, oldestRetained := resolver.cache["11.0.0.1"]
	resolver.mu.Unlock()
	require.Equal(t, openAICodexTicketCountryCacheLimit, cacheSize)
	require.Equal(t, cacheSize, listSize)
	require.True(t, recentRetained)
	require.False(t, oldestRetained)
}

// TestCodexTicketCountryResolverPrioritizesNewestEntries 验证 Codex Ticket Country Resolver Prioritizes Newest Entries 场景
func TestCodexTicketCountryResolverPrioritizesNewestEntries(t *testing.T) {
	release := make(chan struct{})
	resolver := openAICodexTicketCountryResolver{
		limiter: rate.NewLimiter(rate.Inf, 1),
		client: codexTicketCountryTestClient(func(req *http.Request) (*http.Response, error) {
			select {
			case <-release:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			return codexTicketCountryResponse(http.StatusServiceUnavailable, ""), nil
		}),
	}
	defer func() { close(release); waitForCodexTicketCountryLookups(t, &resolver) }()
	entries := make([]OpenAICodexTicketLogEntry, 6)
	for i := range entries {
		entries[i].EgressIP = fmt.Sprintf("8.8.8.%d", i+1)
	}
	resolver.enrich(context.Background(), entries)
	resolver.mu.Lock()
	active := make(map[string]bool)
	for ip := range resolver.inflight {
		active[ip] = true
	}
	resolver.mu.Unlock()
	require.Equal(t, map[string]bool{"8.8.8.3": true, "8.8.8.4": true, "8.8.8.5": true, "8.8.8.6": true}, active)
}

// TestCodexTicketCountryResolverIgnoresUnknownOrPrivateIP 验证 Codex Ticket Country Resolver Ignores Unknown Or Private IP 场景
func TestCodexTicketCountryResolverIgnoresUnknownOrPrivateIP(t *testing.T) {
	var resolver openAICodexTicketCountryResolver
	for _, ip := range []string{"", "private-host", "proxy-user:proxy-secret@private-host:1080", "8.8.8.8/path", "127.0.0.1", "192.168.0.1", "::1", "::ffff:192.168.0.1", "fe80::1", "2606:4700:4700::1111%eth0"} {
		require.Empty(t, resolver.lookup(ip, time.Now()))
	}
	require.Nil(t, resolver.client)
	require.Empty(t, resolver.inflight)
}

// TestCodexTicketCountryFailuresAreCachedAnd429BacksOff 验证 Codex Ticket Country Failures Are Cached And429 Backs Off 场景
func TestCodexTicketCountryFailuresAreCachedAnd429BacksOff(t *testing.T) {
	for _, scenario := range []string{"network", "response", "rate limit"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int64
			resolver := openAICodexTicketCountryResolver{
				limiter: rate.NewLimiter(rate.Inf, 1),
				client: codexTicketCountryTestClient(func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					if scenario == "network" {
						return nil, errors.New("private network diagnostic")
					}
					if scenario == "rate limit" {
						response := codexTicketCountryResponse(http.StatusTooManyRequests, "")
						response.Header.Set("Retry-After", "120")
						return response, nil
					}
					return codexTicketCountryResponse(http.StatusOK, `{"ip":"1.1.1.1","country":"US"}`), nil
				}),
			}
			require.Empty(t, resolver.lookup("8.8.8.8", time.Now()))
			waitForCodexTicketCountryLookups(t, &resolver)
			for range 5 {
				require.Empty(t, resolver.lookup("8.8.8.8", time.Now()))
			}
			require.Equal(t, int64(1), calls.Load())
			if scenario == "rate limit" {
				require.Empty(t, resolver.lookup("9.9.9.9", time.Now()))
				resolver.mu.Lock()
				backoff, active := resolver.backoffUntil, len(resolver.inflight)
				resolver.mu.Unlock()
				require.True(t, backoff.After(time.Now().Add(119*time.Second)))
				require.Zero(t, active)
				require.Equal(t, int64(1), calls.Load())
			}
		})
	}
}

// TestCodexTicketCountryBackoffIsBounded 验证 Codex Ticket Country Backoff Is Bounded 场景
func TestCodexTicketCountryBackoffIsBounded(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tt := range []struct {
		header string
		want   time.Duration
	}{
		{"", time.Minute}, {"-5", time.Minute}, {"120", 2 * time.Minute},
		{"3600", 15 * time.Minute}, {"9999999999999999", time.Minute},
		{now.Add(3 * time.Minute).UTC().Format(http.TimeFormat), 3 * time.Minute},
	} {
		require.Equal(t, now.Add(tt.want), openAICodexTicketCountryBackoff(tt.header, now))
	}
}

// TestCodexTicketLogPollingEnrichesWithoutBlockingOrStartingHarvest 验证 Codex Ticket Log Polling Enriches Without Blocking Or Starting Harvest 场景
func TestCodexTicketLogPollingEnrichesWithoutBlockingOrStartingHarvest(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var queries atomic.Int64
	upstream := &httpUpstreamRecorder{}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true}, upstream)
	svc.openaiCodexTicketCountries.client = codexTicketCountryTestClient(func(req *http.Request) (*http.Response, error) {
		queries.Add(1)
		close(started)
		select {
		case <-release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		return codexTicketCountryResponse(http.StatusOK, `{"ip":"8.8.8.8","country":"US"}`), nil
	})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		waitForCodexTicketCountryLookups(t, &svc.openaiCodexTicketCountries)
	}()
	account := ticketTestAccount(41)
	for range 2 {
		svc.openaiCodexTicketLogs.append(account.ID, openAICodexTicketDefaultModel, OpenAICodexTicketLogEntry{EgressIP: "8.8.8.8", Attempt: 1})
	}
	ctx, cancel := context.WithCancel(context.Background())
	logs, err := svc.OpenAICodexTicketLogs(ctx, account, openAICodexTicketDefaultModel, time.Now())
	require.NoError(t, err)
	require.Len(t, logs.Entries, 2)
	require.Equal(t, "8.8.8.8", logs.Entries[0].EgressIP)
	require.Empty(t, logs.Entries[0].EgressCountryCode)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("country lookup did not start")
	}
	cancel() // The next poll can reuse the bounded lookup after closing a dialog.
	close(release)
	waitForCodexTicketCountryLookups(t, &svc.openaiCodexTicketCountries)
	logs, err = svc.OpenAICodexTicketLogs(context.Background(), account, openAICodexTicketDefaultModel, time.Now())
	require.NoError(t, err)
	for _, entry := range logs.Entries {
		require.Equal(t, "US", entry.EgressCountryCode)
	}
	require.Equal(t, int64(1), queries.Load())
	require.Empty(t, upstream.requests)
	require.Equal(t, 0, logs.Status.Attempts)
	require.Empty(t, svc.openaiCodexTicketLogs.snapshot(account.ID, openAICodexTicketDefaultModel)[0].EgressCountryCode, "country enrichment does not mutate the event store")
	require.Equal(t, rate.Limit(8), svc.openaiCodexTicketCountries.limiter.Limit())
	require.Equal(t, 1, svc.openaiCodexTicketCountries.limiter.Burst())
}
