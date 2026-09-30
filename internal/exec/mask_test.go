package exec

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// captureLogs routes the default slog logger into a buffer at debug level for
// the duration of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestOSRunner_MaskedAssignments_NeverRenderValue pins #529: an argv
// assignment marked secret stays out of the debug log, the error text, and
// echoed stderr, while the command itself still receives the real value.
func TestOSRunner_MaskedAssignments_NeverRenderValue(t *testing.T) {
	const secret = "https://ingest.example/v1?api_key=hunter2hunter2" //nolint:gosec // G101: a fake key the mask must hide
	entry := "OTEL_EXPORTER_OTLP_ENDPOINT=" + secret
	logs := captureLogs(t)

	ctx := WithMaskedAssignments(context.Background(), []string{entry})
	// The child echoes its argv to stderr and fails: the worst case, where the
	// value reaches both the error text and the stderr the runner retains.
	_, err := OSRunner{}.Run(ctx, "sh", "-c", `echo "saw $1" >&2; exit 3`, "sh", entry)
	if err == nil {
		t.Fatal("expected the command to fail")
	}
	msg := err.Error()
	if strings.Contains(msg, secret) || strings.Contains(logs.String(), secret) {
		t.Fatalf("secret rendered:\nerror: %s\nlogs: %s", msg, logs.String())
	}
	if !strings.Contains(msg, "OTEL_EXPORTER_OTLP_ENDPOINT="+Redacted) {
		t.Errorf("error should keep the key and mask the value: %s", msg)
	}
	if !strings.Contains(msg, "saw OTEL_EXPORTER_OTLP_ENDPOINT="+Redacted) {
		t.Errorf("stderr echo should be scrubbed, not dropped: %s", msg)
	}
}

func TestOSRunner_MaskedAssignments_ChildGetsRealValue(t *testing.T) {
	entry := "K=real-value-123"
	ctx := WithMaskedAssignments(context.Background(), []string{entry})
	out, err := OSRunner{}.Run(ctx, "sh", "-c", `printf %s "$1"`, "sh", entry)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != entry {
		t.Errorf("child saw %q, want the unmasked %q", out, entry)
	}
}

func TestOSRunner_NoMask_ArgvUnchangedInError(t *testing.T) {
	_, err := OSRunner{}.Run(context.Background(), "sh", "-c", "exit 1", "sh", "K=plain")
	if err == nil || !strings.Contains(err.Error(), "K=plain") {
		t.Errorf("unmasked argv should render as before, got %v", err)
	}
}

// TestMaskText_ShortValueEchoedBare pins a CodeRabbit finding on #531: a
// marked value under minScrubLen echoed on its own used to survive.
func TestMaskText_ShortValueEchoedBare(t *testing.T) {
	m := maskFrom(WithMaskedAssignments(context.Background(), []string{"K=hunter2"}))
	got := m.text("tmux: bad value hunter2 here")
	if strings.Contains(got, "hunter2") {
		t.Errorf("short value survived: %q", got)
	}
}

// TestMaskText_ShortValueOnlyAsWholeWord keeps the short-value rule from
// mangling unrelated text: "1" is redacted where it stands alone, not inside
// "10" or "v1.2".
func TestMaskText_ShortValueOnlyAsWholeWord(t *testing.T) {
	m := maskFrom(WithMaskedAssignments(context.Background(), []string{"T=1"}))
	got := m.text("exit 1 after 10 tries on v1.2")
	want := "exit " + Redacted + " after 10 tries on v1.2"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestMaskText_LongerValueBeforeItsPrefix pins the other #531 finding: when
// one marked value is a prefix of another, replacing the shorter one first
// left the longer one's tail — here, the token — in the text.
func TestMaskText_LongerValueBeforeItsPrefix(t *testing.T) {
	entries := []string{"BASE=https://ingest.example", "ENDPOINT=https://ingest.example/v1/tok-secret-123"}
	for i := 0; i < 20; i++ { // map iteration order used to decide this; run it enough to catch that
		m := maskFrom(WithMaskedAssignments(context.Background(), entries))
		got := m.text("posting to https://ingest.example/v1/tok-secret-123 failed")
		if strings.Contains(got, "tok-secret-123") {
			t.Fatalf("token tail survived: %q", got)
		}
	}
}

func TestMaskText_AdjacentShortValuesStayGlued(t *testing.T) {
	m := maskFrom(WithMaskedAssignments(context.Background(), []string{"T=1"}))
	if got := m.text("pane 11 and 1"); got != "pane 11 and "+Redacted {
		t.Errorf("got %q", got)
	}
}

// TestMaskText_ShortEntryOnlyAsWholeWord: a short KEY=VALUE entry must not
// rewrite a longer token that merely contains it ("X=a" inside "MAX=abc").
func TestMaskText_ShortEntryOnlyAsWholeWord(t *testing.T) {
	m := maskFrom(WithMaskedAssignments(context.Background(), []string{"X=a"}))
	if got := m.text("MAX=abc and X=a"); got != "MAX=abc and X="+Redacted {
		t.Errorf("got %q", got)
	}
}

// TestMaskText_OverlappingValuesLeaveNoFragment pins #661's example: the two
// values share "E1", so masking one value at a time consumed the shared bytes
// and left the other's unshared part ("SECRETON") in the clear.
//
// Mutation: restore the one-value-per-pass text (ReplaceAll per value,
// longest then lexical) and "SECRETON" survives.
func TestMaskText_OverlappingValuesLeaveNoFragment(t *testing.T) {
	m := maskFrom(WithMaskedAssignments(context.Background(), []string{"A=SECRETONE1", "B=E1TWO22XYZ"}))
	got := m.text("got SECRETONE1TWO22XYZ back")
	if want := "got " + Redacted + " back"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestMaskText_OverlappingSelfMatchLeavesNoFragment: one value overlapping
// its own next occurrence. A non-overlapping ReplaceAll takes the first and
// leaves the second's tail.
//
// Mutation: restart each search at the end of the last match instead of one
// byte past its start (from = end in cover's long-pattern loop) and the tail
// "AAB" of the second occurrence survives.
func TestMaskText_OverlappingSelfMatchLeavesNoFragment(t *testing.T) {
	m := maskFrom(WithMaskedAssignments(context.Background(), []string{"K=AABAABAAB"}))
	got := m.text("AABAABAABAAB")
	if got != Redacted {
		t.Errorf("got %q, want %q", got, Redacted)
	}
}

// TestMaskText_EntryKeepsItsKeyWhenAValueOverlapsItsEnd: a run that starts
// with a whole entry still renders as KEY=[redacted] when another value
// extends it.
func TestMaskText_EntryKeepsItsKeyWhenAValueOverlapsItsEnd(t *testing.T) {
	m := maskFrom(WithMaskedAssignments(context.Background(), []string{"KEY=SECRETONE1", "B=E1TWO22XYZ"}))
	got := m.text("saw KEY=SECRETONE1TWO22XYZ.")
	if want := "saw KEY=" + Redacted + "."; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestMaskText_OverlapDifferential builds seeded random text in which every
// byte from the values' alphabet ("A", "B") lies inside at least one whole
// occurrence of a marked value — values are laid down whole, often
// overlapping the previous one's tail, between noise that never uses that
// alphabet. So after masking, no "A" or "B" may remain: any that does is a
// fragment of a value whose full occurrence was in the text, which is
// stronger than asking that no 3-byte window of a value survives.
//
// Mutation: restore the one-value-per-pass text and this fails within the
// first few hundred cases (overlapping values leave their unshared part).
func TestMaskText_OverlapDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(661)) //nolint:gosec // G404: deterministic test fixture, not crypto
	randFrom := func(alphabet string, n int) string {
		var b strings.Builder
		for range n {
			b.WriteByte(alphabet[rng.Intn(len(alphabet))])
		}
		return b.String()
	}
	for c := range 3000 {
		values := make([]string, 1+rng.Intn(4))
		entries := make([]string, len(values))
		for i := range values {
			values[i] = randFrom("AB", minScrubLen+rng.Intn(5))
			entries[i] = "k" + strconv.Itoa(i) + "=" + values[i]
		}
		var text string
		for range 1 + rng.Intn(8) {
			switch rng.Intn(4) {
			case 0:
				text += randFrom("xy -=", 1+rng.Intn(4))
			case 1:
				i := rng.Intn(len(entries))
				text += entries[i]
			default:
				// Lay the value down over the longest suffix of text that is
				// a prefix of it, or a shorter one at random.
				v := values[rng.Intn(len(values))]
				overlaps := []int{0}
				for k := 1; k < len(v) && k <= len(text); k++ {
					if strings.HasSuffix(text, v[:k]) {
						overlaps = append(overlaps, k)
					}
				}
				k := overlaps[rng.Intn(len(overlaps))]
				text += v[k:]
			}
		}
		m := maskFrom(WithMaskedAssignments(context.Background(), entries))
		if got := m.text(text); strings.ContainsAny(got, "AB") {
			t.Fatalf("case %d: values %q\ntext %q\ngot  %q: a value fragment survived", c, values, text, got)
		}
	}
}

// TestMaskText_ShortValueNextToAMaskedRunIsScrubbed: a short value glued to
// a longer masked value is glued to text that becomes [redacted], not to a
// word, so it is scrubbed too, on either side. The #686 review found the
// value-before-run side leaking ("zz" stayed visible in front of the run).
//
// Mutation: make cover's isWord ignore coverage (return isWordByte(s[i])) and
// both cases leave "zz" visible.
func TestMaskText_ShortValueNextToAMaskedRunIsScrubbed(t *testing.T) {
	m := maskFrom(WithMaskedAssignments(context.Background(), []string{"L=LONGSECRET", "S=zz"}))
	for _, in := range []string{"LONGSECRETzz", "zzLONGSECRET", "zzLONGSECRETzz"} {
		if got := m.text(in); got != Redacted {
			t.Errorf("text(%q) = %q, want %q", in, got, Redacted)
		}
	}
}

// TestMaskText_ShortValueOverlappingTheStartOfARunIsScrubbed: a short value
// whose end overlaps the start of a longer masked value. Main's sequential
// replace consumed the shared byte with the long value first, so the short
// one no longer matched and its first byte stayed visible.
//
// Mutation: skip short patterns in cover (collect none into short) and "a"
// stays visible.
func TestMaskText_ShortValueOverlappingTheStartOfARunIsScrubbed(t *testing.T) {
	m := maskFrom(WithMaskedAssignments(context.Background(), []string{"S=ab", "L=bcdefghij"}))
	if got := m.text("abcdefghij"); got != Redacted {
		t.Errorf("got %q, want %q", got, Redacted)
	}
}

// TestMaskText_ShortValuesQualifyEachOtherToAFixpoint: "-ab" qualifies
// because the byte after it is covered by the long value. "!-a" ends right
// before its "b", so it qualifies only once "-ab" is covered, but it sorts
// first ("!" < "-") and is checked before "-ab" in any one pass. Only the
// re-check of "-ab"'s newly covered bytes reaches it.
//
// Mutation: drop the drain() call after a short mark in cover (so no match is
// re-checked once its neighbour is covered) and "!" stays visible.
func TestMaskText_ShortValuesQualifyEachOtherToAFixpoint(t *testing.T) {
	m := maskFrom(WithMaskedAssignments(context.Background(), []string{"L=LONGSECRET", "A=-ab", "B=!-a"}))
	if got := m.text("!-abLONGSECRET"); got != Redacted {
		t.Errorf("got %q, want %q", got, Redacted)
	}
}

// TestMaskText_LongValueWithNoMatchIsNearLinear: a 4 KiB value that almost
// matches everywhere ("aaa…ab") against 8 MiB of "a". Comparing it at every
// byte is 32 G byte-comparisons; one strings.Index scan is linear.
//
// Mutation: replace the long-pattern Index scan with a HasPrefix test at every
// byte and this takes about 2.5 s (0.04 s as written, 0.19 s under -race).
func TestMaskText_LongValueWithNoMatchIsNearLinear(t *testing.T) {
	m := maskFrom(WithMaskedAssignments(context.Background(), []string{"K=" + strings.Repeat("a", 4095) + "b"}))
	s := strings.Repeat("a", 8<<20)
	start := time.Now()
	if got := m.text(s); got != s {
		t.Fatal("text changed a stream with no match")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("text took %v on 8 MiB with no match; want well under a second", elapsed)
	}
}

// TestOSRunner_MaskedAssignments_FailureStdoutMaskedInOutput pins #664: a
// failing command's stdout rides CommandError.Output, which any caller can
// reach through errors.As, so a marked value echoed there must be masked like
// the stderr tail. Every capturing Runner method shares runAndWrap, the only
// constructor that sets Output, so each is checked.
//
// Mutation: drop the mask.text call on the failure-path stdout in runAndWrap
// and every case carries the value in Output.
func TestOSRunner_MaskedAssignments_FailureStdoutMaskedInOutput(t *testing.T) {
	const value = "stdout-secret-664" //nolint:gosec // G101: a fake value the mask must hide
	entry := "K=" + value
	ctx := WithMaskedAssignments(context.Background(), []string{entry})
	script := []string{"-c", `echo "saw $1"; echo "bare $2"; exit 5`, "sh", entry, value}
	runs := map[string]func() (string, error){
		"Run": func() (string, error) { return OSRunner{}.Run(ctx, "sh", script...) },
		"RunWithInput": func() (string, error) {
			return OSRunner{}.RunWithInput(ctx, "", "sh", script...)
		},
		"RunWithEnv": func() (string, error) {
			return OSRunner{}.RunWithEnv(ctx, map[string]string{"X": "1"}, "sh", script...)
		},
		"RunWithEnvFiltered": func() (string, error) {
			return OSRunner{}.RunWithEnvFiltered(ctx, nil, []string{"X"}, "sh", script...)
		},
	}
	for name, run := range runs {
		_, err := run()
		var cmdErr *CommandError
		if !errors.As(err, &cmdErr) {
			t.Fatalf("%s: error = %T (%v), want *CommandError", name, err, err)
		}
		if want := "saw K=" + Redacted + "\nbare " + Redacted; cmdErr.Output != want {
			t.Errorf("%s: Output = %q, want %q", name, cmdErr.Output, want)
		}
	}
}

// mainMaskedBytes reproduces the masking argMask.text did before #661, one
// pattern at a time over the running result (whole entries, then bare
// values, each longest first then lexical; strings.ReplaceAll from
// minScrubLen, else the whole-word replace), while tracking which byte of s
// each output byte came from. It returns which bytes of s that algorithm hid.
// The differential below holds the new cover to never showing one of them.
func mainMaskedBytes(entries []string, s string) []bool {
	type cell struct {
		b    byte
		orig int // index into s, or -1 for a byte of replacement text
	}
	cells := make([]cell, len(s))
	for i := range len(s) { // bytes, not runes
		cells[i] = cell{s[i], i}
	}
	bytesOf := func(cs []cell) string {
		b := make([]byte, len(cs))
		for i, c := range cs {
			b[i] = c.b
		}
		return string(b)
	}
	replace := func(v, with string, wholeWord bool) {
		cur := bytesOf(cells)
		var out []cell
		last := 0
		for from := 0; ; {
			rel := strings.Index(cur[from:], v)
			if rel < 0 {
				break
			}
			i := from + rel
			end := i + len(v)
			ok := true
			if wholeWord {
				gluedBefore := i > 0 && isWordByte(v[0]) && isWordByte(cur[i-1])
				gluedAfter := end < len(cur) && isWordByte(v[len(v)-1]) && isWordByte(cur[end])
				ok = !gluedBefore && !gluedAfter
			}
			if ok {
				out = append(out, cells[last:i]...)
				for j := range len(with) {
					out = append(out, cell{with[j], -1})
				}
				last = end
			}
			from = end
		}
		cells = append(out, cells[last:]...)
	}
	var es, vs []string
	shown := map[string]string{}
	for _, e := range entries {
		key, v, _ := strings.Cut(e, "=")
		es, vs = append(es, e), append(vs, v)
		shown[e] = key + "=" + Redacted
	}
	longestFirst(es)
	longestFirst(vs)
	for _, e := range es {
		replace(e, shown[e], len(e) < minScrubLen)
	}
	for _, v := range vs {
		replace(v, Redacted, len(v) < minScrubLen)
	}
	masked := make([]bool, len(s))
	for i := range masked {
		masked[i] = true
	}
	for _, c := range cells {
		if c.orig >= 0 {
			masked[c.orig] = false
		}
	}
	return masked
}

// TestMaskText_DifferentialAgainstMain runs seeded random layouts over three
// alphabets (plain letters, a two-byte rune, and "_" so word edges vary) with
// short and long values, entries, and overlap, and holds cover to two rules:
// every byte main's one-pattern-at-a-time algorithm hid stays hidden (no
// regression), and every byte of every occurrence of a value of minScrubLen
// or more is hidden (no fragment of a fully present value).
//
// Mutation: make cover's isWord ignore coverage and the first rule fails on
// the #686 shape (a short value right before a long one); drop the
// long-pattern loop's from = i + 1 in favor of from = end and the second rule
// fails on self-overlap.
func TestMaskText_DifferentialAgainstMain(t *testing.T) {
	rng := rand.New(rand.NewSource(686)) //nolint:gosec // G404: deterministic test fixture, not crypto
	for _, alphabet := range [][]string{{"A", "B"}, {"A", "é"}, {"A", "_", "B"}} {
		randFrom := func(n int) string {
			var b strings.Builder
			for range n {
				b.WriteString(alphabet[rng.Intn(len(alphabet))])
			}
			return b.String()
		}
		for c := range 4000 {
			values := make([]string, 1+rng.Intn(4))
			entries := make([]string, len(values))
			for i := range values {
				values[i] = randFrom(1 + rng.Intn(11))
				entries[i] = "k" + strconv.Itoa(i) + "=" + values[i]
			}
			var text string
			for range 1 + rng.Intn(8) {
				switch rng.Intn(4) {
				case 0:
					text += []string{" ", "-", randFrom(1 + rng.Intn(3))}[rng.Intn(3)]
				case 1:
					text += entries[rng.Intn(len(entries))]
				default:
					v := values[rng.Intn(len(values))]
					overlaps := []int{0}
					for k := 1; k < len(v) && k <= len(text); k++ {
						if strings.HasSuffix(text, v[:k]) {
							overlaps = append(overlaps, k)
						}
					}
					text += v[overlaps[rng.Intn(len(overlaps))]:]
				}
			}
			m := maskFrom(WithMaskedAssignments(context.Background(), entries))
			covered := m.cover(text)
			for i, hid := range mainMaskedBytes(entries, text) {
				if hid && !covered.has(i) {
					t.Fatalf("alphabet %q case %d: entries %q\ntext %q\nmain hid byte %d, the new cover shows it: %q", alphabet, c, entries, text, i, m.text(text))
				}
			}
			for _, v := range values {
				if len(v) < minScrubLen {
					continue
				}
				for from := 0; ; {
					rel := strings.Index(text[from:], v)
					if rel < 0 {
						break
					}
					i := from + rel
					for j := i; j < i+len(v); j++ {
						if !covered.has(j) {
							t.Fatalf("alphabet %q case %d: entries %q\ntext %q\nbyte %d of value %q is visible: %q", alphabet, c, entries, text, j, v, m.text(text))
						}
					}
					from = i + 1
				}
			}
		}
	}
}

// TestMaskText_ShortCascadeIsLinear pins #708 item 1. With K=:ab:a, every
// ":ab:a" in ":ab:ab…:ab:a" is glued to the "b" after it except the last, and
// covering each one unglues the one before it: a cascade of 21000 links right
// to left. The rescan-until-stable loop needed one full pass per link (26 s
// for a 64 KiB tail on the issue's machine, 3.9 s here at 24 KiB); the
// worklist re-checks only the matches beside newly covered bytes. The whole
// stream must still end up covered.
//
// Mutation: make drain rescan every short pattern over the whole stream until
// nothing changes (the old fixpoint) and this takes tens of seconds.
func TestMaskText_ShortCascadeIsLinear(t *testing.T) {
	m := maskFrom(WithMaskedAssignments(context.Background(), []string{"K=:ab:a"}))
	s := strings.Repeat(":ab", 21000) + ":a"
	start := time.Now()
	if got := m.text(s); got != Redacted {
		t.Fatalf("the cascade did not cover the stream: %.40q…", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("text took %v on a %d-byte cascade; want milliseconds", elapsed, len(s))
	}
}

// TestMaskText_SelfOverlappingLongValueIsLinear pins #708 item 2: a 16 KiB
// "a…a" value against 16 MiB of "a" matches at every byte, and verifying
// each match in full is 2.7e11 byte comparisons (about 10 s). eachMatch
// carries the KMP state across overlapping matches instead (about 0.3 s).
//
// Mutation: in eachMatch, drop the KMP carry (q starts at 0) and resume the
// Index scan at i+1, as the old loop did, and this takes several seconds.
func TestMaskText_SelfOverlappingLongValueIsLinear(t *testing.T) {
	m := maskFrom(WithMaskedAssignments(context.Background(), []string{"K=" + strings.Repeat("a", 16<<10)}))
	s := strings.Repeat("a", 16<<20)
	start := time.Now()
	if got := m.text(s); got != Redacted {
		t.Fatalf("text left part of the stream: %.40q…", got)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("text took %v on 16 MiB matching at every byte; want well under a second", elapsed)
	}
}

// TestEachMatch_FindsEveryOverlappingOccurrence checks eachMatch against a
// byte-by-byte scan over small random strings, where borders and overlaps are
// dense.
//
// Mutation: reset q to 0 instead of p's border after a match found inside the
// KMP loop, and a run of overlapping matches loses all but its first two.
func TestEachMatch_FindsEveryOverlappingOccurrence(t *testing.T) {
	rng := rand.New(rand.NewSource(708)) //nolint:gosec // G404: deterministic test fixture, not crypto
	randStr := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = "ab"[rng.Intn(2)]
		}
		return string(b)
	}
	for iter := 0; iter < 20000; iter++ {
		p := randStr(1 + rng.Intn(6))
		s := randStr(rng.Intn(40))
		var want, got []int
		for i := 0; i+len(p) <= len(s); i++ {
			if s[i:i+len(p)] == p {
				want = append(want, i)
			}
		}
		eachMatch(s, p, func(i int) { got = append(got, i) })
		if !slices.Equal(got, want) {
			t.Fatalf("eachMatch(%q, %q) = %v, want %v", s, p, got, want)
		}
	}
}

// Every cut position through a glued entry, including the one that lands
// exactly after '=', must keep the short value out of the tail.
func TestMaskedTail_NoCutPositionExposesTheGluedValue(t *testing.T) {
	m := maskFrom(WithMaskedAssignments(context.Background(), []string{"LONGKEYNAME=ab"}))
	const stream = "xxxxxxxxxxxxxxxxxxxxxxxx LONGKEYNAME=abcd tail"
	for limit := 1; limit <= len(stream); limit++ {
		tb := &tailBuffer{limit: limit}
		_, _ = tb.Write([]byte(stream))
		if got, _ := maskedTail(tb, m); strings.Contains(got, "ab") {
			t.Errorf("limit %d: the value survived the cut: %q", limit, got)
		}
	}
}

// TestMaskedTail_CutInsideAnEntryKeyHidesTheGluedValue pins #708 item 4.
// LONGKEYNAME=ab is long enough to be masked anywhere, but a cut inside its
// key leaves "EYNAME=ab" followed by "cd": the value is whole, yet glued to a
// word byte, so the whole-word rule for a short value does not fire and "ab"
// used to show. straddleLen now drops an entry suffix holding the whole value.
//
// Mutation: delete the entries loop from straddleLen and the tail keeps
// "EYNAME=abcd".
func TestMaskedTail_CutInsideAnEntryKeyHidesTheGluedValue(t *testing.T) {
	m := maskFrom(WithMaskedAssignments(context.Background(), []string{"LONGKEYNAME=ab"}))
	tb := &tailBuffer{limit: 16}
	_, _ = tb.Write([]byte("xxxxxxxxxxxxxxxxxxxxxxxx LONGKEYNAME=abcd tail"))
	got, dropped := maskedTail(tb, m)
	if strings.Contains(got, "ab") {
		t.Errorf("the value survived the cut: %q", got)
	}
	if got != "cd tail" || dropped != int64(len("xxxxxxxxxxxxxxxxxxxxxxxx LONGKEYNAME=ab")) {
		t.Errorf("got %q, dropped %d", got, dropped)
	}
}

// straddleLenReference is straddleLen as it stood before #749: every suffix
// length probed with HasPrefix, and no round cap.
func straddleLenReference(m argMask, s string) int {
	total := 0
	for {
		n := 0
		for _, v := range m.values {
			for l := min(len(v)-1, len(s)); l > n; l-- {
				if strings.HasPrefix(s, v[len(v)-l:]) {
					n = l
					break
				}
			}
		}
		for _, e := range m.entries {
			_, v, _ := strings.Cut(e, "=")
			for l := min(len(e)-1, len(s)); l > max(n, len(v)-1); l-- {
				if strings.HasPrefix(s, e[len(e)-l:]) {
					n = l
					break
				}
			}
		}
		for n < len(s) && !utf8.RuneStart(s[n]) {
			n++
		}
		if n == 0 {
			return total
		}
		s = s[n:]
		total += n
	}
}

// TestStraddleLen_MatchesTheSuffixProbe checks the KMP straddleLen against the
// probe-every-suffix loop it replaced (#749), over small random masks and
// tails where suffix/prefix overlaps are dense. Tails are short enough that
// the round cap never engages.
//
// Mutation: in suffixPrefix, drop the "q == len(p)" reset (a full match of p
// mid-v then indexes past p) or return q only when it equals len(p), and
// this finds a disagreement.
func TestStraddleLen_MatchesTheSuffixProbe(t *testing.T) {
	rng := rand.New(rand.NewSource(749)) //nolint:gosec // G404: deterministic test fixture, not crypto
	randStr := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = "ab="[rng.Intn(3)]
		}
		return string(b)
	}
	for iter := 0; iter < 20000; iter++ {
		var entries []string
		for k := 0; k < 1+rng.Intn(3); k++ {
			entries = append(entries, "K"+randStr(rng.Intn(3))+"="+randStr(1+rng.Intn(6)))
		}
		m := maskFrom(WithMaskedAssignments(context.Background(), entries))
		s := randStr(rng.Intn(maxStraddleRounds / 2))
		if got, want := m.straddleLen(s), straddleLenReference(m, s); got != want {
			t.Fatalf("straddleLen(%q) with %q = %d, want %d", s, entries, got, want)
		}
	}
}

// TestStraddleLen_OneByteRoundsAreBounded pins #749 item 2. With A=cb masked,
// a tail of "b" drops one byte per round, and every round also probes a
// 128 KiB value (Linux's cap on one argv element) that almost prefixes the
// tail ("b…bc"). Probing each suffix length with HasPrefix made a round
// quadratic in that value's length, times one round per byte of the 64 KiB
// tail: 24.8 s for a 4 KiB value before the fix.
//
// Mutation: delete the maxStraddleRounds cap and this takes about 90 s; keep
// the cap but restore the HasPrefix probe loop in place of suffixPrefix and
// it takes about 20 s (0.2 s as written).
func TestStraddleLen_OneByteRoundsAreBounded(t *testing.T) {
	m := maskFrom(WithMaskedAssignments(context.Background(), []string{"A=cb", "K=" + strings.Repeat("b", 128<<10-2) + "c"}))
	tb := &tailBuffer{limit: maxStderrTail}
	_, _ = tb.Write([]byte("zz" + strings.Repeat("b", 2*maxStderrTail)))
	start := time.Now()
	got, dropped := maskedTail(tb, m)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("maskedTail took %v; want well under a second", elapsed)
	}
	// Past the cap the rest of the tail is dropped: every byte is gone and
	// counted.
	if got != "" || dropped != int64(2+2*maxStderrTail) {
		t.Errorf("got %.20q, dropped %d; want empty, %d", got, dropped, 2+2*maxStderrTail)
	}
}
