// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package ask

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func TestSafeURL(t *testing.T) {
	cases := map[string]bool{
		"https://example.com/a?b=c":   true,
		"http://example.com":          true,
		"javascript:alert(1)":         false,
		"JAVASCRIPT:alert(1)":         false,
		"data:text/html,<script>":     false,
		"vbscript:x":                  false,
		"//example.com/x":             false,
		"https://":                    false,
		"https://user:pw@example.com": false,
		"https://exa mple.com":        false,
		"https://example.com/\x1b[2J": false,
		"https://example.com/‮":       false,
		"":                            false,
		"file:///etc/passwd":          false,
	}
	for in, want := range cases {
		if got := SafeURL(in); got != want {
			t.Errorf("SafeURL(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNormalize(t *testing.T) {
	ok := Ask{Title: " Rotate the key ", Bucket: BucketNow, Steps: "1. a\r\n2. b\n\n"}
	got, err := Normalize(ok)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Rotate the key" || got.Steps != "1. a\n2. b" || len(got.Allow) != len(Kinds) {
		t.Fatalf("normalized = %+v", got)
	}
	bad := []Ask{
		{Title: "", Bucket: BucketNow},
		{Title: "two\nlines", Bucket: BucketNow},
		{Title: "esc \x1b[31m", Bucket: BucketNow},
		{Title: "bidi ‮", Bucket: BucketNow},
		{Title: strings.Repeat("x", MaxTitle+1), Bucket: BucketNow},
		{Title: "t", Bucket: "later"},
		{Title: "t", Bucket: BucketNow, Steps: "bell \a"},
		{Title: "t", Bucket: BucketNow, Steps: strings.Repeat("x", MaxSteps+1)},
		{Title: "t", Bucket: BucketNow, Links: []Link{{URL: "javascript:alert(1)"}}},
		{Title: "t", Bucket: BucketNow, Links: []Link{{URL: "https://x.example", Label: "a\nb"}}},
		{Title: "t", Bucket: BucketNow, Notify: "t1; rm -rf /"},
		{Title: "t", Bucket: BucketNow, Notify: "-x"},
		{Title: "t", Bucket: BucketNow, Allow: []Kind{"approve"}},
		{Title: "t", Bucket: BucketNow, From: "a\x00b"},
		{Title: string([]byte{0xff, 0xfe}), Bucket: BucketNow},
	}
	for i, a := range bad {
		if _, err := Normalize(a); !errors.Is(err, ErrInvalid) {
			t.Errorf("case %d (%q): err = %v, want ErrInvalid", i, a.Title, err)
		}
	}
}

func openItem(allow ...Kind) Item {
	if len(allow) == 0 {
		allow = Kinds
	}
	return Item{Ask: Ask{ID: "ask-00000001", Allow: allow}, State: StateOpen}
}

func TestCheckResponse(t *testing.T) {
	future := t0.Add(2 * time.Hour)
	tooFar := t0.Add(MaxSnooze + time.Hour)
	past := t0.Add(-time.Minute)
	cases := []struct {
		name string
		it   Item
		r    Response
		want error
	}{
		{"done", openItem(), Response{Kind: KindDone}, nil},
		{"reply", openItem(), Response{Kind: KindReply, Text: " ok "}, nil},
		{"empty reply", openItem(), Response{Kind: KindReply, Text: "  "}, ErrInvalid},
		{"link", openItem(), Response{Kind: KindLink, URL: "https://example.com"}, nil},
		{"js link", openItem(), Response{Kind: KindLink, URL: "javascript:alert(1)"}, ErrInvalid},
		{"snooze", openItem(), Response{Kind: KindSnooze, Until: &future}, nil},
		{"snooze past", openItem(), Response{Kind: KindSnooze, Until: &past}, ErrInvalid},
		{"snooze far", openItem(), Response{Kind: KindSnooze, Until: &tooFar}, ErrInvalid},
		{"snooze none", openItem(), Response{Kind: KindSnooze}, ErrInvalid},
		{"not allowed", openItem(KindDone), Response{Kind: KindReply, Text: "x"}, ErrNotAllowed},
		{"answered", Item{Ask: Ask{ID: "ask-00000001", Allow: Kinds}, State: StateAnswered}, Response{Kind: KindDone}, ErrClosed},
		{"unknown kind", openItem(Kind("approve")), Response{Kind: "approve"}, ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CheckResponse(tc.it, tc.r, t0)
			if tc.want == nil && err != nil {
				t.Fatalf("err = %v", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.name == "reply" && got.Text != "ok" {
				t.Fatalf("reply not trimmed: %q", got.Text)
			}
		})
	}
}

func TestFold(t *testing.T) {
	a1 := Ask{ID: "ask-00000001", Title: "one", Bucket: BucketNow, Allow: Kinds}
	a2 := Ask{ID: "ask-00000002", Title: "two", Bucket: BucketSoon, Allow: Kinds}
	a3 := Ask{ID: "ask-00000003", Title: "three", Bucket: BucketComing, Allow: Kinds}
	later := t0.Add(time.Hour)
	expired := t0.Add(-time.Hour)
	recs := []Record{
		{Ask: &a1}, {Ask: &a2}, {Ask: &a3},
		{Response: &Response{ID: "resp-1", Ask: a1.ID, Kind: KindSnooze, Until: &later}},
		{Response: &Response{ID: "resp-2", Ask: a2.ID, Kind: KindReply, Text: "yes", At: t0}},
		// A response after the answer is ignored.
		{Response: &Response{ID: "resp-3", Ask: a2.ID, Kind: KindDone}},
		{Response: &Response{ID: "resp-4", Ask: a3.ID, Kind: KindSnooze, Until: &expired}},
		// A response to an unknown ask is ignored.
		{Response: &Response{ID: "resp-5", Ask: "ask-99999999", Kind: KindDone}},
		{Notified: &Notified{Response: "resp-2", OK: true, Address: "t1"}},
		// A duplicate ask id keeps the first.
		{Ask: &Ask{ID: a1.ID, Title: "dup", Bucket: BucketNow}},
	}
	items := Fold(recs, t0)
	if len(items) != 3 {
		t.Fatalf("items = %d", len(items))
	}
	if items[0].State != StateSnoozed || items[0].Ask.Title != "one" {
		t.Errorf("a1 = %+v", items[0])
	}
	if items[1].State != StateAnswered || items[1].Answer.Kind != KindReply || len(items[1].Responses) != 1 || len(items[1].Notified) != 1 {
		t.Errorf("a2 = %+v", items[1])
	}
	if items[2].State != StateOpen || items[2].SnoozedUntil != nil {
		t.Errorf("a3 expired snooze should be open: %+v", items[2])
	}
	open, snoozed, answered := Grouped(items)
	if len(open[BucketComing]) != 1 || len(snoozed) != 1 || len(answered) != 1 {
		t.Errorf("grouped = %v %v %v", open, snoozed, answered)
	}
}

func TestNotifyTextIsOneLine(t *testing.T) {
	a := Ask{ID: "ask-00000001", Title: "Rotate"}
	r := Response{Kind: KindReply, Text: "line one\nline two\r\nthree"}
	got := NotifyText(a, r)
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("notify text has a line break: %q", got)
	}
	if !strings.HasPrefix(got, "DESK-RESPONSE ask-00000001 reply: line one line two three") {
		t.Fatalf("notify text = %q", got)
	}
}

func TestParseKindsAndBucket(t *testing.T) {
	ks, err := ParseKinds("done, reply,done")
	if err != nil || len(ks) != 2 {
		t.Fatalf("ParseKinds = %v %v", ks, err)
	}
	if _, err := ParseKinds("approve"); err == nil {
		t.Fatal("ParseKinds accepted approve")
	}
	if b, err := ParseBucket("NOW"); err != nil || b != BucketNow {
		t.Fatalf("ParseBucket = %v %v", b, err)
	}
	if _, err := ParseBucket("urgent"); err == nil {
		t.Fatal("ParseBucket accepted urgent")
	}
}

// TestNotifyTextIsShellInert pins the notify line's defense: typed at a shell
// prompt, it must hold no character that quotes, substitutes, separates,
// redirects, globs or comments, whatever the agent's title and the
// operator's reply contain.
func TestNotifyTextIsShellInert(t *testing.T) {
	hostile := "Run `make deploy` $(rm -rf ~) ; echo hi & x | y > /tmp/z < a 'q' \"d\" {a,b} [c] *?! #c ~u ^h %j a=b \\ \x1b[2J \u202e"
	until := t0.Add(time.Hour)
	for _, r := range []Response{
		{Kind: KindDone, Via: "web"},
		{Kind: KindReply, Text: "ok, go ahead;" + hostile, Via: "cli"},
		{Kind: KindLink, URL: "https://x.example/a;b?c=$(id)&d=`id`#frag", Via: "ui"},
		{Kind: KindSnooze, Until: &until, Via: "web$(id)"},
	} {
		got := NotifyText(Ask{ID: "ask-00000001", Title: hostile}, r)
		if i := strings.IndexAny(got, shellMeta); i >= 0 {
			t.Errorf("%s: shell metacharacter %q at %d in %q", r.Kind, got[i], i, got)
		}
		for _, c := range got {
			if c < 0x20 || c == 0x7f || c == '\u202e' {
				t.Errorf("%s: control or format character %U in %q", r.Kind, c, got)
			}
		}
		if !strings.HasPrefix(got, "DESK-RESPONSE ask-00000001 "+string(r.Kind)) {
			t.Errorf("prefix: %q", got)
		}
	}
}
