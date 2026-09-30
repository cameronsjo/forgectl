package docs

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/yuin/goldmark/text"
)

// blockIDStressBody builds the #801 shape: n inline %% comments, each its
// own paragraph, then n block-id lines. Every comment is a hidden and a
// masked segment, so a per-line scan over all segments is quadratic.
func blockIDStressBody(size int) []byte {
	const comment, marker = "%%a%%\n\n", "x ^b\n\n"
	n := size / (len(comment) + len(marker))
	var b bytes.Buffer
	b.Grow(size)
	b.WriteString(strings.Repeat(comment, n))
	b.WriteString(strings.Repeat(marker, n))
	return b.Bytes()
}

// TestScanBlockIDs_LinearInSegments bounds the vault block-id pass on a
// 1 MB document dense with comment segments (#801) against the same pass on
// a body a sixteenth that size. Linear, the full body costs about 16 times
// the baseline; the quadratic pass took 7.6 s at full size, about 256 times
// its baseline. The ratio, not a wall-clock number, is the assertion, so a
// slow runner or -race scales both sides together, as
// TestRender_DuplicateHeadings_LinearTime does; the fixed floor absorbs
// timer noise on a fast one.
//
// Mutation that turns it red: in scanBlockIDs, test each line against every
// code and hidden segment in turn instead of codeSet and hiddenSet (the #801
// quadratic pass).
func TestScanBlockIDs_LinearInSegments(t *testing.T) {
	const scale = 16
	body := blockIDStressBody(maxScanBytes - 1)
	scan, err := scanBodyFor(RootVault, body)
	if err != nil {
		t.Fatalf("scanBodyFor: %v", err)
	}
	if len(scan.masked)+len(scan.hidden) < 50_000 {
		t.Fatalf("fixture yields %d masked + %d hidden segments; the stress shape needs tens of thousands",
			len(scan.masked), len(scan.hidden))
	}
	small := blockIDStressBody((maxScanBytes - 1) / scale)
	smallScan, err := scanBodyFor(RootVault, small)
	if err != nil {
		t.Fatalf("scanBodyFor (baseline): %v", err)
	}

	// The fastest of three baseline runs, so one scheduling hiccup cannot
	// inflate the budget.
	baseline := time.Duration(1<<63 - 1)
	for range 3 {
		begin := time.Now()
		_ = scanBlockIDs(small, smallScan.masked, smallScan.hidden)
		baseline = min(baseline, time.Since(begin))
	}
	budget := 3*scale*baseline + 250*time.Millisecond

	// The measured run reports rather than calling t: on a timeout this
	// goroutine outlives the test, and a t method called after the test
	// returns panics.
	type result struct {
		took time.Duration
		ids  []string
	}
	done := make(chan result, 1)
	go func() {
		begin := time.Now()
		ids := scanBlockIDs(body, scan.masked, scan.hidden)
		done <- result{time.Since(begin), ids}
	}()
	segments := len(scan.masked) + len(scan.hidden)
	select {
	case r := <-done:
		if len(r.ids) != 1 || r.ids[0] != "b" {
			t.Errorf("scanBlockIDs = %v, want [b]", r.ids)
		}
		if r.took > budget {
			t.Errorf("scanBlockIDs took %v on a %d-byte body with %d segments, over the %v budget (baseline %v); want linear-time (#801)",
				r.took, len(body), segments, budget, baseline)
		}
		t.Logf("scanBlockIDs: %v on %d segments (baseline %v, budget %v)", r.took, segments, baseline, budget)
	case <-time.After(budget):
		t.Fatalf("scanBlockIDs did not finish within %v on a %d-byte body with %d segments (baseline %v); want linear-time (#801)",
			budget, len(body), segments, baseline)
	}
}

// TestSegmentSet_MatchesLinearScan checks segmentSet.overlaps against the
// direct per-segment test it replaced, on unsorted, overlapping, nested and
// zero-width segments.
func TestSegmentSet_MatchesLinearScan(t *testing.T) {
	rng := rand.New(rand.NewSource(801)) //nolint:gosec // G404: deterministic test fixture, not crypto
	for round := 0; round < 200; round++ {
		segs := make([]text.Segment, rng.Intn(12))
		for i := range segs {
			start := rng.Intn(60)
			segs[i] = text.NewSegment(start, start+rng.Intn(15))
		}
		set := newSegmentSet(segs)
		for start := 0; start < 70; start++ {
			for end := start; end < start+20; end++ {
				want := false
				for _, seg := range segs {
					if seg.Start < end && seg.Stop > start {
						want = true
						break
					}
				}
				if got := set.overlaps(start, end); got != want {
					t.Fatalf("segs %v: overlaps(%d, %d) = %v, want %v", segs, start, end, got, want)
				}
			}
		}
	}
}
