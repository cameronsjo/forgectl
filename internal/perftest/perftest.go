// Package perftest holds the timing rule the linear-time tests share
// (forgectl#879): a test that guards against a quadratic regression times an
// operation at size n and at k·n in the same process and asserts the ratio,
// instead of holding one run to a wall-clock bound.
//
// A wall-clock bound fails whenever the host is loaded. A wall-clock ratio
// fails too, under enough load: a millisecond run can finish inside one
// scheduler time slice, so the fastest of several gets the CPU to itself,
// while a run many slices long is preempted on every one of them and is
// slowed by the whole oversubscription factor. Measured with eight CPU
// burners on four cores, that pushed linear ratios of 8 past 100. So the
// runs are timed in process CPU time, which waiting on a runqueue does not
// add to, and fall back to the wall clock only where CPU time cannot be read.
// A base side under Floor is refused, since its ratio is noise; a caller
// whose base is cheaper than that passes both sides through Amortize first.
// Only tests import this package.
//
// Linear reads the whole test process's CPU time, not the calling
// goroutine's, so anything else running in the test binary during a
// measurement counts on both sides of the ratio. Never call it from a test
// that calls t.Parallel, or from a subtest of one: that test then runs
// alongside every other parallel test in its package, and their CPU time
// lands on its clock. A test that stays sequential is safe from t.Parallel
// elsewhere in its package, since Go runs parallel tests only with other
// parallel tests, but not from a goroutine an earlier test left running.
// Packages run in separate processes, so go test -p does not reach it.
package perftest

import (
	"math"
	"runtime"
	"testing"
	"time"
)

// Runs is how many small/large pairs Linear times at most.
const Runs = 5

// Floor is the least CPU time the base side of a ratio may measure. Below it
// a ratio is noise: process CPU time is accounted in scheduler ticks on some
// kernels (up to 4 ms), and a run that short can read as a fraction of its
// true cost, which inflates the ratio's small side and lets quadratic work
// pass (forgectl#964). 5 ms is a little over one such tick; a test whose base
// side measures less must do more work, not relax the floor.
const Floor = 5 * time.Millisecond

// maxReps bounds Amortize's repeat count, so work that costs nothing at all
// fails Floor instead of looping a million times.
const maxReps = 1000

// Ceiling is Linear's absolute backstop on one large run: a regression slow
// enough that timing more pairs is a waste. The ratio is the assertion; a
// true hang never returns, and go test's -timeout catches it.
const Ceiling = time.Minute

// Linear fails t unless large, which does k times the input of small, runs
// in less than k²/2 times small's time. Linear work puts the ratio near k and
// quadratic near k², so with k of at least 4 the limit leaves a factor of two
// or more on each side. It also fails t if one large run exceeds Ceiling, and
// refuses, with t.Fatalf, a small side that measures under Floor.
func Linear(t testing.TB, what string, k int, small, large func()) {
	t.Helper()
	if k < 4 {
		t.Fatalf("perftest.Linear: k = %d, want at least 4 so linear and quadratic are apart", k)
	}
	limit := float64(k*k) / 2
	s, l, clock := fastest(Runs, limit, Ceiling, timed, small, large)
	ratio := float64(l) / float64(max(s, 1))
	t.Logf("%s: %v at n, %v at %d·n in %s, ratio %.1f (limit %.0f)", what, s, l, k, clock, ratio, limit)
	if l > Ceiling {
		t.Errorf("%s: one run at %d·n cost %v, over the %v backstop", what, k, l, Ceiling)
	} else if s < Floor {
		t.Fatalf("%s: the %v run at n is under the %v floor, so its ratio is noise; do more work at n", what, s, Floor)
	} else if ratio > limit {
		t.Errorf("%s: %d·n cost %v against %v at n, a ratio of %.1f over the %.0f limit; want linear time (linear is about %d, quadratic about %d)",
			what, k, l, s, ratio, limit, k, k*k)
	}
}

// Within fails t unless subject runs in less than limit times base's time,
// timed as Linear times its runs. It is for a bound that is not a growth
// rate: a capped operation on an input past its cap against the same
// input the operation never had to do the capped work on. It also fails t
// if one subject run exceeds Ceiling, and refuses, with t.Fatalf, a base that
// measures under Floor.
func Within(t testing.TB, what string, limit float64, base, subject func()) {
	t.Helper()
	if limit < 2 {
		t.Fatalf("perftest.Within: limit = %v, want at least 2 so noise on the base side cannot fail it", limit)
	}
	b, s, clock := fastest(Runs, limit, Ceiling, timed, base, subject)
	ratio := float64(s) / float64(max(b, 1))
	t.Logf("%s: %v against a base of %v in %s, ratio %.1f (limit %.0f)", what, s, b, clock, ratio, limit)
	if s > Ceiling {
		t.Errorf("%s: one run cost %v, over the %v backstop", what, s, Ceiling)
	} else if b < Floor {
		t.Fatalf("%s: the base run of %v is under the %v floor, so its ratio is noise; do more work in the base", what, b, Floor)
	} else if ratio > limit {
		t.Errorf("%s: cost %v against a base of %v, a ratio of %.1f over the %.0f limit", what, s, b, ratio, limit)
	}
}

// Amortize returns small and large each repeated enough times that small
// costs at least twice Floor, the same count on both sides so their ratio is
// unchanged. A test whose small side is a fraction of a millisecond uses it
// instead of growing its input, which can move the work across a size cap or
// guard the test exists to exercise. The count comes from timing small up to
// three times; work that already costs twice Floor is not repeated.
func Amortize(small, large func()) (func(), func()) {
	reps := repsFor(small, 2*Floor, timed)
	return repeat(reps, small), repeat(reps, large)
}

// repsFor is how many runs of f cost at least target, from the fastest of up
// to three runs, stopping at the first that is already that long. measure
// times one run, as fastest's does. It is at most maxReps.
func repsFor(f func(), target time.Duration, measure func(func()) (time.Duration, bool)) int {
	best := time.Duration(math.MaxInt64)
	for range 3 {
		d, _ := measure(f)
		best = min(best, d)
		if best >= target {
			return 1
		}
	}
	return int(min(int64(maxReps), (int64(target)+int64(max(best, 1))-1)/int64(max(best, 1))))
}

func repeat(n int, f func()) func() {
	if n <= 1 {
		return f
	}
	return func() {
		for range n {
			f()
		}
	}
}

// fastest times small and large alternately, up to runs pairs, and returns
// the fastest run of each. It stops early once the fastest large run is at
// most limit times the fastest small one, so an idle machine pays for one
// pair and a loaded one gets more chances at a quiet sample; the minimum
// discards runs that cache contention from other processes inflated. It also
// stops once one large run exceeds ceiling, so a regressed operation fails
// after one slow pair. measure times one run; Linear passes timed. clock names
// what the runs were timed in: "CPU time", "wall time", or, if reading CPU
// time failed for some runs and not others, "mixed CPU and wall time", whose
// ratio means nothing.
func fastest(runs int, limit float64, ceiling time.Duration, measure func(func()) (time.Duration, bool), small, large func()) (fastSmall, fastLarge time.Duration, clock string) {
	fastSmall, fastLarge = time.Duration(math.MaxInt64), time.Duration(math.MaxInt64)
	cpuRuns, allRuns := 0, 0
	sample := func(f func()) time.Duration {
		d, cpu := measure(f)
		allRuns++
		if cpu {
			cpuRuns++
		}
		return d
	}
	for range runs {
		fastSmall = min(fastSmall, sample(small))
		last := sample(large)
		fastLarge = min(fastLarge, last)
		if float64(fastLarge) <= limit*float64(fastSmall) || last > ceiling {
			break
		}
	}
	switch cpuRuns {
	case allRuns:
		clock = "CPU time"
	case 0:
		clock = "wall time"
	default:
		clock = "mixed CPU and wall time"
	}
	return fastSmall, fastLarge, clock
}

// timed is f's cost in process CPU time, or in wall time where CPU time
// cannot be read, and whether it is CPU time. A GC before the run keeps one
// side's garbage off the other side's clock.
func timed(f func()) (time.Duration, bool) {
	runtime.GC()
	cpuStart, cpuOK := cpuTime()
	start := time.Now()
	f()
	wall := time.Since(start)
	if cpuEnd, ok := cpuTime(); cpuOK && ok {
		return cpuEnd - cpuStart, true
	}
	return wall, false
}
