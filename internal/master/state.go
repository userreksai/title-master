package master

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Event struct {
	ID          string    `json:"id"`
	Key         string    `json:"key"`
	Kind        string    `json:"kind"`
	Text        string    `json:"text"`
	CreatedAt   time.Time `json:"created_at"`
	PendingURLs []string  `json:"pending_urls"`
}

type State struct {
	Version     int                     `json:"version"`
	Domains     map[string]DomainState  `json:"domains"`
	Machines    map[string]MachineState `json:"machines"`
	Outbox      []Event                 `json:"outbox"`
	LastRoundAt time.Time               `json:"last_round_at"`
}

type Repository struct {
	mu    sync.Mutex
	path  string
	state State
	lock  *os.File
}

func OpenRepository(path string) (*Repository, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := lockState(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("master already running or state lock unavailable: %w", err)
	}
	r := &Repository{path: path, lock: lock, state: State{Version: 1, Domains: map[string]DomainState{}, Machines: map[string]MachineState{}, Outbox: []Event{}}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &r.state)
	}
	if err == nil && (r.state.Version != 1 || r.state.Domains == nil || r.state.Machines == nil) {
		err = errors.New("invalid state schema")
	}
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("cannot load state (baseline preserved; refusing reset): %w", err)
	}
	return r, nil
}

func (r *Repository) Close() error { return r.lock.Close() }

// Baseline and pending deliveries are committed together. A failed disk write
// never advances the in-memory baseline or loses a pending notification.
func (r *Repository) Update(change func(*State)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := json.Marshal(r.state)
	if err != nil {
		return err
	}
	var next State
	if err = json.Unmarshal(b, &next); err != nil {
		return err
	}
	change(&next)
	b, err = json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(r.path), ".title-state-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, r.path); err != nil {
		return err
	}
	r.state = next
	return nil
}

func (r *Repository) Snapshot() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, _ := json.Marshal(r.state)
	var s State
	_ = json.Unmarshal(b, &s)
	return s
}

func appendEvent(s *State, key, kind, text string, urls []string, at time.Time) {
	if len(urls) == 0 {
		return
	}
	s.Outbox = append(s.Outbox, Event{ID: eventID(), Key: key, Kind: kind, Text: text, CreatedAt: at, PendingURLs: append([]string(nil), urls...)})
}

func brief(v string) string {
	r := []rune(v)
	if len(r) > 500 {
		return string(r[:500]) + "…"
	}
	return v
}

func applyDecision(s *State, domain Domain, d Decision, initialFailure bool, urls []string, at time.Time) {
	previous, exists := s.Domains[domain.ID]
	changed := exists && previous.Value != d.Value
	changedAt := previous.ChangedAt
	if !exists || changed {
		changedAt = at
	}
	s.Domains[domain.ID] = DomainState{Domain: domain.Domain, Value: d.Value, CheckedAt: at, ChangedAt: changedAt, ValidAgents: d.Valid, Votes: d.Votes, Detail: d.Detail}
	if !changed && !(!exists && initialFailure && d.Failed) {
		return
	}
	old := "未建立基线"
	if exists {
		old = previous.Value.String()
	}
	text := fmt.Sprintf("网站标题状态变更\n域名：%s\n标签：%s\n上次：%s\n本次：%s\n确认节点：%d/%d\n时间：%s", domain.Domain, brief(domain.Tag), brief(old), brief(d.Value.String()), d.Votes, d.Valid, at.Format(time.RFC3339))
	appendEvent(s, "domain:"+domain.ID, "domain", text, urls, at)
}

func applyMachine(s *State, key, label string, healthy bool, detail string, urls []string, at time.Time) {
	previous, exists := s.Machines[key]
	s.Machines[key] = MachineState{Healthy: healthy, Detail: detail, CheckedAt: at}
	if (!exists && healthy) || (exists && previous.Healthy == healthy) {
		return
	}
	status := "故障"
	if healthy {
		status = "恢复"
	}
	text := strings.Join([]string{"监控机器/服务" + status, "节点：" + label, "说明：" + brief(detail), "时间：" + at.Format(time.RFC3339)}, "\n")
	appendEvent(s, "machine:"+key, "machine", text, urls, at)
}
