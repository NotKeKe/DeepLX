package proxy

// Offline, deterministic tests for the proxy-pool fix. No test contacts a real
// proxy or a real list URL: every list fetch goes through a local httptest
// server and every probe goes through ProxyPool.ProbeFn.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeList is a local proxy-list server. Tests change body/status between
// refreshes to exercise fetchProxyList without any external network.
type fakeList struct {
	srv    *httptest.Server
	mu     sync.RWMutex
	body   string
	status int
	hits   atomic.Int64
}

func newFakeList(t *testing.T, body string) *fakeList {
	t.Helper()
	fl := &fakeList{body: body}
	fl.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl.hits.Add(1)
		fl.mu.RLock()
		body, status := fl.body, fl.status
		fl.mu.RUnlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(fl.srv.Close)
	return fl
}

func (f *fakeList) URL() string { return f.srv.URL }

func (f *fakeList) setBody(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body = body
}

func (f *fakeList) setStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

// testProxyHost is used only for URL parsing; injected probes never dial it.
const testProxyHost = "127.0.0.1:9"

// proxyURL returns the i-th synthetic proxy URL used by the helpers below.
func proxyURL(i int) string {
	return fmt.Sprintf("http://%s/p/%06d", testProxyHost, i)
}

func proxyURLs(n int) []string {
	urls := make([]string, n)
	for i := range urls {
		urls[i] = proxyURL(i)
	}
	return urls
}

func proxyListBody(n int) string {
	var b strings.Builder
	for _, u := range proxyURLs(n) {
		b.WriteString(u)
		b.WriteByte('\n')
	}
	return b.String()
}

// newTestPool builds a pool whose listURL points at fl. Nothing is fetched or
// probed until the test calls UpdatePool.
func newTestPool(t *testing.T, fl *fakeList, opts ...PoolOption) *ProxyPool {
	t.Helper()
	p := NewProxyPool(fl.URL(), 5, 3, opts...)
	t.Cleanup(p.Stop)
	return p
}

// okProbe returns a ProbeFn that reports every proxy as available with the
// latency from latencies, or latency when the URL is not in the map.
func okProbe(latencies map[string]int, fallback int) ProbeFunc {
	return func(proxyURL string) (int, error) {
		if latency, ok := latencies[proxyURL]; ok {
			return latency, nil
		}
		return fallback, nil
	}
}

// notePeak stores v in dst when it is a new maximum.
func notePeak(dst *atomic.Int64, v int64) {
	for {
		old := dst.Load()
		if v <= old || dst.CompareAndSwap(old, v) {
			return
		}
	}
}

// TestUpdatePoolBoundsProbesAndGoroutines is the core regression test for the
// 85k-line-list defect: a 5000-line list with MaxPool=200 must probe exactly 200
// proxies and must not spawn anywhere near one goroutine per list line.
func TestUpdatePoolBoundsProbesAndGoroutines(t *testing.T) {
	const (
		listLines = 5000
		maxPool   = 200
	)
	fl := newFakeList(t, proxyListBody(listLines))
	p := newTestPool(t, fl, WithMaxPool(maxPool), WithProbeConcurrency(32))

	var probes, peakGoroutines atomic.Int64
	p.ProbeFn = func(string) (int, error) {
		probes.Add(1)
		notePeak(&peakGoroutines, int64(runtime.NumGoroutine()))
		// Give the worker pool time to reach its steady state so the sampled
		// goroutine count is representative of the refresh peak.
		time.Sleep(2 * time.Millisecond)
		return 5, nil
	}

	start := time.Now()
	p.UpdatePool()
	t.Logf("refresh of %d-line list took %v", listLines, time.Since(start))

	if got := probes.Load(); got != maxPool {
		t.Fatalf("probed %d proxies, want exactly MaxPool=%d (list had %d lines)", got, maxPool, listLines)
	}
	if got := p.ProxyCount(); got != maxPool {
		t.Errorf("ProxyCount() = %d, want %d", got, maxPool)
	}
	if got := p.AvailableCount(); got != maxPool {
		t.Errorf("AvailableCount() = %d, want %d", got, maxPool)
	}
	if got := len(p.GetState().Proxies); got != maxPool {
		t.Errorf("len(GetState().Proxies) = %d, want %d", got, maxPool)
	}
	// Workers = min(ProbeConcurrency=32, candidates=200) = 32, plus the feeder,
	// the update goroutine and test/server goroutines; "cap+snapshot" = 200+20.
	if got := peakGoroutines.Load(); got > 220 {
		t.Errorf("peak goroutines during UpdatePool = %d, want <= 220 (cap 200 + snapshot margin); fix is not bounding workers", got)
	}
	if got := peakGoroutines.Load(); got < 2 {
		t.Errorf("peak goroutines = %d, probe hook never observed concurrent workers", got)
	}
	if !strings.Contains(fl.URL(), "http") {
		t.Fatalf("bad fake list URL %q", fl.URL())
	}
	if got := fl.hits.Load(); got != 1 {
		t.Errorf("list server hit %d times, want 1", got)
	}

	t.Run("invalid lines are skipped without probing", func(t *testing.T) {
		bad := newFakeList(t, "not-a-url\nhttp:///no-host\n\n   \n")
		p := newTestPool(t, bad)
		var probes atomic.Int64
		p.ProbeFn = func(string) (int, error) {
			probes.Add(1)
			return 1, nil
		}
		p.UpdatePool()
		if got := probes.Load(); got != 0 {
			t.Errorf("probed %d invalid entries, want 0", got)
		}
		if got := p.ProxyCount(); got != 0 {
			t.Errorf("ProxyCount() = %d, want 0", got)
		}
	})
}

// TestUpdatePoolRetainsFailedProxiesButNeverServesThem checks the two halves of
// the old "dead proxies look available" bug: probe failures must be recorded as
// "failed" (retained, counted, never handed out) and the survivors must be
// sorted by ascending latency.
func TestUpdatePoolRetainsFailedProxiesButNeverServesThem(t *testing.T) {
	fl := newFakeList(t, proxyListBody(5))
	p := newTestPool(t, fl)

	// Deliberately out of order: 10ms, 30ms, 20ms; entries 3 and 4 fail.
	latencies := map[string]int{
		proxyURL(0): 10,
		proxyURL(1): 30,
		proxyURL(2): 20,
	}
	p.ProbeFn = func(u string) (int, error) {
		if latency, ok := latencies[u]; ok {
			return latency, nil
		}
		return 0, fmt.Errorf("dial %s: connection refused", u)
	}

	p.UpdatePool()

	state := p.GetState()
	if len(state.Proxies) != 5 {
		t.Fatalf("len(Proxies) = %d, want 5 (failed probes must be retained)", len(state.Proxies))
	}
	if got := p.AvailableCount(); got != 3 {
		t.Errorf("AvailableCount() = %d, want 3", got)
	}
	if got := p.ProxyCount(); got != 5 {
		t.Errorf("ProxyCount() = %d, want 5", got)
	}

	wantOrder := []struct {
		url     string
		latency int
	}{
		{proxyURL(0), 10},
		{proxyURL(2), 20},
		{proxyURL(1), 30},
	}
	for i, want := range wantOrder {
		info := state.Proxies[i]
		if info.Status != "available" {
			t.Errorf("Proxies[%d].Status = %q, want available", i, info.Status)
		}
		if info.URL != want.url {
			t.Errorf("Proxies[%d].URL = %q, want %q (available proxies must sort ascending by latency)", i, info.URL, want.url)
		}
		if info.LatencyMs != want.latency {
			t.Errorf("Proxies[%d].LatencyMs = %d, want %d", i, info.LatencyMs, want.latency)
		}
	}
	for i := 3; i < len(state.Proxies); i++ {
		info := state.Proxies[i]
		if info.Status != "failed" {
			t.Errorf("Proxies[%d].Status = %q, want failed", i, info.Status)
		}
		if info.FailCount != 1 {
			t.Errorf("Proxies[%d].FailCount = %d, want 1", i, info.FailCount)
		}
		if info.LastFailReason == nil || *info.LastFailReason == "" {
			t.Errorf("Proxies[%d].LastFailReason is empty, want probe error reason", i)
		}
	}

	for i := 0; i < 30; i++ {
		info := p.GetNextProxy()
		if info == nil {
			t.Fatalf("GetNextProxy() returned nil on call %d while 3 proxies are available", i)
		}
		if _, ok := latencies[info.URL]; !ok {
			t.Fatalf("GetNextProxy() served failed proxy %q", info.URL)
		}
	}
}

// TestGetNextProxyRoundRobin pins the new rotation behaviour: N available
// proxies are returned exactly once each per N calls.
func TestGetNextProxyRoundRobin(t *testing.T) {
	const n = 5
	fl := newFakeList(t, proxyListBody(n))
	p := newTestPool(t, fl)
	p.ProbeFn = okProbe(nil, 7)
	p.UpdatePool()

	counts := make(map[string]int, n)
	for i := 0; i < 2*n; i++ {
		info := p.GetNextProxy()
		if info == nil {
			t.Fatalf("GetNextProxy() = nil on call %d", i)
		}
		counts[info.URL]++
	}
	if len(counts) != n {
		t.Fatalf("saw %d distinct proxies in %d calls, want %d (round-robin)", len(counts), 2*n, n)
	}
	for _, u := range proxyURLs(n) {
		if got := counts[u]; got != 2 {
			t.Errorf("proxy %q served %d times in 10 calls, want 2", u, got)
		}
	}
	if got := p.GetState().TotalRequests; got != int64(2*n) {
		t.Errorf("TotalRequests = %d, want %d", got, 2*n)
	}
}

// TestGetNextProxyReturnsCopy makes sure callers own the returned ProxyInfo:
// mutating it (URL, latency, status, ...) must not corrupt pool state, and the
// rotation must keep handing out the original entries.
func TestGetNextProxyReturnsCopy(t *testing.T) {
	const n = 3
	fl := newFakeList(t, proxyListBody(n))
	p := newTestPool(t, fl)
	p.ProbeFn = okProbe(map[string]int{proxyURL(0): 11, proxyURL(1): 22, proxyURL(2): 33}, 0)
	p.UpdatePool()

	first := p.GetNextProxy()
	if first == nil {
		t.Fatal("GetNextProxy() = nil")
	}
	origURL, origLatency := first.URL, first.LatencyMs
	first.URL = "http://mutated:1"
	first.LatencyMs = 999999
	first.Status = "failed"
	first.FailCount = 42
	first.LastUsed = time.Now().Add(24 * time.Hour)
	first.LastFailReason = nil

	state := p.GetState()
	found := false
	for _, info := range state.Proxies {
		if info.URL == origURL {
			found = true
			if info.LatencyMs != origLatency {
				t.Errorf("pool latency for %q = %d after mutating the returned copy, want %d", origURL, info.LatencyMs, origLatency)
			}
			if info.Status != "available" || info.FailCount != 0 {
				t.Errorf("pool state for %q changed: status=%q failCount=%d", origURL, info.Status, info.FailCount)
			}
		}
		if info.URL == "http://mutated:1" {
			t.Errorf("mutated URL leaked into pool state")
		}
	}
	if !found {
		t.Fatalf("original proxy %q disappeared from state", origURL)
	}

	second := p.GetNextProxy()
	if second == nil {
		t.Fatal("second GetNextProxy() = nil")
	}
	if second.URL == origURL {
		t.Errorf("round-robin returned %q twice in a row with %d proxies", origURL, n)
	}

	// GetState must also be a copy.
	state.Proxies[0].LatencyMs = -12345
	if got := p.GetState().Proxies[0].LatencyMs; got == -12345 {
		t.Errorf("GetState() returned a shared slice: mutating it changed pool state")
	}
}

// TestConcurrentGetNextProxyMarkFailedAndRefresh is the -race target: every
// accessor and mutator runs concurrently (GetNextProxy, MarkFailed, UpdatePool,
// GetState, AvailableCount, ProxyCount). Run with:
//
//	go test -race -run TestConcurrent ./proxy/
func TestConcurrentGetNextProxyMarkFailedAndRefresh(t *testing.T) {
	const n = 20
	fl := newFakeList(t, proxyListBody(n))
	p := newTestPool(t, fl)
	p.ProbeFn = okProbe(nil, 5)
	p.UpdatePool()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Callers: hot path, must always be safe.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if info := p.GetNextProxy(); info != nil {
					_ = info.URL
					_ = info.LatencyMs
				}
				_ = p.AvailableCount()
				_ = p.ProxyCount()
				_ = p.GetState()
			}
		}()
	}

	// Failure reporter: flips available entries to failed.
	for f := 0; f < 2; f++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			i := seed
			for {
				select {
				case <-stop:
					return
				default:
				}
				state := p.GetState()
				if len(state.Proxies) > 0 {
					p.MarkFailed(state.Proxies[i%len(state.Proxies)].URL, "test failure")
					i++
				}
				time.Sleep(100 * time.Microsecond)
			}
		}(f)
	}

	// Refresher: replaces the whole state while readers are in flight.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			p.UpdatePool()
			time.Sleep(500 * time.Microsecond)
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()

	if got := p.ProxyCount(); got != n {
		t.Errorf("ProxyCount() = %d after concurrent refresh, want %d", got, n)
	}
	if got := p.AvailableCount(); got < 0 || got > n {
		t.Errorf("AvailableCount() = %d, want 0..%d", got, n)
	}
}

// TestCountAccessors covers AvailableCount/ProxyCount semantics for empty,
// failed-only, populated and single-entry pools, including MarkFailed's cached
// counter and its O(1) allocation-free properties.
func TestCountAccessors(t *testing.T) {
	empty := NewEmptyProxyPool()
	t.Cleanup(empty.Stop)
	if got := empty.AvailableCount(); got != 0 {
		t.Errorf("empty AvailableCount() = %d, want 0", got)
	}
	if got := empty.ProxyCount(); got != 0 {
		t.Errorf("empty ProxyCount() = %d, want 0", got)
	}
	if info := empty.GetNextProxy(); info != nil {
		t.Errorf("empty GetNextProxy() = %+v, want nil", info)
	}

	fl := newFakeList(t, proxyListBody(3))
	p := newTestPool(t, fl)

	// Everything fails: counted but not available.
	p.ProbeFn = func(string) (int, error) { return 0, fmt.Errorf("down") }
	p.UpdatePool()
	if got := p.ProxyCount(); got != 3 {
		t.Errorf("all-failed ProxyCount() = %d, want 3", got)
	}
	if got := p.AvailableCount(); got != 0 {
		t.Errorf("all-failed AvailableCount() = %d, want 0", got)
	}
	if info := p.GetNextProxy(); info != nil {
		t.Errorf("all-failed GetNextProxy() = %+v, want nil", info)
	}

	// Everything recovers on the next refresh.
	p.ProbeFn = okProbe(nil, 4)
	p.UpdatePool()
	if got := p.AvailableCount(); got != 3 {
		t.Errorf("AvailableCount() = %d, want 3", got)
	}
	if got := p.ProxyCount(); got != 3 {
		t.Errorf("ProxyCount() = %d, want 3", got)
	}

	// MarkFailed flips one entry and decrements the cached count once.
	p.MarkFailed(proxyURL(0), "boom")
	if got := p.AvailableCount(); got != 2 {
		t.Errorf("after MarkFailed AvailableCount() = %d, want 2", got)
	}
	if got := p.ProxyCount(); got != 3 {
		t.Errorf("after MarkFailed ProxyCount() = %d, want 3", got)
	}
	for i := 0; i < 10; i++ {
		if info := p.GetNextProxy(); info == nil {
			t.Fatal("GetNextProxy() = nil while 2 proxies remain available")
		} else if info.URL == proxyURL(0) {
			t.Fatalf("MarkFailed proxy %q was served", info.URL)
		}
	}
	p.MarkFailed(proxyURL(0), "boom again")
	if got := p.AvailableCount(); got != 2 {
		t.Errorf("double MarkFailed decremented twice: AvailableCount() = %d, want 2", got)
	}

	// A failed refresh must not corrupt the counts, and MarkFailed for an
	// unknown URL only bumps FailedRequests.
	fl.setStatus(http.StatusInternalServerError)
	p.UpdatePool()
	if got := p.AvailableCount(); got != 2 {
		t.Errorf("failed refresh changed AvailableCount() to %d, want 2", got)
	}
	if got := p.ProxyCount(); got != 3 {
		t.Errorf("failed refresh changed ProxyCount() to %d, want 3", got)
	}
	beforeFailed := p.GetState().FailedRequests
	p.MarkFailed("http://127.0.0.1:9/p/unknown", "unknown")
	if got := p.GetState().FailedRequests; got != beforeFailed+1 {
		t.Errorf("FailedRequests = %d after MarkFailed(unknown), want %d", got, beforeFailed+1)
	}
	if got := p.AvailableCount(); got != 2 {
		t.Errorf("MarkFailed(unknown) changed AvailableCount() to %d, want 2", got)
	}

	// Single-entry pool.
	fl1 := newFakeList(t, proxyListBody(1))
	p1 := newTestPool(t, fl1)
	p1.ProbeFn = okProbe(nil, 2)
	p1.UpdatePool()
	if got := p1.AvailableCount(); got != 1 {
		t.Errorf("single AvailableCount() = %d, want 1", got)
	}
	if got := p1.ProxyCount(); got != 1 {
		t.Errorf("single ProxyCount() = %d, want 1", got)
	}

	// "Cheap" = O(1) under RLock, no slice copy: zero allocations per call.
	if allocs := testing.AllocsPerRun(1000, func() {
		_ = p1.AvailableCount()
		_ = p1.ProxyCount()
	}); allocs != 0 {
		t.Errorf("AvailableCount/ProxyCount allocate %v times per call, want 0 (would mean slice copying)", allocs)
	}
}

// TestTranslateUsesCheapAccessor is the static half of the "GetState in the hot
// path" fix: translate.go must use the O(1) accessor and must not call GetState
// (which copies the whole Proxies slice) inside the retry loop.
func TestTranslateUsesCheapAccessor(t *testing.T) {
	path := filepath.Join("..", "translate", "translate.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(src)

	if !strings.Contains(text, ".ProxyCount()") && !strings.Contains(text, ".AvailableCount()") {
		t.Errorf("translate.go does not use the O(1) pool accessor (ProxyCount/AvailableCount); hot path still asks for a full copy")
	}
	if strings.Contains(text, "GetState()") {
		t.Errorf("translate.go still calls GetState() on the translation hot path; use AvailableCount()/ProxyCount() instead")
	}
	if strings.Contains(text, ".GetState().Proxies") {
		t.Errorf("translate.go still copies the whole Proxies slice via GetState().Proxies")
	}
	if !strings.Contains(text, "GetNextProxy()") {
		t.Errorf("translate.go no longer calls GetNextProxy(); the retry loop changed unexpectedly")
	}
	if strings.Contains(text, "[Proxy] Using:") {
		t.Errorf("translate.go still logs a \"[Proxy] Using:\" line per proxy attempt; the per-translation log spam is back")
	}
}

// TestFetchProxyList covers the list-fetch hardening: line trimming, non-2xx
// aborts, redirect following and the 8 MiB body cap. All servers are httptest.
func TestFetchProxyList(t *testing.T) {
	t.Run("trims CRLF lines", func(t *testing.T) {
		fl := newFakeList(t, "http://127.0.0.1:9/a\r\nhttp://127.0.0.1:9/b\r\n\r\n")
		p := newTestPool(t, fl)
		lines, err := p.fetchProxyList()
		if err != nil {
			t.Fatalf("fetchProxyList() error = %v", err)
		}
		want := []string{"http://127.0.0.1:9/a", "http://127.0.0.1:9/b"}
		if len(lines) != len(want) {
			t.Fatalf("got %d lines %q, want %d", len(lines), lines, len(want))
		}
		for i := range want {
			if lines[i] != want[i] {
				t.Errorf("lines[%d] = %q, want %q (CRLF/space not trimmed)", i, lines[i], want[i])
			}
		}
	})

	t.Run("non-2xx aborts refresh and keeps old state", func(t *testing.T) {
		fl := newFakeList(t, proxyListBody(3))
		p := newTestPool(t, fl)
		p.ProbeFn = okProbe(nil, 1)
		p.UpdatePool()
		if got := p.ProxyCount(); got != 3 {
			t.Fatalf("initial ProxyCount() = %d, want 3", got)
		}
		fl.setStatus(http.StatusInternalServerError)
		if _, err := p.fetchProxyList(); err == nil {
			t.Fatal("fetchProxyList() error = nil for HTTP 500, want error")
		}
		p.UpdatePool()
		if got := p.ProxyCount(); got != 3 {
			t.Errorf("failed refresh dropped proxies: ProxyCount() = %d, want 3 (state must survive a failed refresh)", got)
		}
	})

	t.Run("oversized body is rejected", func(t *testing.T) {
		fl := newFakeList(t, strings.Repeat("x", maxProxyListBytes+1))
		p := newTestPool(t, fl)
		lines, err := p.fetchProxyList()
		if err == nil {
			t.Fatalf("fetchProxyList() = %d lines, nil; want error for >%d-byte body", len(lines), maxProxyListBytes)
		}
		if !strings.Contains(err.Error(), "exceeds") {
			t.Errorf("error = %v, want it to mention the size limit", err)
		}
		p.UpdatePool()
		if got := p.ProxyCount(); got != 0 {
			t.Errorf("ProxyCount() = %d after oversized refresh, want 0", got)
		}
	})

	t.Run("redirect is followed", func(t *testing.T) {
		var dest *fakeList
		dest = newFakeList(t, proxyListBody(2))
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, dest.URL(), http.StatusFound)
		}))
		t.Cleanup(redirector.Close)
		p := NewProxyPool(redirector.URL, 5, 3)
		t.Cleanup(p.Stop)
		lines, err := p.fetchProxyList()
		if err != nil {
			t.Fatalf("fetchProxyList() through redirect error = %v", err)
		}
		if len(lines) != 2 {
			t.Errorf("got %d lines through redirect, want 2", len(lines))
		}
	})

	t.Run("unreachable URL fails fast", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		deadURL := srv.URL
		srv.Close()
		p := NewProxyPool(deadURL, 5, 3)
		t.Cleanup(p.Stop)
		start := time.Now()
		if _, err := p.fetchProxyList(); err == nil {
			t.Fatal("fetchProxyList() error = nil for a closed server, want error")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("unreachable list URL took %v to fail, want well under the %v client timeout", elapsed, listFetchTimeout)
		}
		p.UpdatePool()
		if got := p.ProxyCount(); got != 0 {
			t.Errorf("ProxyCount() = %d after failed fetch, want 0", got)
		}
	})
}

// TestFetchProxyListHangingURLTimesOut is intentionally slow: it proves the
// client timeout is actually set on the list fetch. It waits for the real
// listFetchTimeout (30s), so it is skipped under -short.
func TestFetchProxyListHangingURLTimesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the real 30s list-fetch timeout")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never answers; return when the client gives up
	}))
	t.Cleanup(srv.Close)

	p := NewProxyPool(srv.URL, 5, 3)
	t.Cleanup(p.Stop)

	start := time.Now()
	p.UpdatePool()
	elapsed := time.Since(start)

	if elapsed < listFetchTimeout-5*time.Second {
		t.Fatalf("UpdatePool() returned after %v with a hanging list URL; want it to wait for the %v client timeout", elapsed, listFetchTimeout)
	}
	if elapsed > listFetchTimeout+20*time.Second {
		t.Fatalf("UpdatePool() took %v with a hanging list URL; want roughly %v", elapsed, listFetchTimeout)
	}
	if got := p.ProxyCount(); got != 0 {
		t.Errorf("ProxyCount() = %d after timed-out fetch, want 0", got)
	}
}

// TestSingleProxyMarkFailedKeepsProxyServable pins review finding R1: a pool
// built by NewSingleProxyPool has no refresh path (Start is never called), so
// MarkFailed must record the failure without demoting its only proxy. List-mode
// demotion is covered by TestCountAccessors.
//
// The startup probe dials 127.0.0.1:9 (closed loopback port) and fails fast, so
// this test touches no external network; the latency falls back to
// latencyTimeout*1000 as before.
func TestSingleProxyMarkFailedKeepsProxyServable(t *testing.T) {
	const singleURL = "http://127.0.0.1:9/p/single"
	p := NewSingleProxyPool(singleURL, 1)
	t.Cleanup(p.Stop)

	if got := p.AvailableCount(); got != 1 {
		t.Fatalf("AvailableCount() before MarkFailed = %d, want 1", got)
	}

	p.MarkFailed(singleURL, "request_error")

	if got := p.AvailableCount(); got != 1 {
		t.Errorf("single-proxy AvailableCount() = %d after MarkFailed, want 1: a single explicitly configured proxy has no refresh path and must stay in rotation", got)
	}
	if got := p.ProxyCount(); got != 1 {
		t.Errorf("single-proxy ProxyCount() = %d after MarkFailed, want 1", got)
	}
	for i := 0; i < 3; i++ {
		info := p.GetNextProxy()
		if info == nil {
			t.Fatalf("single-proxy GetNextProxy() = nil on call %d after MarkFailed, want the configured proxy", i)
		}
		if info.URL != singleURL {
			t.Errorf("GetNextProxy().URL = %q, want %q", info.URL, singleURL)
		}
		if info.Status != "available" {
			t.Errorf("GetNextProxy().Status = %q, want available", info.Status)
		}
	}

	state := p.GetState()
	if state.FailedRequests != 1 {
		t.Errorf("FailedRequests = %d, want 1: MarkFailed bookkeeping must still happen", state.FailedRequests)
	}
	if len(state.Proxies) != 1 {
		t.Fatalf("len(GetState().Proxies) = %d, want 1", len(state.Proxies))
	}
	entry := state.Proxies[0]
	if entry.FailCount != 1 {
		t.Errorf("Proxies[0].FailCount = %d, want 1", entry.FailCount)
	}
	if entry.LastFailReason == nil || *entry.LastFailReason != "request_error" {
		t.Errorf("Proxies[0].LastFailReason = %v, want %q", entry.LastFailReason, "request_error")
	}
	if entry.Status != "available" {
		t.Errorf("Proxies[0].Status = %q, want available (single-proxy mode must not be poisoned)", entry.Status)
	}
}

// TestUpdatePoolRotatesProbeWindow pins review finding R2: each refresh probes a
// bounded window of MaxPool entries but the window advances, so a dead or
// blocked head of a huge upstream list cannot pin the pool forever. The rotation
// is deterministic (offset advances by one window per refresh), which is what we
// assert here.
func TestUpdatePoolRotatesProbeWindow(t *testing.T) {
	const (
		listLines = 1000
		maxPool   = 100
	)

	fl := newFakeList(t, proxyListBody(listLines))
	p := newTestPool(t, fl, WithMaxPool(maxPool), WithProbeConcurrency(8))

	var mu sync.Mutex
	var current []string
	p.ProbeFn = func(proxyURL string) (int, error) {
		mu.Lock()
		current = append(current, proxyURL)
		mu.Unlock()
		return 1, nil
	}
	refresh := func(t *testing.T) map[string]struct{} {
		t.Helper()
		mu.Lock()
		current = nil
		mu.Unlock()
		p.UpdatePool()
		mu.Lock()
		urls := append([]string(nil), current...)
		mu.Unlock()
		return toSet(urls)
	}

	first := refresh(t)
	second := refresh(t)

	if len(first) != maxPool {
		t.Fatalf("first refresh probed %d unique URLs, want %d", len(first), maxPool)
	}
	if len(second) != maxPool {
		t.Fatalf("second refresh probed %d unique URLs, want %d", len(second), maxPool)
	}
	if sameSet(first, second) {
		t.Errorf("two refreshes probed the identical URL set (window did not rotate)")
	}
	for u := range first {
		if _, ok := second[u]; ok {
			t.Errorf("URL %q probed on both refreshes; expected disjoint windows of %d", u, maxPool)
		}
	}
	if got := p.ProxyCount(); got != maxPool {
		t.Errorf("ProxyCount() after two refreshes = %d, want %d", got, maxPool)
	}
	for _, entry := range p.GetState().Proxies {
		if _, ok := second[entry.URL]; !ok {
			t.Errorf("state still contains %q, which was not probed by the latest refresh", entry.URL)
		}
	}

	t.Run("list at or below MaxPool probes every line on every refresh", func(t *testing.T) {
		const small = 50
		smallList := newFakeList(t, proxyListBody(small))
		p := newTestPool(t, smallList, WithMaxPool(maxPool), WithProbeConcurrency(8))
		var mu sync.Mutex
		var current []string
		p.ProbeFn = func(proxyURL string) (int, error) {
			mu.Lock()
			current = append(current, proxyURL)
			mu.Unlock()
			return 1, nil
		}
		refresh := func(t *testing.T) map[string]struct{} {
			t.Helper()
			mu.Lock()
			current = nil
			mu.Unlock()
			p.UpdatePool()
			mu.Lock()
			urls := append([]string(nil), current...)
			mu.Unlock()
			return toSet(urls)
		}
		first := refresh(t)
		second := refresh(t)
		if len(first) != small || len(second) != small {
			t.Errorf("small list probes = %d then %d, want %d each (all lines every refresh)", len(first), len(second), small)
		}
		if !sameSet(first, second) {
			t.Errorf("small list refresh probed different URL sets: %v vs %v", first, second)
		}
		if got := p.ProxyCount(); got != small {
			t.Errorf("ProxyCount() = %d, want %d", got, small)
		}
	})
}

// TestUpdatePoolDeduplicatesListURLs pins review finding R3: duplicate list
// entries must be probed and stored once, and MarkFailed on a duplicated URL
// must remove it completely (the old code kept the duplicate "twin" available).
func TestUpdatePoolDeduplicatesListURLs(t *testing.T) {
	const unique = 50
	var b strings.Builder
	for _, u := range proxyURLs(unique) {
		b.WriteString(u)
		b.WriteByte('\n')
		b.WriteString(u) // duplicate line
		b.WriteByte('\n')
	}
	fl := newFakeList(t, b.String())
	p := newTestPool(t, fl, WithMaxPool(200)) // cap >= line count: only dedupe limits probes

	var probes atomic.Int64
	p.ProbeFn = func(string) (int, error) {
		probes.Add(1)
		return 3, nil
	}
	p.UpdatePool()

	if got := probes.Load(); got != unique {
		t.Errorf("probed %d URLs for a 100-line list with %d unique URLs, want %d (duplicates must be probed once)", got, unique, unique)
	}
	if got := p.ProxyCount(); got != unique {
		t.Errorf("ProxyCount() = %d, want %d unique entries", got, unique)
	}
	seen := make(map[string]int, unique)
	for _, entry := range p.GetState().Proxies {
		seen[entry.URL]++
	}
	if len(seen) != unique {
		t.Errorf("state holds %d distinct URLs, want %d", len(seen), unique)
	}
	for u, count := range seen {
		if count != 1 {
			t.Errorf("state holds URL %q %d times, want 1", u, count)
		}
	}

	duplicatedURL := proxyURL(7)
	p.MarkFailed(duplicatedURL, "request_error")
	if got := p.AvailableCount(); got != unique-1 {
		t.Errorf("AvailableCount() after MarkFailed = %d, want %d", got, unique-1)
	}
	if got := p.ProxyCount(); got != unique {
		t.Errorf("ProxyCount() after MarkFailed = %d, want %d (failed entries are retained)", got, unique)
	}
	for i := 0; i < 4*unique; i++ {
		if info := p.GetNextProxy(); info != nil && info.URL == duplicatedURL {
			t.Fatalf("GetNextProxy() served failed duplicated URL %q (the duplicate twin survived MarkFailed)", duplicatedURL)
		}
	}
}

// toSet / sameSet are tiny helpers for the window-rotation test.
func toSet(urls []string) map[string]struct{} {
	set := make(map[string]struct{}, len(urls))
	for _, u := range urls {
		set[u] = struct{}{}
	}
	return set
}

func sameSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for u := range a {
		if _, ok := b[u]; !ok {
			return false
		}
	}
	return true
}

// TestPoolDefaultsAndOptions pins the default bounds and the additive
// PoolOption API (the 3-argument NewProxyPool call keeps compiling elsewhere).
func TestPoolDefaultsAndOptions(t *testing.T) {
	p := NewProxyPool("http://127.0.0.1:9/list", 5, 3)
	t.Cleanup(p.Stop)
	if got, want := p.MaxPool, defaultMaxPool; got != want {
		t.Errorf("default MaxPool = %d, want %d", got, want)
	}
	if got, want := p.ProbeConcurrency, defaultProbeConcurrency; got != want {
		t.Errorf("default ProbeConcurrency = %d, want %d", got, want)
	}
	if p.ProbeFn != nil {
		t.Errorf("ProbeFn default = non-nil, want nil (real network probe)")
	}

	q := NewProxyPool("http://127.0.0.1:9/list", 5, 3, WithMaxPool(7), WithProbeConcurrency(3))
	t.Cleanup(q.Stop)
	if q.MaxPool != 7 {
		t.Errorf("WithMaxPool(7): MaxPool = %d, want 7", q.MaxPool)
	}
	if q.ProbeConcurrency != 3 {
		t.Errorf("WithProbeConcurrency(3): ProbeConcurrency = %d, want 3", q.ProbeConcurrency)
	}

	r := NewProxyPool("http://127.0.0.1:9/list", 5, 3, WithMaxPool(0), WithProbeConcurrency(-1))
	t.Cleanup(r.Stop)
	if got := r.MaxPool; got != defaultMaxPool {
		t.Errorf("WithMaxPool(0): MaxPool = %d, want default %d", got, defaultMaxPool)
	}
	if got := r.ProbeConcurrency; got != defaultProbeConcurrency {
		t.Errorf("WithProbeConcurrency(-1): ProbeConcurrency = %d, want default %d", got, defaultProbeConcurrency)
	}
}
