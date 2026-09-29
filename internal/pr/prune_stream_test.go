package pr

// Test plan for the streaming compaction (forgectl#554)
//
// scanRepairLog + copyKept (Classification: two-pass rewrite of the audit log)
//   [x] Output is byte-identical to the pre-#554 in-memory implementation on
//       every fixture: over-long lines (at, under and across the buffer
//       bound, and in several chunks), CR endings, empty lines, unterminated
//       tails of every kind, unpaired intents, id-less and zero-TS rows
//   [x] A 200k-line log prunes end to end to the legacy bytes
//   [x] A log that changes between the passes refuses, dropping nothing
//   [x] Neither pass holds an over-long line: a 32 MiB log with no '\n'
//       allocates a constant, not its size
//   [x] Pass one retains per-id state only, not per-line
//   [x] The over-long bound matches readRepairLogTail's at 8191/8192/8193,
//       with and without '\r' and a final newline

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// --- The pre-#554 implementation, kept verbatim as the oracle ---------------
//
// legacyReadRepairLogLines, legacyClassifyRepairRows and legacyRender are
// origin/main 89b0401's readRepairLogLines, classifyRepairRows and the buffer
// loop of writeRepairLogAtomic, with the file open replaced by a byte slice.
// They are the definition of "the same output"; do not edit them to make a
// test pass.

func legacyReadRepairLogLines(data []byte) ([][]byte, error) {
	var lines [][]byte
	r := bufio.NewReaderSize(bytes.NewReader(data), maxRepairLogLineBytes)
	for {
		line, rerr := r.ReadBytes('\n')
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return nil, fmt.Errorf("read repair audit log: %w", rerr)
		}
		if line = bytes.TrimSuffix(line, []byte("\n")); len(line) > 0 {
			lines = append(lines, line)
		}
		if rerr != nil {
			return lines, nil
		}
	}
}

func legacyClassifyRepairRows(lines [][]byte, cutoff time.Time) ([][]byte, int) {
	type group struct {
		intent    bool
		completed bool
		datable   bool
		old       bool
	}
	groups := make(map[string]*group, len(lines))
	ids := make([]string, len(lines))
	for i, line := range lines {
		if len(line) >= maxRepairLogLineBytes {
			continue
		}
		var row RepairRow
		if err := json.Unmarshal(line, &row); err != nil || row.ID == "" {
			continue
		}
		ids[i] = row.ID
		g := groups[row.ID]
		if g == nil {
			g = &group{datable: true, old: true}
			groups[row.ID] = g
		}
		switch row.Outcome {
		case repairOutcomeIntent:
			g.intent = true
		case repairOutcomeApplied, repairOutcomeFailed:
			g.completed = true
		}
		switch {
		case row.TS.IsZero():
			g.datable = false
		case !row.TS.Before(cutoff):
			g.old = false
		}
	}
	keep := make([][]byte, 0, len(lines))
	dropped := 0
	for i, line := range lines {
		if g := groups[ids[i]]; g != nil && g.intent && g.completed && g.datable && g.old {
			dropped++
			continue
		}
		keep = append(keep, line)
	}
	return keep, dropped
}

func legacyRender(lines [][]byte) []byte {
	var buf bytes.Buffer
	for _, line := range lines {
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// legacyCompact is the whole pre-#554 rewrite of data.
func legacyCompact(t *testing.T, data []byte, cutoff time.Time) (out []byte, kept, dropped int) {
	t.Helper()
	lines, err := legacyReadRepairLogLines(data)
	if err != nil {
		t.Fatal(err)
	}
	keep, dropped := legacyClassifyRepairRows(lines, cutoff)
	return legacyRender(keep), len(keep), dropped
}

// streamCompact is the new two-pass rewrite of data, and asserts pass one's
// counts are the ones pass two reproduced.
func streamCompact(t *testing.T, data []byte, cutoff time.Time) (out []byte, kept, dropped int) {
	t.Helper()
	plan, err := scanRepairLog(bytes.NewReader(data), cutoff)
	if err != nil {
		t.Fatalf("scanRepairLog: %v", err)
	}
	var buf bytes.Buffer
	if err := plan.copyKept(bytes.NewReader(data), &buf); err != nil {
		t.Fatalf("copyKept: %v", err)
	}
	return buf.Bytes(), plan.kept, plan.dropped
}

// --- Fixtures ----------------------------------------------------------------

// paddedRowLine is a row rendered to exactly n bytes of content (the '\n' is
// not counted), padded through Detail, with suffix (a '\r', say) inside n.
func paddedRowLine(t *testing.T, id, outcome string, ts time.Time, n int, suffix string) []byte {
	t.Helper()
	row := RepairRow{TS: ts, ID: id, Mode: RepairModeRollback, Outcome: outcome, RecordPath: "/tmp/x.json"}
	base, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	pad := n - len(suffix) - len(base) - len(`,"detail":""`)
	if pad < 0 {
		t.Fatalf("a %d-byte row cannot hold its own fields (%d)", n, len(base))
	}
	row.Detail = strings.Repeat("d", pad)
	line, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	line = append(line, suffix...)
	if len(line) != n {
		t.Fatalf("fixture line is %d bytes, want %d", len(line), n)
	}
	return line
}

func joinLog(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func term(line []byte) []byte { return append(bytes.Clone(line), '\n') }

// variedLog is every shape the byte-identity test covers, terminated.
func variedLog(t *testing.T, now time.Time) []byte {
	t.Helper()
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-time.Hour)
	cr := func(b []byte) []byte { return append(bytes.Clone(b), '\r') }
	return joinLog(
		term(rowLine(t, "pair-old", repairOutcomeIntent, old)),
		term(rowLine(t, "unpaired-old", repairOutcomeIntent, old)),
		[]byte("\n\n"), // empty lines are gone from the output
		term([]byte("\r")),
		term([]byte("{not json")),
		term([]byte("{not json\r")),
		term(cr(rowLine(t, "crpair-old", repairOutcomeIntent, old))),
		term(rowLine(t, "pair-old", repairOutcomeApplied, old)),
		term(rowLine(t, "straddle", repairOutcomeIntent, old)),
		term(rowLine(t, "recent", repairOutcomeIntent, recent)),
		term(bytes.Repeat([]byte("x"), 9000)),
		term(paddedRowLine(t, "big-pair", repairOutcomeIntent, old, maxRepairLogLineBytes, "")),
		term(rowLine(t, "big-pair", repairOutcomeApplied, old)),
		term(paddedRowLine(t, "just-under", repairOutcomeIntent, old, maxRepairLogLineBytes-1, "")),
		term(paddedRowLine(t, "just-under", repairOutcomeApplied, old, maxRepairLogLineBytes-1, "\r")),
		term(bytes.Repeat([]byte("y"), 2*maxRepairLogLineBytes-1)), // '\n' lands exactly on the second buffer's end
		term(bytes.Repeat([]byte("z"), 2*maxRepairLogLineBytes)),   // the '\n' is a chunk of its own
		term(bytes.Repeat([]byte("w"), 5*maxRepairLogLineBytes+17)),
		term(cr(rowLine(t, "crpair-old", repairOutcomeFailed, old))),
		term(rowLine(t, "straddle", repairOutcomeApplied, recent)),
		term(rowLine(t, "recent", repairOutcomeApplied, recent)),
		term(rowLine(t, "", repairOutcomeApplied, old)),
		term(rowLine(t, "zero-ts", repairOutcomeIntent, time.Time{})),
		term(rowLine(t, "zero-ts", repairOutcomeApplied, old)),
		term(rowLine(t, "dup", repairOutcomeIntent, old)),
		term(rowLine(t, "dup", repairOutcomeApplied, old)),
		term(rowLine(t, "dup", repairOutcomeApplied, old)),
		term(rowLine(t, "unpaired-late", repairOutcomeIntent, old)),
	)
}

// TestStreamCompaction_MatchesTheLegacyOutputByteForByte is the contract of
// #554: the streaming rewrite changes how much is held, never what is written.
//
// Mutation that turns it red: in copyKept, drop the write(nl) after an
// over-long line's end chunk (the "unterminated over-long tail" and every
// over-long fixture fail); or in walkRepairLog, TrimSuffix "\r\n" instead of
// "\n" (every CR fixture fails).
func TestStreamCompaction_MatchesTheLegacyOutputByteForByte(t *testing.T) {
	now := fixedTime()
	cutoff := now.Add(-24 * time.Hour)
	old := now.Add(-48 * time.Hour)
	body := variedLog(t, now)

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"only newlines", []byte("\n\n\n")},
		{"the varied log, terminated", body},
		{"an unterminated parseable tail that settles an old pair",
			joinLog(body, rowLine(t, "unpaired-late", repairOutcomeApplied, old))},
		{"an unterminated unparseable tail", joinLog(body, []byte("{not json"))},
		{"an unterminated CR tail", joinLog(body, []byte("{not json\r"))},
		{"an unterminated over-long tail", joinLog(body, bytes.Repeat([]byte("x"), 3*maxRepairLogLineBytes+5))},
		{"an unterminated tail of exactly the bound",
			joinLog(body, paddedRowLine(t, "unpaired-late", repairOutcomeApplied, old, maxRepairLogLineBytes, ""))},
		{"an unterminated tail one under the bound",
			joinLog(body, paddedRowLine(t, "unpaired-late", repairOutcomeApplied, old, maxRepairLogLineBytes-1, ""))},
		{"a log that is one unterminated over-long line", bytes.Repeat([]byte("q"), 4*maxRepairLogLineBytes)},
		{"a log of only unpaired intents",
			joinLog(term(rowLine(t, "a", repairOutcomeIntent, old)), term(rowLine(t, "b", repairOutcomeIntent, old)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, wantKept, wantDropped := legacyCompact(t, tc.data, cutoff)
			got, kept, dropped := streamCompact(t, tc.data, cutoff)
			if !bytes.Equal(got, want) {
				t.Fatalf("streamed output differs from the legacy rewrite:\nlegacy %d bytes %.400q\nstream %d bytes %.400q",
					len(want), want, len(got), got)
			}
			if kept != wantKept || dropped != wantDropped {
				t.Fatalf("kept/dropped = %d/%d, legacy %d/%d", kept, dropped, wantKept, wantDropped)
			}
		})
	}

	// The varied log must actually exercise both outcomes, or the equality
	// above could hold over a rewrite that drops nothing.
	if _, _, dropped := legacyCompact(t, body, cutoff); dropped != 9 {
		t.Fatalf("the varied fixture drops %d rows under the legacy code, want 9 (pair-old, crpair-old, just-under, dup)", dropped)
	}
}

// TestPrune_ALargeLogCompactsToTheLegacyBytes runs the real Prune over a
// 200k-line log and checks the file it leaves against the legacy rewrite of
// the same bytes plus compaction's own intent row.
//
// Mutation that turns it red: in scanRepairLog, count p.dropped over every
// group rather than settled ones, or in repairLogPlan.drops, return true for
// an unpaired intent (either split between the passes trips the changed-log
// refusal, so the outcome is "failed"); or make repairGroup.settled ignore
// completed, which both passes agree on (the dropped count and bytes differ).
func TestPrune_ALargeLogCompactsToTheLegacyBytes(t *testing.T) {
	if testing.Short() {
		t.Skip("200k-line log")
	}
	c := pruneClient(t, repairRunner(nil))
	old := time.Now().UTC().Add(-200 * 24 * time.Hour)
	recent := time.Now().UTC().Add(-time.Hour)

	var b bytes.Buffer
	const pairs = 95_000
	for i := range pairs {
		id := fmt.Sprintf("p%014d", i)
		_, _ = b.Write(term(rowLine(t, id, repairOutcomeIntent, old)))
		_, _ = b.Write(term(rowLine(t, id, repairOutcomeApplied, old)))
	}
	for i := range 10_000 {
		switch i % 5 {
		case 0:
			_, _ = b.Write(term(rowLine(t, fmt.Sprintf("u%014d", i), repairOutcomeIntent, old)))
		case 1:
			_, _ = b.Write(term(rowLine(t, fmt.Sprintf("r%014d", i), repairOutcomeApplied, recent)))
		case 2:
			_, _ = b.WriteString("{not json\r\n")
		case 3:
			_, _ = b.Write(term(bytes.Repeat([]byte("x"), maxRepairLogLineBytes+i%7)))
		case 4:
			_, _ = b.WriteString("\n")
		}
	}
	_, _ = b.WriteString("{an unterminated tail")
	before := b.Bytes()
	if n := bytes.Count(before, []byte("\n")) + 1; n < 200_000 {
		t.Fatalf("fixture is %d lines, want at least 200k", n)
	}
	if err := os.WriteFile(c.repairLogPath(), before, 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := c.Prune(context.Background(), PruneOpts{
		OlderThan: 30 * 24 * time.Hour, LogRetention: 90 * 24 * time.Hour, Yes: true,
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if report.Log.Outcome != pruneOutcomeCompacted || report.Log.Dropped != 2*pairs {
		t.Fatalf("log outcome = %q, dropped = %d (error %q), want %q dropping %d",
			report.Log.Outcome, report.Log.Dropped, report.Log.Error, pruneOutcomeCompacted, 2*pairs)
	}

	after, err := os.ReadFile(c.repairLogPath()) //nolint:gosec // the test's own t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	// after = rewrite(before + separator + intent) + completion. Recover the
	// two rows compaction wrote about itself, then rebuild the rest with the
	// legacy code.
	lines := bytes.Split(bytes.TrimSuffix(after, []byte("\n")), []byte("\n"))
	if len(lines) < 2 {
		t.Fatalf("compacted log has %d lines", len(lines))
	}
	intent, completion := lines[len(lines)-2], lines[len(lines)-1]
	var ir, cr RepairRow
	if err := json.Unmarshal(intent, &ir); err != nil || ir.Mode != RepairModePrune || ir.Outcome != repairOutcomeIntent {
		t.Fatalf("second-to-last line is not compaction's intent: %.200q (%v)", intent, err)
	}
	if err := json.Unmarshal(completion, &cr); err != nil || cr.ID != ir.ID || cr.Outcome != repairOutcomeApplied {
		t.Fatalf("last line is not compaction's completion: %.200q (%v)", completion, err)
	}
	// appendRepairRow puts a separator before the intent because before ends
	// mid-line (forgectl#549).
	input := joinLog(before, []byte("\n"), term(intent))
	want, _, _ := legacyCompact(t, input, time.Now().UTC().Add(-90*24*time.Hour))
	want = joinLog(want, term(completion))
	if !bytes.Equal(after, want) {
		t.Fatalf("the compacted log differs from the legacy rewrite (%d vs %d bytes)", len(after), len(want))
	}
}

// TestRewriteRepairLog_ALogThatChangesBetweenThePassesRefuses: the lock rules
// out a change between pass one and pass two for every forgectl writer. This
// pins what happens if something outside that contract appends anyway — a
// row pass one never classified must not be copied under the old plan.
//
// Mutation that turns it red: delete the size/kept/dropped comparison in
// copyKept.
func TestRewriteRepairLog_ALogThatChangesBetweenThePassesRefuses(t *testing.T) {
	now := fixedTime()
	cutoff := now.Add(-24 * time.Hour)
	data := variedLog(t, now)
	plan, err := scanRepairLog(bytes.NewReader(data), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	changed := joinLog(data, term(rowLine(t, "pair-old", repairOutcomeIntent, now)))
	err = plan.copyKept(bytes.NewReader(changed), io.Discard)
	if !errors.Is(err, errRepairLogChanged) {
		t.Fatalf("copyKept over a changed log = %v, want errRepairLogChanged", err)
	}

	// The same refusal through the atomic writer leaves the old log in place
	// and no temp file behind.
	c := pruneClient(t, repairRunner(nil))
	if err := os.WriteFile(c.repairLogPath(), changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.writeRepairLogAtomic(plan, bytes.NewReader(changed)); !errors.Is(err, errRepairLogChanged) {
		t.Fatalf("writeRepairLogAtomic = %v, want errRepairLogChanged", err)
	}
	onDisk, err := os.ReadFile(c.repairLogPath()) //nolint:gosec // the test's own t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, changed) {
		t.Fatal("a refused rewrite replaced the log")
	}
	entries, err := os.ReadDir(c.SessionsDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a temp log was left behind: %s", e.Name())
		}
	}
}

// repeatReader yields n bytes of b, forever cycling, without holding them.
type repeatReader struct {
	b byte
	n int64
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.n {
		p = p[:r.n]
	}
	for i := range p {
		p[i] = r.b
	}
	r.n -= int64(len(p))
	return len(p), nil
}

// allocatedBy reports the bytes fn allocates on the heap.
func allocatedBy(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestStreamCompaction_AnOverLongLineIsNeverHeld is the OOM the issue comment
// names: a log with no '\n' at all. Both passes must cost a constant, not the
// file's size.
//
// Mutation that turns it red: in walkRepairLog, replace the chunked
// ReadSlice loop with ReadBytes('\n') (the pre-#554 reader), which assembles
// the whole line.
func TestStreamCompaction_AnOverLongLineIsNeverHeld(t *testing.T) {
	const size = 32 << 20
	const budget = 1 << 20
	cutoff := fixedTime()
	var plan *repairLogPlan
	scanned := allocatedBy(func() {
		var err error
		plan, err = scanRepairLog(&repeatReader{b: 'x', n: size}, cutoff)
		if err != nil {
			t.Error(err)
		}
	})
	if t.Failed() {
		return
	}
	copied := allocatedBy(func() {
		if err := plan.copyKept(&repeatReader{b: 'x', n: size}, io.Discard); err != nil {
			t.Error(err)
		}
	})
	if plan.kept != 1 || plan.size != size {
		t.Fatalf("plan = %d kept over %d bytes, want the one line over %d", plan.kept, plan.size, size)
	}
	if scanned > budget || copied > budget {
		t.Fatalf("a %d MiB line allocated %d bytes to scan and %d to copy, want each under %d — the line is being held",
			size>>20, scanned, copied, budget)
	}
}

// TestScanRepairLog_RetainsPerIDStateNotPerLine pins the stated bound: pass
// one's plan grows with distinct ids, not with lines. 200k rows over four ids
// must retain a few groups, not the rows.
//
// Mutation that turns it red: have scanRepairLog keep each line it reads
// (e.g. a lines [][]byte field on the plan appended with bytes.Clone(line)).
func TestScanRepairLog_RetainsPerIDStateNotPerLine(t *testing.T) {
	now := fixedTime()
	var b bytes.Buffer
	for i := range 200_000 {
		_, _ = b.Write(term(rowLine(t, fmt.Sprintf("id%d", i%4), repairOutcomeApplied, now)))
	}
	data := b.Bytes()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	plan, err := scanRepairLog(bytes.NewReader(data), now)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	var retained uint64
	if after.HeapAlloc > before.HeapAlloc {
		retained = after.HeapAlloc - before.HeapAlloc
	}
	runtime.KeepAlive(plan)
	runtime.KeepAlive(data)
	if len(plan.groups) != 4 || plan.kept != 200_000 {
		t.Fatalf("plan = %d groups, %d kept, want 4 and 200000", len(plan.groups), plan.kept)
	}
	if retained > 1<<20 {
		t.Fatalf("pass one retained %d bytes over a %d-byte log of four ids — it is holding lines", retained, len(data))
	}
}

// TestOverLongBound_MatchesReadRepairLogTail pins the one number both readers
// must agree on (issue #554's comment): a line readRepairLogTail skips as
// over-long is exactly one compaction keeps unparsed, and a line it shows is
// one compaction may settle. A split here would let compaction drop a pair no
// reader could see, or keep forever one every reader shows as settled.
//
// Mutation that turns it red: size walkRepairLog's reader at
// maxRepairLogLineBytes+1 (the 8192 cases flip), or at -1 (the 8191 cases).
func TestOverLongBound_MatchesReadRepairLogTail(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	cutoff := now.Add(-24 * time.Hour)
	for _, n := range []int{maxRepairLogLineBytes - 1, maxRepairLogLineBytes, maxRepairLogLineBytes + 1} {
		for _, suffix := range []string{"", "\r"} {
			for _, terminated := range []bool{true, false} {
				name := fmt.Sprintf("%d bytes, cr=%t, terminated=%t", n, suffix != "", terminated)
				t.Run(name, func(t *testing.T) {
					big := paddedRowLine(t, "edge", repairOutcomeApplied, old, n, suffix)
					data := joinLog(term(rowLine(t, "edge", repairOutcomeIntent, old)), big)
					if terminated {
						data = append(data, '\n')
					}
					c := pruneClient(t, repairRunner(nil))
					if err := os.WriteFile(c.repairLogPath(), data, 0o600); err != nil {
						t.Fatal(err)
					}
					rows, _, skipped, err := c.readRepairLogTail(100)
					if err != nil {
						t.Fatal(err)
					}
					plan, err := scanRepairLog(bytes.NewReader(data), cutoff)
					if err != nil {
						t.Fatal(err)
					}
					readerSaw := skipped == 0 && len(rows) == 2
					compactionSettled := plan.dropped == 2
					if readerSaw != compactionSettled {
						t.Fatalf("readRepairLogTail showed the completion = %t (skipped %d), compaction settled the pair = %t",
							readerSaw, skipped, compactionSettled)
					}
					if wantSeen := n < maxRepairLogLineBytes; readerSaw != wantSeen {
						t.Fatalf("a %d-byte line was shown = %t, want %t", n, readerSaw, wantSeen)
					}
				})
			}
		}
	}
}
