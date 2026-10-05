package desk

import (
	"reflect"
	"strings"
	"testing"
)

// Ported from run-and-watch's OutputHandlingTest.

func TestNormalizeStripsEscapesAndControls(t *testing.T) {
	for in, want := range map[string]string{
		"\x1b[31mFAIL\x1b[0m x\r":               "FAIL x",
		"\x1b]0;title\x07a\bb\x04\x00c\tz":      "abc\tz",
		"zero\u200bwidth \u202eflip \ufeffbom":  "zerowidth flip bom",
		"c1 csi \u009b31mred and osc \u009dt\a": "c1 csi red and osc ",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactorAfterNormalizeCatchesAColorizedValue(t *testing.T) {
	var r Redactor
	r.Add("token", map[string]string{"tok": "s3cr3tvalue", "short": "abc"})
	if got := r.Apply(Normalize("x s3cr\x1b[31m3tval\x1b[0mue abc")); got != "x <redacted:OUT_token_tok> abc" {
		t.Errorf("got %q", got)
	}
}

func TestRedactorMatchesAValueWithControlCharacters(t *testing.T) {
	var r Redactor
	r.Add("token", map[string]string{"tok": "s3cr3tvalue\r", "esc": "\x1b[1mbold-secret\x1b[0m"})
	if got := r.Apply(Normalize("a s3cr3tvalue\r b bold-secret")); got != "a <redacted:OUT_token_tok> b <redacted:OUT_token_esc>" {
		t.Errorf("got %q", got)
	}
}

func TestRedactorReplacesTheLongerValueFirst(t *testing.T) {
	var r Redactor
	r.Add("a", map[string]string{"k": "secret"})
	r.Add("b", map[string]string{"k": "secret-longer"})
	if got := r.Apply("x secret-longer y"); got != "x <redacted:OUT_b_k> y" {
		t.Errorf("got %q", got)
	}
}

func TestCleanCatchesValuesSplitOrPrefixedByControls(t *testing.T) {
	var r Redactor
	r.Add("token", map[string]string{"tok": "S3cr3tvalue"})
	const mark = "<redacted:OUT_token_tok>"
	for _, printed := range []string{
		"S3cr\u009b31m3tvalue", "\x1bS3cr3tvalue", "S3cr\u200b3tvalue",
		"S3cr\ufeff3tvalue", "S3cr\u00853tvalue", "\x1b[S3cr3tvalue",
		"\u009bS3cr3tvalue", "S3cr\u20663tvalue", "S3cr\u180e3tvalue",
		"S3cr\u034f3tvalue", "S3cr\ufe0f3tvalue", "S3cr\U000e00413tvalue",
		"\x1bS3cr\x1b[31m3tvalue", "\x1b[1m\x1bS3cr\u009b0m3tvalue",
	} {
		got := Clean(&r, "a "+printed+" b")
		if !strings.Contains(got, mark) || strings.Contains(got, "3tvalue") {
			t.Errorf("Clean(%q) = %q, want the value redacted", printed, got)
		}
	}
	if got := Clean(&r, "\x1b[31mFAIL\x1b[0m x\r"); got != "FAIL x" {
		t.Errorf("plain line = %q", got)
	}
}

func TestParseOutputs(t *testing.T) {
	outs, keys, warns := ParseOutputs([]byte("img=a=b\n\n1bad=x\nnovalue\nimg2=\nimg=last\r\nnul=a\x00b\n"))
	if want := map[string]string{"img": "last", "img2": ""}; !reflect.DeepEqual(outs, want) {
		t.Errorf("outs = %q, want %q", outs, want)
	}
	if want := []string{"img", "img2"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("keys = %q, want %q", keys, want)
	}
	if len(warns) != 3 {
		t.Fatalf("warns = %q, want 3", warns)
	}
	joined := strings.Join(warns, " ")
	for _, leaked := range []string{"1bad", "novalue", "a\x00b"} {
		if strings.Contains(joined, leaked) {
			t.Errorf("a warning quotes the line's content %q: %q", leaked, joined)
		}
	}
}
