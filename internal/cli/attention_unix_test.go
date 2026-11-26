// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/attention"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/desk/ask"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
)

const (
	attnSocket  = "/tmp/forgectl-attention-test.sock"
	attnSession = "5e55a1d0-aaaa-4bbb-8ccc-000000000001"
)

// attnEnv is one test's state directory and desk directory.
type attnEnv struct {
	state, desk string
}

// newAttnEnv points XDG_STATE_HOME and DESK_DIR at fresh directories and
// sets the caller's herdr socket. The desk directory is not created.
func newAttnEnv(t *testing.T) attnEnv {
	t.Helper()
	root := t.TempDir()
	e := attnEnv{state: filepath.Join(root, "state"), desk: filepath.Join(root, "desk")}
	t.Setenv("XDG_STATE_HOME", e.state)
	t.Setenv("DESK_DIR", e.desk)
	t.Setenv("HERDR_SOCKET_PATH", attnSocket)
	oldHc := attentionHcList
	// No test reaches a real hc: a dev machine's own hc would make results
	// depend on its live terminals.
	attentionHcList = func(context.Context) ([]byte, string, error) {
		return nil, "", errors.New("hc: test has no hc")
	}
	t.Cleanup(func() { attentionHcList = oldHc })
	return e
}

// herdrAgents is a fake runner answering `herdr agent list` with these
// (pane, session, worktree) rows.
func herdrAgents(rows ...[3]string) *exec.FakeRunner {
	var agents []map[string]any
	for _, r := range rows {
		a := map[string]any{
			"pane_id": r[0], "agent": "claude", "agent_status": "idle", "cwd": "/Users/x/Projects/widget",
			"agent_session": map[string]any{"agent": "claude", "kind": "id", "source": "herdr:claude", "value": r[1]},
		}
		if r[2] != "" {
			a["tokens"] = map[string]any{"cwd": "/Users/x/wt/" + r[2], "worktree": r[2]}
		}
		agents = append(agents, a)
	}
	body, _ := json.Marshal(map[string]any{"id": "cli:agent:list", "result": map[string]any{"agents": agents}})
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "herdr" && strings.Join(args, " ") == "agent list" {
			return string(body), nil
		}
		return "", fmt.Errorf("unexpected %s %v", name, args)
	}}
}

func herdrDown() *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return "", errors.New("herdr: not running") }}
}

func attnRun(t *testing.T, deps module.Deps, args ...string) (string, string, error) {
	t.Helper()
	if deps.Runner == nil {
		deps.Runner = herdrDown()
	}
	var out, errOut bytes.Buffer
	cmd := newAttentionCmd(deps)
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

func attnList(t *testing.T, deps module.Deps) attentionList {
	t.Helper()
	out, errOut, err := attnRun(t, deps, "list", "--json")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, errOut)
	}
	var l attentionList
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		t.Fatalf("list --json: %v\n%s", err, out)
	}
	return l
}

func TestAttentionSweepFile(t *testing.T) {
	e := newAttnEnv(t)
	sweep := filepath.Join(e.state, "cadence", "sweep.json")
	if err := os.MkdirAll(filepath.Dir(sweep), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sweep, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if l := attnList(t, module.Deps{}); l.Skipped != 1 || l.Sources.Sweep != sourceMalformed {
		t.Errorf("malformed sweep: %+v", l)
	}
	good := `{"schemaVersion":2,"generatedAt":"2026-10-16T12:00:00Z","workers":[{"name":"cos-w","blockedBy":"prompt"}],"prs":[{"repo":"acme/widget","number":3,"title":"Fix","state":"marker-clean"}]}`
	if err := os.WriteFile(sweep, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	l := attnList(t, module.Deps{})
	if l.Skipped != 0 || len(l.Rows) != 2 || l.Rows[0].Kind != attention.RowWorker || l.Rows[1].Kind != attention.RowPR {
		t.Errorf("sweep rows: %+v", l)
	}
}

// list --json carries each row's key and tier, says why herdr is
// unavailable, and dates the sweep.
func TestAttentionListKeysTiersAndSourceDetail(t *testing.T) {
	e := newAttnEnv(t)
	sweep := filepath.Join(e.state, "cadence", "sweep.json")
	if err := os.MkdirAll(filepath.Dir(sweep), 0o700); err != nil {
		t.Fatal(err)
	}
	good := `{"schemaVersion":2,"generatedAt":"2026-10-16T12:00:00Z","workers":[{"name":"cos-w","blockedBy":"prompt"}],"prs":[{"repo":"acme/widget","number":3,"title":"Fix","state":"marker-clean"}]}`
	if err := os.WriteFile(sweep, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	l := attnList(t, module.Deps{Runner: herdrDown()})
	if len(l.Rows) != 2 {
		t.Fatalf("rows = %+v", l.Rows)
	}
	if w := l.Rows[0]; !attention.ValidKey(w.Key) || w.Tier != attention.TierRaise {
		t.Errorf("worker row key/tier = %q/%q", w.Key, w.Tier)
	}
	if p := l.Rows[1]; !attention.ValidKey(p.Key) || p.Tier != attention.TierCount {
		t.Errorf("pr row key/tier = %q/%q", p.Key, p.Tier)
	}
	if l.Sources.Herdr != sourceUnavailable || !strings.Contains(l.Sources.HerdrDetail, "not running") {
		t.Errorf("herdr source = %q, detail %q", l.Sources.Herdr, l.Sources.HerdrDetail)
	}
	want := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	if l.Sources.SweepGeneratedAt == nil || !l.Sources.SweepGeneratedAt.Equal(want) {
		t.Errorf("sweepGeneratedAt = %v, want %v", l.Sources.SweepGeneratedAt, want)
	}

	// The text view leads each row with the short key.
	out, _, err := attnRun(t, module.Deps{}, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, l.Rows[0].Key[:attentionShortKey]+" ") {
		t.Errorf("text view does not lead with the short key:\n%s", out)
	}
}

// fakeHcLs makes attentionHcList answer with these live terminals (and one
// finished one) on core selection sel, and counts its calls.
func fakeHcLs(t *testing.T, sel string, addrs ...string) *int {
	t.Helper()
	var ts []map[string]any
	for _, a := range addrs {
		ts = append(ts, map[string]any{"address": a, "agent": "claude-code", "state": "live", "status": "idle", "cwd": "/x", "where": "s / t"})
	}
	ts = append(ts, map[string]any{"address": "t99", "state": "finished"})
	body, _ := json.Marshal(map[string]any{"terminals": ts})
	calls := 0
	attentionHcList = func(context.Context) ([]byte, string, error) {
		calls++
		return body, sel, nil
	}
	return &calls
}

// A desk item queued from a herdr-app terminal points at that terminal while
// hc lists it live; a finished or unlisted one gives no terminal, and hc
// being down says why in sources.
func TestAttentionListDeskTerminalFromCore(t *testing.T) {
	e := newAttnEnv(t)
	for _, sub := range []string{"pending", "running", "done", "skipped"} {
		if err := os.MkdirAll(filepath.Join(e.desk, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const here = "0123456789abcdef0123456789abcdef"
	add := func(name, term, core string) {
		body := "# WHAT: " + name + "\necho hi\n"
		writeDeskFile(t, filepath.Join(e.desk, "pending", name+".sh"), body)
		writeDeskFile(t, filepath.Join(e.desk, "pending", name+".meta.json"),
			`{"added_at":"2026-10-01T00:00:00Z","sha256":"`+sha(body)+`","kind":"script","terminal":"`+term+`","terminal_core":"`+core+`"}`)
	}
	add("01-live", "t148", here)
	add("02-finished", "t99", here)
	add("03-forged", "../../sh", here)
	add("04-othercore", "t148", "ffffffffffffffffffffffffffffffff")

	calls := fakeHcLs(t, here, "t148")
	l := attnList(t, module.Deps{Runner: herdrDown()})
	if *calls != 1 {
		t.Errorf("hc ls ran %d times, want 1", *calls)
	}
	if l.Sources.Core != sourceOK || len(l.Rows) != 4 {
		t.Fatalf("core %q, rows %+v", l.Sources.Core, l.Rows)
	}
	got := map[string]string{}
	for _, r := range l.Rows {
		got[r.Tag] = r.Terminal
	}
	if got["01-live"] != "t148" || got["02-finished"] != "" || got["03-forged"] != "" || got["04-othercore"] != "" {
		t.Errorf("terminals = %v, want only 01-live at t148", got)
	}

	attentionHcList = func(context.Context) ([]byte, string, error) { return nil, "", errors.New("hc: not running") }
	l = attnList(t, module.Deps{Runner: herdrDown()})
	if l.Sources.Core != sourceUnavailable || l.Sources.CoreDetail == "" {
		t.Errorf("hc down: core %q detail %q", l.Sources.Core, l.Sources.CoreDetail)
	}
	for _, r := range l.Rows {
		if r.Terminal != "" {
			t.Errorf("terminal %q with hc down", r.Terminal)
		}
	}
}

// With no desk item naming a terminal, hc is never asked and sources.core
// is absent: nothing in the queue would use the answer.
func TestAttentionListSkipsHcWithoutADeskTerminal(t *testing.T) {
	e := newAttnEnv(t)
	for _, sub := range []string{"pending", "running", "done", "skipped"} {
		if err := os.MkdirAll(filepath.Join(e.desk, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	body := "# WHAT: plain\necho hi\n"
	writeDeskFile(t, filepath.Join(e.desk, "pending", "01-plain.sh"), body)
	writeDeskFile(t, filepath.Join(e.desk, "pending", "01-plain.meta.json"), `{"added_at":"2026-10-01T00:00:00Z","sha256":"`+sha(body)+`","kind":"script"}`)
	calls := fakeHcLs(t, "0123456789abcdef0123456789abcdef", "t148")
	l := attnList(t, module.Deps{Runner: herdrDown()})
	if *calls != 0 || l.Sources.Core != sourceAbsent || len(l.Rows) != 1 {
		t.Errorf("hc calls %d, core %q, rows %d; want 0, absent, 1", *calls, l.Sources.Core, len(l.Rows))
	}
}

func TestDrainNeedsYou(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	stuck := &drain.SlotsHold{Cond: "timeout", Reason: "claude-slots timed out", Since: now.Add(-20 * time.Minute)}
	live := func(drain.Status) bool { return true }
	dead := func(drain.Status) bool { return false }
	base := func() drain.Status {
		return drain.Status{V: drain.StatusVersion, Status: drain.StatusRunning, PID: 42, ProcessStart: 7, LastTick: now.Add(-10 * time.Second), IntervalSeconds: 30, SlotsHold: stuck}
	}
	cases := []struct {
		name  string
		edit  func(*drain.Status)
		alive func(drain.Status) bool
		want  bool
	}{
		{"live drain stuck past SlotsStuckAfter", func(*drain.Status) {}, live, true},
		{"crashed drain with a hold", func(*drain.Status) {}, dead, false},
		{"stale tick", func(s *drain.Status) { s.LastTick = now.Add(-time.Hour) }, live, false},
		{"paused", func(s *drain.Status) { s.Status = drain.StatusPaused }, live, false},
		{"stopped", func(s *drain.Status) { s.Status = drain.StatusStopped }, live, false},
		{"full cap is the normal hold", func(s *drain.Status) { s.SlotsHold = &drain.SlotsHold{Cond: "held", Since: now.Add(-time.Hour)} }, live, false},
		{"fault not yet stuck", func(s *drain.Status) { s.SlotsHold = &drain.SlotsHold{Cond: "timeout", Since: now.Add(-time.Minute)} }, live, false},
		{"no hold", func(s *drain.Status) { s.SlotsHold = nil }, live, false},
	}
	for _, c := range cases {
		st := base()
		c.edit(&st)
		got := drainNeedsYou(st, now, c.alive)
		if (got != nil) != c.want {
			t.Errorf("%s: drainNeedsYou = %v, want row %v", c.name, got, c.want)
		}
		if got != nil && !got.Since.Equal(stuck.Since) {
			t.Errorf("%s: since = %v, want the hold's %v", c.name, got.Since, stuck.Since)
		}
	}
}

// End to end: a drain.json in the surface directory with a stuck hold, and a
// live process, puts one raise-tier drain row in list --json whose text is
// fixed; none of the drain's own reason reaches it.
func TestAttentionListDrainRow(t *testing.T) {
	e := newAttnEnv(t)
	oldNow, oldAlive := attentionNow, attentionDrainAlive
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	attentionNow = func() time.Time { return now }
	alive := true
	attentionDrainAlive = func(drain.Status) bool { return alive }
	t.Cleanup(func() { attentionNow, attentionDrainAlive = oldNow, oldAlive })

	if l := attnList(t, module.Deps{}); l.Sources.Drain != sourceAbsent {
		t.Fatalf("no drain.json: drain source = %q, want absent", l.Sources.Drain)
	}

	dir := filepath.Join(e.state, "forgectl", "surface")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := drain.Status{
		V: drain.StatusVersion, Status: drain.StatusRunning, PID: 42, ProcessStart: 7,
		LastTick: now.Add(-10 * time.Second), IntervalSeconds: 30, Counts: map[string]int{}, Attention: []drain.Attention{},
		SlotsHold: &drain.SlotsHold{Cond: "exit 2", Reason: "IGNORE ME: free text from the drain", Since: now.Add(-30 * time.Minute)},
	}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "drain.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	l := attnList(t, module.Deps{})
	if l.Sources.Drain != sourceOK || len(l.Rows) != 1 {
		t.Fatalf("drain source %q, rows %+v", l.Sources.Drain, l.Rows)
	}
	r := l.Rows[0]
	if r.Kind != attention.RowDrain || r.Tier != attention.TierRaise || !attention.ValidKey(r.Key) || !r.At.Equal(st.SlotsHold.Since) {
		t.Errorf("drain row = %+v", r)
	}
	if strings.Contains(r.Text, "IGNORE") {
		t.Errorf("drain text carries the drain's own reason: %q", r.Text)
	}

	alive = false
	if l := attnList(t, module.Deps{}); len(l.Rows) != 0 {
		t.Errorf("a dead drain still shows a row: %+v", l.Rows)
	}

	if err := os.WriteFile(filepath.Join(dir, "drain.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if l := attnList(t, module.Deps{}); l.Sources.Drain != sourceMalformed || len(l.Rows) != 0 {
		t.Errorf("malformed drain.json: source %q rows %+v", l.Sources.Drain, l.Rows)
	}
}

func TestAttentionRefusesABadConfig(t *testing.T) {
	newAttnEnv(t)
	bad := "-x"
	deps := module.Deps{Cfg: config.Config{Attention: config.AttentionConfig{WorkerPrefix: &bad}}}
	_, _, err := attnRun(t, deps, "list", "--json")
	if ExitCode(err) != exitUsage || !strings.Contains(err.Error(), "attention.worker_prefix") {
		t.Errorf("exit %d (%v), want %d naming the key", ExitCode(err), err, exitUsage)
	}
}

// writeDeskFile writes a desk file with a fixed mtime.
func writeDeskFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// deskSnapshot is every entry under dir: name, mode, mtime and, for a
// file, its bytes. Any create, remove, move, chmod, touch or write changes it.
func deskSnapshot(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%s %v %d", p, fi.Mode(), fi.ModTime().UnixNano())
		if fi.Mode().IsRegular() {
			data, err := os.ReadFile(p) //nolint:gosec // G304: a test path
			if err != nil {
				return err
			}
			line += " " + string(data)
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func sha(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// TestAttentionListReadsTheDeskWithoutWriting: a hand-dropped item gets no
// meta, a changed item stays in pending/, and nothing under the desk changes
// name, mode, mtime or content. A waiting item queued from a session's pane
// absorbs that session's run action.

func TestAttentionListReadsTheDeskWithoutWriting(t *testing.T) {
	e := newAttnEnv(t)
	for _, sub := range []string{"pending", "running", "done", "skipped"} {
		if err := os.MkdirAll(filepath.Join(e.desk, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const hand, changed, queued = "# WHAT: by hand\necho hi\n", "# WHAT: edited\necho changed\n", "# WHAT: fix the widget\n# WHY: test\necho fix\n"
	added := time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)
	writeDeskFile(t, filepath.Join(e.desk, "pending", "01-hand.sh"), hand)
	writeDeskFile(t, filepath.Join(e.desk, "pending", "02-changed.sh"), changed)
	writeDeskFile(t, filepath.Join(e.desk, "pending", "02-changed.meta.json"),
		`{"added_at":"2026-10-01T00:00:00Z","sha256":"`+sha("the bytes it was queued with")+`","kind":"script"}`)
	writeDeskFile(t, filepath.Join(e.desk, "pending", "03-fix.sh"), queued)
	writeDeskFile(t, filepath.Join(e.desk, "pending", "03-fix.meta.json"),
		`{"added_at":"`+added+`","sha256":"`+sha(queued)+`","kind":"script","signal_pane":"w1:p2"}`)
	for _, d := range []string{e.desk, filepath.Join(e.desk, "pending")} {
		old := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}
	deps := module.Deps{Runner: herdrAgents([3]string{"w1:p2", attnSession, ""})}

	before := deskSnapshot(t, e.desk)
	l := attnList(t, deps)
	if after := deskSnapshot(t, e.desk); after != before {
		t.Fatalf("attention list changed the desk:\nbefore\n%s\nafter\n%s", before, after)
	}
	if l.Sources.Desk != sourceOK {
		t.Fatalf("desk source = %s", l.Sources.Desk)
	}
	var names []string
	for _, r := range l.Rows {
		if r.Kind != attention.RowDesk {
			t.Errorf("non-desk row %+v", r)
			continue
		}
		names = append(names, r.Tag)
		if r.Tag == "03-fix" && (r.Pane != "w1:p2" || r.Session != attnSession || r.Text != "fix the widget" || r.Hash != sha(queued)[:12]) {
			t.Errorf("03-fix row = %+v", r)
		}
	}
	if strings.Join(names, ",") != "01-hand,03-fix" {
		t.Errorf("desk rows = %v, want the hand-dropped and queued items (the changed one is refused, not waiting)", names)
	}
}

// queueDeskItem makes e.desk a desk holding one waiting item queued from
// pane.
func queueDeskItem(t *testing.T, e attnEnv, name, pane string) {
	t.Helper()
	const body = "# WHAT: fix the widget\n# WHY: test\necho fix\n"
	writeDeskFile(t, filepath.Join(e.desk, "pending", name+".sh"), body)
	writeDeskFile(t, filepath.Join(e.desk, "pending", name+".meta.json"),
		`{"added_at":"2026-10-16T12:00:00Z","sha256":"`+sha(body)+`","kind":"script","signal_pane":"`+pane+`"}`)
	for _, sub := range []string{"running", "done", "skipped"} {
		if err := os.MkdirAll(filepath.Join(e.desk, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// addAsks records asks in e.desk's asks log through the real store and
// returns their ids, oldest first.
func addAsks(t *testing.T, e attnEnv, clock *time.Time, titles ...string) []string {
	t.Helper()
	s, err := ask.Open(e.desk, ask.WithClock(func() time.Time { return *clock }))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, title := range titles {
		a, err := s.Add(ask.Ask{Title: title, Bucket: ask.BucketNow, From: "cos-fix-1", Allow: []ask.Kind{ask.KindDone, ask.KindSnooze}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
		*clock = clock.Add(time.Minute)
	}
	return ids
}

func respondAsk(t *testing.T, e attnEnv, clock time.Time, r ask.Response) {
	t.Helper()
	s, err := ask.Open(e.desk, ask.WithClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Respond(t.Context(), r); err != nil {
		t.Fatal(err)
	}
}

// Open asks are rows at the operator-action cost, oldest first; an answered
// ask is gone and a snoozed one is hidden until its snooze ends.
func TestAttentionListShowsOpenAsks(t *testing.T) {
	e := newAttnEnv(t)
	start := time.Now().UTC().Add(-time.Hour)
	clock := start
	ids := addAsks(t, e, &clock, "Sign in to the registry", "Approve the deploy key", "Check the phone", "Answered already")
	respondAsk(t, e, clock, ask.Response{Ask: ids[3], Kind: ask.KindDone, Via: "ui"})
	until := time.Now().UTC().Add(2 * time.Hour)
	respondAsk(t, e, clock, ask.Response{Ask: ids[2], Kind: ask.KindSnooze, Until: &until, Via: "ui"})

	l := attnList(t, module.Deps{Cfg: config.Config{}})
	if l.Sources.Asks != sourceOK {
		t.Fatalf("asks source = %q", l.Sources.Asks)
	}
	var got []string
	for _, r := range l.Rows {
		if r.Kind != attention.RowAsk {
			continue
		}
		got = append(got, r.Tag)
		if r.Cost != attention.CostAsk || r.Tier != attention.TierCount || r.Bucket != "now" || r.Worker != "cos-fix-1" || r.Origin != attention.OriginWorker || !attention.ValidKey(r.Key) {
			t.Errorf("ask row = %+v", r)
		}
	}
	if strings.Join(got, ",") != ids[0]+","+ids[1] {
		t.Errorf("ask rows = %v, want the two open asks %v oldest first", got, ids[:2])
	}

	// Once the snooze ends, the ask is back.
	prev := attentionNow
	attentionNow = func() time.Time { return until.Add(time.Minute) }
	t.Cleanup(func() { attentionNow = prev })
	n := 0
	for _, r := range attnList(t, module.Deps{}).Rows {
		if r.Kind == attention.RowAsk {
			n++
		}
	}
	if n != 3 {
		t.Errorf("after the snooze ends: %d ask rows, want 3", n)
	}
}

// Ask text is untrusted: a hostile title never reaches the terminal raw, and
// a line that is not a record is counted, never shown.
func TestAttentionListCleansAskText(t *testing.T) {
	e := newAttnEnv(t)
	clock := time.Now().UTC().Add(-time.Hour)
	addAsks(t, e, &clock, "first")
	log := filepath.Join(e.desk, ask.SubDir, ask.LogName)
	f, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G304: the test's own asks log
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"v":1,"ask":{"id":"ask-0000beef","at":"2026-10-16T12:00:00Z","title":"evil\u001b]0;pwned\u0007 \u202eright","bucket":"now","allow":["done"]}}` + "\nnot a record\n"
	if _, err := f.WriteString(raw); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	l := attnList(t, module.Deps{})
	if l.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 (the line that is not a record)", l.Skipped)
	}
	out, _, err := attnRun(t, module.Deps{}, "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out, "\x1b\x07\u202e") || !strings.Contains(out, "evil]0;pwned right") {
		t.Errorf("text view printed hostile ask text raw or lost it:\n%q", out)
	}
}

// Reading the asks takes no lock and changes nothing in the asks directory:
// no mode, no file, no mtime.
func TestAttentionListReadsAsksWithoutWriting(t *testing.T) {
	e := newAttnEnv(t)
	clock := time.Now().UTC().Add(-time.Hour)
	addAsks(t, e, &clock, "one", "two")
	dir := filepath.Join(e.desk, ask.SubDir)
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // G302: a broad directory a writer would narrow, to prove a reader leaves it
		t.Fatal(err)
	}
	old := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	before := deskSnapshot(t, dir)
	l := attnList(t, module.Deps{})
	if after := deskSnapshot(t, dir); after != before {
		t.Fatalf("attention list changed the asks directory:\nbefore\n%s\nafter\n%s", before, after)
	}
	if l.Sources.Asks != sourceOK || len(l.Rows) != 2 {
		t.Errorf("sources.asks = %q, rows = %+v", l.Sources.Asks, l.Rows)
	}
}

func TestAttentionListAsksSourceStates(t *testing.T) {
	e := newAttnEnv(t)
	if l := attnList(t, module.Deps{}); l.Sources.Asks != sourceAbsent {
		t.Errorf("no asks log: sources.asks = %q, want absent", l.Sources.Asks)
	}
	clock := time.Now().UTC().Add(-time.Hour)
	addAsks(t, e, &clock, "held here")

	// A named desk keeps the asks local, host or not.
	t.Setenv(attentionAsksHostEnv, "desk-host")
	if l := attnList(t, module.Deps{}); l.Sources.Asks != sourceOK || len(l.Rows) != 1 {
		t.Errorf("DESK_DIR set: sources.asks = %q, rows %+v, want the local ask", l.Sources.Asks, l.Rows)
	}
	// With no named desk, the asks are on the host and nothing is read.
	t.Setenv("DESK_DIR", "")
	if l := attnList(t, module.Deps{}); l.Sources.Asks != sourceRemote || len(l.Rows) != 0 {
		t.Errorf("remote host: sources.asks = %q, rows %+v, want remote and none", l.Sources.Asks, l.Rows)
	}

	// A symlinked asks directory is refused: an error, and the rest still reports.
	t.Setenv(attentionAsksHostEnv, "")
	t.Setenv("DESK_DIR", e.desk)
	dir := filepath.Join(e.desk, ask.SubDir)
	if err := os.Rename(dir, dir+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir+".real", dir); err != nil {
		t.Fatal(err)
	}
	_, errOut, err := attnRun(t, module.Deps{}, "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "asks not read") {
		t.Errorf("stderr = %q, want an asks-not-read note", errOut)
	}
	if l := attnList(t, module.Deps{}); l.Sources.Asks != sourceError {
		t.Errorf("symlinked asks dir: sources.asks = %q, want error", l.Sources.Asks)
	}
}

// TestAttentionSessionSources pins where an agent row's session comes from:
// herdr's agent_session when present, else a Claude pane's `session` token.
func TestAttentionSessionSources(t *testing.T) {
	const tok = "70c3e550-aaaa-4bbb-8ccc-000000000002"
	sess := func(v string) *herdr.AgentSession {
		return &herdr.AgentSession{Agent: "claude", Kind: "id", Source: "herdr:claude", Value: v}
	}
	cases := []struct {
		name  string
		agent string
		as    *herdr.AgentSession
		tok   *herdr.AgentTokens
		want  string
	}{
		{"agent_session only", "claude", sess(attnSession), nil, attnSession},
		{"token only", "claude", nil, &herdr.AgentTokens{Session: tok}, tok},
		{"both equal", "claude", sess(attnSession), &herdr.AgentTokens{Session: attnSession}, attnSession},
		{"both differ: agent_session wins", "claude", sess(attnSession), &herdr.AgentTokens{Session: tok}, attnSession},
		{"empty agent_session value: token", "claude", sess(""), &herdr.AgentTokens{Session: tok}, tok},
		{"invalid token ignored", "claude", nil, &herdr.AgentTokens{Session: "-rf /"}, ""},
		{"token on a non-claude agent ignored", "codex", nil, &herdr.AgentTokens{Session: tok}, ""},
		{"neither", "claude", nil, &herdr.AgentTokens{Worktree: "wt"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := herdr.Agent{Pane: herdr.Pane{PaneID: "w1:p2", Agent: c.agent, AgentSession: c.as}, Tokens: c.tok}
			if got := attentionAgent(a).Session; got != c.want {
				t.Errorf("session = %q, want %q", got, c.want)
			}
		})
	}
}

// TestAttentionListMatchesATokenOnlySession is the live shape: herdr's own
// Claude integration is off, so the row has no agent_session and the
// herdr-bridge mod's `session` token is the only place the id appears: a desk
// item queued from that pane still gets the session.
func TestAttentionListMatchesATokenOnlySession(t *testing.T) {
	e := newAttnEnv(t)
	queueDeskItem(t, e, "03-fix", "w1:p2")
	body, _ := json.Marshal(map[string]any{"id": "cli:agent:list", "result": map[string]any{"agents": []map[string]any{{
		"pane_id": "w1:p2", "agent": "claude", "agent_status": "idle", "cwd": "/Users/x/Projects/widget",
		"agent_session": nil, "tokens": map[string]any{"session": attnSession, "cwd": "/Users/x/Projects/widget"},
	}}}})
	deps := module.Deps{Runner: &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "herdr" && strings.Join(args, " ") == "agent list" {
			return string(body), nil
		}
		return "", fmt.Errorf("unexpected %s %v", name, args)
	}}}
	rows := attnList(t, deps).Rows
	if len(rows) == 0 {
		t.Fatal("list has no rows")
	}
	for _, r := range rows {
		if r.Pane != "w1:p2" || r.Origin != attention.OriginOwn || r.Session != attnSession {
			t.Errorf("row = %+v, want pane w1:p2, origin own", r)
		}
	}
}

func TestAttentionAgentNames(t *testing.T) {
	cases := []struct {
		name string
		a    herdr.Agent
		want []string
	}{
		{"name first", herdr.Agent{Pane: herdr.Pane{PaneID: "w1:p2", Name: "cos-x", CWD: "/r/free-form"}}, []string{"cos-x", "free-form"}},
		{"no name: basenames", herdr.Agent{Pane: herdr.Pane{PaneID: "w1:p2", CWD: "/r/free-form"}}, []string{"free-form"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := attentionAgent(c.a).Names; !slices.Equal(got, c.want) {
				t.Errorf("names = %v, want %v", got, c.want)
			}
		})
	}
}

// TestAttentionWorkerOriginFromAgentName: a worker whose worktree is not
// named cos-* is still tagged worker by the name `herdr agent list` reports
// (forgectl#1286). This is the live path: herdr JSON in, row origin out.
func TestAttentionWorkerOriginFromAgentName(t *testing.T) {
	e := newAttnEnv(t)
	queueDeskItem(t, e, "03-fix", "w1:p2")
	body, _ := json.Marshal(map[string]any{"id": "cli:agent:list", "result": map[string]any{"agents": []map[string]any{{
		"pane_id": "w1:p2", "agent": "claude", "agent_status": "idle", "cwd": "/Users/x/wt/free-form", "name": "cos-fix-12",
		"agent_session": map[string]any{"agent": "claude", "kind": "id", "source": "herdr:claude", "value": attnSession},
	}}}})
	deps := module.Deps{Runner: &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "herdr" && strings.Join(args, " ") == "agent list" {
			return string(body), nil
		}
		return "", fmt.Errorf("unexpected %s %v", name, args)
	}}}
	rows := attnList(t, deps).Rows
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	for _, r := range rows {
		if r.Origin != attention.OriginWorker {
			t.Errorf("row from agent cos-fix-12 in a free-form worktree = %+v, want origin worker", r)
		}
	}
}
