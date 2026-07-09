package pr

// Test plan for agent.go
//
// LaunchPathFor (Classification: pure routing table)
//   [x] default "" → InlineSeeded (agent A)
//   [x] "claude" → InlineSeeded (agent A, explicit)
//   [x] "escalation" → BareTUIEscalation (agent B, present but not-yet-wired)
//   [x] unknown agent → InlineSeeded (falls back to the only wired path)
//   [x] LaunchPath.String renders both variants

import "testing"

func TestLaunchPathFor(t *testing.T) {
	cases := []struct {
		agent string
		want  LaunchPath
	}{
		{"", InlineSeeded},
		{"claude", InlineSeeded},
		{"escalation", BareTUIEscalation},
		{"totally-unknown", InlineSeeded},
	}
	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			if got := LaunchPathFor(tc.agent); got != tc.want {
				t.Errorf("LaunchPathFor(%q) = %v, want %v", tc.agent, got, tc.want)
			}
		})
	}
}

func TestLaunchPathString(t *testing.T) {
	if InlineSeeded.String() != "inline-seeded" {
		t.Errorf("InlineSeeded.String() = %q", InlineSeeded.String())
	}
	if BareTUIEscalation.String() != "bare-tui-escalation" {
		t.Errorf("BareTUIEscalation.String() = %q", BareTUIEscalation.String())
	}
}
