package master

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/userreksai/title-master/internal/settings"
)

func postEvent(ctx context.Context, client *http.Client, cfg settings.Webhooks, event Event, target string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	text := event.Text + "\n事件ID：" + event.ID
	var payload any
	switch cfg.Format {
	case "feishu":
		payload = map[string]any{"msg_type": "text", "content": map[string]string{"text": text}}
	case "wecom":
		payload = map[string]any{"msgtype": "text", "text": map[string]string{"content": text}}
	default:
		payload = map[string]any{"event_id": event.ID, "kind": event.Kind, "text": text, "created_at": event.CreatedAt}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(b))
	if err != nil {
		return errors.New("invalid webhook URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", event.ID)
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("webhook connection failed or timed out")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook HTTP %d", resp.StatusCode)
	}
	b, err = io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return errors.New("webhook response read failed")
	}
	if cfg.Format == "generic" {
		return nil
	}
	var body map[string]json.RawMessage
	if err = json.Unmarshal(b, &body); err != nil || body == nil {
		return errors.New("webhook returned invalid JSON acknowledgement")
	}
	// Feishu/Lark variants use code or StatusCode; WeCom uses errcode.
	keys := []string{"code", "StatusCode"}
	if cfg.Format == "wecom" {
		keys = []string{"errcode"}
	}
	found := false
	for _, key := range keys {
		raw, ok := body[key]
		if !ok {
			continue
		}
		found = true
		var code int
		if json.Unmarshal(raw, &code) != nil || code != 0 {
			return errors.New("webhook business acknowledgement rejected")
		}
	}
	if !found {
		return errors.New("webhook acknowledgement code is missing")
	}
	return nil
}

type Notifier struct {
	Repo   *Repository
	Config settings.Webhooks
	Client *http.Client
	Logger *slog.Logger
}

func (n *Notifier) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	type completion struct{ key, id string }
	busy := map[string]bool{}
	retryAt := map[string]time.Time{}
	results := make(chan completion, n.Config.Workers)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case result := <-results:
			delete(busy, result.key)
			retryAt[result.id] = time.Now().Add(time.Duration(n.Config.RetryIntervalSeconds) * time.Second)
		case <-ticker.C:
		}
		s := n.Repo.Snapshot()
		seen := map[string]bool{}
		for _, event := range s.Outbox {
			if seen[event.Key] {
				continue
			}
			seen[event.Key] = true
			if busy[event.Key] || time.Now().Before(retryAt[event.ID]) || len(busy) >= n.Config.Workers {
				continue
			}
			busy[event.Key] = true
			wg.Add(1)
			go func(e Event) {
				defer wg.Done()
				defer func() { results <- completion{e.Key, e.ID} }()
				for _, target := range e.PendingURLs {
					if ctx.Err() != nil {
						return
					}
					if err := postEvent(ctx, n.Client, n.Config, e, target); err != nil {
						n.Logger.Warn("webhook delivery pending", "event_id", e.ID, "error", err)
						continue
					}
					if err := n.Repo.Update(func(s *State) {
						for i := range s.Outbox {
							if s.Outbox[i].ID != e.ID {
								continue
							}
							pending := s.Outbox[i].PendingURLs[:0]
							for _, u := range s.Outbox[i].PendingURLs {
								if u != target {
									pending = append(pending, u)
								}
							}
							s.Outbox[i].PendingURLs = pending
							if len(pending) == 0 {
								s.Outbox = append(s.Outbox[:i], s.Outbox[i+1:]...)
							}
							break
						}
					}); err != nil {
						n.Logger.Error("persist webhook acknowledgement", "event_id", e.ID, "error", err)
					}
				}
			}(event)
		}
		// Bound retry bookkeeping to currently pending event IDs.
		active := map[string]bool{}
		for _, e := range s.Outbox {
			active[e.ID] = true
		}
		for id := range retryAt {
			if !active[id] {
				delete(retryAt, id)
			}
		}
	}
}
