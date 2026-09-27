package master

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/userreksai/title-master/internal/protocol"
	"github.com/userreksai/title-master/internal/settings"
)

type Runner struct {
	Config settings.Master
	Repo   *Repository
	Logger *slog.Logger
	Client *NodeClient
}

func NewRunner(cfg settings.Master, repo *Repository, logger *slog.Logger) *Runner {
	return &Runner{Config: cfg, Repo: repo, Logger: logger, Client: &NodeClient{Config: cfg, HTTP: httpClient()}}
}

func (r *Runner) recordMachine(key, label string, healthy bool, detail string) error {
	if healthy {
		detail = "健康检查通过"
	}
	return r.Repo.Update(func(s *State) {
		applyMachine(s, key, label, healthy, detail, r.Config.Webhooks.MachineURLs, time.Now().UTC())
	})
}

func (r *Runner) Round(ctx context.Context) error {
	started := time.Now()
	domains, err := LoadDomains(ctx, r.Client.HTTP, r.Config.SEO)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if saveErr := r.recordMachine("seo-api", "SEO 数据接口", false, err.Error()); saveErr != nil {
			return saveErr
		}
		return fmt.Errorf("read SEO domains: %w", err)
	}
	if err = r.recordMachine("seo-api", "SEO 数据接口", true, ""); err != nil {
		return err
	}
	worst := ((len(domains)+r.Config.PerAgentConcurrency-1)/r.Config.PerAgentConcurrency)*r.Config.RPCTimeoutSeconds + 2*r.Config.HealthTimeoutSeconds
	r.Logger.Info("title round started", "domains", len(domains), "agents", len(r.Config.Agents), "per_agent_concurrency", r.Config.PerAgentConcurrency, "rpc_budget_seconds", worst)
	if worst >= r.Config.IntervalSeconds {
		r.Logger.Warn("round budget exceeds interval; increase concurrency or interval (e.g. 900 seconds)")
	}
	results := make(chan NodeResult, len(r.Config.Agents))
	for _, node := range r.Config.Agents {
		go func(n settings.Node) { results <- r.Client.Scan(ctx, n, domains) }(node)
	}
	healthy := make([]NodeResult, 0, len(r.Config.Agents))
	var saveErr error
	for range r.Config.Agents {
		result := <-results
		if ctx.Err() != nil {
			continue
		}
		if e := r.recordMachine("agent:"+result.Node.Name, result.Node.Name+" "+result.Node.URL, result.Healthy, result.Detail); e != nil {
			saveErr = e
		}
		if result.Healthy {
			healthy = append(healthy, result)
		} else {
			r.Logger.Warn("agent excluded from entire round", "agent", result.Node.Name, "error", result.Detail)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if saveErr != nil {
		return saveErr
	}
	quorum := len(healthy) >= r.Config.MinHealthyAgents
	if err = r.recordMachine("quorum", "有效 agent 数量", quorum, fmt.Sprintf("有效 %d，需要至少 %d；本轮保持域名基线", len(healthy), r.Config.MinHealthyAgents)); err != nil {
		return err
	}
	confirmed, unresolved := 0, 0
	err = r.Repo.Update(func(s *State) {
		at := time.Now().UTC()
		for _, domain := range domains {
			observations := make([]protocol.Observation, 0, len(healthy))
			for _, node := range healthy {
				if o, ok := node.Items[domain.ID]; ok {
					observations = append(observations, o)
				}
			}
			d, ok := Decide(observations, r.Config.FailurePercent, r.Config.MinHealthyAgents)
			if !ok {
				unresolved++
				continue
			}
			applyDecision(s, domain, d, r.Config.AlertOnInitialFailure, r.Config.Webhooks.DomainURLs, at)
			confirmed++
		}
		s.LastRoundAt = at
	})
	r.Logger.Info("title round completed", "duration", time.Since(started), "healthy_agents", len(healthy), "confirmed", confirmed, "unresolved", unresolved)
	return err
}

func (r *Runner) healthSweep(ctx context.Context) error {
	results := make(chan NodeResult, len(r.Config.Agents))
	for _, node := range r.Config.Agents {
		go func(n settings.Node) {
			err := r.Client.Health(ctx, n)
			res := NodeResult{Node: n, Healthy: err == nil}
			if err != nil {
				res.Detail = err.Error()
			}
			results <- res
		}(node)
	}
	var saveErr error
	for range r.Config.Agents {
		res := <-results
		if ctx.Err() != nil {
			continue
		}
		if err := r.recordMachine("agent:"+res.Node.Name, res.Node.Name+" "+res.Node.URL, res.Healthy, res.Detail); err != nil {
			saveErr = err
		}
	}
	return saveErr
}

// Scheduling is start-to-start, skips missed ticks, and never overlaps rounds.
func (r *Runner) Run(ctx context.Context, once bool) error {
	nctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	n := &Notifier{Repo: r.Repo, Config: r.Config.Webhooks, Client: httpClient(), Logger: r.Logger}
	wg.Add(1)
	go func() { defer wg.Done(); n.Run(nctx) }()
	defer func() { cancel(); wg.Wait() }()
	for {
		started := time.Now()
		err := r.Round(ctx)
		if once {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			r.Logger.Error("title round failed", "error", err)
		}
		next := started.Add(time.Duration(r.Config.IntervalSeconds) * time.Second)
		if !next.After(time.Now()) {
			next = time.Now().Add(time.Duration(r.Config.IntervalSeconds) * time.Second)
		}
		for time.Now().Before(next) {
			delay := time.Until(next)
			healthDelay := time.Duration(r.Config.HealthIntervalSeconds) * time.Second
			if delay > healthDelay {
				delay = healthDelay
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
			if time.Now().Before(next) {
				if err := r.healthSweep(ctx); err != nil {
					r.Logger.Error("save health status", "error", err)
				}
			}
		}
	}
}
