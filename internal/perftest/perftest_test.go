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

// busy spends d of the clock Linear times with: process CPU time where it
// can be read, wall time where not. Spinning to a time rather than for an
// iteration count keeps the self-tests above Floor on any host, fast or slow.
func busy(d time.Duration) func() {
	return func() {
		cpuStart, cpuOK := cpuTime()
		wallStart := time.Now()
		x := sink
		for {
			for range 1000 {
				x = x*6364136223846793005 + 1442695040888963407
			}
			if cpuNow, ok := cpuTime(); cpuOK && ok {
				if cpuNow-cpuStart >= d {
					break
				}
			} else if time.Since(wallStart) >= d {
				break
			}
		}
		sink = x
	}
}

// busyUnit is twice Floor: far enough above it that the small side of every
// self-test clears the floor, close enough that the self-tests stay quick.
const busyUnit = 2 * Floor

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
// The small side is busyUnit, twice Floor (forgectl#964: at a quarter of
// that the test passed quadratic work on CI).
// Mutation: comparing the ratio against k*k instead of k*k/2 turns this red
// (the quadratic ratio sits at the limit and passes).
func TestLinear_FailsQuadraticWork(t *testing.T) {
	const k = 5
	f := run(k, busy(busyUnit), busy(k*k*busyUnit))
	if len(f.errors) != 1 || !strings.Contains(f.errors[0], "want linear time") {
		t.Errorf("errors = %q, want one ratio failure", f.errors)
	}
}

// TestLinear_ScalesUpASmallSide: a real small side under Floor is repeated
// until it clears it, and the ratio is then judged as usual, so a fast host
// passes linear work instead of failing on the floor (forgectl#919). The
// log line says the sides were repeated.
// Mutation: in scaled, return after the first measurement whatever it read
// (the pre-#919 behaviour) and this is refused on the floor.
func TestLinear_ScalesUpASmallSide(t *testing.T) {
	f := run(8, busy(Floor/10), busy(8*Floor/10))
	if len(f.errors)+len(f.fatals) != 0 {
		t.Errorf("a sub-floor small side was not scaled up: errors %q, fatals %q", f.errors, f.fatals)
	}
	if len(f.logs) != 1 || !strings.Contains(f.logs[0], "repeated") {
		t.Errorf("logs = %q, want one line saying the sides were repeated", f.logs)
	}
}

// counter is a fake clock for scaled: each side adds its cost in units to
// ticks when it runs, and measure reads a run's ticks as that many
// milliseconds of CPU time. A side under Floor therefore costs exactly its
// units times the repetitions scaled wraps it in.
type counter struct {
	ticks int
	runs  int
}

func (c *counter) side(units int) func() { return func() { c.ticks += units } }

func (c *counter) measure(f func()) (time.Duration, bool) {
	c.ticks = 0
	c.runs++
	f()
	return time.Duration(c.ticks) * time.Millisecond, true
}

// runFake calls linear with c's clock against a fakeTB.
func runFake(c *counter, k, small, large int) *fakeTB {
	f := &fakeTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		linear(f, "fake", k, c.side(small), c.side(large), c.measure)
	}()
	<-done
	return f
}

// TestLinear_ScaledFakeClock pins the scale-up on a fake clock, where the
// cost of each side is exact. A 1 ms small side (under the 5 ms Floor) is
// repeated to twice Floor in one step, and then linear work passes and
// quadratic work fails as it would have at that size; work that costs
// nothing is refused on the floor once maxScale is reached, after a bounded
// number of measurements.
// Mutations: in scaled, return after the first measurement (the linear and
// quadratic rows are refused on the floor); repeat only the small side (the
// quadratic row passes); drop the maxScale bound (the zero-cost row never
// returns).
func TestLinear_ScaledFakeClock(t *testing.T) {
	const k = 8
	for _, tc := range []struct {
		name         string
		small, large int
		wantError    string
		wantFatal    string
		wantLog      string
	}{
		{name: "linear", small: 1, large: k, wantLog: "repeated 10 times"},
		{name: "quadratic", small: 1, large: k * k, wantError: "want linear time", wantLog: "repeated 10 times"},
		{name: "costs nothing", small: 0, large: 0, wantFatal: "even repeated 1024 times"},
	} {
		c := &counter{}
		f := runFake(c, k, tc.small, tc.large)
		if got := strings.Join(f.errors, "; "); (tc.wantError == "") != (got == "") || !strings.Contains(got, tc.wantError) {
			t.Errorf("%s: errors = %q, want %q", tc.name, f.errors, tc.wantError)
		}
		if got := strings.Join(f.fatals, "; "); (tc.wantFatal == "") != (got == "") || !strings.Contains(got, tc.wantFatal) {
			t.Errorf("%s: fatals = %q, want %q", tc.name, f.fatals, tc.wantFatal)
		}
		if tc.wantLog != "" && (len(f.logs) != 1 || !strings.Contains(f.logs[0], tc.wantLog)) {
			t.Errorf("%s: logs = %q, want one line containing %q", tc.name, f.logs, tc.wantLog)
		}
		// Ten doublings reach maxScale; each measurement is at most Runs pairs.
		if c.runs > 2*Runs*11 {
			t.Errorf("%s: %d measurements, want the scale-up bounded", tc.name, c.runs)
		}
	}
}

// TestWithin_ScaledFakeClock: Within scales a sub-floor base as Linear does,
// and still fails a subject over its limit after scaling.
// Mutation: calling fastest directly in within (no scaled) turns both rows
// red on the floor.
func TestWithin_ScaledFakeClock(t *testing.T) {
	for _, tc := range []struct {
		name          string
		base, subject int
		wantError     string
	}{
		{name: "within the limit", base: 2, subject: 3},
		{name: "over the limit", base: 2, subject: 16, wantError: "over the 4 limit"},
	} {
		c := &counter{}
		f := &fakeTB{}
		done := make(chan struct{})
		go func() {
			defer close(done)
			within(f, "fake", 4, c.side(tc.base), c.side(tc.subject), c.measure)
		}()
		<-done
		if len(f.fatals) != 0 {
			t.Errorf("%s: fatals = %q, want the base scaled past the floor", tc.name, f.fatals)
		}
		if got := strings.Join(f.errors, "; "); (tc.wantError == "") != (got == "") || !strings.Contains(got, tc.wantError) {
			t.Errorf("%s: errors = %q, want %q", tc.name, f.errors, tc.wantError)
		}
	}
}

// runWithin calls Within against a fakeTB and returns what it reported.
func runWithin(limit float64, base, subject func()) *fakeTB {
	f := &fakeTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Within(f, "busy", limit, base, subject)
	}()
	<-done
	return f
}

// TestWithin_PassesEqualWork: a subject doing the base's work passes, and
// the log line names the limit. Mutation: dropping the limit from the log
// line turns this red.
func TestWithin_PassesEqualWork(t *testing.T) {
	f := runWithin(4, busy(busyUnit), busy(busyUnit))
	if len(f.errors)+len(f.fatals) != 0 {
		t.Errorf("equal work failed: errors %q, fatals %q", f.errors, f.fatals)
	}
	if len(f.logs) != 1 || !strings.Contains(f.logs[0], "limit 4") {
		t.Errorf("logs = %q, want one line naming the limit", f.logs)
	}
}

// TestWithin_FailsWorkOverTheLimit: a subject doing 16 times the base's
// work fails a limit of 4. Mutation: dropping the ratio check turns this
// red.
func TestWithin_FailsWorkOverTheLimit(t *testing.T) {
	f := runWithin(4, busy(busyUnit), busy(16*busyUnit))
	if len(f.errors) != 1 || !strings.Contains(f.errors[0], "over the 4 limit") {
		t.Errorf("errors = %q, want one ratio failure", f.errors)
	}
}

// TestWithin_RefusesSmallLimit: a limit under 2 would fail equal work on
// noise, so Within refuses it before timing anything. Mutation: dropping
// the limit < 2 check turns this red.
func TestWithin_RefusesSmallLimit(t *testing.T) {
	calls := 0
	f := runWithin(1, func() { calls++ }, func() { calls++ })
	if len(f.fatals) != 1 || !strings.Contains(f.fatals[0], "limit = 1") {
		t.Errorf("fatals = %q, want one naming limit = 1", f.fatals)
	}
	if calls != 0 {
		t.Errorf("base and subject ran %d times, want 0 after the refusal", calls)
	}
}

// TestWithin_RefusesSmallBase: Within refuses a base still under Floor at
// maxScale repetitions, as Linear refuses a small side.
// Mutation: dropping the b < Floor check turns this red.
func TestWithin_RefusesSmallBase(t *testing.T) {
	c := &counter{}
	f := &fakeTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		within(f, "fake", 4, c.side(0), c.side(0), c.measure)
	}()
	<-done
	if len(f.fatals) != 1 || !strings.Contains(f.fatals[0], "floor") {
		t.Errorf("fatals = %q, want one naming the floor", f.fatals)
	}
}

// TestRepsFor: the count that brings the fastest of up to three runs to the
// target, one when a run already gets there, and never past maxReps.
// Mutations: using the last run instead of the fastest turns the first row
// red; dropping the early return turns the second red (extra runs); dropping
// the maxReps cap turns the third red.
func TestRepsFor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		runs     []time.Duration
		wantReps int
		wantRuns int
	}{
		{"fastest of three", []time.Duration{4 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}, 5, 3},
		{"already long enough", []time.Duration{10 * time.Millisecond, time.Millisecond}, 1, 1},
		{"a zero reading is skipped", []time.Duration{0, 2 * time.Millisecond, 3 * time.Millisecond}, 5, 3},
		{"capped", []time.Duration{time.Nanosecond}, maxReps, 3},
		{"all zero readings", []time.Duration{0}, maxReps, 3},
	} {
		n := 0
		measure := func(func()) (time.Duration, bool) {
			d := tc.runs[min(n, len(tc.runs)-1)]
			n++
			return d, true
		}
		if got := repsFor(func() {}, 10*time.Millisecond, measure); got != tc.wantReps || n != tc.wantRuns {
			t.Errorf("%s: reps %d after %d runs, want %d after %d", tc.name, got, n, tc.wantReps, tc.wantRuns)
		}
	}
}

// TestAmortize_RepeatsBothSidesAlike: a small side far under 2·Floor is
// repeated until it clears it, and the large side runs the same number of
// times per call, so the ratio is the one the test asked for.
// Mutations: repeating only the small side turns the count check red;
// returning small unrepeated turns the clock check red.
func TestAmortize_RepeatsBothSidesAlike(t *testing.T) {
	smallCalls, largeCalls := 0, 0
	spin := busy(Floor / 10)
	small, large := Amortize(func() { smallCalls++; spin() }, func() { largeCalls++; spin() })
	smallCalls, largeCalls = 0, 0
	d, _ := timed(small)
	large()
	if smallCalls < 2 || smallCalls != largeCalls {
		t.Errorf("small ran %d times and large %d, want the same count of at least 2", smallCalls, largeCalls)
	}
	if d < Floor {
		t.Errorf("amortized small side costs %v, want at least the %v floor", d, Floor)
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
