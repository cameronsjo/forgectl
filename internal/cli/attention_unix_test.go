// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/attention"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/module"
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
	oldTTY := attentionStdinIsTerminal
	attentionStdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { attentionStdinIsTerminal = oldTTY })
	return e
}

func (e attnEnv) spool() string { return filepath.Join(e.state, "cadence", "attention") }

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

func attnRun(t *testing.T, deps module.Deps, stdin string, args ...string) (string, string, error) {
	t.Helper()
	if deps.Runner == nil {
		deps.Runner = herdrDown()
	}
	var out, errOut bytes.Buffer
	cmd := newAttentionCmd(deps)
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

func attnList(t *testing.T, deps module.Deps) attentionList {
	t.Helper()
	out, errOut, err := attnRun(t, deps, "", "list", "--json")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, errOut)
	}
	var l attentionList
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		t.Fatalf("list --json: %v\n%s", err, out)
	}
	return l
}

const attnReply = "Done with the widget.\n\n**What's next:**\n- **Question 1**: Ship it today? Recommended: yes\n- **Action 2 (Cameron)**: merge the widget PR\n"

func TestAttentionPutThenList(t *testing.T) {
	newAttnEnv(t)
	// The record claims w1:p9, but herdr puts the session in w1:p2: Jump must
	// use herdr's pane. w1:p9 is listed too (as another session), so the
	// record is not pruned as a dead pane.
	deps := module.Deps{Runner: herdrAgents([3]string{"w1:p2", attnSession, ""}, [3]string{"w1:p9", "someone-else", ""})}
	out, errOut, err := attnRun(t, deps, attnReply, "put", "--session", attnSession, "--turn", "turn-1",
		"--pane", "w1:p9", "--socket", attnSocket, "--cwd", "widget")
	if err != nil {
		t.Fatalf("put: %v\n%s", err, errOut)
	}
	if out != "items=2 seen=2 read=2\n" {
		t.Errorf("put printed %q", out)
	}
	l := attnList(t, deps)
	if len(l.Rows) != 2 || l.Skipped != 0 {
		t.Fatalf("list = %+v", l)
	}
	q := l.Rows[1]
	if q.Kind != attention.KindQuestion || q.Pane != "w1:p2" || q.Session != attnSession || q.Rec != attention.RecNow || q.Origin != attention.OriginOwn || q.Turn != "turn-1" {
		t.Errorf("question row = %+v", q)
	}
	if a := l.Rows[0]; a.Kind != attention.KindAction || a.Actor != attention.ActorCameron || a.NotOnDesk {
		t.Errorf("action row = %+v", a)
	}
	if l.Sources != (attentionSources{Spool: sourceOK, Desk: sourceAbsent, Herdr: sourceOK, Sweep: sourceAbsent}) {
		t.Errorf("sources = %+v", l.Sources)
	}
}

func TestAttentionWorkerOrigin(t *testing.T) {
	newAttnEnv(t)
	deps := module.Deps{Runner: herdrAgents([3]string{"w1:p2", attnSession, "cos-fix-12"})}
	if _, errOut, err := attnRun(t, deps, attnReply, "put", "--session", attnSession, "--turn", "t1", "--pane", "w1:p2", "--socket", attnSocket); err != nil {
		t.Fatalf("put: %v\n%s", err, errOut)
	}
	for _, r := range attnList(t, deps).Rows {
		if r.Origin != attention.OriginWorker {
			t.Errorf("row from a cos- worktree = %+v, want origin worker", r)
		}
	}
	deps.Cfg.Attention.WorkerPrefix = new("w-")
	for _, r := range attnList(t, deps).Rows {
		if r.Origin != attention.OriginOwn {
			t.Errorf("with worker_prefix w-, row = %+v, want origin own", r)
		}
	}
}

func TestAttentionPutRefusesBadFlags(t *testing.T) {
	e := newAttnEnv(t)
	for name, args := range map[string][]string{
		"traversing session": {"put", "--session", "../x", "--turn", "t"},
		"flag-like session":  {"put", "--session", "-x", "--turn", "t"},
		"missing turn":       {"put", "--session", attnSession},
		"bad pane":           {"put", "--session", attnSession, "--turn", "t", "--pane", "-w1"},
		"relative socket":    {"put", "--session", attnSession, "--turn", "t", "--socket", "h.sock"},
		"rm bad session":     {"rm", "--session", "a/b"},
	} {
		_, _, err := attnRun(t, module.Deps{}, attnReply, args...)
		if ExitCode(err) != exitUsage {
			t.Errorf("%s: exit %d (%v), want %d", name, ExitCode(err), err, exitUsage)
		}
	}
	if _, err := os.Stat(e.spool()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused put created the spool: %v", err)
	}
}

func TestAttentionPutRefusesATerminal(t *testing.T) {
	newAttnEnv(t)
	attentionStdinIsTerminal = func() bool { return true }
	cmd := newAttentionCmd(module.Deps{Runner: herdrDown()})
	cmd.SetArgs([]string{"put", "--session", attnSession, "--turn", "t"})
	cmd.SetIn(os.Stdin)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.ExecuteContext(t.Context()); ExitCode(err) != exitUsage {
		t.Errorf("exit %d (%v), want %d", ExitCode(err), err, exitUsage)
	}
}

func TestAttentionPutKeepsTheTailOfALongReply(t *testing.T) {
	newAttnEnv(t)
	long := strings.Repeat("filler line\n", 20000) + attnReply
	out, errOut, err := attnRun(t, module.Deps{}, long, "put", "--session", attnSession, "--turn", "t")
	if err != nil || out != "items=2 seen=2 read=2\n" {
		t.Fatalf("put = %q %v\n%s", out, err, errOut)
	}
}

func TestAttentionListNeverPrintsAMalformedRecord(t *testing.T) {
	e := newAttnEnv(t)
	if _, _, err := attnRun(t, module.Deps{}, attnReply, "put", "--session", attnSession, "--turn", "t"); err != nil {
		t.Fatal(err)
	}
	const marker = "PLANTED-MARKER-7f3a"
	planted := `{"session":"planted","turn":"t","at":"2026-10-16T12:00:00Z","pane":"-` + marker + `","items":[{"kind":"q","n":1,"text":"` + marker + `"}]}`
	if err := os.WriteFile(filepath.Join(e.spool(), "planted.json"), []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"list", "--json"}, {"list"}} {
		out, errOut, err := attnRun(t, module.Deps{}, "", args...)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out+errOut, marker) {
			t.Errorf("%v printed the malformed record's content:\n%s%s", args, out, errOut)
		}
	}
	if l := attnList(t, module.Deps{}); l.Skipped != 1 || len(l.Rows) != 2 {
		t.Errorf("list = %+v, want 2 rows and 1 skipped", l)
	}
}

func TestAttentionListPrunesAVisibleDeadPane(t *testing.T) {
	e := newAttnEnv(t)
	live := module.Deps{Runner: herdrAgents([3]string{"w1:p2", attnSession, ""})}
	if _, _, err := attnRun(t, live, attnReply, "put", "--session", attnSession, "--turn", "t", "--pane", "w1:p2", "--socket", attnSocket); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(e.spool(), attnSession+".json")
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("put wrote no record: %v", err)
	}
	// herdr unavailable: the record is kept (TTL only).
	if l := attnList(t, module.Deps{Runner: herdrDown()}); len(l.Rows) != 2 || l.Sources.Herdr != sourceUnavailable {
		t.Fatalf("with herdr down: %+v", l)
	}
	// herdr up, the pane gone: pruned.
	gone := module.Deps{Runner: herdrAgents([3]string{"w1:p3", "other", ""})}
	if l := attnList(t, gone); len(l.Rows) != 0 {
		t.Errorf("rows = %+v, want the dead pane's record pruned", l.Rows)
	}
	if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("record still on disk: %v", err)
	}
}

func TestAttentionPutEvictsTheSamePanesOldSession(t *testing.T) {
	e := newAttnEnv(t)
	deps := module.Deps{Runner: herdrAgents([3]string{"w1:p2", "new-session", ""})}
	for _, s := range []string{"old-session", "new-session"} {
		if _, _, err := attnRun(t, deps, attnReply, "put", "--session", s, "--turn", "t", "--pane", "w1:p2", "--socket", attnSocket); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.spool(), "old-session.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the pane's old session record survived: %v", err)
	}
}

func TestAttentionRm(t *testing.T) {
	e := newAttnEnv(t)
	if _, _, err := attnRun(t, module.Deps{}, "", "rm", "--session", attnSession); err != nil {
		t.Errorf("rm with no spool: %v", err)
	}
	if _, _, err := attnRun(t, module.Deps{}, attnReply, "put", "--session", attnSession, "--turn", "t"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := attnRun(t, module.Deps{}, "", "rm", "--session", attnSession); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.spool(), attnSession+".json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("record survived rm: %v", err)
	}
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

func TestAttentionRefusesABadConfig(t *testing.T) {
	newAttnEnv(t)
	deps := module.Deps{Cfg: config.Config{Attention: config.AttentionConfig{TTLHours: new(0)}}}
	_, _, err := attnRun(t, deps, "", "list", "--json")
	if ExitCode(err) != exitUsage || !strings.Contains(err.Error(), "attention.ttl_hours") {
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
	if _, _, err := attnRun(t, deps, "**What's next:**\n- **Action 1 (Cameron)**: run the fix script on the desk\n",
		"put", "--session", attnSession, "--turn", "t", "--pane", "w1:p2", "--socket", attnSocket); err != nil {
		t.Fatal(err)
	}

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
			t.Errorf("non-desk row %+v: the run action should merge into 03-fix", r)
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

func TestAttentionPutKeepsItsRecordBeforeHerdrSeesThePane(t *testing.T) {
	e := newAttnEnv(t)
	// herdr is up but has not detected the new session's pane yet.
	deps := module.Deps{Runner: herdrAgents([3]string{"w1:p1", "other", ""})}
	if _, _, err := attnRun(t, deps, attnReply, "put", "--session", attnSession, "--turn", "t", "--pane", "w1:p7", "--socket", attnSocket); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.spool(), attnSession+".json")); err != nil {
		t.Errorf("put pruned the record it just wrote: %v", err)
	}
}

func TestAttentionListDegradesOnAnUnsafeSpool(t *testing.T) {
	e := newAttnEnv(t)
	if err := os.MkdirAll(e.spool(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.spool(), 0o755); err != nil { //nolint:gosec // G302: the test needs a wide mode to see it refused
		t.Fatal(err)
	}
	sweep := filepath.Join(e.state, "cadence", "sweep.json")
	good := `{"schemaVersion":2,"generatedAt":"2026-10-16T12:00:00Z","workers":[{"name":"cos-w","blockedBy":"prompt"}]}`
	if err := os.WriteFile(sweep, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := attnRun(t, module.Deps{}, "", "list", "--json")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var l attentionList
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		t.Fatal(err)
	}
	if l.Sources.Spool != sourceError || len(l.Rows) != 1 || !strings.Contains(errOut, "spool not read") {
		t.Errorf("list = %+v, stderr %q; want the sweep row and spool=error", l, errOut)
	}
}

func TestAttentionPutEvictsOnlyWhenHerdrVouches(t *testing.T) {
	e := newAttnEnv(t)
	// The victim's record, on a pane herdr lists for the victim.
	victim := module.Deps{Runner: herdrAgents([3]string{"w1:p2", "victim", ""})}
	if _, _, err := attnRun(t, victim, attnReply, "put", "--session", "victim", "--turn", "t", "--pane", "w1:p2", "--socket", attnSocket); err != nil {
		t.Fatal(err)
	}
	// Another writer claims the same pane; herdr does not list it there.
	if _, _, err := attnRun(t, victim, attnReply, "put", "--session", "intruder", "--turn", "t", "--pane", "w1:p2", "--socket", attnSocket); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.spool(), "victim.json")); err != nil {
		t.Errorf("a put herdr does not vouch for evicted another session's record: %v", err)
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
// herdr-bridge mod's `session` token is the only place the id appears.
func TestAttentionListMatchesATokenOnlySession(t *testing.T) {
	newAttnEnv(t)
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
	if _, errOut, err := attnRun(t, deps, attnReply, "put", "--session", attnSession, "--turn", "t1", "--pane", "w1:p2", "--socket", attnSocket); err != nil {
		t.Fatalf("put: %v\n%s", err, errOut)
	}
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

// attnPutJSON runs `put --json` and decodes stdout strictly: exactly one
// object with only the contract's keys, and nothing after it.
func attnPutJSON(t *testing.T, stdin string, args ...string) (attentionPut, string) {
	t.Helper()
	out, errOut, err := attnRun(t, module.Deps{}, stdin, append([]string{"put", "--json"}, args...)...)
	if err != nil {
		t.Fatalf("put --json: %v\n%s", err, errOut)
	}
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	var got attentionPut
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("put --json stdout is not one contract object: %v\n%s", err, out)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err == nil {
		t.Fatalf("put --json printed more than one JSON value:\n%s", out)
	}
	if rest := out[dec.InputOffset():]; strings.TrimSpace(rest) != "" {
		t.Fatalf("put --json printed text after the object: %q", rest)
	}
	return got, out
}

func TestAttentionPutJSONEchoesTheRecordedItems(t *testing.T) {
	e := newAttnEnv(t)
	long := strings.Repeat("word ", 80)
	reply := "Body.\n\n**What's next:**\n" +
		"- **Question 1**: Ship it\u200b today? Recommended: yes\n" +
		"- **A.** Red\n" +
		"- **Action 2 (Cameron)**: merge the widget PR\n" +
		"- **Action 3 (Claude)**: " + long + "Recommended: later\n" +
		"- **Action 4**: tidy \x1b[31mthe\x1b[0m docs\n" +
		"5. **Nothing owed by me.** The gizmo branch is merged.\n"
	got, _ := attnPutJSON(t, reply, "--session", attnSession, "--turn", "turn-7")
	if got.Session != attnSession || got.Turn != "turn-7" {
		t.Errorf("session/turn = %q/%q", got.Session, got.Turn)
	}

	data, err := os.ReadFile(filepath.Join(e.spool(), attnSession+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec attention.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	var want []attentionPutItem
	for _, it := range rec.Items {
		if it.Kind == attention.KindQuestion || it.Kind == attention.KindAction {
			want = append(want, attentionPutItem{Kind: it.Kind, N: it.N, Actor: it.Actor, Text: it.Text, Rec: it.Rec})
		}
	}
	if len(want) != 4 {
		t.Fatalf("fixture recorded %d question/action items, want 4: %+v", len(want), rec.Items)
	}
	if len(got.Items) != len(want) {
		t.Fatalf("items = %+v, want %+v", got.Items, want)
	}
	for i := range want {
		if got.Items[i] != want[i] {
			t.Errorf("item %d = %+v, record holds %+v", i, got.Items[i], want[i])
		}
	}
	if a := got.Items[2]; a.Actor != attention.ActorClaude || a.Rec != attention.RecLater || len([]rune(a.Text)) > attention.MaxText {
		t.Errorf("Claude action = %+v, want actor Claude, rec later, text at most %d runes", a, attention.MaxText)
	}
	if a := got.Items[3]; a.Actor != "" || a.Rec != "" || strings.ContainsRune(a.Text, '\x1b') {
		t.Errorf("unattributed action = %+v, want empty actor and rec, no escape", a)
	}
	if got.Seen != 6 || got.Read != 6 {
		t.Errorf("seen/read = %d/%d, want 6/6", got.Seen, got.Read)
	}
}

func TestAttentionPutJSONWithNoBlockGivesEmptyItems(t *testing.T) {
	newAttnEnv(t)
	got, out := attnPutJSON(t, "No closing block here.\n", "--session", attnSession, "--turn", "t")
	if got.Items == nil || len(got.Items) != 0 {
		t.Errorf("items = %#v, want []", got.Items)
	}
	if !strings.Contains(out, `"items": []`) {
		t.Errorf("stdout has no literal empty items array:\n%s", out)
	}
	for _, key := range []string{`"session"`, `"turn"`, `"seen": 0`, `"read": 0`} {
		if !strings.Contains(out, key) {
			t.Errorf("stdout lacks %s:\n%s", key, out)
		}
	}
}

func TestAttentionPutJSONKeepsEmptyFieldsPresent(t *testing.T) {
	newAttnEnv(t)
	_, out := attnPutJSON(t, "**What's next:**\n- **Action 1**: tidy the docs\n", "--session", attnSession, "--turn", "t")
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Items) != 1 {
		t.Fatalf("items = %v", raw.Items)
	}
	for _, key := range []string{"kind", "n", "actor", "text", "rec"} {
		if _, ok := raw.Items[0][key]; !ok {
			t.Errorf("item lacks %q: %v", key, raw.Items[0])
		}
	}
}

func TestAttentionPutWithoutJSONIsUnchanged(t *testing.T) {
	newAttnEnv(t)
	out, errOut, err := attnRun(t, module.Deps{}, attnReply, "put", "--session", attnSession, "--turn", "t")
	if err != nil || out != "items=2 seen=2 read=2\n" {
		t.Fatalf("put = %q %v\n%s", out, err, errOut)
	}
}

// A refused put --json keeps the --json error contract: nothing on stdout,
// one failure object on stderr, the human run's exit code.
func TestAttentionPutJSONRefusalIsOneStderrObject(t *testing.T) {
	isolateJSONContractEnv(t)
	oldTTY := attentionStdinIsTerminal
	attentionStdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { attentionStdinIsTerminal = oldTTY })
	root := productionJSONRoot(herdrDown())
	root.SetIn(strings.NewReader(attnReply))
	out, errOut, err := runJSONThroughFang(t, root, "attention", "put", "--json", "--session", "../x", "--turn", "t")
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	if ExitCode(err) != exitUsage {
		t.Errorf("exit %d (%v), want %d", ExitCode(err), err, exitUsage)
	}
	got := decodeOneStderrObject(t, errOut)
	if got["code"] != jsonCodeUsage {
		t.Errorf("code = %v, want %s", got["code"], jsonCodeUsage)
	}
	// The refusal must be put's own, not an unknown-command error.
	if msg, _ := got["error"].(string); !strings.Contains(msg, "session is not a session id") {
		t.Errorf("error = %q, want put's session refusal", msg)
	}
}
