package master

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/userreksai/title-master/internal/protocol"
	"github.com/userreksai/title-master/internal/settings"
)

func newRepo(t *testing.T) *Repository {
	t.Helper()
	r, err := OpenRepository(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func fakeNode(t *testing.T, name string, current *atomic.Value, down *atomic.Bool, checks *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token-1234567890" {
			w.WriteHeader(401)
			return
		}
		if down.Load() {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/healthz" {
			_ = json.NewEncoder(w).Encode(protocol.Health{Version: 1, Name: name, Ready: true, MaxConcurrent: 20, MaxTimeoutSeconds: 15})
			return
		}
		checks.Add(1)
		var req protocol.Request
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		o := current.Load().(protocol.Observation)
		o.Version = 1
		o.ID = req.ID
		o.Domain = req.Domain
		o.CheckedAt = time.Now()
		o.FinalURL = "https://" + req.Domain + "/"
		_ = json.NewEncoder(w).Encode(o)
	}))
}

func TestMasterSEOThreeNodesTransitionsAndMachineIsolation(t *testing.T) {
	seo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/domains" || r.Header.Get("Authorization") != "Bearer seo-token" {
			t.Error("wrong SEO integration request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []Domain{{ID: "1", Domain: "example.com", Active: true}, {ID: "archived", Domain: "archived.com", Active: false}}, "count": 2})
	}))
	defer seo.Close()
	repo := newRepo(t)
	cfg := settings.DefaultMaster()
	cfg.SEO.BaseURL = seo.URL
	cfg.SEO.Token = "seo-token"
	cfg.Webhooks.DomainURLs = []string{"https://business.example/hook"}
	cfg.Webhooks.MachineURLs = []string{"https://machines.example/hook"}
	var value atomic.Value
	value.Store(title("123"))
	var checks atomic.Int32
	down := make([]*atomic.Bool, 3)
	for i, name := range []string{"a", "b", "c"} {
		down[i] = &atomic.Bool{}
		node := fakeNode(t, name, &value, down[i], &checks)
		defer node.Close()
		cfg.Agents = append(cfg.Agents, settings.Node{Name: name, URL: node.URL, Token: "test-token-1234567890"})
	}
	runner := NewRunner(cfg, repo, slog.New(slog.NewTextHandler(io.Discard, nil)))
	round := func() {
		t.Helper()
		if err := runner.Round(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	round()
	value.Store(siteError("500服务失败"))
	round()
	round()
	value.Store(title("123"))
	round()
	state := repo.Snapshot()
	if len(state.Outbox) != 2 || checks.Load() != 12 {
		t.Fatalf("outbox=%d checks=%d", len(state.Outbox), checks.Load())
	}
	for _, event := range state.Outbox {
		if event.Kind != "domain" || event.PendingURLs[0] != cfg.Webhooks.DomainURLs[0] {
			t.Fatal("wrong webhook routing")
		}
	}
	down[0].Store(true)
	round()
	state = repo.Snapshot()
	if len(state.Outbox) != 3 || state.Outbox[2].Kind != "machine" {
		t.Fatal("offline node must only alert machine group")
	}
	if state.Domains["1"].ValidAgents != 2 {
		t.Fatal("offline node included in denominator")
	}
	for _, d := range down {
		d.Store(true)
	}
	value.Store(title("must not apply"))
	round()
	if repo.Snapshot().Domains["1"].Value.Title != "123" {
		t.Fatal("all agents down overwrote baseline")
	}
}

func TestScanDiscardsEarlierResultsAfterRPCFailure(t *testing.T) {
	var checks atomic.Int32
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_ = json.NewEncoder(w).Encode(protocol.Health{Version: 1, Name: "a", Ready: true, MaxConcurrent: 20, MaxTimeoutSeconds: 15})
			return
		}
		if checks.Add(1) == 2 {
			w.WriteHeader(500)
			return
		}
		var req protocol.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(protocol.Observation{Version: 1, ID: req.ID, Domain: req.Domain, Kind: "timeout", CheckedAt: time.Now()})
	}))
	defer node.Close()
	cfg := settings.DefaultMaster()
	cfg.PerAgentConcurrency = 1
	c := NodeClient{Config: cfg, HTTP: httpClient()}
	r := c.Scan(context.Background(), settings.Node{Name: "a", URL: node.URL}, []Domain{{ID: "a", Domain: "a.com"}, {ID: "b", Domain: "b.com"}, {ID: "c", Domain: "c.com"}})
	if r.Healthy || len(r.Items) != 0 || checks.Load() != 2 {
		t.Fatalf("partial round was counted: %+v", r)
	}
}

func TestStateRestartLockAndCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	r, err := OpenRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := OpenRepository(path); err == nil {
		second.Close()
		t.Fatal("concurrent master lock missing")
	}
	if err = r.Update(func(s *State) {
		applyDecision(s, Domain{ID: "1", Domain: "a.com"}, Decision{Value: Value{"title", "a"}, Valid: 1, Votes: 1}, true, nil, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	r, err = OpenRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err = r.Update(func(s *State) {
		applyDecision(s, Domain{ID: "1", Domain: "a.com"}, Decision{Value: Value{"title", "a"}, Valid: 1, Votes: 1}, true, []string{"https://example.com/hook"}, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if len(r.Snapshot().Outbox) != 0 {
		t.Fatal("restart generated duplicate alert")
	}
	r.Close()
	if err = os.WriteFile(path, []byte(`{"version":1,"domains":`), 0600); err != nil {
		t.Fatal(err)
	}
	if broken, e := OpenRepository(path); e == nil {
		broken.Close()
		t.Fatal("corrupted state silently reset")
	}
}

func TestPostScanHealthFailureDiscardsDomainTimeouts(t *testing.T) {
	var healthCalls atomic.Int32
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			if healthCalls.Add(1) > 1 {
				w.WriteHeader(503)
				return
			}
			_ = json.NewEncoder(w).Encode(protocol.Health{Version: 1, Name: "a", Ready: true, MaxConcurrent: 20, MaxTimeoutSeconds: 15})
			return
		}
		var req protocol.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(protocol.Observation{Version: 1, ID: req.ID, Domain: req.Domain, Kind: "timeout", CheckedAt: time.Now()})
	}))
	defer node.Close()
	c := NodeClient{Config: settings.DefaultMaster(), HTTP: httpClient()}
	r := c.Scan(context.Background(), settings.Node{Name: "a", URL: node.URL}, []Domain{{ID: "1", Domain: "a.com"}})
	if r.Healthy || len(r.Items) != 0 || healthCalls.Load() != 2 {
		t.Fatalf("post-scan network failure counted as website failure: %+v", r)
	}
}

func TestWebhookChecksBusinessCodeAndDestinationProgress(t *testing.T) {
	for _, tc := range []struct {
		format, body string
		ok           bool
	}{{"feishu", `{"code":0}`, true}, {"feishu", `{"StatusCode":0}`, true}, {"feishu", `{"code":9499}`, false}, {"feishu", `{}`, false}, {"wecom", `{"errcode":0}`, true}, {"wecom", `{"errcode":123}`, false}, {"generic", "", true}} {
		t.Run(tc.format+tc.body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Idempotency-Key") != "id" {
					t.Error("missing idempotency key")
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			err := postEvent(context.Background(), httpClient(), settings.Webhooks{Format: tc.format, TimeoutSeconds: 1}, Event{ID: "id", Text: "change"}, server.URL)
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v", err)
			}
		})
	}
	repo := newRepo(t)
	var good, bad atomic.Int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { good.Add(1); _, _ = io.WriteString(w, `{"code":0}`) }))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bad.Add(1); _, _ = io.WriteString(w, `{"code":123}`) }))
	defer b.Close()
	_ = repo.Update(func(s *State) { appendEvent(s, "d:1", "domain", "changed", []string{a.URL, b.URL}, time.Now()) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	n := Notifier{Repo: repo, Config: settings.Webhooks{Format: "feishu", TimeoutSeconds: 1, RetryIntervalSeconds: 1, Workers: 2}, Client: httpClient(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	go func() { n.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for bad.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	s := repo.Snapshot()
	if good.Load() != 1 || bad.Load() < 2 || len(s.Outbox) != 1 || len(s.Outbox[0].PendingURLs) != 1 || s.Outbox[0].PendingURLs[0] != b.URL {
		t.Fatalf("good=%d bad=%d outbox=%+v", good.Load(), bad.Load(), s.Outbox)
	}
}

func TestConcurrencyBoundAndFiveHundredDomains(t *testing.T) {
	var running, maxSeen, count atomic.Int32
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_ = json.NewEncoder(w).Encode(protocol.Health{Version: 1, Name: "a", Ready: true, MaxConcurrent: 20, MaxTimeoutSeconds: 15})
			return
		}
		n := running.Add(1)
		defer running.Add(-1)
		for old := maxSeen.Load(); n > old; old = maxSeen.Load() {
			if maxSeen.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		count.Add(1)
		var req protocol.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(protocol.Observation{Version: 1, ID: req.ID, Domain: req.Domain, Kind: "timeout", CheckedAt: time.Now()})
	}))
	defer node.Close()
	domains := make([]Domain, 500)
	for i := range domains {
		domains[i] = Domain{ID: time.Unix(int64(i), 0).String(), Domain: "example.com"}
	}
	c := NodeClient{Config: settings.DefaultMaster(), HTTP: httpClient()}
	r := c.Scan(context.Background(), settings.Node{Name: "a", URL: node.URL}, domains)
	if !r.Healthy || len(r.Items) != 500 || count.Load() != 500 || maxSeen.Load() > 20 {
		t.Fatalf("healthy=%v items=%d count=%d max=%d", r.Healthy, len(r.Items), count.Load(), maxSeen.Load())
	}
}
