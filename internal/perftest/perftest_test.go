package perftest

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeTB records what Linear reports instead of failing the real test.
// Fatalf ends the calling goroutine, as testing.T's does, so run calls
// Linear on a goroutine of its own. The embedded TB is nil: calling any
// method fakeTB does not override panics, which flags a new dependency.
type fakeTB struct {
	testing.TB
	logs, errors, fatals []string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Logf(format string, args ...any) {
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}

func (f *fakeTB) Errorf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.fatals = append(f.fatals, fmt.Sprintf(format, args...))
	runtime.Goexit()
}

// run calls Linear against a fakeTB and returns what it reported.
func run(k int, small, large func()) *fakeTB {
	f := &fakeTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Linear(f, "busy", k, small, large)
	}()
	<-done
	return f
}

// sink keeps busy's loop from being optimized away.
var sink uint64

// busy spends CPU time in proportion to n.
func busy(n int) func() {
	return func() {
		x := sink
		for range n {
			x = x*6364136223846793005 + 1442695040888963407
		}
		sink = x
	}
}

// busyUnit is about a millisecond of busy work, so a small side is well
// above timer resolution.
const busyUnit = 1_000_000

// TestLinear_RefusesSmallK: a k under 4 puts linear and quadratic too close
// to tell apart, so Linear refuses it before timing anything.
// Mutation: dropping the k < 4 check turns this red.
func TestLinear_RefusesSmallK(t *testing.T) {
	calls := 0
	f := run(3, func() { calls++ }, func() { calls++ })
	if len(f.fatals) != 1 || !strings.Contains(f.fatals[0], "k = 3") {
		t.Errorf("fatals = %q, want one naming k = 3", f.fatals)
	}
	if calls != 0 {
		t.Errorf("small and large ran %d times, want 0 after the refusal", calls)
	}
}

// TestLinear_PassesLinearWork: large doing k times small's work passes, and
// the log line names the clock the runs were timed in.
// Mutation: dropping the clock from the log line turns this red.
func TestLinear_PassesLinearWork(t *testing.T) {
	const k = 8
	f := run(k, busy(busyUnit), busy(k*busyUnit))
	if len(f.errors)+len(f.fatals) != 0 {
		t.Errorf("linear work failed: errors %q, fatals %q", f.errors, f.fatals)
	}
	want := "in wall time"
	if _, ok := cpuTime(); ok {
		want = "in CPU time"
	}
	if len(f.logs) != 1 || !strings.Contains(f.logs[0], want) {
		t.Errorf("logs = %q, want one line saying %q", f.logs, want)
	}
}

// TestLinear_FailsQuadraticWork: large doing k² times small's work fails.
// Mutation: comparing the ratio against k*k instead of k*k/2 turns this red
// (the quadratic ratio sits at the limit and passes).
func TestLinear_FailsQuadraticWork(t *testing.T) {
	const k = 8
	f := run(k, busy(busyUnit/4), busy(k*k*busyUnit/4))
	if len(f.errors) != 1 || !strings.Contains(f.errors[0], "want linear time") {
		t.Errorf("errors = %q, want one ratio failure", f.errors)
	}
}

// script is a measure that returns the next scripted duration for each
// side, and counts the runs.
type script struct {
	small, large []time.Duration
	cpu          []bool
	smallRuns    int
	largeRuns    int
}

func (s *script) measure(isLarge func() bool) func(func()) (time.Duration, bool) {
	calls := 0
	return func(func()) (time.Duration, bool) {
		cpu := s.cpu == nil || s.cpu[calls%len(s.cpu)]
		calls++
		if isLarge() {
			s.largeRuns++
			return s.large[min(s.largeRuns, len(s.large))-1], cpu
		}
		s.smallRuns++
		return s.small[min(s.smallRuns, len(s.small))-1], cpu
	}
}

// fastestScripted runs fastest with scripted timings: runs alternate small,
// large, so the side is known from the call count.
func fastestScripted(s *script, limit float64, ceiling time.Duration) (time.Duration, time.Duration, string) {
	n := 0
	isLarge := func() bool { n++; return n%2 == 0 }
	return fastest(Runs, limit, ceiling, s.measure(isLarge), func() {}, func() {})
}

// TestFastest_StopsAtFirstPairWithinLimit: an idle machine pays for one
// pair. Mutation: dropping the early break turns this red (Runs pairs).
func TestFastest_StopsAtFirstPairWithinLimit(t *testing.T) {
	s := &script{small: []time.Duration{10 * time.Millisecond}, large: []time.Duration{50 * time.Millisecond}}
	fs, fl, _ := fastestScripted(s, 32, time.Minute)
	if s.smallRuns != 1 || s.largeRuns != 1 {
		t.Errorf("ran %d small and %d large, want 1 pair", s.smallRuns, s.largeRuns)
	}
	if fs != 10*time.Millisecond || fl != 50*time.Millisecond {
		t.Errorf("fastest = %v, %v; want 10ms, 50ms", fs, fl)
	}
}

// TestFastest_RetriesUntilWithinLimit: a noisy large run gets more chances,
// up to Runs pairs, and the fastest of each side is kept even when the last
// run is slower. Mutation: keeping the last large run instead of the
// minimum turns the second case red; dropping the early break turns the
// first red.
func TestFastest_RetriesUntilWithinLimit(t *testing.T) {
	for _, tc := range []struct {
		large    []time.Duration
		wantRuns int
		want     time.Duration
	}{
		{[]time.Duration{time.Second, 2 * time.Second, 50 * time.Millisecond, time.Second}, 3, 50 * time.Millisecond},
		// Never within the 320 ms limit: all Runs pairs, and the fastest
		// is the first, not the last.
		{[]time.Duration{400 * time.Millisecond, 2 * time.Second, time.Second}, Runs, 400 * time.Millisecond},
	} {
		s := &script{small: []time.Duration{10 * time.Millisecond}, large: tc.large}
		_, fl, _ := fastestScripted(s, 32, time.Minute)
		if s.largeRuns != tc.wantRuns || fl != tc.want {
			t.Errorf("large %v: ran %d, fastest %v; want %d runs, fastest %v", tc.large, s.largeRuns, fl, tc.wantRuns, tc.want)
		}
	}
}

// TestFastest_StopsOverCeiling: a large run over the ceiling ends the loop
// after one pair, so a regression fails without Runs slow runs.
// Mutation: dropping `|| last > ceiling` turns this red.
func TestFastest_StopsOverCeiling(t *testing.T) {
	s := &script{small: []time.Duration{time.Millisecond}, large: []time.Duration{2 * time.Second}}
	fastestScripted(s, 32, time.Second)
	if s.largeRuns != 1 {
		t.Errorf("ran %d large runs over the ceiling, want 1", s.largeRuns)
	}
}

// TestFastest_NamesTheClock: the clock is CPU time only when every run was
// timed in CPU time, and a mix is named as one.
// Mutation: naming the clock from the last run alone turns the mixed row red.
func TestFastest_NamesTheClock(t *testing.T) {
	for _, tc := range []struct {
		cpu  []bool
		want string
	}{
		{[]bool{true}, "CPU time"},
		{[]bool{false}, "wall time"},
		{[]bool{false, true, true, true}, "mixed CPU and wall time"},
	} {
		// Never within the limit, so all Runs pairs run and every entry of
		// cpu is used.
		s := &script{small: []time.Duration{time.Millisecond}, large: []time.Duration{time.Second}, cpu: tc.cpu}
		if _, _, clock := fastestScripted(s, 32, time.Minute); clock != tc.want {
			t.Errorf("cpu %v: clock = %q, want %q", tc.cpu, clock, tc.want)
		}
	}
}
