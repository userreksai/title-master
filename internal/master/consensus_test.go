package master

import (
	"testing"
	"time"

	"github.com/userreksai/title-master/internal/protocol"
)

func title(s string) protocol.Observation {
	return protocol.Observation{Kind: "title", Title: s, StatusCode: 200}
}
func siteError(s string) protocol.Observation { o := title(s); o.StatusCode = 500; return o }
func timeout() protocol.Observation           { return protocol.Observation{Kind: "timeout"} }

func TestConsensusThresholdAndExclusions(t *testing.T) {
	cases := []struct {
		name    string
		items   []protocol.Observation
		percent float64
		min     int
		ok      bool
		value   Value
	}{
		{"all agree", []protocol.Observation{title("123"), title("123"), title("123")}, 100, 1, true, Value{"title", "123"}},
		{"100 percent blocks title minority", []protocol.Observation{title("new"), title("new"), title("old")}, 100, 1, false, Value{}},
		{"configured majority", []protocol.Observation{title("new"), title("new"), title("old")}, 66, 1, true, Value{"title", "new"}},
		{"tie holds", []protocol.Observation{title("a"), title("b")}, 50, 1, false, Value{}},
		{"website timeout counts in denominator", []protocol.Observation{title("a"), timeout()}, 100, 1, false, Value{}},
		{"100 percent failures", []protocol.Observation{timeout(), timeout(), timeout()}, 100, 1, true, Value{"failure", ""}},
		{"failure minority holds", []protocol.Observation{timeout(), timeout(), title("a")}, 100, 1, false, Value{}},
		{"66 percent failures", []protocol.Observation{timeout(), timeout(), title("a")}, 66, 1, true, Value{"failure", ""}},
		{"low failure threshold takes precedence", []protocol.Observation{timeout(), timeout(), title("a")}, 30, 1, true, Value{"failure", ""}},
		{"error page retained", []protocol.Observation{siteError("500服务失败"), siteError("500服务失败")}, 100, 1, true, Value{"title", "500服务失败"}},
		{"one healthy node after exclusion", []protocol.Observation{title("a")}, 100, 1, true, Value{"title", "a"}},
		{"minimum healthy", []protocol.Observation{title("a")}, 100, 2, false, Value{}},
		{"no valid nodes", nil, 100, 1, false, Value{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := Decide(tc.items, tc.percent, tc.min)
			if ok != tc.ok || (ok && d.Value != tc.value) {
				t.Fatalf("got %+v,%v", d, ok)
			}
		})
	}
}

func TestOnlyTransitionsNotifyAndRecovery(t *testing.T) {
	s := State{Domains: map[string]DomainState{}, Machines: map[string]MachineState{}}
	domain := Domain{ID: "1", Domain: "example.com"}
	urls := []string{"https://hook.example/a"}
	now := time.Now()
	for _, o := range []protocol.Observation{title("123"), siteError("500服务失败"), siteError("500服务失败"), title("123")} {
		d, _ := Decide([]protocol.Observation{o, o, o}, 100, 1)
		applyDecision(&s, domain, d, true, urls, now)
	}
	if len(s.Outbox) != 2 {
		t.Fatalf("expected exactly two alerts, got %d", len(s.Outbox))
	}
	for _, o := range []protocol.Observation{timeout(), timeout(), title("123")} {
		d, _ := Decide([]protocol.Observation{o}, 100, 1)
		applyDecision(&s, domain, d, true, urls, now)
	}
	if len(s.Outbox) != 4 {
		t.Fatalf("timeout transition/recovery alerts = %d", len(s.Outbox))
	}
	applyMachine(&s, "agent:1", "agent-1", false, "offline", urls, now)
	applyMachine(&s, "agent:1", "agent-1", false, "other transport error", urls, now)
	applyMachine(&s, "agent:1", "agent-1", true, "recovered", urls, now)
	if len(s.Outbox) != 6 {
		t.Fatalf("machine transition dedup failed: %d", len(s.Outbox))
	}
}
