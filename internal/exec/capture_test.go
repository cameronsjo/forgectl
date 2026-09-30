package exec

// Test plan for capture.go and runAndWrap's bounded capture (#512)
//
// tailBuffer (Classification: bounded writer)
//   [x] Invariant: keeps exactly the last limit bytes across mixed write
//       sizes, and counts every byte it drops
// maskedTail (Classification: masking under truncation)
//   [x] Boundary: a cut through a multi-byte rune never starts the text on a
//       continuation byte
// OSRunner.Run (Classification: process boundary)
//   [x] Unhappy: megabytes of stderr keep the tail (the real error) and the
//       exact dropped count, and Error() says it was truncated
//   [x] Security: a masked value split by the cut leaves no fragment in
//       Error(), Stderr, or the log, and a whole value right after the
//       fragment is still masked
//   [x] Security: a fragment whose masked form outgrows the longest value
//       (short values inside it expand to [redacted]) is still removed
//   [x] Security: overlapping values, where removing one fragment exposes
//       the end of another, leave neither
//   [x] Security: a withheld argv value longer than every masked entry,
//       split by the cut, neither panics nor leaves a fragment (#925)
//   [x] Unhappy: stdout past the ceiling kills the child and fails with
//       ErrOutputTooLarge and no partial output
//   [x] Happy: a large legitimate stdout (2 MiB) comes back whole

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Mutation: drop `t.dropped += int64(excess)` from the compaction branch (or
// from the large-write branch) and the dropped count comes up short.
func TestTailBuffer_KeepsLastLimitBytesAndCountsTheRest(t *testing.T) {
	const limit = 16
	tb := &tailBuffer{limit: limit}
	var all bytes.Buffer
	// Small writes (past 2*limit, so compaction runs), then one write larger
	// than the limit, then small writes again.
	chunks := []string{"abc", "defghij", "klmnopqrstu", "vwxyz0123456789", "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "!", "@#$", "%^&*()_+-="}
	for _, c := range chunks {
		n, err := tb.Write([]byte(c))
		if n != len(c) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", c, n, err)
		}
		all.WriteString(c)
		got, dropped := tb.tail()
		want := all.Bytes()
		if len(want) > limit {
			want = want[len(want)-limit:]
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("after %q: tail = %q, want %q", c, got, want)
		}
		if wantDropped := int64(all.Len() - len(want)); dropped != wantDropped {
			t.Fatalf("after %q: dropped = %d, want %d", c, dropped, wantDropped)
		}
		if cap(tb.buf) > 4*limit {
			t.Fatalf("after %q: buffer grew to cap %d, want bounded near %d", c, cap(tb.buf), limit)
		}
	}
}

// Mutation: delete the RuneStart loop in maskedTail and the text starts on
// the continuation byte of the split "é".
func TestMaskedTail_CutThroughRuneStartsOnRuneBoundary(t *testing.T) {
	tb := &tailBuffer{limit: 4}
	_, _ = tb.Write([]byte("xé123")) // "é" is 2 bytes; the cut lands between them
	got, dropped := maskedTail(tb, argMask{})
	if !utf8.ValidString(got) || got != "123" {
		t.Fatalf("maskedTail = %q, want %q", got, "123")
	}
	if dropped != 3 {
		t.Fatalf("dropped = %d, want 3 (x, both bytes of é)", dropped)
	}
}

// A long masked value contains short masked values ("k1", scrubbed as whole
// words), so masking the fragment makes it longer: each 2-byte "k1" becomes
// the 10-byte "[redacted]". The brief's first sketch, mask the tail and then
// trim the longest value's length, leaves the fragment's end past the trim.
//
// Mutation: replace maskedTail's body with that sketch
// (s := mask.text(string(raw)); s = s[min(len(longestValue), len(s)):]) and
// "Zq9Kv7Wm2Pn4" survives.
func TestMaskedTail_FragmentThatGrowsWhenMaskedIsStillRemoved(t *testing.T) {
	const long = ".k1.k1.k1.k1.k1.k1.k1.k1.k1.k1Zq9Kv7Wm2Pn4" //nolint:gosec // G101: a fake value the mask must hide
	mask := maskOf(t, "LONG="+long, "SHORT=k1")
	const tail = " fatal: done"
	// The cut lands one byte into the long value.
	tb := &tailBuffer{limit: len(long) - 1 + len(tail)}
	_, _ = tb.Write([]byte("prefix-bytes-the-cap-drops" + long + tail))
	got, dropped := maskedTail(tb, mask)
	if dropped == 0 {
		t.Fatal("nothing dropped; the layout did not cross the cap")
	}
	if strings.Contains(got, "Zq9Kv7") || strings.Contains(got, "Wm2Pn4") {
		t.Fatalf("fragment survived: %q", got)
	}
	if got != tail {
		t.Fatalf("maskedTail = %q, want %q", got, tail)
	}
}

// The stream carries "SECRETONE1" immediately followed by the rest of
// "E1TWO22XYZ", so the two values overlap on "E1". The cut lands inside the
// first. Dropping the first fragment ("RETONE1") exposes "TWO22XYZ", a proper
// suffix of the second value, which the mask can no longer match.
//
// Mutation: make straddleLen a single pass (return n after the first
// iteration) and "TWO22XYZ" survives.
func TestMaskedTail_OverlappingFragmentsAreAllRemoved(t *testing.T) {
	mask := maskOf(t, "A=SECRETONE1", "B=E1TWO22XYZ")
	tb := &tailBuffer{limit: 32}
	_, _ = tb.Write([]byte("xxxxxxxxxxxxSECRETONE1TWO22XYZ tail of stderr"))
	got, _ := maskedTail(tb, mask)
	if strings.Contains(got, "RETONE") || strings.Contains(got, "TWO22") {
		t.Fatalf("fragment survived: %q", got)
	}
	if got != " tail of stderr" {
		t.Fatalf("maskedTail = %q, want %q", got, " tail of stderr")
	}
}

func maskOf(t *testing.T, entries ...string) argMask {
	t.Helper()
	return maskFrom(WithMaskedAssignments(t.Context(), entries))
}

// Mutation: give tailBuffer an unreachable limit in runAndWrap (or restore the
// strings.Builder) and Stderr carries the whole megabyte.
func TestOSRunner_Run_StderrKeepsTailAndDroppedCount(t *testing.T) {
	const filler = 1 << 20
	const reason = "fatal: the real reason\n"
	_, err := OSRunner{}.Run(t.Context(), "sh", "-c",
		fmt.Sprintf(`head -c %d /dev/zero | tr '\0' x >&2; printf '%s' >&2; exit 1`, filler, strings.TrimSuffix(reason, "\n")+`\n`))
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("error = %T %v, want *CommandError", err, err)
	}
	if !strings.HasSuffix(cmdErr.Stderr, "fatal: the real reason") {
		t.Fatalf("Stderr lost its tail: ...%q", cmdErr.Stderr[max(0, len(cmdErr.Stderr)-40):])
	}
	if len(cmdErr.Stderr) > maxStderrTail {
		t.Fatalf("len(Stderr) = %d, want <= %d", len(cmdErr.Stderr), maxStderrTail)
	}
	if want := int64(filler + len(reason) - maxStderrTail); cmdErr.StderrDropped != want {
		t.Fatalf("StderrDropped = %d, want %d", cmdErr.StderrDropped, want)
	}
	if !strings.Contains(err.Error(), "[stderr truncated, ") {
		t.Fatalf("Error() does not say stderr was truncated: %.120q", err.Error())
	}
	if cmdErr.ExitCode != 1 {
		t.Fatalf("ExitCode = %d, want 1", cmdErr.ExitCode)
	}
}

// The stream is laid out so the 64 KiB cut lands 20 bytes before the end of
// the first echo of the secret, and a second whole echo follows right after:
//
//	x * 100000 | secret | secret | \n | y * pad | fatal: done\n
//	                  ^cut
//
// Mutation: make straddleLen return 0 (or drop the straddleLen call from
// maskedTail) and the 20-byte fragment survives the cut into all three
// surfaces.
func TestOSRunner_Run_MaskedValueSplitByCutLeavesNoFragment(t *testing.T) {
	const secret = "Zq9Kv7Wm2Pn4Rt6Yb8Hc1Jd5Lf3Gs0Qe" //nolint:gosec // G101: a fake value the mask must hide
	const fragment = 20
	const reason = "fatal: done\n"
	pad := maxStderrTail - fragment - len(secret) - 1 - len(reason)
	entry := "TOKEN=" + secret
	logs := captureLogs(t)

	ctx := WithMaskedAssignments(t.Context(), []string{entry})
	script := fmt.Sprintf(`v=${1#TOKEN=}
head -c 100000 /dev/zero | tr '\0' x >&2
printf '%%s%%s\n' "$v" "$v" >&2
head -c %d /dev/zero | tr '\0' y >&2
printf 'fatal: done\n' >&2
exit 1`, pad)
	_, err := OSRunner{}.Run(ctx, "sh", "-c", script, "sh", entry)
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("error = %T %v, want *CommandError", err, err)
	}
	if !strings.HasSuffix(cmdErr.Stderr, "fatal: done") {
		t.Fatalf("Stderr lost its tail")
	}
	if cmdErr.StderrDropped == 0 {
		t.Fatal("StderrDropped = 0; the layout did not cross the cap, so the test proves nothing")
	}
	surfaces := map[string]string{"Error()": err.Error(), "Stderr": cmdErr.Stderr, "log": logs.String()}
	// Any 6-byte window of the secret counts as a leaked fragment. The
	// filler bytes (x, y) never occur in it, so a window cannot match by
	// accident.
	for name, text := range surfaces {
		for i := 0; i+6 <= len(secret); i++ {
			if w := secret[i : i+6]; strings.Contains(text, w) {
				t.Fatalf("%s carries secret fragment %q (offset %d)", name, w, i)
			}
		}
	}
}

// Mutation: remove the Kill in ceilingWriter.Write and the exec'd sleep runs
// its full 30s, blowing the elapsed bound; drop the `if stdout.over` block
// in runAndWrap and the error is "signal: killed"; drop `w.buf = nil` in
// ceilingWriter.Write and Output carries the partial stream.
func TestOSRunner_Run_StdoutPastCeilingFailsClosed(t *testing.T) {
	start := time.Now()
	// SIGPIPE is ignored so the flood itself cannot end the child: only the
	// runner's kill can stop the exec'd sleep.
	_, err := OSRunner{stdoutCeiling: 1 << 20}.Run(t.Context(), "sh", "-c", `trap '' PIPE; yes 2>/dev/null; exec sleep 30`)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("error = %v, want ErrOutputTooLarge", err)
	}
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("error = %T, want *CommandError", err)
	}
	if cmdErr.Output != "" {
		t.Fatalf("Output carries %d bytes of a partial stream; want none", len(cmdErr.Output))
	}
	if cmdErr.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1", cmdErr.ExitCode)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("returned after %v; the child was not killed at the ceiling", elapsed)
	}
}

// It runs at the production ceiling, the zero OSRunner. Mutation: set
// maxStdoutBytes to 1 MiB and this legitimate 2 MiB result fails.
func TestOSRunner_Run_LargeLegitimateStdoutPassesWhole(t *testing.T) {
	const size = 2 << 20
	out, err := OSRunner{}.Run(context.Background(), "sh", "-c", fmt.Sprintf(`head -c %d /dev/zero | tr '\0' z`, size))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != size || strings.Trim(out, "z") != "" {
		t.Fatalf("stdout = %d bytes, want %d bytes of z", len(out), size)
	}
}

// TestOSRunner_RunDiscardingStdout_PastTheCeilingSucceeds pins #661: a caller
// that throws stdout away (a workflow `run` step) must not fail because the
// command printed more than maxStdoutBytes. It writes 1 MiB past the real,
// production ceiling.
//
// Mutation: pass r.ceiling() instead of discardStdout in RunDiscardingStdout
// (or drop the discard branch in ceilingWriter.Write) and this fails with
// ErrOutputTooLarge.
func TestOSRunner_RunDiscardingStdout_PastTheCeilingSucceeds(t *testing.T) {
	size := maxStdoutBytes + 1<<20
	err := OSRunner{}.RunDiscardingStdout(t.Context(), "sh", "-c", fmt.Sprintf(`head -c %d /dev/zero`, size))
	if err != nil {
		t.Fatalf("RunDiscardingStdout: %v", err)
	}
}

// TestOSRunner_RunDiscardingStdout_FailureKeepsMaskedStderr: discarding stdout
// changes nothing else about the failure path. The error is a *CommandError
// with the exit code and the masked stderr, and Output is empty because no
// stdout was kept, even though the child printed the value there too.
//
// Mutation: pass argMask{} instead of maskFrom(ctx) in RunDiscardingStdout and
// the value reaches Stderr and the error text.
func TestOSRunner_RunDiscardingStdout_FailureKeepsMaskedStderr(t *testing.T) {
	const value = "discard-secret-661" //nolint:gosec // G101: a fake value the mask must hide
	entry := "K=" + value
	ctx := WithMaskedAssignments(t.Context(), []string{entry})
	err := OSRunner{}.RunDiscardingStdout(ctx, "sh", "-c", `echo "$1"; echo "bad $1" >&2; exit 4`, "sh", entry)
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("error = %T (%v), want *CommandError", err, err)
	}
	if cmdErr.ExitCode != 4 {
		t.Errorf("ExitCode = %d, want 4", cmdErr.ExitCode)
	}
	if cmdErr.Output != "" {
		t.Errorf("Output = %q, want empty: stdout was discarded", cmdErr.Output)
	}
	if cmdErr.Stderr != "bad K="+Redacted {
		t.Errorf("Stderr = %q, want %q", cmdErr.Stderr, "bad K="+Redacted)
	}
	if strings.Contains(err.Error(), value) {
		t.Errorf("error text carries the masked value: %s", err)
	}
}

// TestOSRunner_Run_LongWithheldArgSplitByCutLeavesNoFragment pins #925 end
// to end. A user span withholds a positional argument longer than every
// masked entry (here there are none), maskFor adds it as a bare value, and
// the child echoes it into stderr past the 64 KiB tail cap with the cut
// landing inside the first echo. straddleLen used to index past its failure
// table on this path and panic the whole process.
//
// Mutation: size straddleLen's longest from m.entries only and the Run
// panics; make straddleLen return 0 and the fragment survives into every
// surface.
func TestOSRunner_Run_LongWithheldArgSplitByCutLeavesNoFragment(t *testing.T) {
	const secret = "Wv3Ht8Np1Qc6Lr0Js5Dk9Mf2Bg7Zt4Pw" //nolint:gosec // G101: a fake value the span must withhold
	const fragment = 20
	const reason = "fatal: done\n"
	pad := maxStderrTail - fragment - len(secret) - 1 - len(reason)
	script := fmt.Sprintf(`head -c 100000 /dev/zero | tr '\0' x >&2
printf '%%s%%s\n' "$1" "$1" >&2
head -c %d /dev/zero | tr '\0' y >&2
printf 'fatal: done\n' >&2
exit 1`, pad)
	args := []string{"-c", script, "sh", secret}
	ctx := WithOpaqueArgs(t.Context(), 3, 1)
	if v := maskFor(ctx, args).data().values; !slices.Contains(v, secret) {
		t.Fatalf("maskFor does not withhold the argument (%d values); the fixture is broken", len(v))
	}
	logs := captureLogs(t)

	_, err := OSRunner{}.Run(ctx, "sh", args...)
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("error = %T %v, want *CommandError", err, err)
	}
	if !strings.HasSuffix(cmdErr.Stderr, "fatal: done") {
		t.Fatalf("Stderr lost its tail")
	}
	if cmdErr.StderrDropped == 0 {
		t.Fatal("StderrDropped = 0; the layout did not cross the cap, so the test proves nothing")
	}
	surfaces := map[string]string{"Error()": err.Error(), "Stderr": cmdErr.Stderr, "log": logs.String()}
	for name, text := range surfaces {
		for i := 0; i+6 <= len(secret); i++ {
			if w := secret[i : i+6]; strings.Contains(text, w) {
				t.Fatalf("%s carries secret fragment %q (offset %d)", name, w, i)
			}
		}
	}
}
