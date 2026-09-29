package exec

import (
	"bytes"
	"context"
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"testing"
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
// Mutation: advance past each whole match instead of byte by byte (i = pend-1
// after a match) and the tail "AAB" of the second occurrence survives.
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
// word, so it is scrubbed too — as it was when the longer value was replaced
// first.
//
// Mutation: drop the coverage test from gluedBefore (the i-1 >= end term) and
// "zz" survives after the [redacted].
func TestMaskText_ShortValueNextToAMaskedRunIsScrubbed(t *testing.T) {
	m := maskFrom(WithMaskedAssignments(context.Background(), []string{"L=LONGSECRET", "S=zz"}))
	if got := m.text("LONGSECRETzz"); got != Redacted+Redacted {
		t.Errorf("got %q", got)
	}
}
