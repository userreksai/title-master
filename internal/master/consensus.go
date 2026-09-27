package master

import (
	"fmt"
	"sort"
	"time"

	"github.com/userreksai/title-master/internal/protocol"
)

type Value struct {
	Kind  string `json:"kind"`
	Title string `json:"title,omitempty"`
}

func (v Value) String() string {
	if v.Kind == "title" {
		return v.Title
	}
	return "异常：标题获取失败（含超时/HTTP错误/无标题）"
}

type Decision struct {
	Value  Value
	Failed bool
	Votes  int
	Valid  int
	Detail string
}

// The same threshold applies to title changes and site failures. An exact
// tie never changes state, including when the threshold is below 50%.
func Decide(items []protocol.Observation, percent float64, minHealthy int) (Decision, bool) {
	n := len(items)
	if n < minHealthy {
		return Decision{}, false
	}
	failures := 0
	counts := map[Value]int{}
	failedTitles := map[Value]int{}
	details := make([]string, 0, n)
	for _, o := range items {
		if o.Failed() {
			failures++
		}
		if o.Kind == "title" {
			counts[Value{Kind: "title", Title: o.Title}]++
			if o.Failed() {
				failedTitles[Value{Kind: "title", Title: o.Title}]++
			}
		}
		details = append(details, fmt.Sprintf("%s HTTP=%d title=%q", o.Kind, o.StatusCode, o.Title))
	}
	failureConfirmed := float64(failures)*100/float64(n)+1e-9 >= percent
	// Confirmed failures take precedence at low thresholds. Preserve an error
	// page's title only when that title also reaches the configured threshold.
	if failureConfirmed {
		counts = failedTitles
	}
	// Select a unique largest title, only if it meets the configured fraction
	// of ALL valid nodes (failed website observations stay in the denominator).
	best := Value{}
	votes := 0
	tie := false
	for value, count := range counts {
		if count > votes {
			best = value
			votes = count
			tie = false
		} else if count == votes {
			tie = true
		}
	}
	sort.Strings(details)
	d := Decision{Failed: failureConfirmed, Valid: n, Detail: fmt.Sprint(details)}
	if !tie && float64(votes)*100/float64(n)+1e-9 >= percent {
		d.Value = best
		d.Votes = votes
		return d, true
	}
	if failureConfirmed {
		d.Value = Value{Kind: "failure"}
		d.Votes = failures
		return d, true
	}
	return Decision{}, false
}

type Domain struct {
	ID     string `json:"id"`
	Domain string `json:"domain"`
	Tag    string `json:"tag"`
	Active bool   `json:"active"`
}

type DomainState struct {
	Domain      string    `json:"domain"`
	Value       Value     `json:"value"`
	CheckedAt   time.Time `json:"checked_at"`
	ChangedAt   time.Time `json:"changed_at"`
	ValidAgents int       `json:"valid_agents"`
	Votes       int       `json:"votes"`
	Detail      string    `json:"detail"`
}

type MachineState struct {
	Healthy   bool      `json:"healthy"`
	Detail    string    `json:"detail"`
	CheckedAt time.Time `json:"checked_at"`
}
