package master

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/userreksai/title-master/internal/protocol"
	"github.com/userreksai/title-master/internal/settings"
)

func eventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func httpClient() *http.Client {
	return &http.Client{Transport: &http.Transport{MaxIdleConns: 256, MaxIdleConnsPerHost: 200, IdleConnTimeout: 60 * time.Second, MaxResponseHeaderBytes: 64 << 10}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

func getJSON(ctx context.Context, client *http.Client, method, target, token string, body any, out any, limit int64) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return errors.New("invalid request URL")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("connection failed or request timed out")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return errors.New("read response failed")
	}
	if int64(len(b)) > limit {
		return errors.New("response too large")
	}
	if err = json.Unmarshal(b, out); err != nil {
		return errors.New("invalid JSON response")
	}
	return nil
}

func LoadDomains(ctx context.Context, client *http.Client, cfg settings.SEO) ([]Domain, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	var response struct {
		Items *[]Domain `json:"items"`
		Count *int      `json:"count"`
	}
	if err := getJSON(ctx, client, http.MethodGet, strings.TrimRight(cfg.BaseURL, "/")+"/api/v1/domains", cfg.Token, nil, &response, 8<<20); err != nil {
		return nil, err
	}
	if response.Items == nil || response.Count == nil || *response.Count != len(*response.Items) {
		return nil, errors.New("SEO response is missing items/count or is incomplete")
	}
	result := make([]Domain, 0, len(*response.Items))
	seen := map[string]bool{}
	names := map[string]bool{}
	for _, d := range *response.Items {
		if !d.Active {
			continue
		}
		if d.ID == "" || strings.TrimSpace(d.Domain) == "" || seen[d.ID] || names[d.Domain] {
			return nil, errors.New("invalid or duplicate SEO domain")
		}
		seen[d.ID] = true
		names[d.Domain] = true
		result = append(result, d)
	}
	return result, nil
}

type NodeResult struct {
	Node    settings.Node
	Healthy bool
	Detail  string
	Items   map[string]protocol.Observation
}

type NodeClient struct {
	Config settings.Master
	HTTP   *http.Client
}

func (c *NodeClient) Health(ctx context.Context, node settings.Node) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.Config.HealthTimeoutSeconds)*time.Second)
	defer cancel()
	var h protocol.Health
	if err := getJSON(ctx, c.HTTP, http.MethodGet, node.URL+"/healthz?fresh=1", node.Token, nil, &h, 64<<10); err != nil {
		return err
	}
	if h.Version != protocol.Version || h.Name != node.Name || !h.Ready {
		return errors.New("health probe failed, protocol or agent name mismatch")
	}
	if h.MaxConcurrent < c.Config.PerAgentConcurrency || h.MaxTimeoutSeconds < c.Config.TitleTimeoutSeconds {
		return errors.New("agent capacity/timeout is below master configuration")
	}
	return nil
}

func (c *NodeClient) Check(ctx context.Context, node settings.Node, domain Domain) (protocol.Observation, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.Config.RPCTimeoutSeconds)*time.Second)
	defer cancel()
	id := eventID()
	req := protocol.Request{Version: protocol.Version, ID: id, Domain: domain.Domain, TimeoutSeconds: c.Config.TitleTimeoutSeconds}
	var o protocol.Observation
	if err := getJSON(ctx, c.HTTP, http.MethodPost, node.URL+"/v1/title", node.Token, req, &o, 64<<10); err != nil {
		return o, err
	}
	if o.Version != protocol.Version || o.ID != id || o.Domain != domain.Domain || o.CheckedAt.IsZero() || o.DurationMS < 0 || o.DurationMS > int64(c.Config.TitleTimeoutSeconds*1000+1000) {
		return o, errors.New("invalid agent observation identity or timing")
	}
	switch o.Kind {
	case "title":
		if strings.TrimSpace(o.Title) == "" || len(o.Title) > 16384 || o.StatusCode < 200 || o.StatusCode > 599 || settings.ValidURL(o.FinalURL) != nil {
			return o, errors.New("invalid title observation")
		}
	case "timeout", "fetch_error", "http_error", "empty_title", "too_large":
		if o.Title != "" {
			return o, errors.New("failure response contains unexpected title")
		}
	default:
		return o, errors.New("unknown agent observation kind")
	}
	return o, nil
}

// Any service/transport/health failure invalidates this node's ENTIRE round.
// Website timeouts are valid observations and do not trip this circuit.
func (c *NodeClient) Scan(parent context.Context, node settings.Node, domains []Domain) NodeResult {
	result := NodeResult{Node: node, Items: map[string]protocol.Observation{}}
	if err := c.Health(parent, node); err != nil {
		result.Detail = err.Error()
		return result
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var mu sync.Mutex
	var once sync.Once
	var failed error
	fail := func(err error) { once.Do(func() { failed = err; cancel() }) }
	done := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(time.Duration(c.Config.HealthIntervalSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := c.Health(ctx, node); err != nil {
					fail(err)
					return
				}
			}
		}
	}()
	var next atomic.Int64
	var wg sync.WaitGroup
	for range c.Config.PerAgentConcurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				i := int(next.Add(1) - 1)
				if i >= len(domains) {
					return
				}
				o, err := c.Check(ctx, node, domains[i])
				if err != nil {
					if ctx.Err() == nil {
						fail(err)
					}
					return
				}
				mu.Lock()
				result.Items[domains[i].ID] = o
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(done)
	<-watchDone
	if parent.Err() != nil {
		result.Detail = "round canceled"
		result.Items = nil
		return result
	}
	if failed == nil {
		failed = c.Health(parent, node)
	}
	if failed != nil {
		result.Detail = failed.Error()
		result.Items = nil
		return result
	}
	result.Healthy = true
	return result
}
