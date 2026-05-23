package proxy

import (
	"io/ioutil"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"
)

type ProxyState struct {
	LastUpdated    time.Time   `json:"lastUpdated"`
	UpdateInterval  int         `json:"updateInterval"`
	LatencyTimeout  int         `json:"latencyTimeout"`
	Proxies         []ProxyInfo `json:"proxies"`
	TotalRequests   int64       `json:"totalRequests"`
	FailedRequests  int64       `json:"failedRequests"`
}

type ProxyInfo struct {
	URL           string  `json:"url"`
	LatencyMs     int     `json:"latencyMs"`
	LastUsed      time.Time `json:"lastUsed"`
	FailCount     int     `json:"failCount"`
	LastFailReason *string `json:"lastFailReason,omitempty"`
	Status        string  `json:"status"`
}

type ProxyPool struct {
	listURL         string
	updateInterval  int
	latencyTimeout  int
	mu              sync.RWMutex
	proxyCache      sync.Map
	state           ProxyState
	stopChan        chan struct{}
}

func NewProxyPool(listURL string, updateInterval int, latencyTimeout int) *ProxyPool {
	return &ProxyPool{
		listURL:        listURL,
		updateInterval: updateInterval,
		latencyTimeout: latencyTimeout,
		stopChan:       make(chan struct{}),
		state: ProxyState{
			UpdateInterval: updateInterval,
			LatencyTimeout: latencyTimeout,
			Proxies:        []ProxyInfo{},
		},
	}
}

func NewSingleProxyPool(proxyURL string, latencyTimeout int) *ProxyPool {
	latency := checkLatencyStatic(proxyURL, latencyTimeout)
	return &ProxyPool{
		latencyTimeout: latencyTimeout,
		stopChan:       make(chan struct{}),
		state: ProxyState{
			UpdateInterval: 0,
			LatencyTimeout: latencyTimeout,
			Proxies: []ProxyInfo{
				{
					URL:       proxyURL,
					LatencyMs: latency,
					LastUsed:  time.Now(),
					FailCount: 0,
					Status:    "available",
				},
			},
		},
	}
}

func NewEmptyProxyPool() *ProxyPool {
	return &ProxyPool{
		stopChan: make(chan struct{}),
		state: ProxyState{
			Proxies: []ProxyInfo{},
		},
	}
}

func checkLatencyStatic(proxyURL string, latencyTimeout int) int {
	proxyFunc := func(_ *http.Request) (*url.URL, error) {
		return url.Parse(proxyURL)
	}

	client := &http.Client{
		Transport: &http.Transport{Proxy: proxyFunc},
		Timeout:   time.Duration(latencyTimeout) * time.Second,
	}

	start := time.Now()
	req, _ := http.NewRequest("HEAD", "https://www.google.com", nil)
	resp, err := client.Do(req)
	elapsed := time.Since(start)

	if err != nil {
		return latencyTimeout * 1000
	}
	resp.Body.Close()

	if resp.StatusCode == 429 {
		return latencyTimeout * 1000
	}

	return int(elapsed.Milliseconds())
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
		return
	}

	var wg sync.WaitGroup
	proxyChan := make(chan ProxyInfo, len(proxyList))

	for _, proxyURL := range proxyList {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			latency := p.checkLatency(url)
			proxyChan <- ProxyInfo{
				URL:       url,
				LatencyMs: latency,
				LastUsed:  time.Now(),
				FailCount: 0,
				Status:    "available",
			}
		}(proxyURL)
	}

	wg.Wait()
	close(proxyChan)

	var proxies []ProxyInfo
	for proxy := range proxyChan {
		proxies = append(proxies, proxy)
	}

	sort.Slice(proxies, func(i, j int) bool {
		return proxies[i].LatencyMs < proxies[j].LatencyMs
	})

	p.mu.Lock()
	p.state.Proxies = proxies
	p.state.LastUpdated = time.Now()
	for _, proxy := range proxies {
		p.proxyCache.Store(proxy.URL, proxy)
	}
	p.mu.Unlock()
}

func (p *ProxyPool) fetchProxyList() ([]string, error) {
	resp, err := http.Get(p.listURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	lines := make([]string, 0)
	start := 0
	for i := 0; i < len(body); i++ {
		if body[i] == '\n' {
			line := string(body[start:i])
			if len(line) > 0 {
				lines = append(lines, line)
			}
			start = i + 1
		}
	}
	if start < len(body) {
		line := string(body[start:])
		if len(line) > 0 {
			lines = append(lines, line)
		}
	}

	return lines, nil
}

func (p *ProxyPool) checkLatency(proxyURL string) int {
	proxy := func(_ *http.Request) (*url.URL, error) {
		return url.Parse(proxyURL)
	}

	client := &http.Client{
		Transport: &http.Transport{Proxy: proxy},
		Timeout:   time.Duration(p.latencyTimeout) * time.Second,
	}

	start := time.Now()
	req, _ := http.NewRequest("HEAD", "https://www.google.com", nil)
	resp, err := client.Do(req)
	elapsed := time.Since(start)

	if err != nil {
		return p.latencyTimeout * 1000
	}
	resp.Body.Close()

	if resp.StatusCode == 429 {
		return p.latencyTimeout * 1000
	}

	return int(elapsed.Milliseconds())
}

func (p *ProxyPool) GetNextProxy() *ProxyInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for i := range p.state.Proxies {
		if p.state.Proxies[i].Status == "available" {
			proxy := &p.state.Proxies[i]
			proxy.LastUsed = time.Now()
			p.state.TotalRequests++
			return proxy
		}
	}

	return nil
}

func (p *ProxyPool) MarkFailed(proxyURL string, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.state.FailedRequests++

	if proxy, ok := p.proxyCache.Load(proxyURL); ok {
		pinfo := proxy.(ProxyInfo)
		pinfo.FailCount++
		pinfo.LastFailReason = &reason
		pinfo.Status = "failed"
		p.proxyCache.Store(proxyURL, pinfo)

		for i := range p.state.Proxies {
			if p.state.Proxies[i].URL == proxyURL {
				p.state.Proxies[i].FailCount++
				p.state.Proxies[i].LastFailReason = &reason
				p.state.Proxies[i].Status = "failed"
				break
			}
		}
	}
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