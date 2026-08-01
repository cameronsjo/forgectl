package resume

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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

// hexDigits builds distinct session-id suffixes; validSessionID admits only
// hex and dashes, so a test generating ids has to stay inside that alphabet.
const hexDigits = "0123456789abcdef"

// storeRecord puts one record in the snapshot store. taskDir may be empty.
func (f *fixture) storeRecord(id, cwd string, lastSeen time.Time, taskDir string) {
	f.t.Helper()
	if err := Save(f.Paths.StoreDir, &Record{
		ID: id, Cwd: cwd, LastSeen: lastSeen, TaskDir: taskDir,
	}); err != nil {
		f.t.Fatalf("store record %s: %v", id, err)
	}
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

// TestScan_FilterFindsBeyondThePrefilterPool covers a false negative that read
// as certainty: the pool is cut before enrichment and filtering, so a session
// matching the filter but ranking outside the top Limit*prefilterFactor by
// global recency was not "shown lower" — it was absent, and the caller reported
// "no session matched".
func TestScan_FilterFindsBeyondThePrefilterPool(t *testing.T) {
	f := newFixture(t)
	base := time.Now()
	// The wanted session is the OLDEST of many, well past any pool bound.
	const wantedID = "beef0000-0000-0000-0000-000000000001"
	f.history(wantedID, "/work/needle", "the one", base.Add(-500*time.Hour))
	for i := range 40 {
		id := "cafe0000-0000-0000-0000-0000000000" + string(rune('a'+i/16)) + string(rune('a'+i%16))
		f.history(id, "/work/haystack", "noise", base.Add(-time.Duration(i)*time.Minute))
	}

	got, err := Scan(f.Paths, Opts{Filter: "needle", Limit: 2})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 || got[0].ID != wantedID {
		t.Fatalf("filter found %d session(s) (%q), want the one outside the pool — a bounded miss must not read as absent",
			len(got), firstID(got))
	}
}

// TestLiveEntries_IsDeterministic guards a coin flip with durable consequences:
// Snapshot awards a contested team task directory to whichever live session it
// processes first, and that claim is sticky forever. Ranging over a map made
// the winner depend on Go's iteration order rather than on the sessions.
func TestLiveEntries_IsDeterministic(t *testing.T) {
	f := newFixture(t)
	pinPids(t, map[int]bool{101: true, 102: true, 103: true})
	f.registryFile(101, "cccc0000-0000-0000-0000-000000000003", "/repo/c", "c")
	f.registryFile(102, "aaaa0000-0000-0000-0000-000000000001", "/repo/a", "a")
	f.registryFile(103, "bbbb0000-0000-0000-0000-000000000002", "/repo/b", "b")

	var first []string
	for range 8 {
		var ids []string
		for _, e := range LiveEntries(f.Paths) {
			ids = append(ids, e.SessionID)
		}
		if first == nil {
			first = ids
			continue
		}
		if !slices.Equal(ids, first) {
			t.Fatalf("LiveEntries order varies between calls: %v vs %v", ids, first)
		}
	}
	if !slices.IsSorted(first) {
		t.Errorf("LiveEntries = %v, want it sorted by session id", first)
	}
}

// TestCaptureState reports the two numbers doctor uses to detect an unwired
// Stop hook — the only machine-visible signature of a capture that never ran.
func TestCaptureState(t *testing.T) {
	f := newFixture(t)
	pinPids(t, map[int]bool{201: true})
	f.registryFile(201, "aaaa1111-0000-0000-0000-000000000001", "/repo/a", "a")

	live, stored := CaptureState(f.Paths)
	if live != 1 || stored != 0 {
		t.Fatalf("CaptureState = (%d live, %d stored), want (1, 0)", live, stored)
	}

	if res := Snapshot(f.Paths, time.Now()); len(res.Errs) != 0 {
		t.Fatalf("Snapshot: %v", res.Errs)
	}
	if live, stored = CaptureState(f.Paths); live != 1 || stored != 1 {
		t.Errorf("CaptureState after a snapshot = (%d, %d), want (1, 1)", live, stored)
	}
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

	branch, title := readTranscript(f.Paths, id, cwd, nil)
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
// anything but a session-id shape. The leading-dash cases are the argv half:
// "-c" and friends pass a bare hex-plus-dash charset and would reach
// `claude --resume <id>` as flag-shaped tokens.
func TestSave_RejectsBadSessionID(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{
		"", "../escape", "a/b", "id.with.dots", strings.Repeat("a", 65),
		"-c", "-d", "-a", "--fork-session",
	} {
		if err := Save(dir, &Record{ID: bad}); err == nil {
			t.Errorf("Save accepted session id %q", bad)
		}
	}
}

// TestLoad_RejectsAnIDThatDisagreesWithItsFilename covers the store's own
// admission point. LoadAll keys on the id INSIDE the record, so a body that
// disagreed with its filename would have put an unvalidated string into Scan's
// session map — reaching transcript path joins and claude's argv, the exact
// class the history and registry checks close.
func TestLoad_RejectsAnIDThatDisagreesWithItsFilename(t *testing.T) {
	dir := t.TempDir()
	const name = "aaaabbbb-0000-0000-0000-000000000001"
	body := `{"id":"../../../../etc/passwd","cwd":"/repo/evil","last_seen":"2026-07-30T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, ok := Load(dir, name); ok {
		t.Error("Load accepted a record whose body id disagrees with its filename")
	}
	if got := LoadAll(dir); len(got) != 0 {
		t.Errorf("LoadAll returned %d record(s), want 0", len(got))
	}
	for id := range LoadAll(dir) {
		if !validSessionID(id) {
			t.Errorf("LoadAll surfaced an invalid session id %q", id)
		}
	}
}

// TestScan_RejectsHostileIDsAtAdmission is the defense-in-depth check behind
// the security review's shared root: ids are validated where they ENTER the
// package, not at each of the several sinks they later reach (filepath.Join for
// transcripts and task dirs, and claude's argv).
func TestScan_RejectsHostileIDsAtAdmission(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	f.history("../../../../etc/passwd", "/repo/evil", "traversal", now)
	f.history("-c", "/repo/flag", "flag-shaped", now)
	f.history("ffffffff-0000-0000-0000-00000000900d", "/repo/good", "fine", now.Add(-time.Hour))

	got, err := Scan(f.Paths, Opts{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Scan admitted %d sessions, want only the well-formed one", len(got))
	}
	if got[0].Repo != "good" {
		t.Errorf("admitted %q, want the well-formed session", got[0].Repo)
	}
}

// TestScanHistory_SurvivesACorruptMiddleLine covers an availability bug that a
// json.Decoder stream made invisible: a Decoder does not resynchronize at line
// boundaries, so one bad record ended the whole read — and because the file is
// append-only, what that discarded was everything NEWER than the corruption.
func TestScanHistory_SurvivesACorruptMiddleLine(t *testing.T) {
	const newest = "cafe0001-0000-0000-0000-000000000001"
	f := newFixture(t)
	now := time.Now()
	f.history("cafe0002-0000-0000-0000-000000000002", "/repo/old", "old", now.Add(-2*time.Hour))

	// A corrupt record lands between the old session and the newest one.
	fh, err := os.OpenFile(f.Paths.historyPath(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open history: %v", err)
	}
	if _, err := fh.WriteString("{\"display\":\"truncated\",\n"); err != nil {
		t.Fatalf("write corruption: %v", err)
	}
	_ = fh.Close()

	f.history(newest, "/repo/new", "newest", now)

	got, err := Scan(f.Paths, Opts{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) == 0 || got[0].ID != newest {
		t.Fatalf("first row = %q, want %q — a corrupt middle line must not hide everything after it", firstID(got), newest)
	}
}

// TestRestore_WillNotFollowADanglingSymlink covers the specific hole in a
// Stat-then-WriteFile pair: os.Stat on a DANGLING symlink errors, so the
// already-present skip is missed and the write follows the link to create its
// target somewhere else. O_EXCL refuses to follow it.
func TestRestore_WillNotFollowADanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim.json")
	if err := os.Symlink(outside, filepath.Join(dir, "1.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	res, err := Restore(dir, []Task{{ID: "1", Raw: json.RawMessage(`{"id":"1"}`)}})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("Restore followed a dangling symlink and created the file outside the task directory")
	}
	if res.Written != 0 || res.Skipped != 1 {
		t.Errorf("Restore wrote %d / skipped %d, want 0 / 1 — an existing entry always wins", res.Written, res.Skipped)
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

// TestScan_ResumableSortsAboveLive is the ordering fix for the picker's worst
// habit: recency alone put running sessions on top, and a running session
// cannot be continued — so the list led with rows that answer a selection by
// refusing it. Live rows stay in the list (--fork makes them actionable), just
// below everything actually resumable.
func TestScan_ResumableSortsAboveLive(t *testing.T) {
	f := newFixture(t)
	pinPids(t, map[int]bool{501: true, 502: true})
	now := time.Now()

	// The two live sessions are the MOST recent — the realistic case.
	const liveNewest = "aaaa0000-0000-0000-0000-000000000001"
	const liveOlder = "aaaa0000-0000-0000-0000-000000000002"
	const deadNewer = "bbbb0000-0000-0000-0000-000000000003"
	const deadOldest = "bbbb0000-0000-0000-0000-000000000004"

	f.history(liveNewest, "/repo/live-a", "p", now)
	f.history(liveOlder, "/repo/live-b", "p", now.Add(-time.Minute))
	f.history(deadNewer, "/repo/dead-a", "p", now.Add(-time.Hour))
	f.history(deadOldest, "/repo/dead-b", "p", now.Add(-2*time.Hour))
	f.registryFile(501, liveNewest, "/repo/live-a", "live-a")
	f.registryFile(502, liveOlder, "/repo/live-b", "live-b")

	got, err := Scan(f.Paths, Opts{IncludeLive: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("Scan returned %d sessions, want 4", len(got))
	}
	// The two resumable rows come first, in recency order.
	if got[0].ID != deadNewer || got[1].ID != deadOldest {
		t.Errorf("resumable rows = %s, %s; want %s, %s (newest first)", got[0].ID, got[1].ID, deadNewer, deadOldest)
	}
	// The two live rows come last. Their order between themselves is NOT
	// asserted: a live session's LastActive is widened by the registry's
	// updatedAt, so it reflects current process activity rather than the last
	// recorded prompt — which is the point of that widening.
	live := map[string]bool{got[2].ID: true, got[3].ID: true}
	if !live[liveNewest] || !live[liveOlder] {
		t.Errorf("live rows = %s, %s; want the two running sessions last", got[2].ID, got[3].ID)
	}
	for i, s := range got {
		if s.Live != (i >= 2) {
			t.Errorf("row %d live=%v — every resumable row must sort above every live one", i, s.Live)
		}
	}
}

// --- pruning -----------------------------------------------------------
//
// What retires a record is TRANSCRIPT EXISTENCE, not age. The store is only
// useful for as long as `claude --resume` can still open the session, and that
// window is Claude Code's transcript retention — operator-configurable, and on
// this author's machine set well past the default. An age cut would delete
// snapshots for sessions that are still rescuable, which is the one outcome
// task rescue exists to prevent. TestPrune_KeepsARecordWhoseTranscriptSurvives
// pins that; the age constant survives only as a backstop.

// TestPrune_DropsRecordsWhoseTranscriptIsGone is the base case: Claude Code has
// aged the transcript out, so nothing can resume that session and its snapshot
// is dead weight.
func TestPrune_DropsRecordsWhoseTranscriptIsGone(t *testing.T) {
	const kept = "a1a1a1a1-0000-0000-0000-000000000001"
	const gone = "a1a1a1a1-0000-0000-0000-000000000002"
	f := newFixture(t)
	now := time.Now()
	keptCwd, goneCwd := t.TempDir(), t.TempDir()

	f.transcript(kept, keptCwd, "main", "still resumable")
	f.storeRecord(kept, keptCwd, now.Add(-40*24*time.Hour), "")
	f.storeRecord(gone, goneCwd, now.Add(-40*24*time.Hour), "")

	res := Prune(f.Paths, LoadAll(f.Paths.StoreDir), nil, now)
	if len(res.Errs) != 0 {
		t.Fatalf("Prune errors: %v", res.Errs)
	}
	if res.Removed != 1 {
		t.Fatalf("Removed = %d, want 1", res.Removed)
	}
	if _, ok := Load(f.Paths.StoreDir, gone); ok {
		t.Error("the record whose transcript is gone survived")
	}
	if _, ok := Load(f.Paths.StoreDir, kept); !ok {
		t.Error("the record whose transcript survives was deleted")
	}
}

// TestPrune_KeepsARecordWhoseTranscriptSurvives pins the deviation from the
// filed shape. This record is 100 days old — well past any age cut anyone would
// propose — and `claude --resume` can still open it, so deleting its snapshot
// would destroy a rescue that is still live.
func TestPrune_KeepsARecordWhoseTranscriptSurvives(t *testing.T) {
	const id = "a2a2a2a2-0000-0000-0000-000000000001"
	f := newFixture(t)
	now := time.Now()
	cwd := t.TempDir()

	f.transcript(id, cwd, "main", "old but resumable")
	f.storeRecord(id, cwd, now.Add(-100*24*time.Hour), "")

	res := Prune(f.Paths, LoadAll(f.Paths.StoreDir), nil, now)
	if res.Removed != 0 {
		t.Fatalf("Removed = %d, want 0 — age is not the key, the transcript is", res.Removed)
	}
	if _, ok := Load(f.Paths.StoreDir, id); !ok {
		t.Fatal("a 100-day-old record with a live transcript was deleted")
	}
}

// TestPrune_NeverDropsALiveSession covers the keep set. A session snapshotted
// this pass may have no transcript on disk yet, and it is the LAST record that
// may be deleted — keep also carries ids whose Save failed.
func TestPrune_NeverDropsALiveSession(t *testing.T) {
	const live = "a3a3a3a3-0000-0000-0000-000000000001"
	const other = "a3a3a3a3-0000-0000-0000-000000000002"
	f := newFixture(t)
	now := time.Now()
	otherCwd := t.TempDir()

	// `other` has a transcript, so the mass-delete guard is satisfied and the
	// only thing standing between `live` and deletion is the keep set.
	f.transcript(other, otherCwd, "main", "resumable")
	f.storeRecord(other, otherCwd, now, "")
	f.storeRecord(live, t.TempDir(), now, "")

	res := Prune(f.Paths, LoadAll(f.Paths.StoreDir), map[string]bool{live: true}, now)
	if res.Removed != 0 {
		t.Fatalf("Removed = %d, want 0", res.Removed)
	}
	if _, ok := Load(f.Paths.StoreDir, live); !ok {
		t.Fatal("a session written this pass was pruned")
	}
}

// TestPrune_SkipsEverythingWhenNoTranscriptResolves pins the mass-delete guard.
// Zero transcripts across the whole store is the signature of a broken lookup —
// an unreadable projects tree, a Claude Code layout change — not of N dead
// sessions, and trusting it would empty the store in one pass.
func TestPrune_SkipsEverythingWhenNoTranscriptResolves(t *testing.T) {
	ids := []string{
		"a4a4a4a4-0000-0000-0000-000000000001",
		"a4a4a4a4-0000-0000-0000-000000000002",
		"a4a4a4a4-0000-0000-0000-000000000003",
	}
	f := newFixture(t)
	now := time.Now()
	for _, id := range ids {
		f.transcript(id, t.TempDir(), "main", "resumable")
		f.storeRecord(id, t.TempDir(), now.Add(-40*24*time.Hour), "")
	}
	// The lookup breaks: every transcript disappears at once.
	if err := os.RemoveAll(f.Paths.projectsDir()); err != nil {
		t.Fatalf("empty the projects tree: %v", err)
	}
	if err := os.MkdirAll(f.Paths.projectsDir(), 0o700); err != nil {
		t.Fatalf("recreate the projects tree: %v", err)
	}

	res := Prune(f.Paths, LoadAll(f.Paths.StoreDir), nil, now)
	if res.Removed != 0 {
		t.Fatalf("Removed = %d, want 0 — zero hits is a broken lookup, not N dead sessions", res.Removed)
	}
	for _, id := range ids {
		if _, ok := Load(f.Paths.StoreDir, id); !ok {
			t.Errorf("record %s deleted on a zero-hit pass", id)
		}
	}
}

// TestPrune_AgeCeiling covers the backstop's two halves, and the first half is
// the one that matters: LastSeen ages from last USE, not from last
// resumability. A record can be ancient and still openable by
// `claude --resume`, so age NEVER overrides a transcript that resolved —
// deleting there would destroy the task bodies of a live rescue.
//
// What the ceiling actually does is release records whose transcript is GONE
// when the hits guard is refusing to act on that absence, so a machine whose
// lookup stays broken still retires its dead records eventually.
func TestPrune_AgeCeiling(t *testing.T) {
	t.Run("a resolving transcript outranks any age", func(t *testing.T) {
		const id = "a5a5a5a5-0000-0000-0000-000000000001"
		f := newFixture(t)
		now := time.Now()
		cwd := t.TempDir()

		f.transcript(id, cwd, "main", "ancient but resumable")
		f.storeRecord(id, cwd, now.Add(-200*24*time.Hour), "")

		res := Prune(f.Paths, LoadAll(f.Paths.StoreDir), nil, now)
		if res.Removed != 0 {
			t.Fatalf("Removed = %d, want 0 — the session is still resumable", res.Removed)
		}
		if _, ok := Load(f.Paths.StoreDir, id); !ok {
			t.Fatal("a 200-day-old record was deleted despite a transcript claude can still open")
		}
	})

	t.Run("releases a transcript-less record even when hits is zero", func(t *testing.T) {
		const id = "a5a5a5a5-0000-0000-0000-000000000002"
		f := newFixture(t)
		now := time.Now()

		// No transcript anywhere, so nothing vouches for the lookup and the
		// hits guard is blocking. The ceiling is the only release path left.
		f.storeRecord(id, t.TempDir(), now.Add(-200*24*time.Hour), "")

		res := Prune(f.Paths, LoadAll(f.Paths.StoreDir), nil, now)
		if res.Removed != 1 {
			t.Fatalf("Removed = %d, want 1 — past the ceiling with no transcript it goes", res.Removed)
		}
		if _, ok := Load(f.Paths.StoreDir, id); ok {
			t.Fatal("a 200-day-old transcript-less record survived the backstop")
		}
	})
}

// TestPrune_IsThrottled covers the cost guard. Prune runs from a Stop hook at
// every turn end; resolving transcripts for the whole store on each of those is
// a daily job's cost paid per turn.
func TestPrune_IsThrottled(t *testing.T) {
	const hit = "a6a6a6a6-0000-0000-0000-000000000001"
	const first = "a6a6a6a6-0000-0000-0000-000000000002"
	const second = "a6a6a6a6-0000-0000-0000-000000000003"
	f := newFixture(t)
	now := time.Now()
	hitCwd := t.TempDir()

	f.transcript(hit, hitCwd, "main", "resumable")
	f.storeRecord(hit, hitCwd, now, "")
	f.storeRecord(first, t.TempDir(), now.Add(-40*24*time.Hour), "")

	if res := Prune(f.Paths, LoadAll(f.Paths.StoreDir), nil, now); res.Removed != 1 {
		t.Fatalf("first pass Removed = %d, want 1", res.Removed)
	}

	// A second dead record appears; the next pass inside the interval must not
	// go looking for it.
	f.storeRecord(second, t.TempDir(), now.Add(-40*24*time.Hour), "")
	res := Prune(f.Paths, LoadAll(f.Paths.StoreDir), nil, now.Add(time.Hour))
	if res.Removed != 0 || res.Orphans != 0 {
		t.Fatalf("second pass = %+v, want a no-op inside the throttle interval", res)
	}
	if _, ok := Load(f.Paths.StoreDir, second); !ok {
		t.Fatal("the throttled pass pruned anyway")
	}
}

// TestPrune_SweepsOrphanTempFiles covers two leaks nothing else can see: a .tmp
// from a Save that died between CreateTemp and Rename, and a .json that no
// longer parses (LoadAll skips it, so it is invisible to every other reader).
// The grace period is what keeps the sweep from racing a concurrent Save.
func TestPrune_SweepsOrphanTempFiles(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	stale := now.Add(-2 * time.Hour)

	staleTmp := filepath.Join(f.Paths.StoreDir, "a7a7a7a7-0000-0000-0000-000000000001.123.tmp")
	staleJSON := filepath.Join(f.Paths.StoreDir, "a7a7a7a7-0000-0000-0000-000000000002.json")
	freshTmp := filepath.Join(f.Paths.StoreDir, "a7a7a7a7-0000-0000-0000-000000000003.456.tmp")
	freshJSON := filepath.Join(f.Paths.StoreDir, "a7a7a7a7-0000-0000-0000-000000000004.json")
	for _, path := range []string{staleTmp, staleJSON, freshTmp, freshJSON} {
		f.write(path, "{ truncated")
	}
	for _, path := range []string{staleTmp, staleJSON} {
		if err := os.Chtimes(path, stale, stale); err != nil {
			t.Fatalf("age %s: %v", path, err)
		}
	}

	res := Prune(f.Paths, LoadAll(f.Paths.StoreDir), nil, now)
	if len(res.Errs) != 0 {
		t.Fatalf("Prune errors: %v", res.Errs)
	}
	if res.Orphans != 2 {
		t.Fatalf("Orphans = %d, want 2", res.Orphans)
	}
	for _, path := range []string{staleTmp, staleJSON} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("stale orphan %s survived", filepath.Base(path))
		}
	}
	for _, path := range []string{freshTmp, freshJSON} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("fresh file %s was swept — the grace period must not race a live Save", filepath.Base(path))
		}
	}
}

// TestSnapshot_ReportsPruned is the end-to-end wiring: the Stop hook's own pass
// bounds the store, and says so — with retired records and deleted debris kept
// as SEPARATE counts, because "a record I wanted is gone" and "a temp file was
// cleaned up" are not the same event and a sum cannot be taken apart later.
func TestSnapshot_ReportsPruned(t *testing.T) {
	const live = "a8a8a8a8-0000-0000-0000-000000000001"
	const hit = "a8a8a8a8-0000-0000-0000-000000000002"
	const dead = "a8a8a8a8-0000-0000-0000-000000000003"
	f := newFixture(t)
	pinPids(t, map[int]bool{999: true})
	now := time.Now()
	liveCwd, hitCwd := t.TempDir(), t.TempDir()

	f.history(live, liveCwd, "prompt", now)
	f.registryFile(999, live, liveCwd, "running")
	f.transcript(hit, hitCwd, "main", "resumable")
	f.storeRecord(hit, hitCwd, now.Add(-40*24*time.Hour), "")
	f.storeRecord(dead, t.TempDir(), now.Add(-40*24*time.Hour), "")

	// One piece of debris alongside, so the two counts can be told apart.
	tmp := filepath.Join(f.Paths.StoreDir, "a8a8a8a8-0000-0000-0000-000000000009.77.tmp")
	f.write(tmp, "{ truncated")
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(tmp, old, old); err != nil {
		t.Fatalf("age the temp file: %v", err)
	}

	res := Snapshot(f.Paths, now)
	if len(res.Errs) != 0 {
		t.Fatalf("Snapshot errors: %v", res.Errs)
	}
	if res.Sessions != 1 {
		t.Fatalf("Sessions = %d, want 1", res.Sessions)
	}
	if res.Pruned != 1 {
		t.Fatalf("Pruned = %d, want 1 — records retired", res.Pruned)
	}
	if res.Swept != 1 {
		t.Fatalf("Swept = %d, want 1 — debris deleted, counted apart from records", res.Swept)
	}
	if _, ok := Load(f.Paths.StoreDir, dead); ok {
		t.Error("the dead record survived a snapshot pass")
	}
	if _, ok := Load(f.Paths.StoreDir, live); !ok {
		t.Error("the live session's own record was pruned by its own snapshot")
	}
}

// TestPrune_TreatsAZeroLastSeenAsUnknownAge is the age half's mass-delete
// guard. A record whose last_seen is missing — written by a forgectl that
// predates the field, or truncated before it was written — unmarshals to the
// zero time, which reads as ~2000 years old. Without this the backstop deletes
// it on the first pass however live its transcript is.
func TestPrune_TreatsAZeroLastSeenAsUnknownAge(t *testing.T) {
	const zeroed = "a9a9a9a9-0000-0000-0000-000000000001"
	const dated = "a9a9a9a9-0000-0000-0000-000000000002"
	f := newFixture(t)
	now := time.Now()

	// Neither has a transcript, so hits is zero and the age ceiling is the only
	// thing that can retire either one. The control is the point: `dated` MUST
	// go, so the survival of `zeroed` is the guard and not an inert pass.
	f.write(filepath.Join(f.Paths.StoreDir, zeroed+".json"), `{"id":"`+zeroed+`","cwd":"/gone"}`)
	f.storeRecord(dated, "/gone", now.Add(-200*24*time.Hour), "")
	store := LoadAll(f.Paths.StoreDir)
	if _, ok := store[zeroed]; !ok {
		t.Fatalf("fixture record did not load")
	}

	res := Prune(f.Paths, store, nil, now)
	if res.Removed != 1 {
		t.Fatalf("Removed = %d, want 1 — only the dated record is past the ceiling", res.Removed)
	}
	if _, ok := Load(f.Paths.StoreDir, zeroed); !ok {
		t.Fatal("a record with no last_seen was aged out — a missing field is unknown age, not ancient")
	}
	if _, ok := Load(f.Paths.StoreDir, dated); ok {
		t.Fatal("the 200-day-old control survived, so this test proves nothing")
	}
}

// TestPrune_SweepKeepsAValidRecordMissingFromTheStoreMap is the orphan sweep's
// mass-delete guard. The store map is a caller's argument; a stale or
// wrong-directory one would make every valid record on disk look like debris.
// Deletion is earned by failing to LOAD, never by absence from that map.
func TestPrune_SweepKeepsAValidRecordMissingFromTheStoreMap(t *testing.T) {
	const id = "b1b1b1b1-0000-0000-0000-000000000001"
	f := newFixture(t)
	now := time.Now()
	cwd := t.TempDir()

	f.transcript(id, cwd, "main", "resumable")
	f.storeRecord(id, cwd, now, "")
	path := filepath.Join(f.Paths.StoreDir, id+".json")
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("age the record: %v", err)
	}

	// An empty store map — the shape a wrong-directory LoadAll would produce.
	res := Prune(f.Paths, map[string]*Record{}, nil, now)
	if res.Orphans != 0 {
		t.Fatalf("Orphans = %d, want 0 — a loadable record is not debris", res.Orphans)
	}
	if _, ok := Load(f.Paths.StoreDir, id); !ok {
		t.Fatal("a valid record was swept because the caller's store map did not list it")
	}
}

// TestPrune_FollowsASymlinkedProjectDir covers a false-absent channel the hits
// guard cannot see. os.ReadDir fills DirEntry.Type() from the directory entry,
// not the link target, so a symlinked project directory reports IsDir() ==
// false and would drop out of the index entirely — making every session inside
// it read as transcript-less, which is a deletion verdict.
func TestPrune_FollowsASymlinkedProjectDir(t *testing.T) {
	const linked = "c1c1c1c1-0000-0000-0000-000000000001"
	const plain = "c1c1c1c1-0000-0000-0000-000000000002"
	f := newFixture(t)
	now := time.Now()
	plainCwd := t.TempDir()

	// `plain` resolves normally and supplies the hit, so the guard is open and
	// `linked` is genuinely at risk.
	f.transcript(plain, plainCwd, "main", "resolves normally")
	f.storeRecord(plain, plainCwd, now, "")

	// `linked` lives behind a symlinked project dir, under a name the cwd slug
	// cannot guess — so only the directory list can find it.
	realDir := filepath.Join(t.TempDir(), "elsewhere")
	f.write(filepath.Join(realDir, linked+".jsonl"), `{"type":"user","gitBranch":"main"}`+"\n")
	link := filepath.Join(f.Paths.projectsDir(), "-Volumes-external-project")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	f.storeRecord(linked, t.TempDir(), now, "")

	// Pin the premise this guard exists for, rather than assuming it.
	entries, err := os.ReadDir(f.Paths.projectsDir())
	if err != nil {
		t.Fatalf("read projects: %v", err)
	}
	sawSymlinkNotDir := false
	for _, e := range entries {
		if e.Name() == "-Volumes-external-project" && !e.IsDir() {
			sawSymlinkNotDir = true
		}
	}
	if !sawSymlinkNotDir {
		t.Fatal("premise broken: os.ReadDir reported the symlinked dir as a directory, so this test proves nothing")
	}

	res := Prune(f.Paths, LoadAll(f.Paths.StoreDir), nil, now)
	if res.Removed != 0 {
		t.Fatalf("Removed = %d, want 0 — the transcript is there, behind a symlink", res.Removed)
	}
	if _, ok := Load(f.Paths.StoreDir, linked); !ok {
		t.Fatal("a record whose transcript lives in a symlinked project dir was deleted")
	}
}

// TestPrune_RefusesAPassOverTheCap is the control for every PARTIAL lookup
// failure. The hits guard only catches a TOTAL one: a single transcript
// resolving anywhere authorizes deleting every record that did not. The cap
// needs no theory about which channel broke — retiring most of a store at once
// is not something a working prune does.
//
// It must REFUSE, not merely count: nothing deleted, and an error saying why.
func TestPrune_RefusesAPassOverTheCap(t *testing.T) {
	f := newFixture(t)
	now := time.Now()

	const hit = "c2c2c2c2-0000-0000-0000-000000000000"
	hitCwd := t.TempDir()
	f.transcript(hit, hitCwd, "main", "the lone survivor of a broken lookup")
	f.storeRecord(hit, hitCwd, now, "")

	var doomed []string
	for i := 1; i <= 23; i++ {
		id := "c2c2c2c2-0000-0000-0000-0000000000" + string(hexDigits[i/16]) + string(hexDigits[i%16])
		f.storeRecord(id, t.TempDir(), now, "")
		doomed = append(doomed, id)
	}

	store := LoadAll(f.Paths.StoreDir)
	if len(store) != 24 {
		t.Fatalf("fixture built %d records, want 24", len(store))
	}

	res := Prune(f.Paths, store, nil, now)
	if res.Removed != 0 {
		t.Fatalf("Removed = %d, want 0 — the pass must REFUSE, not delete a capped subset", res.Removed)
	}
	if len(res.Errs) == 0 {
		t.Fatal("the refusal was silent — an operator must be told why nothing was pruned")
	}
	if !strings.Contains(res.Errs[0].Error(), "REFUSED") {
		t.Errorf("refusal error = %q, want it to say so plainly", res.Errs[0])
	}
	for _, id := range doomed {
		if _, ok := Load(f.Paths.StoreDir, id); !ok {
			t.Fatalf("record %s was deleted by a pass that was over the cap", id)
		}
	}
}

// TestPrune_SweepRefusesWhenMostOfTheStoreIsUnparseable applies the same cap to
// the orphan sweep. A directory that suddenly reads as mostly garbage is a
// storage problem, not that much debris.
func TestPrune_SweepRefusesWhenMostOfTheStoreIsUnparseable(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	old := now.Add(-2 * time.Hour)

	const live = "c3c3c3c3-0000-0000-0000-000000000000"
	liveCwd := t.TempDir()
	f.transcript(live, liveCwd, "main", "healthy")
	f.storeRecord(live, liveCwd, now, "")

	var junk []string
	for i := 1; i <= 23; i++ {
		name := "c3c3c3c3-0000-0000-0000-0000000000" + string(hexDigits[i/16]) + string(hexDigits[i%16]) + ".json"
		path := filepath.Join(f.Paths.StoreDir, name)
		f.write(path, "{ not a record")
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("age %s: %v", name, err)
		}
		junk = append(junk, path)
	}

	res := Prune(f.Paths, LoadAll(f.Paths.StoreDir), nil, now)
	if res.Orphans != 0 {
		t.Fatalf("Orphans = %d, want 0 — the sweep must REFUSE past the cap", res.Orphans)
	}
	if len(res.Errs) == 0 {
		t.Fatal("the sweep refused silently")
	}
	for _, path := range junk {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was deleted by a sweep that was over the cap", filepath.Base(path))
		}
	}
}

// TestPrune_SweepKeepsAnUnreadableRecord separates the two ways a record fails
// to load. Unreadable is an environment problem that clears on its own — an I/O
// error, an ownership change, exhausted descriptors — and the record underneath
// may be perfectly good. Only unparseable bytes are debris.
func TestPrune_SweepKeepsAnUnreadableRecord(t *testing.T) {
	const id = "c4c4c4c4-0000-0000-0000-000000000001"
	f := newFixture(t)
	now := time.Now()
	cwd := t.TempDir()

	f.storeRecord(id, cwd, now, "")
	path := filepath.Join(f.Paths.StoreDir, id+".json")
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("age the record: %v", err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("make unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("running with privileges that ignore file modes; cannot stage an unreadable file")
	}

	// An empty store map, so the file reaches the load check that decides it.
	res := Prune(f.Paths, map[string]*Record{}, nil, now)
	if res.Orphans != 0 {
		t.Fatalf("Orphans = %d, want 0 — unreadable is not corrupt", res.Orphans)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("a record that merely could not be READ was deleted as debris")
	}
}

// TestPrune_HonorsTheKillSwitch covers the operator's opt-out of the one part
// of this package that deletes their data.
func TestPrune_HonorsTheKillSwitch(t *testing.T) {
	const hit = "c5c5c5c5-0000-0000-0000-000000000001"
	const dead = "c5c5c5c5-0000-0000-0000-000000000002"
	f := newFixture(t)
	now := time.Now()
	hitCwd := t.TempDir()

	f.transcript(hit, hitCwd, "main", "resumable")
	f.storeRecord(hit, hitCwd, now, "")
	f.storeRecord(dead, t.TempDir(), now.Add(-40*24*time.Hour), "")

	paths := f.Paths
	paths.NoPrune = true
	res := Prune(paths, LoadAll(f.Paths.StoreDir), nil, now)
	if res.Removed != 0 || res.Orphans != 0 || len(res.Errs) != 0 {
		t.Fatalf("Prune = %+v, want a total no-op under the kill switch", res)
	}
	if _, ok := Load(f.Paths.StoreDir, dead); !ok {
		t.Fatal("a record was pruned with the kill switch set")
	}
	if _, err := os.Stat(filepath.Join(f.Paths.StoreDir, pruneMarker)); err == nil {
		t.Error("the disabled pass still stamped the marker, which would mask a later re-enable")
	}

	// The control: the same store prunes with the switch off.
	if res := Prune(f.Paths, LoadAll(f.Paths.StoreDir), nil, now); res.Removed != 1 {
		t.Fatalf("control pass Removed = %d, want 1 — otherwise the switch proves nothing", res.Removed)
	}
}

// TestPrune_IsNotParkedByAFutureDatedMarker covers clock skew and restores from
// backup. A negative age is not "recent" — read that way, pruning stops until
// real time catches up with the stamp.
func TestPrune_IsNotParkedByAFutureDatedMarker(t *testing.T) {
	const hit = "c6c6c6c6-0000-0000-0000-000000000001"
	const dead = "c6c6c6c6-0000-0000-0000-000000000002"
	f := newFixture(t)
	now := time.Now()
	hitCwd := t.TempDir()

	f.transcript(hit, hitCwd, "main", "resumable")
	f.storeRecord(hit, hitCwd, now, "")
	f.storeRecord(dead, t.TempDir(), now.Add(-40*24*time.Hour), "")

	marker := filepath.Join(f.Paths.StoreDir, pruneMarker)
	f.write(marker, "from the future\n")
	ahead := now.Add(72 * time.Hour)
	if err := os.Chtimes(marker, ahead, ahead); err != nil {
		t.Fatalf("date the marker forward: %v", err)
	}

	res := Prune(f.Paths, LoadAll(f.Paths.StoreDir), nil, now)
	if res.Removed != 1 {
		t.Fatalf("Removed = %d, want 1 — a future-dated marker must read as never, not as recent", res.Removed)
	}
}
