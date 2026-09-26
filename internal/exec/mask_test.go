package exec

import (
	"bytes"
	"context"
	"log/slog"
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
