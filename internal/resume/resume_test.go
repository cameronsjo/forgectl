package resume

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The fixture $HOME is built programmatically rather than checked in as
// testdata: every test needs a slightly different corner of the tree (a dead
// pid here, a team config there), and a builder makes each test state what it
// depends on instead of hiding it in a shared directory.

// fixture is a throwaway ~/.claude plus a forgectl store.
type fixture struct {
	t     *testing.T
	Paths Paths
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	p := Paths{
		ClaudeHome: filepath.Join(root, ".claude"),
		StoreDir:   filepath.Join(root, "forgectl", "resume-sessions"),
	}
	for _, d := range []string{p.projectsDir(), p.registryDir(), p.tasksDir(), p.teamsDir(), p.StoreDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	return &fixture{t: t, Paths: p}
}

// write puts arbitrary content at a path under the fixture, creating parents.
func (f *fixture) write(path, content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		f.t.Fatalf("write %s: %v", path, err)
	}
}

// history appends one prompt record to history.jsonl.
func (f *fixture) history(id, cwd, display string, at time.Time) {
	f.t.Helper()
	rec, err := json.Marshal(historyRecord{
		Display: display, Timestamp: at.UnixMilli(), Project: cwd, SessionID: id,
	})
	if err != nil {
		f.t.Fatalf("marshal history: %v", err)
	}
	path := f.Paths.historyPath()
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		f.t.Fatalf("open history: %v", err)
	}
	defer func() { _ = fh.Close() }()
	if _, err := fh.Write(append(rec, '\n')); err != nil {
		f.t.Fatalf("append history: %v", err)
	}
}

// registryFile writes one ~/.claude/sessions/<pid>.json.
func (f *fixture) registryFile(pid int, id, cwd, name string) {
	f.t.Helper()
	body, err := json.Marshal(RegistryEntry{
		Pid: pid, SessionID: id, Cwd: cwd, Name: name,
		Version: "2.1.220", Kind: "interactive", Status: "idle",
		UpdatedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		f.t.Fatalf("marshal registry: %v", err)
	}
	f.write(filepath.Join(f.Paths.registryDir(), itoa(pid)+".json"), string(body))
}

// transcript writes a per-project transcript carrying a branch and a title.
func (f *fixture) transcript(id, cwd, branch, title string) {
	f.t.Helper()
	lines := []string{
		`{"type":"user","gitBranch":"` + branch + `","cwd":"` + cwd + `"}`,
		`{"type":"ai-title","aiTitle":"` + title + `","sessionId":"` + id + `"}`,
	}
	f.write(filepath.Join(f.Paths.projectsDir(), slugify(cwd), id+".jsonl"),
		strings.Join(lines, "\n")+"\n")
}

// task writes one task body into a task directory.
func (f *fixture) task(dir, id, subject string) {
	f.t.Helper()
	f.write(filepath.Join(dir, id+".json"),
		`{"id":"`+id+`","subject":"`+subject+`","status":"pending","blocks":[],"blockedBy":[]}`)
}

// pinPids forces the liveness probe to a fixed verdict per pid for one test.
func pinPids(t *testing.T, alive map[int]bool) {
	t.Helper()
	prev := pidAlive
	pidAlive = func(pid int) bool { return alive[pid] }
	t.Cleanup(func() { pidAlive = prev })
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestScan_OrdersByLastActive pins the picker's single most important
// property: the session you were just in is the first row.
func TestScan_OrdersByLastActive(t *testing.T) {
	f := newFixture(t)
	base := time.Now().Add(-72 * time.Hour)
	f.history("aaaaaaaa-0000-0000-0000-000000000001", "/repo/old", "oldest", base)
	f.history("aaaaaaaa-0000-0000-0000-000000000002", "/repo/mid", "middle", base.Add(24*time.Hour))
	f.history("aaaaaaaa-0000-0000-0000-000000000003", "/repo/new", "newest", base.Add(48*time.Hour))

	got, err := Scan(f.Paths, Opts{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []string{"/repo/new", "/repo/mid", "/repo/old"}
	if len(got) != len(want) {
		t.Fatalf("Scan returned %d sessions, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Cwd != w {
			t.Errorf("row %d cwd = %q, want %q", i, got[i].Cwd, w)
		}
	}
	if got[0].Repo != "new" {
		t.Errorf("Repo = %q, want %q (filepath.Base of the cwd)", got[0].Repo, "new")
	}
	if got[0].LastPrompt != "newest" {
		t.Errorf("LastPrompt = %q, want %q", got[0].LastPrompt, "newest")
	}
}

// TestScan_NamePrecedence walks the four name sources from lowest to highest,
// asserting each one displaces the one below it. The order is the contract: a
// /rename name is what the operator chose, an ai-title is a guess.
func TestScan_NamePrecedence(t *testing.T) {
	const id = "bbbbbbbb-0000-0000-0000-000000000001"
	cwd := t.TempDir()

	t.Run("lane is the floor", func(t *testing.T) {
		f := newFixture(t)
		f.history(id, cwd, "prompt", time.Now())
		f.write(filepath.Join(cwd, ".claude", "sessions", "lane.json"),
			`{"name":"lane-name","session_id":"`+id+`"}`)
		assertName(t, f, id, "lane-name", NameFromLane)
	})

	t.Run("ai-title beats lane", func(t *testing.T) {
		f := newFixture(t)
		f.history(id, cwd, "prompt", time.Now())
		f.write(filepath.Join(cwd, ".claude", "sessions", "lane.json"),
			`{"name":"lane-name","session_id":"`+id+`"}`)
		f.transcript(id, cwd, "main", "ai-title-name")
		assertName(t, f, id, "ai-title-name", NameFromAITitle)
	})

	t.Run("store beats ai-title", func(t *testing.T) {
		f := newFixture(t)
		f.history(id, cwd, "prompt", time.Now())
		f.transcript(id, cwd, "main", "ai-title-name")
		if err := Save(f.Paths.StoreDir, &Record{ID: id, Cwd: cwd, Name: "store-name", NameSource: NameFromRename}); err != nil {
			t.Fatalf("Save: %v", err)
		}
		assertName(t, f, id, "store-name", NameFromStore)
	})

	t.Run("registry rename beats store", func(t *testing.T) {
		f := newFixture(t)
		pinPids(t, map[int]bool{4242: false})
		f.history(id, cwd, "prompt", time.Now())
		f.transcript(id, cwd, "main", "ai-title-name")
		if err := Save(f.Paths.StoreDir, &Record{ID: id, Cwd: cwd, Name: "store-name"}); err != nil {
			t.Fatalf("Save: %v", err)
		}
		f.registryFile(4242, id, cwd, "rename-name")
		assertName(t, f, id, "rename-name", NameFromRename)
	})
}

func assertName(t *testing.T, f *fixture, id, wantName, wantSource string) {
	t.Helper()
	got, err := Scan(f.Paths, Opts{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, s := range got {
		if s.ID != id {
			continue
		}
		if s.Name != wantName {
			t.Errorf("Name = %q, want %q", s.Name, wantName)
		}
		if s.NameSource != wantSource {
			t.Errorf("NameSource = %q, want %q", s.NameSource, wantSource)
		}
		return
	}
	t.Fatalf("session %s missing from Scan output", id)
}

// TestScan_Liveness asserts a dead registry pid is reported dead and a live one
// live — the check the resume refusal is built on. A registry file outliving
// its process is the normal crash case, so trusting the file's mere existence
// would refuse every resume after a crash.
func TestScan_Liveness(t *testing.T) {
	const deadID = "cccccccc-0000-0000-0000-0000000000d1"
	const liveID = "cccccccc-0000-0000-0000-0000000000e2"
	f := newFixture(t)
	pinPids(t, map[int]bool{111: false, 222: true})
	now := time.Now()
	f.history(deadID, "/repo/dead", "dead prompt", now)
	f.history(liveID, "/repo/live", "live prompt", now.Add(time.Minute))
	f.registryFile(111, deadID, "/repo/dead", "dead-session")
	f.registryFile(222, liveID, "/repo/live", "live-session")

	// Default: live sessions are excluded from the resumable list.
	got, err := Scan(f.Paths, Opts{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 || got[0].ID != deadID {
		t.Fatalf("Scan() = %d sessions (first %q), want only the dead one", len(got), firstID(got))
	}
	if got[0].Live {
		t.Error("dead pid reported Live")
	}

	// IncludeLive surfaces it so the caller can refuse it by name.
	got, err = Scan(f.Paths, Opts{IncludeLive: true})
	if err != nil {
		t.Fatalf("Scan(IncludeLive): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Scan(IncludeLive) = %d sessions, want 2", len(got))
	}
	for _, s := range got {
		if s.ID == liveID {
			if !s.Live {
				t.Error("live pid reported dead")
			}
			if s.Pid != 222 {
				t.Errorf("Pid = %d, want 222 — the refusal names it", s.Pid)
			}
		}
	}
}

func firstID(s []Session) string {
	if len(s) == 0 {
		return ""
	}
	return s[0].ID
}

// TestScan_Filter checks the substring match the one-hit shortcut depends on.
func TestScan_Filter(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	f.history("dddddddd-0000-0000-0000-000000000001", "/work/forgectl", "a", now)
	f.history("dddddddd-0000-0000-0000-000000000002", "/work/cadence", "b", now.Add(time.Minute))

	got, err := Scan(f.Paths, Opts{Filter: "FORGE"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 || got[0].Repo != "forgectl" {
		t.Fatalf("filter %q matched %d sessions (want 1: forgectl)", "FORGE", len(got))
	}
}

// TestRestore_NeverOverwrites is the safety property that lets Restore run
// before every resume: a task file the live session owns always wins.
func TestRestore_NeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	const live = `{"id":"1","subject":"the live one"}`
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte(live), 0o600); err != nil {
		t.Fatalf("seed live task: %v", err)
	}

	res, err := Restore(dir, []Task{
		{ID: "1", Subject: "snapshotted", Raw: json.RawMessage(`{"id":"1","subject":"snapshotted"}`)},
		{ID: "2", Subject: "missing", Raw: json.RawMessage(`{"id":"2","subject":"missing"}`)},
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.Written != 1 || res.Skipped != 1 {
		t.Errorf("Restore wrote %d / skipped %d, want 1 / 1", res.Written, res.Skipped)
	}
	got, err := os.ReadFile(filepath.Join(dir, "1.json"))
	if err != nil {
		t.Fatalf("read back task 1: %v", err)
	}
	if string(got) != live {
		t.Errorf("task 1 body = %s, want the live body %s — Restore must never clobber", got, live)
	}
	if res.Watermark != 2 {
		t.Errorf("watermark = %d, want 2 (the highest restored id)", res.Watermark)
	}
	wm, err := os.ReadFile(filepath.Join(dir, ".highwatermark"))
	if err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if string(wm) != "2" {
		t.Errorf(".highwatermark = %q, want %q (bare decimal, no newline)", wm, "2")
	}
}

// TestRestore_WatermarkNeverLowered guards against handing a resumed session an
// id that already exists on disk.
func TestRestore_WatermarkNeverLowered(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".highwatermark"), []byte("9"), 0o600); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}
	res, err := Restore(dir, []Task{{ID: "1", Raw: json.RawMessage(`{"id":"1"}`)}})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.Watermark != 9 {
		t.Errorf("watermark = %d, want 9 — an existing higher watermark stands", res.Watermark)
	}
}

// TestRestore_ReplaysRawBodies checks that a field forgectl does not model
// survives a snapshot/restore round trip.
func TestRestore_ReplaysRawBodies(t *testing.T) {
	src := t.TempDir()
	const body = `{"id":"1","subject":"s","futureField":{"nested":true}}`
	if err := os.WriteFile(filepath.Join(src, "1.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tasks := ReadTaskDir(src)
	if len(tasks) != 1 {
		t.Fatalf("ReadTaskDir returned %d tasks, want 1", len(tasks))
	}

	dst := t.TempDir()
	if _, err := Restore(dst, tasks); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "1.json"))
	if err != nil {
		t.Fatalf("read restored: %v", err)
	}
	if string(got) != body {
		t.Errorf("restored body = %s, want the original bytes %s", got, body)
	}
}

// TestSnapshot_LearnsTaskDirAndKeepsDeletedTasks is the core of the feature.
//
// Claude Code deletes task JSON when a session ends, so a later snapshot
// legitimately sees fewer tasks than an earlier one. Following the live set
// down to empty would discard exactly what the snapshot exists to keep.
func TestSnapshot_LearnsTaskDirAndKeepsDeletedTasks(t *testing.T) {
	const id = "eeeeeeee-0000-0000-0000-000000000001"
	f := newFixture(t)
	pinPids(t, map[int]bool{777: true})
	cwd := t.TempDir()
	f.history(id, cwd, "prompt", time.Now())
	f.registryFile(777, id, cwd, "renamed-session")

	taskDir := filepath.Join(f.Paths.tasksDir(), id)
	f.task(taskDir, "1", "first")
	f.task(taskDir, "2", "second")

	res := Snapshot(f.Paths, time.Now())
	if len(res.Errs) != 0 {
		t.Fatalf("Snapshot errors: %v", res.Errs)
	}
	if res.Sessions != 1 || res.Tasks != 2 {
		t.Fatalf("Snapshot captured %d sessions / %d tasks, want 1 / 2", res.Sessions, res.Tasks)
	}
	if res.Learned != 1 {
		t.Errorf("Learned = %d, want 1 — the session→task-dir pairing is recorded on first sight", res.Learned)
	}

	rec, ok := Load(f.Paths.StoreDir, id)
	if !ok {
		t.Fatal("no store record written")
	}
	if rec.TaskDir != taskDir {
		t.Errorf("TaskDir = %q, want %q", rec.TaskDir, taskDir)
	}
	if rec.Name != "renamed-session" || rec.NameSource != NameFromRename {
		t.Errorf("name = %q/%q, want renamed-session/%s", rec.Name, rec.NameSource, NameFromRename)
	}

	// Claude Code deletes the tasks; the next snapshot must not follow.
	if err := os.RemoveAll(taskDir); err != nil {
		t.Fatalf("simulate task deletion: %v", err)
	}
	res = Snapshot(f.Paths, time.Now())
	if len(res.Errs) != 0 {
		t.Fatalf("second Snapshot errors: %v", res.Errs)
	}
	rec, ok = Load(f.Paths.StoreDir, id)
	if !ok {
		t.Fatal("store record vanished on re-snapshot")
	}
	if len(rec.Tasks) != 2 {
		t.Fatalf("after deletion the store holds %d tasks, want 2 — the snapshot must not follow Claude Code down to zero", len(rec.Tasks))
	}

	// And the rescue path puts them back where a resumed session reads.
	rres, err := RestoreFor(f.Paths, id)
	if err != nil {
		t.Fatalf("RestoreFor: %v", err)
	}
	if rres.Written != 2 {
		t.Errorf("RestoreFor wrote %d tasks, want 2", rres.Written)
	}
	if rres.Dir != TaskDirFor(f.Paths, id) {
		t.Errorf("restored into %q, want the per-session dir %q", rres.Dir, TaskDirFor(f.Paths, id))
	}
}

// TestSnapshot_ResolvesTeamTaskDirByCwd covers the dialect that broke the
// original design: a session whose tasks live in a TEAM directory named after
// a different session id entirely. cwd is the only available join.
func TestSnapshot_ResolvesTeamTaskDirByCwd(t *testing.T) {
	const id = "ffffffff-0000-0000-0000-000000000001"
	const teamDir = "session-abcd1234"
	f := newFixture(t)
	pinPids(t, map[int]bool{888: true})
	cwd := t.TempDir()
	f.history(id, cwd, "prompt", time.Now())
	f.registryFile(888, id, cwd, "team-session")
	f.write(filepath.Join(f.Paths.teamsDir(), teamDir, "config.json"),
		`{"name":"`+teamDir+`","leadSessionId":"abcd1234-1111-2222-3333-444444444444",`+
			`"members":[{"agentId":"team-lead","cwd":"`+cwd+`"}]}`)
	f.task(filepath.Join(f.Paths.tasksDir(), teamDir), "1", "team task")

	res := Snapshot(f.Paths, time.Now())
	if len(res.Errs) != 0 {
		t.Fatalf("Snapshot errors: %v", res.Errs)
	}
	rec, ok := Load(f.Paths.StoreDir, id)
	if !ok {
		t.Fatal("no store record written")
	}
	want := filepath.Join(f.Paths.tasksDir(), teamDir)
	if rec.TaskDir != want {
		t.Fatalf("TaskDir = %q, want the team dir %q — cwd is the only join for this dialect", rec.TaskDir, want)
	}
	if len(rec.Tasks) != 1 {
		t.Fatalf("captured %d tasks from the team dir, want 1", len(rec.Tasks))
	}

	// The rescue replays into the PER-SESSION dir, because that is the one
	// the resumed session reads.
	rres, err := RestoreFor(f.Paths, id)
	if err != nil {
		t.Fatalf("RestoreFor: %v", err)
	}
	if rres.Dir != TaskDirFor(f.Paths, id) {
		t.Errorf("restored into %q, want %q", rres.Dir, TaskDirFor(f.Paths, id))
	}
	if rres.Written != 1 {
		t.Errorf("restored %d tasks, want 1", rres.Written)
	}
}

// TestSnapshot_WillNotStealAnotherSessionsTaskDir covers the cross-contamination
// the cwd heuristic would otherwise allow. Two sessions in one checkout each
// have a team; cwd cannot tell their task directories apart, and the loser
// would absorb the winner's tasks — then hand them to a resumed session as its
// own.
func TestSnapshot_WillNotStealAnotherSessionsTaskDir(t *testing.T) {
	const idA = "aaaa1111-0000-0000-0000-000000000001"
	const idB = "bbbb2222-0000-0000-0000-000000000002"
	const teamDir = "session-abcd1234"
	f := newFixture(t)
	pinPids(t, map[int]bool{901: true})
	cwd := t.TempDir()

	f.history(idB, cwd, "prompt", time.Now())
	f.registryFile(901, idB, cwd, "session-b")
	f.write(filepath.Join(f.Paths.teamsDir(), teamDir, "config.json"),
		`{"name":"`+teamDir+`","members":[{"cwd":"`+cwd+`"}]}`)
	f.task(filepath.Join(f.Paths.tasksDir(), teamDir), "1", "belongs to A")

	// Session A already owns that directory.
	if err := Save(f.Paths.StoreDir, &Record{
		ID: idA, Cwd: cwd, TaskDir: filepath.Join(f.Paths.tasksDir(), teamDir),
	}); err != nil {
		t.Fatalf("Save A: %v", err)
	}

	Snapshot(f.Paths, time.Now())

	rec, ok := Load(f.Paths.StoreDir, idB)
	if !ok {
		t.Fatal("no record written for session B")
	}
	if rec.TaskDir != "" {
		t.Errorf("session B claimed %q, which session A already owns — a wrong guess would harden into a wrong fact", rec.TaskDir)
	}
	if len(rec.Tasks) != 0 {
		t.Errorf("session B absorbed %d of session A's tasks", len(rec.Tasks))
	}
}

// TestFirstLine_ClipsByRune guards the --json path: a byte slice through a
// multi-byte character leaves invalid UTF-8, which json.Marshal silently
// rewrites to U+FFFD.
func TestFirstLine_ClipsByRune(t *testing.T) {
	got := firstLine(strings.Repeat("é", promptWidth+20))
	if !utf8.ValidString(got) {
		t.Fatalf("firstLine produced invalid UTF-8: %q", got)
	}
	if n := len([]rune(got)); n != promptWidth+1 { // +1 for the ellipsis
		t.Errorf("firstLine clipped to %d runes, want %d", n, promptWidth+1)
	}
	if got := firstLine("first line\nsecond line"); got != "first line" {
		t.Errorf("firstLine = %q, want just the first line", got)
	}
}

// TestRepoName keeps a session with no recorded cwd from showing a bare "." in
// the picker's repo column — filepath.Base's answer for an empty path.
func TestRepoName(t *testing.T) {
	if got := repoName(""); got != "" {
		t.Errorf("repoName(\"\") = %q, want empty", got)
	}
	if got := repoName("/work/forgectl"); got != "forgectl" {
		t.Errorf("repoName = %q, want forgectl", got)
	}
}

// TestDialect pins the classification the drift check rests on. The unknown
// case matters most: if a third naming convention read as DialectSession, the
// tripwire would report no drift while Restore wrote where nothing reads.
func TestDialect(t *testing.T) {
	cases := map[string]string{
		"0b0c37da-da2a-4e89-a65f-3e17640a68e5": DialectSession,
		"session-571bdef3":                     DialectTeam,
		"conv_01JABCDEF":                       DialectUnknown,
		"tasklist.2026-07-30":                  DialectUnknown,
	}
	for name, want := range cases {
		if got := Dialect(name); got != want {
			t.Errorf("Dialect(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestDriftCheck_FiresOnAnUnknownDialect is the case the tripwire exists for:
// Claude Code moves to a naming convention forgectl has never seen, so no
// per-session directory is left and Restore's target is dead.
func TestDriftCheck_FiresOnAnUnknownDialect(t *testing.T) {
	f := newFixture(t)
	f.task(filepath.Join(f.Paths.tasksDir(), "conv_01JABCDEF"), "1", "future")
	d := DriftCheck(f.Paths)
	if !d.Drifted() {
		t.Fatalf("DriftCheck = %+v, want drifted — an unrecognized dialect must not read as the safe one", d)
	}
	if d.Newest != DialectUnknown {
		t.Errorf("Newest = %q, want %q", d.Newest, DialectUnknown)
	}
}

// TestReadTranscript_SurvivesAnOverlongLine is the regression guard for a
// bufio.Scanner trap: a token past the cap does not skip, it stops the scan
// unrecoverably. A single pasted blob early in a transcript would then cost the
// session both its branch and its title.
func TestReadTranscript_SurvivesAnOverlongLine(t *testing.T) {
	const id = "dddd4444-0000-0000-0000-000000000001"
	f := newFixture(t)
	cwd := t.TempDir()
	huge := `{"type":"user","pasted":"` + strings.Repeat("x", transcriptBufSize*3) + `"}`
	lines := []string{
		huge,
		`{"type":"user","gitBranch":"feat/after-the-blob"}`,
		`{"type":"ai-title","aiTitle":"found-after-the-blob","sessionId":"` + id + `"}`,
	}
	f.write(filepath.Join(f.Paths.projectsDir(), slugify(cwd), id+".jsonl"),
		strings.Join(lines, "\n")+"\n")

	branch, title := readTranscript(f.Paths, id, cwd)
	if branch != "feat/after-the-blob" {
		t.Errorf("branch = %q, want it read from past the over-long line", branch)
	}
	if title != "found-after-the-blob" {
		t.Errorf("title = %q, want it read from past the over-long line", title)
	}
}

// TestScan_StoreLastSeenWidensLastActive covers the ordering asymmetry: a
// snapshot is written every turn, so it can be newer than the last recorded
// prompt — and after the registry entry is pruned it may be the only surviving
// activity signal.
func TestScan_StoreLastSeenWidensLastActive(t *testing.T) {
	const stale = "eeee5555-0000-0000-0000-000000000001"
	const fresh = "eeee5555-0000-0000-0000-000000000002"
	f := newFixture(t)
	now := time.Now()

	// `fresh` has the OLDER prompt but a newer snapshot; it must still sort first.
	f.history(fresh, "/repo/fresh", "old prompt", now.Add(-4*time.Hour))
	f.history(stale, "/repo/stale", "newer prompt", now.Add(-2*time.Hour))
	if err := Save(f.Paths.StoreDir, &Record{ID: fresh, Cwd: "/repo/fresh", LastSeen: now}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Scan(f.Paths, Opts{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) == 0 || got[0].ID != fresh {
		t.Fatalf("first row = %q, want %q — a fresher snapshot must widen LastActive", firstID(got), fresh)
	}
}

// TestDriftCheck covers the tripwire for the version coupling: a tasks tree
// with any per-session directory is fine; one with only team directories means
// Restore would write where nothing reads.
func TestDriftCheck(t *testing.T) {
	t.Run("no task dirs is not a verdict", func(t *testing.T) {
		f := newFixture(t)
		if d := DriftCheck(f.Paths); d.Checked || d.Drifted() {
			t.Errorf("DriftCheck on an empty tree = %+v, want unchecked", d)
		}
	})

	t.Run("per-session dir present is no drift", func(t *testing.T) {
		f := newFixture(t)
		f.task(filepath.Join(f.Paths.tasksDir(), "session-abcd1234"), "1", "team")
		f.task(filepath.Join(f.Paths.tasksDir(), "11111111-2222-3333-4444-555555555555"), "1", "mine")
		d := DriftCheck(f.Paths)
		if !d.Checked || d.Drifted() {
			t.Errorf("DriftCheck = %+v, want checked and not drifted — the two dialects coexist by design", d)
		}
	})

	t.Run("only team dirs is drift", func(t *testing.T) {
		f := newFixture(t)
		f.task(filepath.Join(f.Paths.tasksDir(), "session-abcd1234"), "1", "team")
		d := DriftCheck(f.Paths)
		if !d.Drifted() {
			t.Errorf("DriftCheck = %+v, want drifted", d)
		}
		if d.Restores != DialectSession || d.Newest != DialectTeam {
			t.Errorf("DriftCheck = %+v, want restores=%s newest=%s", d, DialectSession, DialectTeam)
		}
	})
}

// TestSave_RejectsBadSessionID keeps a store path from being built out of
// anything but a session-id shape.
func TestSave_RejectsBadSessionID(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"", "../escape", "a/b", "id.with.dots", strings.Repeat("a", 65)} {
		if err := Save(dir, &Record{ID: bad}); err == nil {
			t.Errorf("Save accepted session id %q", bad)
		}
	}
}

// TestScan_TolerantOfMissingHome checks the cold-start path: a machine with no
// ~/.claude yet lists nothing rather than failing.
func TestScan_TolerantOfMissingHome(t *testing.T) {
	p := Paths{ClaudeHome: filepath.Join(t.TempDir(), "absent"), StoreDir: filepath.Join(t.TempDir(), "store")}
	got, err := Scan(p, Opts{})
	if err != nil {
		t.Fatalf("Scan with no ~/.claude: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Scan returned %d sessions from an empty home, want 0", len(got))
	}
}

// TestSlugify pins the project-directory encoding against the real names
// observed on disk.
func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"/Users/cameron/Projects/claude-configurations/forgectl": "-Users-cameron-Projects-claude-configurations-forgectl",
		"/Users/cameron/.dotfiles":                               "-Users-cameron--dotfiles",
		"/private/tmp/drill-396-proj":                            "-private-tmp-drill-396-proj",
	}
	for cwd, want := range cases {
		if got := slugify(cwd); got != want {
			t.Errorf("slugify(%q) = %q, want %q", cwd, got, want)
		}
	}
}
