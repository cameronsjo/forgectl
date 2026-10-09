package mail

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPolicyCheck(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	coord := Worker{Name: "coord", Coordinator: true}
	a := Worker{Name: "a"}
	b := Worker{Name: "b"}
	sys := Worker{Name: SystemSender}
	p := DefaultPolicy()

	sent := func(from, to, body string, ago time.Duration) Entry {
		return Entry{Msg: Message{From: from, To: to, Body: body, CreatedAt: now.Add(-ago)}}
	}

	cases := []struct {
		name    string
		p       Policy
		from    Worker
		to      Worker
		body    string
		history []Entry
		want    error
	}{
		{"worker to coordinator", p, a, coord, "hi", nil, nil},
		{"coordinator to worker", p, coord, a, "hi", nil, nil},
		{"worker to worker refused", p, a, b, "hi", nil, ErrPeerBlocked},
		{"worker to worker allowed", Policy{PeerMessages: true}, a, b, "hi", nil, nil},
		{"system to worker", p, sys, a, "hi", nil, nil},
		{"too large", p, coord, a, strings.Repeat("x", p.MaxBody+1), nil, ErrTooLarge},
		{"duplicate", p, coord, a, "hi", []Entry{sent("coord", "a", "hi", 10*time.Second)}, ErrDuplicate},
		{"old duplicate is fine", p, coord, a, "hi", []Entry{sent("coord", "a", "hi", 2*time.Minute)}, nil},
		{"other pair does not count", p, coord, a, "hi", []Entry{sent("coord", "b", "hi", time.Second)}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Check(tc.from, tc.to, tc.body, tc.history, now)
			if tc.want == nil && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	var burst []Entry
	for i := 0; i < p.PerPairPerMinute; i++ {
		burst = append(burst, sent("coord", "a", strings.Repeat("m", i+1), 40*time.Second))
	}
	if err := p.Check(coord, a, "one more", burst, now); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("burst: err = %v, want ErrRateLimited", err)
	}
}

func TestNextAttemptBackoff(t *testing.T) {
	p := DefaultPolicy()
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	if !p.nextAttempt(Entry{}).IsZero() {
		t.Fatal("an unattempted entry is due now")
	}
	for attempts, want := range map[int]time.Duration{1: 5 * time.Second, 2: 10 * time.Second, 5: 80 * time.Second, 6: 2 * time.Minute, 30: 2 * time.Minute} {
		got := p.nextAttempt(Entry{Attempts: attempts, LastAttempt: at}).Sub(at)
		if got != want {
			t.Errorf("attempts %d: wait %s, want %s", attempts, got, want)
		}
	}
}
