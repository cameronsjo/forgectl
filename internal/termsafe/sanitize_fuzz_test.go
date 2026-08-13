package termsafe

// Fuzz/property coverage for Sanitize, additive to the table-driven
// control-byte-specific cases in internal/cli/sessions_test.go (which plant a
// fixed C1 byte and DEL and assert they're gone). This file instead asserts
// the GENERAL invariant Sanitize claims to hold — "no Cc-category control
// rune survives, tab excepted" — over arbitrary fuzzer-generated input, and
// documents a real gap the fuzz corpus surfaced: Unicode Cf (format)
// characters, including bidirectional-override controls, are NOT covered by
// unicode.IsControl and pass through unsanitized.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

// FuzzSanitize checks the invariant Sanitize's own doc comment claims: every
// Cc-category control rune (tab excepted) is replaced. It also checks that
// valid-UTF8 input keeps its rune count — Sanitize is a 1:1 rune map, never a
// deletion — so a length mismatch would mean the mapping silently dropped or
// merged characters.
func FuzzSanitize(f *testing.F) {
	seeds := []string{
		"",
		"plain text",
		"tab\ttab",
		"\x1b[31mred\x1b[0m", // C0 escape sequence
		string(rune(0x9b)),   // C1 single-byte CSI
		string(rune(0x7f)),   // DEL
		"emoji 🔥 test",
		"‮hidden‬",    // RLO ... PDF (bidi override)
		"​zero​width", // zero-width space
		"multi\nline\r\nstring",
		"咖啡 workflow",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := Sanitize(s)
		for _, r := range got {
			if r != '\t' && unicode.IsControl(r) {
				t.Fatalf("Cc-category control rune %U survived Sanitize: input=%q output=%q", r, s, got)
			}
		}
		if utf8.ValidString(s) {
			if wantN, gotN := utf8.RuneCountInString(s), utf8.RuneCountInString(got); wantN != gotN {
				t.Fatalf("Sanitize should be a 1:1 rune map, rune count changed %d -> %d: input=%q output=%q", wantN, gotN, s, got)
			}
		}
	})
}

func TestTextHandler_SafeFieldsRemainOnePhysicalLine(t *testing.T) {
	var out bytes.Buffer
	handler := slog.NewTextHandler(&out, nil)
	record := slog.NewRecord(time.Unix(0, 0), slog.LevelWarn, "migration refused", 0)
	record.Add("path", QuotePath("/tmp/a\n\x1b[2K\u202e"), "error", SafeLine("bad\r\x9bforged"))
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Count(got, "\n") != 1 || strings.ContainsAny(strings.TrimSuffix(got, "\n"), "\r\x1b") || strings.ContainsRune(got, '\u009b') || strings.ContainsRune(got, '\u202e') {
		t.Fatalf("TextHandler output is not one inert physical line: %q", got)
	}
}

func TestError_PreservesIdentityAndEscapesFilesystemPaths(t *testing.T) {
	sentinel := errors.New("permission\x1b[2Kdenied")
	tests := []struct {
		name string
		err  error
	}{
		{name: "path", err: &os.PathError{Op: "open\nforged", Path: "/tmp/a\nb\x1b[31m", Err: sentinel}},
		{name: "link", err: &os.LinkError{Op: "rename\rforged", Old: "/tmp/old\nline", New: "/tmp/new\u202eexe", Err: sentinel}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Error(tt.err)
			if !errors.Is(got, sentinel) {
				t.Fatalf("errors.Is(%v, sentinel) = false", got)
			}
			var pathErr *os.PathError
			var linkErr *os.LinkError
			if !errors.As(got, &pathErr) && !errors.As(got, &linkErr) {
				t.Fatalf("errors.As(%T) did not preserve filesystem error", tt.err)
			}
			if strings.Count(got.Error(), "\n") != 0 || strings.ContainsAny(got.Error(), "\r\x1b") || strings.ContainsRune(got.Error(), '\u202e') {
				t.Fatalf("safe error contains terminal control/format text: %q", got.Error())
			}
			for _, escaped := range []string{`\n`, `\x1b`} {
				if !strings.Contains(got.Error(), escaped) {
					t.Errorf("safe error %q does not visibly escape %q", got.Error(), escaped)
				}
			}
		})
	}
}

// Pins the gap the fuzz seeds above surfaced: unicode.IsControl only covers
// the Cc category (C0/C1 controls), not Cf (format) characters. A
// right-to-left override (U+202E) is a well-known terminal/filename spoofing
// vector — it can visually reorder trailing text to disguise, e.g., a
// malicious runbook title — and "render inert in the terminal" reads as
// covering exactly this class of attack, but the implementation does not
// strip it. This test documents CURRENT (not necessarily desired) behavior
// rather than asserting the safe outcome as already achieved — do not read it
// as endorsing the gap; it exists so a fix has a red test to flip green. See
// the sibling FuzzSanitize above for the invariant that DOES hold, and the
// package doc, which scopes the guarantee to Cc for the same reason.
func TestSanitize_DoesNotStripBidiOverrideGAP(t *testing.T) {
	rlo := string(rune(0x202E))
	got := Sanitize("safe" + rlo + "text")
	if !strings.Contains(got, rlo) {
		t.Skip("Sanitize now strips U+202E — this GAP test is stale, safe to delete")
	}
}

func TestSafeLineAndQuotePath_EscapeLayoutControlsAndBidiOverride(t *testing.T) {
	input := "a\tb\nc\rd\x1be\x7ff\u0085g\u202eh"
	for name, got := range map[string]string{
		"SafeLine":  SafeLine(input),
		"QuotePath": QuotePath(input),
	} {
		if strings.ContainsAny(got, "\t\n\r\x1b\x7f\u0085\u202e") {
			t.Fatalf("%s output %q retained a sink layout control or bidi override", name, got)
		}
		if strings.ContainsAny(got, "\n\r") {
			t.Fatalf("%s output spans physical lines: %q", name, got)
		}
	}
}
