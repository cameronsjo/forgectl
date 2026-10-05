// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// The sources clean what they ingest. The dashboard still escapes every
// string it draws, because a Source is an interface: these fixtures hand it
// raw hostile text, so a draw path that skips escaping puts a control
// sequence on the screen and fails the assertion below.

// hostileDeskOptions is sources whose every string carries a CSI introducer, a
// right-to-left override and a carriage return, none of it cleaned.
func hostileDeskOptions() DeskOptions {
	h := termsafetest.Hostile
	spec := deskSpec()
	spec.Gate.Label = h("accepted?")
	spec.Log.Path = h("out/events.jsonl")
	spec.Exits[3] = h("audit rejected")
	spec.Checks[0].Label = h("audit ok?")

	three := 3
	evs := numberEvents([]runview.Event{
		{Time: deskNow.Add(-9 * time.Minute), Name: "worker_boot"},
		{Time: deskNow.Add(-8 * time.Minute), Name: "stage_started", Step: h("fetch"),
			Fields: []runview.Field{{Key: "stage", Value: h("fetch")}, {Key: h("try"), Value: h("1")}, {Key: "err", Value: h("boom")}}},
		{Time: deskNow.Add(-7 * time.Minute), Name: "stage_refused", Step: h("fetch"),
			Fields: []runview.Field{{Key: "stage", Value: h("fetch")}, {Key: "rc", Value: h("1")}}},
		{Time: deskNow.Add(-6 * time.Minute), Name: h("unmapped"), Step: h("other"),
			Fields: []runview.Field{{Key: h("k"), Value: h("v")}}},
		{Time: deskNow.Add(-5 * time.Minute), Name: "audit_result", Fields: []runview.Field{{Key: "rc", Value: "3"}}},
		{Time: deskNow.Add(-4 * time.Minute), Name: "run_finished", Fields: []runview.Field{{Key: "rc", Value: "3"}}},
	})
	logSrc := &fakeDeskSource{
		name: h("pipeline"),
		spec: spec,
		refs: []runview.RunRef{
			{Source: h("pipeline"), Name: h("alpha-run-7"), Kind: runview.KindLog, Updated: deskNow.Add(-4 * time.Minute)},
			{Source: h("pipeline"), Name: h("beta-run-7"), Kind: runview.KindLog, Updated: deskNow.Add(-3 * time.Hour)},
		},
		deltas: map[string][]runview.Delta{
			h("alpha-run-7"): {{
				Events: evs,
				Defs: []runview.StepDef{
					{ID: h("fetch"), Note: h("pull the inputs")},
					{ID: h("build"), Note: h("compile"), After: []string{h("fetch")}},
				},
				Metrics: []runview.Metric{
					{Seq: 1, Values: map[string]int64{h("in"): 5, h("out"): 2}, Error: true},
					{Seq: 2, Values: map[string]int64{h("in"): 9}},
				},
				Summary: []runview.KV{
					{Key: h("grp") + ".leaf", Value: h("value")},
					{Key: h("grp") + ".other", Value: h("value")},
					{Key: h("top"), Value: h("v")},
				},
				Timing: []runview.StepTiming{{Step: h("fetch"), State: h("skipped"), RC: &three, Dur: time.Second}},
				Err:    errors.New(h("boom")),
			}},
			h("beta-run-7"): {{Err: fmt.Errorf("open: %w: %s", errors.New("denied"), h("detail")), Dropped: 2}},
		},
		items: []runview.QueueItem{
			{Name: h("0007-a"), Kind: h("script"), State: "waiting", What: h("do a thing"), Why: h("because"), SHA256: "x"},
			{Name: h("0008-b"), Kind: h("batch"), State: h("odd"), What: h("another"), Refusal: h("hash differs"), Stale: true, TTY: true},
			{Name: h("0009-c"), Kind: "script", State: "skipped", What: h("skipped one"), SkipReason: h("not needed")},
		},
	}
	deskSrc := &fakeDeskSource{
		name: h("desk"),
		refs: []runview.RunRef{{Source: h("desk"), Name: h("batch-04"), Kind: runview.KindDesk, Updated: deskNow.Add(-2 * time.Hour)}},
		deltas: map[string][]runview.Delta{h("batch-04"): {{
			Events: numberEvents([]runview.Event{{Name: "STEP-START", Step: h("lint")}}),
			Defs:   []runview.StepDef{{ID: h("lint"), Note: h("check")}},
			Live:   runview.LiveLost,
		}}},
	}
	return DeskOptions{
		Sources: []runview.Source{logSrc, deskSrc},
		Failed:  []DeskSourceFailure{{Name: h("bad-spec"), Err: errors.New(h("cause"))}},
		Theme:   theme.Default(),
		// The Nerd Font brand mark is a private-use rune, which the shared
		// assertion rejects by design; the other tui tests use the same set.
		NoIcons: true,
	}
}

// hostileErrorOptions is sources that fail to list, with hostile causes.
func hostileErrorOptions() DeskOptions {
	h := termsafetest.Hostile
	root := &fakeDeskSource{name: h("rooted"), listErr: fmt.Errorf("%w: %s", runview.ErrRoot, h("$PIPELINE_ROOT is unset"))}
	other := &fakeDeskSource{name: h("other"), listErr: errors.New(h("cannot list"))}
	return DeskOptions{Sources: []runview.Source{root, other}, Theme: theme.Default(), NoIcons: true}
}

var hostileSizes = [][2]int{{40, 12}, {60, 30}, {120, 30}, {160, 50}}

// deskScreens renders every view in every layout, plus help and the filter
// prompt, calling check on each screen.
func deskScreens(t *testing.T, opts func() DeskOptions, check func(name, out string)) {
	t.Helper()
	for _, sz := range hostileSizes {
		m := openDesk(t, opts(), sz[0], sz[1])
		name := fmt.Sprintf("%dx%d", sz[0], sz[1])
		for _, v := range []string{"1", "2", "3", "4", "5", "6"} {
			m = pressDesk(t, m, key(v))
			check(name+" view "+v, m.View().Content)
			// The second run too, so its error and drop paths are drawn.
			m2 := pressDesk(t, m, key("]"))
			check(name+" view "+v+" run 2", m2.View().Content)
		}
		check(name+" help", pressDesk(t, m, key("?")).View().Content)
		f := pressDesk(t, m, key("/"))
		f = pressDesk(t, f, key("a"))
		check(name+" filter", f.View().Content)
	}
}

func TestDeskHostileTextNeverReachesTheTerminal(t *testing.T) {
	deskScreens(t, hostileDeskOptions, func(name, out string) {
		termsafetest.AssertInert(t, name, out)
	})
	deskScreens(t, hostileErrorOptions, func(name, out string) {
		termsafetest.AssertInert(t, name, out)
	})
}

// TestDeskHostileTextIsDrawnEscaped is the control for the test above: on a
// roomy screen each view really draws the hostile text (as visible escapes),
// so the inertness check ran against content that carried it.
func TestDeskHostileTextIsDrawnEscaped(t *testing.T) {
	const escaped = `\x1b[31m`
	m := openDesk(t, hostileDeskOptions(), 160, 50)
	for _, v := range []string{"1", "2", "3", "4", "5", "6"} {
		m = pressDesk(t, m, key(v))
		if out := m.View().Content; !strings.Contains(out, escaped) {
			t.Errorf("view %s never drew the escaped hostile text; the inertness test proves nothing for it\n%s", v, out)
		}
	}
	if out := pressDesk(t, m, key("?")).View().Content; !strings.Contains(out, escaped) {
		t.Errorf("help never drew the escaped hostile text\n%s", out)
	}
	e := openDesk(t, hostileErrorOptions(), 160, 50)
	if out := e.View().Content; !strings.Contains(out, escaped) {
		t.Errorf("the no-runs lines never drew the escaped hostile text\n%s", out)
	}
}

// failRecorder records whether AssertInert failed, so a test can check the
// assertion can go red without failing itself.
type failRecorder struct{ failed bool }

func (r *failRecorder) Helper()      {}
func (r *failRecorder) Fatal(...any) { r.failed = true }

// TestDeskInertnessCheckCanGoRed shows the assertion rejects the shapes the
// dashboard must never emit, and accepts a benign screen.
func TestDeskInertnessCheckCanGoRed(t *testing.T) {
	benign := openDesk(t, goldenDeskOptions(true), 120, 30).View().Content
	if !strings.Contains(benign, "\x1b[") {
		t.Fatal("the benign screen carries no styling at all; the SGR allowance is untested")
	}
	var ok failRecorder
	termsafetest.AssertInert(&ok, "benign", benign)
	if ok.failed {
		t.Fatal("a benign screen with only styling was rejected")
	}
	for name, bad := range map[string]string{
		"csi":   "x\x1b[2Jy",
		"osc":   "x\x1b]0;title\x07y",
		"cr":    "x\ry",
		"bidi":  "x" + string(rune(0x202e)) + "y",
		"c1":    "x\u009by",
		"nul":   "x\x00y",
		"bell":  "x\x07y",
		"plain": "x\x1by",
	} {
		var r failRecorder
		termsafetest.AssertInert(&r, name, benign+bad)
		if !r.failed {
			t.Errorf("AssertInert accepted %s", name)
		}
	}
}
