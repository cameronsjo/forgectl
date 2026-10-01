package docs

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/perftest"
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
// a body an eighth that size, both timed in process CPU time
// (perftest.Linear, forgectl#879). Linear, the full body costs about 8 times
// the baseline; the quadratic pass took 7.6 s at full size, and costs about
// 64 times its baseline. Only the ratio is asserted, so host load and -race
// do not fail it.
//
// Mutation that turns it red: in scanBlockIDs, test each line against every
// code and hidden segment in turn instead of codeSet and hiddenSet (the #801
// quadratic pass).
func TestScanBlockIDs_LinearInSegments(t *testing.T) {
	const k = 8
	body := blockIDStressBody(maxScanBytes - 1)
	scan, err := scanBodyFor(RootVault, body)
	if err != nil {
		t.Fatalf("scanBodyFor: %v", err)
	}
	if len(scan.masked)+len(scan.hidden) < 50_000 {
		t.Fatalf("fixture yields %d masked + %d hidden segments; the stress shape needs tens of thousands",
			len(scan.masked), len(scan.hidden))
	}
	small := blockIDStressBody((maxScanBytes - 1) / k)
	smallScan, err := scanBodyFor(RootVault, small)
	if err != nil {
		t.Fatalf("scanBodyFor (baseline): %v", err)
	}
	var ids []string
	smallRun, largeRun := perftest.Amortize(
		func() { _ = scanBlockIDs(small, smallScan.masked, smallScan.hidden) },
		func() { ids = scanBlockIDs(body, scan.masked, scan.hidden) })
	perftest.Linear(t, "scanBlockIDs", k, smallRun, largeRun)
	if len(ids) != 1 || ids[0] != "b" {
		t.Errorf("scanBlockIDs = %v, want [b]", ids)
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
