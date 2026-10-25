package proxy

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	statusAvailable = "available"
	statusFailed    = "failed"

	// A refresh probes at most ProbeConcurrency proxies at a time and keeps at
	// most MaxPool entries from the upstream list. The public list holds tens of
	// thousands of entries, and one goroutine plus TLS buffers per entry costs
	// gigabytes of RAM, so both bounds stay small.
	defaultProbeConcurrency = 32
	defaultMaxPool          = 200

	// maxProxyListBytes caps how much of the upstream list is read.
	maxProxyListBytes = 8 << 20 // 8 MiB

	listFetchTimeout = 30 * time.Second
	probeTargetURL   = "https://www.google.com"
)

type ProxyState struct {
	LastUpdated    time.Time   `json:"lastUpdated"`
	UpdateInterval int         `json:"updateInterval"`
	LatencyTimeout int         `json:"latencyTimeout"`
	Proxies        []ProxyInfo `json:"proxies"`
	TotalRequests  int64       `json:"totalRequests"`
	FailedRequests int64       `json:"failedRequests"`
}

type ProxyInfo struct {
	URL            string    `json:"url"`
	LatencyMs      int       `json:"latencyMs"`
	LastUsed       time.Time `json:"lastUsed"`
	FailCount      int       `json:"failCount"`
	LastFailReason *string   `json:"lastFailReason,omitempty"`
	Status         string    `json:"status"`
}

// ProbeFunc tests whether a proxy is usable and returns its latency in
// milliseconds. A non-nil error marks the proxy as failed.
type ProbeFunc func(proxyURL string) (latencyMs int, err error)

// PoolOption customises a ProxyPool created by NewProxyPool.
type PoolOption func(*ProxyPool)

// WithMaxPool caps how many list entries one refresh probes and keeps.
func WithMaxPool(maxPool int) PoolOption {
	return func(p *ProxyPool) { p.MaxPool = maxPool }
}

// WithProbeConcurrency bounds how many proxies are probed simultaneously.
func WithProbeConcurrency(concurrency int) PoolOption {
	return func(p *ProxyPool) { p.ProbeConcurrency = concurrency }
}

type ProxyPool struct {
	listURL        string
	updateInterval int
	latencyTimeout int
	// singleProxy marks a pool built around one explicitly configured proxy.
	// Such a pool has no refresh path (Start is never called), so MarkFailed
	// must not remove that proxy from rotation.
	singleProxy bool

	// ProbeConcurrency bounds simultaneous probes during a refresh and MaxPool
	// caps how many list entries are probed and kept. ProbeFn replaces the
	// network probe so tests can stay offline; set these before Start or
	// UpdatePool.
	ProbeConcurrency int
	MaxPool          int
	ProbeFn          ProbeFunc

	mu          sync.RWMutex
	state       ProxyState
	nextIndex   int // round-robin cursor into state.Proxies, guarded by mu
	available   int // cached count of available proxies, guarded by mu
	probeOffset int // rotating window start into the fetched list, guarded by mu
	stopChan    chan struct{}
}

func newBaseProxyPool(latencyTimeout int) *ProxyPool {
	return &ProxyPool{
		latencyTimeout:   latencyTimeout,
		ProbeConcurrency: defaultProbeConcurrency,
		MaxPool:          defaultMaxPool,
		stopChan:         make(chan struct{}),
	}
}

func NewProxyPool(listURL string, updateInterval int, latencyTimeout int, opts ...PoolOption) *ProxyPool {
	pool := newBaseProxyPool(latencyTimeout)
	pool.listURL = listURL
	pool.updateInterval = updateInterval
	pool.state = ProxyState{
		UpdateInterval: updateInterval,
		LatencyTimeout: latencyTimeout,
		Proxies:        []ProxyInfo{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(pool)
		}
	}
	if pool.MaxPool < 1 {
		pool.MaxPool = defaultMaxPool
	}
	if pool.ProbeConcurrency < 1 {
		pool.ProbeConcurrency = defaultProbeConcurrency
	}
	return pool
}

func NewSingleProxyPool(proxyURL string, latencyTimeout int) *ProxyPool {
	latency := checkLatencyStatic(proxyURL, latencyTimeout)
	pool := newBaseProxyPool(latencyTimeout)
	pool.state = ProxyState{
		UpdateInterval: 0,
		LatencyTimeout: latencyTimeout,
		Proxies: []ProxyInfo{
			{
				URL:       proxyURL,
				LatencyMs: latency,
				LastUsed:  time.Now(),
				FailCount: 0,
				Status:    statusAvailable,
			},
		},
	}
	pool.singleProxy = true
	pool.available = 1
	return pool
}

func NewEmptyProxyPool() *ProxyPool {
	pool := newBaseProxyPool(0)
	pool.state = ProxyState{
		Proxies: []ProxyInfo{},
	}
	return pool
}

func (p *ProxyPool) probeTimeout() time.Duration {
	seconds := p.latencyTimeout
	if seconds <= 0 {
		seconds = 3
	}
	return time.Duration(seconds) * time.Second
}

// checkLatencyStatic probes a proxy once and falls back to the timeout value as
// latency when the probe fails. Only single-proxy mode uses the fallback: an
// explicitly configured proxy stays usable even if it is slow or down, while a
// proxy discovered from the list is dropped when its probe fails.
func checkLatencyStatic(proxyURL string, latencyTimeout int) int {
	latency, err := probeProxy(proxyURL, time.Duration(latencyTimeout)*time.Second)
	if err != nil {
		return latencyTimeout * 1000
	}
	return latency
}

func (p *ProxyPool) Start() {
	go func() {
		p.UpdatePool()
		ticker := time.NewTicker(time.Duration(p.updateInterval) * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.UpdatePool()
			case <-p.stopChan:
				return
			}
		}
	}()
}

func (p *ProxyPool) Stop() {
	close(p.stopChan)
}

func (p *ProxyPool) UpdatePool() {
	proxyList, err := p.fetchProxyList()
	if err != nil {
		log.Printf("[Proxy] refresh failed: %v", err)
		return
	}

	// Probe a bounded, rotating window of the list: one refresh never allocates
	// more than MaxPool entries, so a huge upstream list cannot blow up memory,
	// and successive refreshes sample different parts of it.
	maxPool := p.maxPoolSize()
	candidates, offset := p.nextCandidateWindow(proxyList, maxPool)

	// Fixed worker pool: goroutine count is bounded by the concurrency limit,
	// and the results channel is pre-sized so nothing blocks on collection.
	probeJobs := make(chan string)
	results := make(chan ProxyInfo, len(candidates))
	workers := min(p.probeConcurrencyLimit(), len(candidates))

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for proxyURL := range probeJobs {
				latency, err := p.checkLatency(proxyURL)
				info := ProxyInfo{URL: proxyURL, LastUsed: time.Now()}
				if err != nil {
					reason := "probe failed: " + err.Error()
					info.FailCount = 1
					info.LastFailReason = &reason
					info.Status = statusFailed
				} else {
					info.LatencyMs = latency
					info.Status = statusAvailable
				}
				results <- info
			}
		}()
	}

	go func() {
		for _, proxyURL := range candidates {
			probeJobs <- proxyURL
		}
		close(probeJobs)
	}()

	wg.Wait()
	close(results)

	proxies := make([]ProxyInfo, 0, len(candidates))
	available := 0
	for info := range results {
		if info.Status == statusAvailable {
			available++
		}
		proxies = append(proxies, info)
	}

	// Fastest available proxies first; failed probes sort last so they can
	// never be handed out even if a caller ignores Status.
	sort.Slice(proxies, func(i, j int) bool {
		iAvailable := proxies[i].Status == statusAvailable
		jAvailable := proxies[j].Status == statusAvailable
		if iAvailable != jAvailable {
			return iAvailable
		}
		return proxies[i].LatencyMs < proxies[j].LatencyMs
	})

	p.mu.Lock()
	p.state.Proxies = proxies
	p.state.LastUpdated = time.Now()
	p.available = available
	p.nextIndex = 0
	p.mu.Unlock()

	// One line per refresh (default: every 5 minutes) so operators can tell the
	// pool is actually being populated without a log line per translation.
	log.Printf("[Proxy] pool refreshed: %d available / %d probed (window offset %d of %d list lines)",
		available, len(candidates), offset, len(proxyList))
}

// nextCandidateWindow returns up to maxPool valid, de-duplicated proxy URLs
// starting at a rotating offset in proxyList, plus the offset it used. The
// offset advances by one window per refresh, so a dead or blocked head of a huge
// list cannot keep the pool empty forever; it stays in memory because the window
// itself is bounded.
func (p *ProxyPool) nextCandidateWindow(proxyList []string, maxPool int) ([]string, int) {
	candidates := make([]string, 0, min(len(proxyList), maxPool))
	if len(proxyList) == 0 {
		return candidates, 0
	}

	window := min(maxPool, len(proxyList))
	p.mu.Lock()
	offset := p.probeOffset % len(proxyList)
	p.probeOffset = (offset + window) % len(proxyList)
	p.mu.Unlock()

	// Dedupe within the window: one dead URL listed twice could otherwise stay
	// "available" after MarkFailed demoted only its first occurrence.
	seen := make(map[string]struct{}, window)
	for i := 0; i < len(proxyList) && len(candidates) < maxPool; i++ {
		proxyURL := proxyList[(offset+i)%len(proxyList)]
		parsed, err := url.Parse(proxyURL)
		if err != nil || parsed.Host == "" {
			continue
		}
		if _, ok := seen[proxyURL]; ok {
			continue
		}
		seen[proxyURL] = struct{}{}
		candidates = append(candidates, proxyURL)
	}

	return candidates, offset
}

func (p *ProxyPool) maxPoolSize() int {
	if p.MaxPool > 0 {
		return p.MaxPool
	}
	return defaultMaxPool
}

func (p *ProxyPool) probeConcurrencyLimit() int {
	if p.ProbeConcurrency > 0 {
		return p.ProbeConcurrency
	}
	return defaultProbeConcurrency
}

func (p *ProxyPool) fetchProxyList() ([]string, error) {
	client := &http.Client{Timeout: listFetchTimeout}
	resp, err := client.Get(p.listURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("proxy list: unexpected status %d", resp.StatusCode)
	}

	// Read one byte past the limit so an oversized body is detected instead of
	// silently truncated.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProxyListBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxProxyListBytes {
		return nil, fmt.Errorf("proxy list exceeds %d bytes", maxProxyListBytes)
	}

	lines := make([]string, 0, 256)
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}

	return lines, nil
}

// checkLatency runs the injected probe when set, otherwise a real network
// probe. It reports a failure instead of inventing a latency, so UpdatePool can
// mark the proxy as failed rather than advertising a dead proxy as available.
func (p *ProxyPool) checkLatency(proxyURL string) (int, error) {
	if p.ProbeFn != nil {
		return p.ProbeFn(proxyURL)
	}
	return probeProxy(proxyURL, p.probeTimeout())
}

// probeProxy performs a one-shot HEAD request through proxyURL. The transport
// disables keep-alives and is closed afterwards: a probe must not pin sockets
// or TLS buffers for proxies that are never used.
func probeProxy(proxyURL string, timeout time.Duration) (int, error) {
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return 0, err
	}
	if parsed.Host == "" {
		return 0, fmt.Errorf("proxy URL has no host")
	}

	transport := &http.Transport{
		Proxy:             func(*http.Request) (*url.URL, error) { return parsed, nil },
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: timeout}
	req, err := http.NewRequest(http.MethodHead, probeTargetURL, nil)
	if err != nil {
		return 0, err
	}

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return 0, fmt.Errorf("probe target rate limited (429)")
	}

	return int(elapsed.Milliseconds()), nil
}

// GetNextProxy returns a copy of the next available proxy in round-robin order,
// or nil when none is available. It updates LastUsed/TotalRequests under the
// write lock, so callers never share mutable pool state.
func (p *ProxyPool) GetNextProxy() *ProxyInfo {
	p.mu.Lock()
	defer p.mu.Unlock()

	total := len(p.state.Proxies)
	if p.available == 0 || total == 0 {
		return nil
	}

	for i := 0; i < total; i++ {
		index := (p.nextIndex + i) % total
		if p.state.Proxies[index].Status != statusAvailable {
			continue
		}
		p.nextIndex = (index + 1) % total
		p.state.Proxies[index].LastUsed = time.Now()
		p.state.TotalRequests++

		info := p.state.Proxies[index]
		return &info
	}

	return nil
}

func (p *ProxyPool) MarkFailed(proxyURL string, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.state.FailedRequests++

	for i := range p.state.Proxies {
		if p.state.Proxies[i].URL != proxyURL {
			continue
		}
		p.state.Proxies[i].FailCount++
		p.state.Proxies[i].LastFailReason = &reason
		// A single explicitly configured proxy has no refresh path to bring it
		// back, so keep it available and let the next attempt retry it (HEAD
		// behaviour). List-mode proxies are still demoted and replaced on the
		// next refresh.
		if p.singleProxy {
			return
		}
		if p.state.Proxies[i].Status == statusAvailable {
			p.available--
		}
		p.state.Proxies[i].Status = statusFailed
		return
	}
}

// AvailableCount reports how many proxies are currently usable. It is O(1) and
// copies nothing, which makes it safe for hot paths such as the translation
// retry loop.
func (p *ProxyPool) AvailableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.available
}

// ProxyCount reports how many proxies the pool holds, failed ones included.
func (p *ProxyPool) ProxyCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.state.Proxies)
}

func (p *ProxyPool) GetState() ProxyState {
	p.mu.RLock()
	defer p.mu.RUnlock()

	proxies := make([]ProxyInfo, len(p.state.Proxies))
	copy(proxies, p.state.Proxies)

	return ProxyState{
		LastUpdated:    p.state.LastUpdated,
		UpdateInterval: p.state.UpdateInterval,
		LatencyTimeout: p.state.LatencyTimeout,
		Proxies:        proxies,
		TotalRequests:  p.state.TotalRequests,
		FailedRequests: p.state.FailedRequests,
	}
}
