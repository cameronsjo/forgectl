// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/tasks"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// doneWriteToken is the value the fake keychain holds under the write entry.
// It differs from tasksTestFakeToken, the read entry's value, so a test can
// tell which entry a request's credential came from.
const doneWriteToken = "tk_" + "feedfacefeedfacefeedfacefeedfacefeedface"

const doneTaskID = 7

// doneTask is the one task the fake board serves.
func doneTask(overrides map[string]any) []byte {
	task := map[string]any{
		"id":           doneTaskID,
		"title":        "ship the closer",
		"description":  "what the task is about",
		"done":         false,
		"project_id":   3,
		"priority":     2,
		"repeat_after": 0,
		"repeat_mode":  0,
	}
	for k, v := range overrides {
		task[k] = v
	}
	// termsafe:allow-raw-json a test fixture served by an httptest server, never terminal output
	raw, err := json.Marshal(task)
	if err != nil {
		panic(err)
	}
	return raw
}

// doneBoard is a one-task Vikunja for the `tasks done` tests. GET serves
// `current`; POST stores what it was sent, as a server that applies an update
// in full does. onGet and onPost change either answer.
type doneBoard struct {
	t  *testing.T
	mu sync.Mutex

	current []byte
	gets    int
	posts   [][]byte
	auth    []string

	onGet  func(n int) (status int, body []byte)
	onPost func(body []byte) (status int, next []byte)
}

var doneTaskPath = regexp.MustCompile(`^/tasks/\d+$`)

func (b *doneBoard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.auth = append(b.auth, r.Header.Get("Authorization"))
	w.Header().Set("Content-Type", "application/json")
	if !doneTaskPath.MatchString(r.URL.Path) {
		b.t.Errorf("unexpected request %s %s: `tasks done` reads and updates one task and nothing else", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
		return
	}
	switch r.Method {
	case http.MethodGet:
		b.gets++
		if b.onGet != nil {
			if status, body := b.onGet(b.gets); status != 0 {
				w.WriteHeader(status)
				_, _ = w.Write(body)
				return
			}
		}
		_, _ = w.Write(b.current)
	case http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		b.posts = append(b.posts, body)
		status, next := http.StatusOK, body
		if b.onPost != nil {
			status, next = b.onPost(body)
		}
		if next != nil {
			b.current = next
		}
		w.WriteHeader(status)
		_, _ = w.Write(b.current)
	default:
		b.t.Errorf("unexpected method %s", r.Method)
		w.WriteHeader(http.StatusTeapot)
	}
}

func (b *doneBoard) requests() (gets, posts int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.gets, len(b.posts)
}

func (b *doneBoard) post(n int) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n >= len(b.posts) {
		b.t.Fatalf("no POST #%d was made (%d were)", n+1, len(b.posts))
	}
	var obj map[string]any
	if err := json.Unmarshal(b.posts[n], &obj); err != nil {
		b.t.Fatalf("POST body is not JSON: %v", err)
	}
	return obj
}

func (b *doneBoard) credentials() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.auth)
}

// doneRig is everything a `tasks done` test needs: a config directory under
// the test's own temp dir, a fake board behind the newTasksClient seam, and a
// fake keychain that answers per service name.
type doneRig struct {
	t      *testing.T
	board  *doneBoard
	runner *exec.FakeRunner
	deps   module.Deps
	// keychain maps a service name to the value `security` prints for it. A
	// service with no entry is an absent keychain item.
	keychain map[string]string
}

// isolateTasksConfigDir points the forgectl config directory at a temp dir.
// A `tasks` test that could reach the cache or the close log must call it: the
// alternative is a test run writing under the developer's real config dir.
func isolateTasksConfigDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	path, err := config.TasksCloseLogPath()
	if err != nil {
		t.Fatalf("config.TasksCloseLogPath: %v", err)
	}
	if !strings.HasPrefix(path, home) {
		t.Fatalf("the close log %q is not under the test home %q", path, home)
	}
	return home
}

func newDoneRig(t *testing.T, task []byte) *doneRig {
	t.Helper()
	isolateTasksConfigDir(t)
	rig := &doneRig{
		t:     t,
		board: &doneBoard{t: t, current: task},
		keychain: map[string]string{
			tasks.DefaultKeychainService:      tasksTestFakeToken,
			tasks.DefaultWriteKeychainService: doneWriteToken,
		},
	}
	srv := httptest.NewServer(rig.board)
	t.Cleanup(srv.Close)

	orig := newTasksClient
	newTasksClient = func(_ context.Context, _ exec.Runner, _ string, token tasks.Token) (*tasks.Client, error) {
		return tasks.NewClientForTesting(srv.URL, token), nil
	}
	t.Cleanup(func() { newTasksClient = orig })

	rig.runner = &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name != tasks.SecurityBinary {
				return "", nil
			}
			// find-generic-password -s <service> -w
			if len(args) < 3 {
				return "", errors.New("security: unexpected arguments")
			}
			value, ok := rig.keychain[args[2]]
			if !ok {
				return "", errors.New("security: the specified item could not be found in the keychain")
			}
			return value + "\n", nil
		},
	}
	rig.deps = module.Deps{Runner: rig.runner, Theme: theme.Default()}
	return rig
}

// keychainReads lists the service names read from the keychain, in order.
func keychainReads(runner *exec.FakeRunner) []string {
	var services []string
	for _, call := range runner.Calls {
		if call.Name != tasks.SecurityBinary {
			continue
		}
		service := "(no service argument)"
		if len(call.Args) >= 3 {
			service = call.Args[2]
		}
		services = append(services, service)
	}
	return services
}

// requireNoSubprocess fails when the runner was asked to run anything at all.
// A refusal that comes before the keychain read must come before every
// subprocess: a test that only looks for calls named tasks.SecurityBinary
// passes when the token is read through any other name.
func requireNoSubprocess(t *testing.T, runner *exec.FakeRunner, when string) {
	t.Helper()
	for _, call := range runner.Calls {
		t.Errorf("%s: a subprocess was run: %s %v", when, call.Name, call.Args)
	}
}

// refusalLines decodes every line of the close log and returns the host
// refusals among them. Any line that is not one JSON object fails the test.
func (r *doneRig) refusalLines() []map[string]any {
	r.t.Helper()
	var refusals []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(r.closeLog(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			r.t.Fatalf("a close log line is not one JSON object: %v\n%q", err, line)
		}
		if rec["event"] == tasks.HostRefusalEvent {
			refusals = append(refusals, rec)
		}
	}
	return refusals
}

func (r *doneRig) run(args ...string) (stdout, stderr string, err error) {
	r.t.Helper()
	return runTasksCmd(r.t, r.deps, args...)
}

// done runs `tasks done <doneTaskID> --evidence …` with extra args appended.
func (r *doneRig) done(extra ...string) (stdout, stderr string, err error) {
	r.t.Helper()
	args := append([]string{"done", fmt.Sprint(doneTaskID), "--evidence", "merged owner/repo#12"}, extra...)
	return r.run(args...)
}

func (r *doneRig) closeLog() string {
	r.t.Helper()
	path, err := config.TasksCloseLogPath()
	if err != nil {
		r.t.Fatalf("config.TasksCloseLogPath: %v", err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a path under the test's own temp dir
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		r.t.Fatalf("read the close log: %v", err)
	}
	return string(raw)
}

// splitRecords separates close-record lines from everything else on a stream.
// A record is one line of JSON that starts with its time field.
func splitRecords(t *testing.T, stream string) (records []map[string]any, rest string) {
	t.Helper()
	var other []string
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, `{"time":`) {
			other = append(other, line)
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a record line is not one JSON object: %v\n%s", err, line)
		}
		records = append(records, rec)
	}
	return records, strings.TrimSpace(strings.Join(other, "\n"))
}

func decodeFailure(t *testing.T, rest string) jsonFailureObject {
	t.Helper()
	var obj jsonFailureObject
	dec := json.NewDecoder(strings.NewReader(rest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&obj); err != nil {
		t.Fatalf("stderr without its record lines is not one failure object: %v\n%s", err, rest)
	}
	if dec.More() {
		t.Fatalf("stderr carries more than one failure object:\n%s", rest)
	}
	return obj
}

// TestTasksDone_ValidatesEverythingBeforeAnyKeychainRead: a call that is going
// to be refused for its own arguments must be refused before the write
// credential is read into the process at all.
func TestTasksDone_ValidatesEverythingBeforeAnyKeychainRead(t *testing.T) {
	id := fmt.Sprint(doneTaskID)
	for _, tc := range []struct {
		name     string
		args     []string
		wantExit int
		wantMsg  []string
	}{
		{"no --evidence", []string{"done", id}, 1, []string{"--evidence"}},
		{"a blank --evidence", []string{"done", id, "--evidence", "   "}, 1, []string{"--evidence"}},
		{"an id with a #", []string{"done", "#12", "--evidence", "x"}, 1, []string{"numeric id, without #"}},
		{"an id that is a word", []string{"done", "abc", "--evidence", "x"}, 1, []string{"numeric id, without #"}},
		{"an id of zero", []string{"done", "0", "--evidence", "x"}, 1, []string{"numeric id, without #"}},
		{"an id with a fraction", []string{"done", "1.5", "--evidence", "x"}, 1, []string{"numeric id, without #"}},
		{"evidence with a line break", []string{"done", id, "--evidence", "one\ntwo"}, 1, []string{"evidence"}},
		{"evidence over the limit", []string{"done", id, "--evidence", strings.Repeat("x", 301)}, 1, []string{"301", "300"}},
		{"evidence holding a token", []string{"done", id, "--evidence", "see " + doneWriteToken}, 1, []string{"token"}},
		{"an explicit --keychain-service", []string{"done", id, "--evidence", "x", "--keychain-service", "vikunja-readonly"}, 1, []string{"--write-keychain-service"}},
		{"an explicit --keychain-service naming the write entry", []string{"done", id, "--evidence", "x", "--keychain-service", "vikunja-write"}, 1, []string{"--write-keychain-service"}},
		{"a write service name with a space", []string{"done", id, "--evidence", "x", "--write-keychain-service", "two words"}, 1, []string{"--write-keychain-service"}},
		{"a write service name with a semicolon", []string{"done", id, "--evidence", "x", "--write-keychain-service", "a;b"}, 1, []string{"--write-keychain-service"}},
		{"an unlisted --host", []string{"done", id, "--evidence", "x", "--host", "other.example"}, exitTasksHostRefused, []string{"allowed_hosts"}},
	} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%v", tc.name, asJSON), func(t *testing.T) {
				rig := newDoneRig(t, doneTask(nil))
				args := tc.args
				if asJSON {
					args = append(slices.Clone(args), "--json")
				}
				_, stderr, err := rig.run(args...)
				if err == nil {
					t.Fatal("the call succeeded, want a refusal")
				}
				if got := ExitCode(err); got != tc.wantExit {
					t.Errorf("ExitCode = %d, want %d", got, tc.wantExit)
				}
				if reads := keychainReads(rig.runner); len(reads) != 0 {
					t.Errorf("the keychain was read (%v) before the arguments were refused", reads)
				}
				requireNoSubprocess(t, rig.runner, "a call refused on its arguments")
				if gets, posts := rig.board.requests(); gets+posts != 0 {
					t.Errorf("%d GET and %d POST were made for a call refused on its arguments", gets, posts)
				}
				message := err.Error()
				if asJSON {
					message = decodeFailure(t, stderr).Error
				}
				for _, want := range tc.wantMsg {
					if !strings.Contains(message, want) {
						t.Errorf("the refusal %q does not contain %q", message, want)
					}
				}
				for _, surface := range []string{message, stderr} {
					if strings.Contains(surface, doneWriteToken) {
						t.Errorf("a refusal echoes the token-shaped evidence: %q", surface)
					}
				}
				// A refused host is the one refusal that leaves a line behind,
				// and that line is not a close record: nothing was closed.
				log := rig.closeLog()
				if tc.wantExit == exitTasksHostRefused {
					if refusals := rig.refusalLines(); len(refusals) != 1 || strings.Count(log, "\n") != 1 {
						t.Errorf("a refused host left %d refusal line(s) in a close log of %d line(s), want one of one: %s",
							len(refusals), strings.Count(log, "\n"), log)
					}
				} else if log != "" {
					t.Errorf("a call refused on its arguments wrote to the close log: %s", log)
				}
				if strings.Contains(log, `"outcome"`) {
					t.Errorf("a refused call wrote a close record: %s", log)
				}
			})
		}
	}
}

func TestTasksDone_UsageFailuresCarryTheUsageCode(t *testing.T) {
	rig := newDoneRig(t, doneTask(nil))
	_, stderr, err := rig.run("done", "abc", "--evidence", "x", "--json")
	if err == nil {
		t.Fatal("want a refusal")
	}
	if got := decodeFailure(t, stderr).Code; got != jsonCodeUsage {
		t.Errorf("code = %q, want %q", got, jsonCodeUsage)
	}
}

// TestTasksDone_ReadsOnlyTheWriteEntry: the verb has its own credential, and
// the request carries that credential and not the read one.
func TestTasksDone_ReadsOnlyTheWriteEntry(t *testing.T) {
	rig := newDoneRig(t, doneTask(nil))
	if _, _, err := rig.done(); err != nil {
		t.Fatalf("done: %v", err)
	}
	if reads := keychainReads(rig.runner); !slices.Equal(reads, []string{tasks.DefaultWriteKeychainService}) {
		t.Errorf("keychain reads = %v, want only %s", reads, tasks.DefaultWriteKeychainService)
	}
	for _, header := range rig.board.credentials() {
		if header != "Bearer "+doneWriteToken {
			t.Errorf("a request carried a credential other than the write entry's")
		}
	}

	other := newDoneRig(t, doneTask(nil))
	other.keychain["second-board-write"] = doneWriteToken
	if _, _, err := other.done("--write-keychain-service", "second-board-write"); err != nil {
		t.Fatalf("done --write-keychain-service: %v", err)
	}
	if reads := keychainReads(other.runner); !slices.Equal(reads, []string{"second-board-write"}) {
		t.Errorf("keychain reads = %v, want only the named write entry", reads)
	}
}

// TestTasksDone_AnAbsentWriteEntryNeverFallsBackToTheReadEntry: with the read
// entry present and the write entry absent, the verb must stop. Falling back
// would send a credential the operator stored for reading to a write call.
func TestTasksDone_AnAbsentWriteEntryNeverFallsBackToTheReadEntry(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%v", asJSON), func(t *testing.T) {
			rig := newDoneRig(t, doneTask(nil))
			delete(rig.keychain, tasks.DefaultWriteKeychainService)

			var extra []string
			if asJSON {
				extra = []string{"--json"}
			}
			stdout, stderr, err := rig.done(extra...)
			if err == nil {
				t.Fatal("done with no write entry succeeded")
			}
			if got := ExitCode(err); got != 1 {
				t.Errorf("ExitCode = %d, want 1", got)
			}
			if reads := keychainReads(rig.runner); !slices.Equal(reads, []string{tasks.DefaultWriteKeychainService}) {
				t.Errorf("keychain reads = %v, want one read of the write entry and none of the read entry", reads)
			}
			if gets, posts := rig.board.requests(); gets+posts != 0 {
				t.Errorf("%d GET and %d POST were made with no write credential", gets, posts)
			}
			message := err.Error()
			if asJSON {
				failure := decodeFailure(t, stderr)
				if failure.Code != "credential_missing" {
					t.Errorf("code = %q, want credential_missing", failure.Code)
				}
				if stdout != "" {
					t.Errorf("stdout = %q, want empty on a failure", stdout)
				}
				message = failure.Error
			}
			for _, want := range []string{
				"--write-keychain-service",
				`security add-generic-password -s vikunja-write -a "$USER" -w`,
				"operator",
			} {
				if !strings.Contains(message, want) {
					t.Errorf("the message %q does not contain %q", message, want)
				}
			}
			// The command ends at -w: `security` then prompts for the value.
			// Anything after it would be read as the secret, on a command line.
			if !strings.HasSuffix(message, `-a "$USER" -w`) {
				t.Errorf("the setup command does not end the message at a bare -w: %q", message)
			}
			if strings.Contains(message, tasksTestFakeToken) || strings.Contains(stderr, tasksTestFakeToken) {
				t.Error("the refusal carries the read entry's token")
			}
		})
	}
}

func TestWriteCredentialSetupCommand_PrintsTheNameOnlyWhenItIsPlain(t *testing.T) {
	for _, name := range []string{"vikunja-write", "second.board_write-2", strings.Repeat("x", 64)} {
		want := `security add-generic-password -s ` + name + ` -a "$USER" -w`
		if got := writeCredentialSetupCommand(name); got != want {
			t.Errorf("writeCredentialSetupCommand(%q) = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"a;b", "two words", "", strings.Repeat("x", 65), "a$(id)", "a\nb", "a`b`", "a'b"} {
		got := writeCredentialSetupCommand(name)
		if got != `security add-generic-password -s <name> -a "$USER" -w` {
			t.Errorf("writeCredentialSetupCommand(%q) = %q, want the placeholder form", name, got)
		}
	}
}

// doneOutcome is one way a `tasks done` call can end, as a board behaviour.
type doneOutcome struct {
	name     string
	task     map[string]any
	arrange  func(*doneRig)
	wantExit int
	wantCode string // "" means the call succeeds
	// wantRecord is the record outcome when an update was sent, "" when none
	// was.
	wantRecord string
}

func doneOutcomes() []doneOutcome {
	return []doneOutcome{
		{name: "closed", wantRecord: tasks.CloseOutcomeClosed},
		{name: "already done", task: map[string]any{"done": true}},
		{
			name: "not found", wantExit: 1, wantCode: "not_found",
			arrange: func(r *doneRig) {
				r.board.onGet = func(int) (int, []byte) { return http.StatusNotFound, []byte(`{"message":"not found"}`) }
			},
		},
		{name: "repeating", task: map[string]any{"repeat_after": 3600}, wantExit: 1, wantCode: "repeating_task"},
		{
			name: "trailer too long", task: map[string]any{"description": strings.Repeat("d", 20_000)},
			wantExit: 1, wantCode: "trailer_too_long",
		},
		{
			name: "not confirmed", wantExit: 1, wantCode: "not_confirmed", wantRecord: tasks.CloseOutcomeNotConfirmed,
			arrange: func(r *doneRig) {
				// Accepted, and the task still reads back open.
				r.board.onPost = func([]byte) (int, []byte) { return http.StatusOK, nil }
			},
		},
		{
			name: "write refused", wantExit: 1, wantCode: "write_refused", wantRecord: tasks.CloseOutcomeWriteRefused,
			arrange: func(r *doneRig) {
				r.board.onPost = func([]byte) (int, []byte) { return http.StatusBadRequest, nil }
			},
		},
		{
			name: "update unauthorized", wantExit: exitTasksUnauthorized, wantCode: "unauthorized", wantRecord: tasks.CloseOutcomeUnauthorized,
			arrange: func(r *doneRig) {
				r.board.onPost = func([]byte) (int, []byte) { return http.StatusUnauthorized, nil }
			},
		},
		{
			name: "pre-read unauthorized", wantExit: exitTasksUnauthorized, wantCode: "unauthorized",
			arrange: func(r *doneRig) {
				r.board.onGet = func(int) (int, []byte) { return http.StatusUnauthorized, []byte(`{"message":"no"}`) }
			},
		},
		{
			name: "unreachable", wantExit: exitTasksUnreachable, wantCode: jsonCodeFailed,
			arrange: func(r *doneRig) {
				srv := httptest.NewServer(http.NotFoundHandler())
				addr := srv.URL
				srv.Close() // connection refused from here on
				newTasksClient = func(_ context.Context, _ exec.Runner, _ string, token tasks.Token) (*tasks.Client, error) {
					return tasks.NewClientForTesting(addr, token), nil
				}
			},
		},
		{
			name: "host refused at the client", wantExit: exitTasksHostRefused, wantCode: jsonCodeFailed,
			arrange: func(*doneRig) {
				newTasksClient = func(context.Context, exec.Runner, string, tasks.Token) (*tasks.Client, error) {
					return nil, fmt.Errorf("%w: test refusal", tasks.ErrHostRefused)
				}
			},
		},
	}
}

func newOutcomeRig(t *testing.T, oc doneOutcome) *doneRig {
	t.Helper()
	rig := newDoneRig(t, doneTask(oc.task))
	if oc.arrange != nil {
		oc.arrange(rig)
	}
	return rig
}

// seedTasksCache writes a cache a wrongful read or rewrite would show up in.
func seedTasksCache(t *testing.T) (path string, before []byte) {
	t.Helper()
	path, err := config.TasksCachePath()
	if err != nil {
		t.Fatalf("config.TasksCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := tasks.Snapshot{Tasks: []tasks.Task{{ID: doneTaskID, Title: "stale cached task", Done: true}}}
	if err := tasks.SaveCache(path, stale); err != nil {
		t.Fatal(err)
	}
	before, err = os.ReadFile(path) //nolint:gosec // G304: a path under the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	return path, before
}

// TestTasksDone_ExitCodesCodesRecordsAndCache drives every outcome in text and
// JSON mode and checks the four things a caller or an operator reads: the exit
// code, the --json failure code, the close record, and that the cache was
// neither read nor written.
func TestTasksDone_ExitCodesCodesRecordsAndCache(t *testing.T) {
	for _, oc := range doneOutcomes() {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%v", oc.name, asJSON), func(t *testing.T) {
				rig := newOutcomeRig(t, oc)
				cachePath, cacheBefore := seedTasksCache(t)

				var extra []string
				if asJSON {
					extra = []string{"--json"}
				}
				stdout, stderr, err := rig.done(extra...)

				if oc.wantCode == "" {
					if err != nil {
						t.Fatalf("done = %v, want success", err)
					}
				} else {
					if err == nil {
						t.Fatal("done succeeded, want a failure")
					}
					if got := ExitCode(err); got != oc.wantExit {
						t.Errorf("ExitCode = %d, want %d", got, oc.wantExit)
					}
				}

				records, rest := splitRecords(t, stderr)
				if oc.wantCode != "" && asJSON {
					if got := decodeFailure(t, rest).Code; got != oc.wantCode {
						t.Errorf("code = %q, want %q", got, oc.wantCode)
					}
					if stdout != "" {
						t.Errorf("stdout = %q, want empty on a --json failure", stdout)
					}
				}

				fileRecords, fileRest := splitRecords(t, rig.closeLog())
				if fileRest != "" {
					t.Errorf("the close log holds something other than record lines: %q", fileRest)
				}
				if oc.wantRecord == "" {
					if len(records) != 0 || len(fileRecords) != 0 {
						t.Fatalf("no update was sent, yet records were written: stderr %v, file %v", records, fileRecords)
					}
				} else {
					if len(records) != 1 || len(fileRecords) != 1 {
						t.Fatalf("want one record on stderr and one in the file, got %d and %d\nstderr: %s", len(records), len(fileRecords), stderr)
					}
					want := map[string]any{
						"task_id":    float64(doneTaskID),
						"project_id": float64(3),
						"surface":    tasks.SurfaceDone,
						"closer":     "cli",
						"evidence":   "merged owner/repo#12",
						"credential": tasks.DefaultWriteKeychainService,
						"host":       tasks.DefaultHost,
						"outcome":    oc.wantRecord,
					}
					for _, rec := range []map[string]any{records[0], fileRecords[0]} {
						for key, value := range want {
							if rec[key] != value {
								t.Errorf("record %s = %v, want %v", key, rec[key], value)
							}
						}
						if _, ok := rec["time"].(string); !ok {
							t.Errorf("record has no time: %v", rec)
						}
					}
					if fmt.Sprint(records[0]) != fmt.Sprint(fileRecords[0]) {
						t.Errorf("the stderr record and the file record differ:\n%v\n%v", records[0], fileRecords[0])
					}
				}
				if strings.Contains(stdout, `"outcome"`) {
					t.Errorf("a close record reached stdout: %q", stdout)
				}

				// The cache is neither a source nor a sink for this verb.
				cacheAfter, readErr := os.ReadFile(cachePath) //nolint:gosec // G304: a path under the test's own temp dir
				if readErr != nil {
					t.Fatalf("read the seeded cache: %v", readErr)
				}
				if !bytes.Equal(cacheBefore, cacheAfter) {
					t.Error("`tasks done` rewrote the cache")
				}
				if strings.Contains(stdout, "stale cached task") || strings.Contains(stderr, "stale cached task") {
					t.Error("`tasks done` served the cache")
				}
				if strings.Contains(stderr, "serving cached data") {
					t.Error("`tasks done` reported a cache fallback")
				}
			})
		}
	}
}

// TestTasksDone_WritesNoCacheOnAFreshConfigDir: with no cache present, a
// close leaves none behind.
func TestTasksDone_WritesNoCacheOnAFreshConfigDir(t *testing.T) {
	rig := newDoneRig(t, doneTask(nil))
	if _, _, err := rig.done(); err != nil {
		t.Fatalf("done: %v", err)
	}
	path, err := config.TasksCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a cache file exists after `tasks done` (stat err %v)", err)
	}
}

func TestTasksDone_JSONSuccessShape(t *testing.T) {
	wantKeys := []string{"already_done", "done", "evidence_recorded", "id", "project_id", "title"}

	decode := func(t *testing.T, stdout string) map[string]any {
		t.Helper()
		var got map[string]any
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
		}
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, wantKeys) {
			t.Errorf("keys = %v, want %v", keys, wantKeys)
		}
		return got
	}

	rig := newDoneRig(t, doneTask(nil))
	stdout, _, err := rig.done("--json")
	if err != nil {
		t.Fatalf("done --json: %v", err)
	}
	got := decode(t, stdout)
	for key, want := range map[string]any{
		"id": float64(doneTaskID), "project_id": float64(3), "title": "ship the closer",
		"done": true, "already_done": false, "evidence_recorded": true,
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}

	already := newDoneRig(t, doneTask(map[string]any{"done": true}))
	stdout, _, err = already.done("--json")
	if err != nil {
		t.Fatalf("done --json on a done task: %v", err)
	}
	got = decode(t, stdout)
	for key, want := range map[string]any{"done": true, "already_done": true, "evidence_recorded": false} {
		if got[key] != want {
			t.Errorf("already done: %s = %v, want %v", key, got[key], want)
		}
	}
	if _, posts := already.board.requests(); posts != 0 {
		t.Errorf("%d POST made for a task that was already done", posts)
	}
}

func TestTasksDone_TextOutput(t *testing.T) {
	t.Run("a clean close is one line", func(t *testing.T) {
		rig := newDoneRig(t, doneTask(map[string]any{"title": "ship \x1b[2Jthe closer"}))
		stdout, stderr, err := rig.done()
		if err != nil {
			t.Fatalf("done: %v", err)
		}
		lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
		if len(lines) != 1 {
			t.Fatalf("stdout has %d lines, want 1:\n%s", len(lines), stdout)
		}
		for _, want := range []string{"closed", fmt.Sprint(doneTaskID), "project 3", "the closer"} {
			if !strings.Contains(lines[0], want) {
				t.Errorf("the line %q does not contain %q", lines[0], want)
			}
		}
		if strings.ContainsRune(stdout, 0x1b) {
			t.Errorf("the title reached the terminal with its escape sequence intact: %q", stdout)
		}
		if strings.Contains(lines[0], "already") {
			t.Errorf("a fresh close is reported as already done: %q", lines[0])
		}
		records, _ := splitRecords(t, stderr)
		if len(records) != 1 {
			t.Errorf("want the record line on stderr, got %d", len(records))
		}
		if recs, _ := splitRecords(t, stdout); len(recs) != 0 {
			t.Error("the record line reached stdout")
		}
	})

	t.Run("already done says so", func(t *testing.T) {
		rig := newDoneRig(t, doneTask(map[string]any{"done": true}))
		stdout, _, err := rig.done()
		if err != nil {
			t.Fatalf("done: %v", err)
		}
		first, _, _ := strings.Cut(stdout, "\n")
		for _, want := range []string{"already done", fmt.Sprint(doneTaskID), "project 3", "ship the closer"} {
			if !strings.Contains(first, want) {
				t.Errorf("the line %q does not contain %q", first, want)
			}
		}
		if strings.HasPrefix(first, "closed") {
			t.Errorf("an already-done task is reported as closed by this call: %q", first)
		}
	})

	t.Run("evidence not recorded adds a second line", func(t *testing.T) {
		rig := newDoneRig(t, doneTask(nil))
		rig.board.onPost = func(body []byte) (int, []byte) {
			// The server marks the task done and drops the description it
			// was sent.
			return http.StatusOK, doneTask(map[string]any{"done": true})
		}
		stdout, _, err := rig.done()
		if err != nil {
			t.Fatalf("done: %v", err)
		}
		lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("stdout has %d lines, want 2:\n%s", len(lines), stdout)
		}
		if !strings.Contains(lines[1], "evidence") || !strings.Contains(lines[1], "not recorded") {
			t.Errorf("the second line %q does not say the evidence was not recorded", lines[1])
		}
	})

	t.Run("a change outside the expected keys adds a second line", func(t *testing.T) {
		rig := newDoneRig(t, doneTask(nil))
		rig.board.onPost = func(body []byte) (int, []byte) {
			var obj map[string]any
			if err := json.Unmarshal(body, &obj); err != nil {
				t.Errorf("POST body: %v", err)
			}
			obj["priority"] = 5
			obj["\x1b[2Jsurprise"] = 1
			// termsafe:allow-raw-json a test fixture served by an httptest server, never terminal output
			next, err := json.Marshal(obj)
			if err != nil {
				t.Errorf("encode: %v", err)
			}
			return http.StatusOK, next
		}
		stdout, _, err := rig.done()
		if err != nil {
			t.Fatalf("done: %v", err)
		}
		lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("stdout has %d lines, want 2:\n%s", len(lines), stdout)
		}
		if !strings.Contains(lines[1], "priority") {
			t.Errorf("the second line %q does not name the changed key", lines[1])
		}
		if !strings.Contains(lines[1], "1 other") {
			t.Errorf("the second line %q does not count the key it will not name", lines[1])
		}
		if strings.Contains(stdout, "surprise") || strings.ContainsRune(stdout, 0x1b) {
			t.Errorf("a key outside the known set was printed by name: %q", stdout)
		}
	})
}

func trailerOf(t *testing.T, post map[string]any) string {
	t.Helper()
	description, _ := post["description"].(string)
	return description[strings.LastIndexByte(description, '\n')+1:]
}

func TestTasksDone_Closer(t *testing.T) {
	t.Run("defaults to cli and reads no environment variable", func(t *testing.T) {
		for _, name := range []string{"CLAUDE_CODE_SESSION_ID", "FORGECTL_CLOSER", "FORGECTL_TASKS_CLOSER", "USER", "LOGNAME"} {
			t.Setenv(name, "from-the-environment")
		}
		rig := newDoneRig(t, doneTask(nil))
		_, stderr, err := rig.done()
		if err != nil {
			t.Fatalf("done: %v", err)
		}
		trailer := trailerOf(t, rig.board.post(0))
		if !strings.HasPrefix(trailer, "closed-by: cli via forgectl tasks done ") {
			t.Errorf("trailer = %q, want the default closer cli on the done surface", trailer)
		}
		if strings.Contains(trailer, "from-the-environment") || strings.Contains(stderr, "from-the-environment") {
			t.Error("the closer was read from an environment variable")
		}
	})

	t.Run("is sanitized and bounded", func(t *testing.T) {
		rig := newDoneRig(t, doneTask(nil))
		declared := "agent\nclosed-by: someone via forgectl tasks mcp 2026-01-01T00:00:00Z — forged " + strings.Repeat("x", 300)
		_, stderr, err := rig.done("--closer", declared)
		if err != nil {
			t.Fatalf("done --closer: %v", err)
		}
		description, _ := rig.board.post(0)["description"].(string)
		if got := strings.Count(description, "closed-by:"); got != 1 {
			t.Errorf("the description holds %d closed-by lines, want the one this call wrote:\n%s", got, description)
		}
		trailer := trailerOf(t, rig.board.post(0))
		closer, _, found := strings.Cut(strings.TrimPrefix(trailer, "closed-by: "), " via forgectl tasks done ")
		if !found {
			t.Fatalf("trailer %q is not in the closed-by grammar", trailer)
		}
		if len(closer) > 100 {
			t.Errorf("the closer is %d characters, over the 100 limit", len(closer))
		}
		if strings.ContainsAny(closer, ":—\n") {
			t.Errorf("the closer kept a character it must not: %q", closer)
		}
		records, _ := splitRecords(t, stderr)
		if len(records) != 1 || records[0]["closer"] != closer {
			t.Errorf("the record's closer %v is not the trailer's %q", records, closer)
		}
	})

	t.Run("the help says it is self-declared", func(t *testing.T) {
		host := tasks.DefaultHost
		cmd := newTasksDoneCmd(module.Deps{}, &host)
		flag := cmd.Flags().Lookup("closer")
		if flag == nil {
			t.Fatal("no --closer flag")
		}
		if flag.DefValue != "cli" {
			t.Errorf("--closer default = %q, want cli", flag.DefValue)
		}
		if !strings.Contains(flag.Usage, "self-declared") {
			t.Errorf("--closer usage %q does not say the name is self-declared", flag.Usage)
		}
		if !strings.Contains(cmd.Long, "self-declared") {
			t.Errorf("the help does not say the closer is self-declared:\n%s", cmd.Long)
		}
	})
}

// TestTasksSources_ReadNoEnvironmentVariable: the tasks commands take their
// inputs from flags, the config file and the keychain. An environment variable
// read here would be a second, unreviewed input to a credentialed write path.
func TestTasksSources_ReadNoEnvironmentVariable(t *testing.T) {
	for _, file := range []string{"tasks.go", "tasks_done.go", "tasks_mcp.go"} {
		src, err := os.ReadFile(file) //nolint:gosec // G304: a literal file name in this package's directory
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, forbidden := range []string{"os.Getenv", "os.LookupEnv", "os.Environ"} {
			if strings.Contains(string(src), forbidden) {
				t.Errorf("%s calls %s", file, forbidden)
			}
		}
	}
}

// TestTasksDone_TheRecordDoesNotDependOnTheLogger: forgectl's global logger
// discards everything unless log_level is set. The close record is not a log
// line and must come out all the same.
func TestTasksDone_TheRecordDoesNotDependOnTheLogger(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	closer := config.SetupLogger(config.Config{}) // the default: log_level unset
	t.Cleanup(func() { _ = closer.Close() })

	rig := newDoneRig(t, doneTask(nil))
	_, stderr, err := rig.done()
	if err != nil {
		t.Fatalf("done: %v", err)
	}
	records, _ := splitRecords(t, stderr)
	fileRecords, _ := splitRecords(t, rig.closeLog())
	if len(records) != 1 || len(fileRecords) != 1 {
		t.Fatalf("with the default logger: %d record(s) on stderr and %d in the file, want one each", len(records), len(fileRecords))
	}
	for _, rec := range []map[string]any{records[0], fileRecords[0]} {
		if rec["outcome"] != tasks.CloseOutcomeClosed {
			t.Errorf("record outcome = %v, want %s", rec["outcome"], tasks.CloseOutcomeClosed)
		}
		// A record is exactly these keys. A line that came through slog
		// carries level and msg and would not be one.
		keys := make([]string, 0, len(rec))
		for k := range rec {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		want := []string{"closer", "credential", "evidence", "host", "outcome", "project_id", "surface", "task_id", "time"}
		if !slices.Equal(keys, want) {
			t.Errorf("record keys = %v, want %v", keys, want)
		}
	}
}

// TestTasksDone_TokenAppearsNowhere plants a recognisable write token and
// greps every surface the verb can reach, across every outcome and both output
// modes: cobra's streams, the process's own streams, the returned error, the
// close log, and the cache.
func TestTasksDone_TokenAppearsNowhere(t *testing.T) {
	for _, oc := range doneOutcomes() {
		t.Run(oc.name, func(t *testing.T) {
			rig := newOutcomeRig(t, oc)
			cachePath, _ := seedTasksCache(t)

			var surfaces []string
			procOut, procErr := captureProcessStd(t, func() {
				for _, extra := range [][]string{nil, {"--json"}, {"--closer", "an agent"}} {
					stdout, stderr, err := rig.done(extra...)
					surfaces = append(surfaces, stdout, stderr)
					if err != nil {
						surfaces = append(surfaces, err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err))
					}
				}
			})
			surfaces = append(surfaces, procOut, procErr, rig.closeLog())
			cache, err := os.ReadFile(cachePath) //nolint:gosec // G304: a path under the test's own temp dir
			if err != nil {
				t.Fatal(err)
			}
			surfaces = append(surfaces, string(cache))

			for i, surface := range surfaces {
				if strings.Contains(surface, doneWriteToken) {
					t.Errorf("surface %d carries the write token:\n%s", i, surface)
				}
				if strings.Contains(surface, tasksTestFakeToken) {
					t.Errorf("surface %d carries the read token:\n%s", i, surface)
				}
			}
		})
	}
}

// TestTasksDone_AFailedFileAppendDoesNotFailTheClose: the task is closed on
// the board by the time the record is written. Reporting the call as failed
// would tell the caller to retry a close that happened.
func TestTasksDone_AFailedFileAppendDoesNotFailTheClose(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%v", asJSON), func(t *testing.T) {
			rig := newDoneRig(t, doneTask(nil))
			logPath, err := config.TasksCloseLogPath()
			if err != nil {
				t.Fatal(err)
			}
			// A directory where the log file belongs: the append cannot open it.
			if err := os.MkdirAll(logPath, 0o700); err != nil {
				t.Fatal(err)
			}

			var extra []string
			if asJSON {
				extra = []string{"--json"}
			}
			stdout, stderr, err := rig.done(extra...)
			if err != nil {
				t.Fatalf("done = %v, want success: the close itself happened", err)
			}
			if asJSON {
				var got map[string]any
				if jsonErr := json.Unmarshal([]byte(stdout), &got); jsonErr != nil || got["done"] != true {
					t.Errorf("stdout = %q, want the success object", stdout)
				}
			} else if !strings.Contains(stdout, "closed") {
				t.Errorf("stdout = %q, want the closed line", stdout)
			}

			records, rest := splitRecords(t, stderr)
			if len(records) != 1 {
				t.Errorf("want the record on stderr even though the file append failed, got %d", len(records))
			}
			notes := strings.Split(rest, "\n")
			if len(notes) != 1 || notes[0] == "" {
				t.Fatalf("want one plain line reporting the failed append, got %q", rest)
			}
			if !strings.Contains(notes[0], "close record") || !strings.Contains(notes[0], "tasks-closes.jsonl") {
				t.Errorf("the note %q does not say the close record could not be appended to its file", notes[0])
			}
			if strings.HasPrefix(notes[0], "{") {
				t.Errorf("the note is not a plain line: %q", notes[0])
			}
		})
	}
}

type failingRecordSink struct{ wrote int }

func (f *failingRecordSink) Write([]byte) (int, error) {
	f.wrote++
	return 0, errors.New("sink refused")
}

type shortRecordSink struct{}

func (shortRecordSink) Write(p []byte) (int, error) { return len(p) - 1, nil }

// TestRecordFanOut_AttemptsEverySinkAndReportsAnyFailure: one sink failing
// must not hide the record from the other, and must not be reported as a
// record that was written.
func TestRecordFanOut_AttemptsEverySinkAndReportsAnyFailure(t *testing.T) {
	line := []byte(`{"time":"x"}` + "\n")

	t.Run("both succeed", func(t *testing.T) {
		var a, b bytes.Buffer
		n, err := newRecordFanOut(&a, &b).Write(line)
		if err != nil || n != len(line) {
			t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(line))
		}
		if a.String() != string(line) || b.String() != string(line) {
			t.Errorf("sinks hold %q and %q, want the line in both", a.String(), b.String())
		}
	})

	t.Run("the first fails", func(t *testing.T) {
		first := &failingRecordSink{}
		var second bytes.Buffer
		_, err := newRecordFanOut(first, &second).Write(line)
		if err == nil {
			t.Fatal("Write = nil, want the first sink's failure")
		}
		if second.String() != string(line) {
			t.Errorf("the second sink holds %q; it must still be written after the first failed", second.String())
		}
	})

	t.Run("the second fails", func(t *testing.T) {
		var first bytes.Buffer
		second := &failingRecordSink{}
		_, err := newRecordFanOut(&first, second).Write(line)
		if err == nil {
			t.Fatal("Write = nil, want the second sink's failure")
		}
		if first.String() != string(line) || second.wrote != 1 {
			t.Errorf("first holds %q and the second was tried %d time(s), want both attempted", first.String(), second.wrote)
		}
	})

	t.Run("both fail", func(t *testing.T) {
		first, second := &failingRecordSink{}, &failingRecordSink{}
		_, err := newRecordFanOut(first, second).Write(line)
		if err == nil {
			t.Fatal("Write = nil, want a failure")
		}
		if first.wrote != 1 || second.wrote != 1 {
			t.Errorf("sinks were tried %d and %d time(s), want one each", first.wrote, second.wrote)
		}
	})

	t.Run("a short write is a failure", func(t *testing.T) {
		var other bytes.Buffer
		_, err := newRecordFanOut(shortRecordSink{}, &other).Write(line)
		if err == nil {
			t.Fatal("Write = nil for a sink that took fewer bytes than it was given")
		}
		if other.String() != string(line) {
			t.Error("the other sink was not written")
		}
	})

	t.Run("through WriteCloseRecord", func(t *testing.T) {
		var kept bytes.Buffer
		rec := tasks.CloseRecord{TaskID: 1, Surface: tasks.SurfaceDone, Evidence: "x", Outcome: tasks.CloseOutcomeClosed}
		if err := tasks.WriteCloseRecord(newRecordFanOut(&kept, &failingRecordSink{}), rec); err == nil {
			t.Error("WriteCloseRecord = nil when one sink failed")
		}
		if !strings.Contains(kept.String(), `"outcome":"closed"`) {
			t.Errorf("the working sink holds %q, want the record", kept.String())
		}
	})
}

func TestTasks_UnknownVerbExitsOneAndNamesIt(t *testing.T) {
	isolateTasksConfigDir(t)
	runner := &exec.FakeRunner{}
	deps := module.Deps{Runner: runner, Theme: theme.Default()}

	_, _, err := runTasksCmd(t, deps, "nosuchverb")
	if err == nil {
		t.Fatal("`tasks nosuchverb` = nil error, want a failure")
	}
	if got := ExitCode(err); got != 1 {
		t.Errorf("ExitCode = %d, want 1", got)
	}
	if !strings.Contains(err.Error(), "nosuchverb") {
		t.Errorf("the error %q does not name the unknown verb", err)
	}
	if len(runner.Calls) != 0 {
		t.Errorf("an unknown verb ran %d subprocess(es)", len(runner.Calls))
	}

	stdout, _, err := runTasksCmd(t, deps, []string{}...)
	if err != nil {
		t.Fatalf("`tasks` with no arguments = %v, want help and a nil error", err)
	}
	if !strings.Contains(stdout, "forgectl tasks ls") {
		t.Errorf("`tasks` with no arguments did not print its help:\n%s", stdout)
	}
}

// A mistyped verb is answered with the verb it is closest to. Without a
// minimum distance set on the `tasks` command, cobra suggests only verbs the
// typo is a prefix of, so `don` got a suggestion and `dne` did not.
func TestTasks_AMistypedVerbSuggestsTheNearestOne(t *testing.T) {
	isolateTasksConfigDir(t)
	runner := &exec.FakeRunner{}
	deps := module.Deps{Runner: runner, Theme: theme.Default()}

	for typo, want := range map[string]string{"dne": "done", "doen": "done", "raedy": "ready", "sohw": "show"} {
		_, _, err := runTasksCmd(t, deps, typo)
		if got := ExitCode(err); got != 1 {
			t.Errorf("`tasks %s`: ExitCode = %d, want 1", typo, got)
		}
		if err == nil || !strings.Contains(err.Error(), "Did you mean this?") {
			t.Errorf("`tasks %s` = %v, want a suggestion", typo, err)
			continue
		}
		_, suggested, _ := strings.Cut(err.Error(), "Did you mean this?")
		if !slices.Contains(strings.Fields(suggested), want) {
			t.Errorf("`tasks %s` suggested %q, want %s among them", typo, strings.TrimSpace(suggested), want)
		}
	}

	// A word near no verb gets no suggestion, and nothing is run either way.
	_, _, err := runTasksCmd(t, deps, "nosuchverb")
	if err == nil || strings.Contains(err.Error(), "Did you mean") {
		t.Errorf("`tasks nosuchverb` = %v, want no suggestion", err)
	}
	requireNoSubprocess(t, runner, "an unknown verb")
}

// The exit-code table in the help has to agree with docs/json-contract.md: a
// credential the server rejects on the update exits 3, like one rejected on
// the first read, and only the other refusals of an update exit 1.
func TestTasksDoneHelp_ExitCodesAndTheCloseLog(t *testing.T) {
	host := tasks.DefaultHost
	long := strings.Join(strings.Fields(newTasksDoneCmd(module.Deps{}, &host).Long), " ")
	for _, want := range []string{
		"an update the server refused for a reason other than the credential",
		"3 the server rejected the credential, on the first read or on the update",
		`"event":"host_refused"`,
		"It goes to the file only",
	} {
		if !strings.Contains(long, want) {
			t.Errorf("the done help does not say %q", want)
		}
	}
	if strings.Contains(long, "an update the server refused, an update") {
		t.Error("the done help still puts every refused update under exit 1")
	}

	contract, err := os.ReadFile(filepath.Join("..", "..", "docs", "json-contract.md"))
	if err != nil {
		t.Fatalf("read the JSON contract: %v", err)
	}
	for _, want := range []string{
		"| `unauthorized` | 3 | The server rejected the credential, on the first read or on the update.",
		"| `write_refused` | 1 |",
		`"event":"host_refused"`,
	} {
		if !strings.Contains(string(contract), want) {
			t.Errorf("docs/json-contract.md no longer says %q, so the help's exit codes have nothing to agree with", want)
		}
	}
}

// tasksVerbDrive says how the verb walk runs one `tasks` subcommand. Every
// subcommand must have an entry: a verb the walk does not know is a verb whose
// keychain use nobody has looked at.
type tasksVerbDrive struct {
	args []string
	// readsKeychain is true for a verb that reads a keychain credential when
	// run with args. The walk covers only those.
	readsKeychain bool
}

var tasksVerbDrives = map[string]tasksVerbDrive{
	"ls":    {args: []string{"ls"}, readsKeychain: true},
	"show":  {args: []string{"show", "1"}, readsKeychain: true},
	"ready": {args: []string{"ready"}, readsKeychain: true},
	"done":  {args: []string{"done", "1", "--evidence", "x"}, readsKeychain: true},
	// The stdio transport: with no --http, mcp reads the keychain.
	"mcp": {args: []string{"mcp"}, readsKeychain: true},
	// cobra's own additions, which run no forgectl code.
	"help":       {},
	"completion": {},
}

// TestTasksVerbs_AnUnlistedHostIsRefusedBeforeAnyKeychainRead walks the
// command's own subcommand list, so a verb added later is covered by this test
// or fails it. For each verb that reads the keychain: `--host other.example`
// must exit 4 without the keychain tool being run once.
func TestTasksVerbs_AnUnlistedHostIsRefusedBeforeAnyKeychainRead(t *testing.T) {
	probe := newTasksCmd(module.Deps{Runner: &exec.FakeRunner{}, Theme: theme.Default()})
	walked := 0
	for _, sub := range probe.Commands() {
		name := sub.Name()
		drive, known := tasksVerbDrives[name]
		if !known {
			t.Errorf("`tasks %s` has no entry in tasksVerbDrives: say how to run it and whether it reads the keychain", name)
			continue
		}
		if !drive.readsKeychain {
			continue
		}
		walked++
		t.Run(name, func(t *testing.T) {
			rig := newDoneRig(t, doneTask(nil))
			args := append(slices.Clone(drive.args), "--host", "other.example")
			stdout, stderr, err := rig.run(args...)
			if err == nil {
				t.Fatalf("`tasks %s --host other.example` succeeded", strings.Join(drive.args, " "))
			}
			if got := ExitCode(err); got != exitTasksHostRefused {
				t.Errorf("ExitCode = %d, want %d (host refused): %v", got, exitTasksHostRefused, err)
			}
			if !tasks.IsHostRefused(err) {
				t.Errorf("the error is not a host refusal: %v", err)
			}
			if !strings.Contains(err.Error(), tasks.AllowedHostsConfigKey) {
				t.Errorf("the refusal %q does not name the config key", err)
			}
			if reads := keychainReads(rig.runner); len(reads) != 0 {
				t.Errorf("the keychain was read %d time(s) (%v) for a host the credential may not go to", len(reads), reads)
			}
			requireNoSubprocess(t, rig.runner, "a refused host")
			if gets, posts := rig.board.requests(); gets+posts != 0 {
				t.Errorf("%d request(s) were made", gets+posts)
			}
			if strings.Contains(stdout, "stale cached task") {
				t.Error("a refused host was answered from cache")
			}

			// The refusal is recorded: one line in the close log, and nothing
			// else there. stderr carries the refusal itself and not the line.
			log := rig.closeLog()
			refusals := rig.refusalLines()
			if len(refusals) != 1 || strings.Count(log, "\n") != 1 {
				t.Fatalf("the close log holds %d refusal line(s) in %d line(s), want exactly one:\n%s",
					len(refusals), strings.Count(log, "\n"), log)
			}
			wantCredential := tasks.DefaultKeychainService
			if name == "done" {
				wantCredential = tasks.DefaultWriteKeychainService
			}
			stamp, _ := refusals[0]["time"].(string)
			if _, err := time.Parse(time.RFC3339, stamp); err != nil || !strings.HasSuffix(stamp, "Z") {
				t.Errorf("time = %q, want a UTC RFC3339 time", stamp)
			}
			delete(refusals[0], "time")
			want := map[string]any{
				"event":      "host_refused",
				"verb":       name,
				"host":       "other.example",
				"credential": wantCredential,
			}
			if !reflect.DeepEqual(refusals[0], want) {
				t.Errorf("refusal line = %v, want %v plus its time", refusals[0], want)
			}
			if strings.Contains(stderr, tasks.HostRefusalEvent) || strings.Contains(stdout, tasks.HostRefusalEvent) {
				t.Errorf("the refusal line was printed; it belongs in the file only:\nstdout %q\nstderr %q", stdout, stderr)
			}
		})
	}
	// The verbs this walk is known to cover today. A walk that found none
	// would pass having checked nothing.
	if walked < 5 {
		t.Errorf("the walk drove %d keychain-reading verb(s), want at least ls, show, ready, done and mcp", walked)
	}
}

// TestTasksVerbs_AListedHostIsAllowed: the config list is what widens the
// rule, and it widens it for the read verbs and the write verb alike.
func TestTasksVerbs_AListedHostIsAllowed(t *testing.T) {
	for _, args := range [][]string{
		{"done", fmt.Sprint(doneTaskID), "--evidence", "x", "--host", "other.example"},
		{"done", fmt.Sprint(doneTaskID), "--evidence", "x", "--host", "OTHER.Example"},
	} {
		rig := newDoneRig(t, doneTask(nil))
		rig.deps.Cfg = config.Config{Tasks: config.TasksConfig{AllowedHosts: []string{"other.example"}}}
		_, stderr, err := rig.run(args...)
		if err != nil {
			t.Fatalf("%v with the host listed = %v, want success", args, err)
		}
		if reads := keychainReads(rig.runner); len(reads) != 1 {
			t.Errorf("%v: keychain reads = %v, want one", args, reads)
		}
		records, _ := splitRecords(t, stderr)
		if len(records) != 1 || records[0]["host"] != args[len(args)-1] {
			t.Errorf("%v: record host = %v, want the host the call named", args, records)
		}
	}

	isolateTasksConfigDir(t)
	_, runner := withFakeTasksBackend(t, fakeVikunjaHandler(t))
	deps := module.Deps{
		Runner: runner, Theme: theme.Default(),
		Cfg: config.Config{Tasks: config.TasksConfig{AllowedHosts: []string{"other.example"}}},
	}
	for _, args := range [][]string{{"ls"}, {"show", "1"}, {"ready"}} {
		if _, _, err := runTasksCmd(t, deps, append(args, "--host", "other.example")...); err != nil {
			t.Errorf("%v with the host listed = %v, want success", args, err)
		}
		if _, _, err := runTasksCmd(t, deps, append(args, "--host", "third.example")...); ExitCode(err) != exitTasksHostRefused {
			t.Errorf("%v with another host = %v, want exit %d", args, err, exitTasksHostRefused)
		}
	}
}

// TestTasksVerbs_ABadKeychainServiceNameIsRefusedBeforeTheRead: the service
// name is handed to the keychain tool as an argument and printed in messages.
func TestTasksVerbs_ABadKeychainServiceNameIsRefusedBeforeTheRead(t *testing.T) {
	for _, args := range [][]string{
		{"ls", "--keychain-service", "a;b"},
		{"show", "1", "--keychain-service", "two words"},
		{"ready", "--keychain-service", strings.Repeat("x", 65)},
		{"mcp", "--keychain-service", "a/b"},
		{"ls", "--keychain-service", ""},
		{"done", "1", "--evidence", "x", "--write-keychain-service", "a$b"},
		{"done", "1", "--evidence", "x", "--write-keychain-service", ""},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			rig := newDoneRig(t, doneTask(nil))
			_, _, err := rig.run(args...)
			if err == nil {
				t.Fatal("the call succeeded, want a refusal")
			}
			if got := ExitCode(err); got != 1 {
				t.Errorf("ExitCode = %d, want 1", got)
			}
			if reads := keychainReads(rig.runner); len(reads) != 0 {
				t.Errorf("the keychain was read with a refused service name: %v", reads)
			}
			requireNoSubprocess(t, rig.runner, "a refused service name")
			if !strings.Contains(err.Error(), "keychain-service") {
				t.Errorf("the refusal %q does not name the flag", err)
			}
		})
	}
}

// TestTasksMCP_HTTPIsNotSubjectToTheHostRule: the HTTP transport reads a token
// file, never the keychain, and is bounded by its required --pin-ip list. An
// IP literal can never pass the host rule's grammar, so reaching the pin's own
// refusal shows the rule was not applied.
func TestTasksMCP_HTTPIsNotSubjectToTheHostRule(t *testing.T) {
	rig := newDoneRig(t, doneTask(nil))
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(doneWriteToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := rig.run("mcp", "--http", "127.0.0.1:0", "--token-file", tokenFile,
		"--pin-ip", "127.0.0.1", "--host", "127.0.0.1")
	if err == nil {
		t.Fatal("mcp --http against a loopback host started, want the pin's refusal")
	}
	if strings.Contains(err.Error(), tasks.AllowedHostsConfigKey) {
		t.Fatalf("the host rule was applied to the HTTP transport: %v", err)
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("mcp --http = %v, want the pin's loopback refusal", err)
	}
	if reads := keychainReads(rig.runner); len(reads) != 0 {
		t.Errorf("the HTTP transport read the keychain: %v", reads)
	}
	// The pin asks for the default gateway, and that is the only subprocess
	// this path may run. Matching on the keychain tool's name alone would miss
	// a read made through any other one.
	for _, call := range rig.runner.Calls {
		if call.Name != "/sbin/route" {
			t.Errorf("the HTTP transport ran %s %v; the gateway lookup is the only subprocess it may run", call.Name, call.Args)
		}
	}
}

// TestMCPServerConfig_StdioRecordsAlsoGoToTheCloseLog: a stdio server's stderr
// ends with the session, so its records are appended to the same file the
// `done` verb writes. The HTTP transport's durable record is its container
// log, and it writes no file.
func TestMCPServerConfig_StdioRecordsAlsoGoToTheCloseLog(t *testing.T) {
	rig := newDoneRig(t, doneTask(nil))
	rec := tasks.CloseRecord{TaskID: 1, Surface: tasks.SurfaceMCP, Evidence: "x", Outcome: tasks.CloseOutcomeClosed}

	var stderr bytes.Buffer
	cmd := newTasksCmd(rig.deps)
	cmd.SetErr(&stderr)

	if err := tasks.WriteCloseRecord(mcpServerConfig(cmd, ":3000", "some-entry", "board.example").Records, rec); err != nil {
		t.Fatalf("http: WriteCloseRecord: %v", err)
	}
	if !strings.Contains(stderr.String(), `"outcome":"closed"`) {
		t.Errorf("http: the record did not reach stderr: %q", stderr.String())
	}
	if log := rig.closeLog(); log != "" {
		t.Errorf("http: the record was appended to the close log: %q", log)
	}

	stderr.Reset()
	if err := tasks.WriteCloseRecord(mcpServerConfig(cmd, "", "some-entry", "board.example").Records, rec); err != nil {
		t.Fatalf("stdio: WriteCloseRecord: %v", err)
	}
	if !strings.Contains(stderr.String(), `"outcome":"closed"`) {
		t.Errorf("stdio: the record did not reach stderr: %q", stderr.String())
	}
	if log := rig.closeLog(); log != stderr.String() {
		t.Errorf("stdio: the close log holds %q, want the same line stderr got %q", log, stderr.String())
	}
}

func TestPrintTaskDetail_SanitizesARelationKind(t *testing.T) {
	var out bytes.Buffer
	task := tasks.Task{ID: 1, Title: "t", RelatedTasks: map[string][]tasks.Task{
		"blo\x1b[2Jcked\nforged row": {{ID: 2, Title: "other"}},
	}}
	if err := printTaskDetail(&out, task); err != nil {
		t.Fatalf("printTaskDetail: %v", err)
	}
	if strings.ContainsRune(out.String(), 0x1b) {
		t.Errorf("a relation kind reached the terminal with its escape sequence intact: %q", out.String())
	}
	if strings.Contains(out.String(), "\nforged row") {
		t.Errorf("a relation kind started a line of its own: %q", out.String())
	}
	if !strings.Contains(out.String(), "#2") {
		t.Errorf("the relation row is missing: %q", out.String())
	}
}

// The description is cut from the end, and the end is where the closed-by
// line is. A long description must still show who closed the task and why.
func TestPrintTaskDetail_ShowsTheLastLineOfALongDescription(t *testing.T) {
	trailer := "closed-by: hermes via forgectl tasks done 2026-01-02T03:04:05Z — merged owner/repo#12"
	body := strings.Repeat("d", 2000)

	render := func(t *testing.T, description string) string {
		t.Helper()
		var out bytes.Buffer
		if err := printTaskDetail(&out, tasks.Task{ID: 1, Title: "t", Description: description}); err != nil {
			t.Fatalf("printTaskDetail: %v", err)
		}
		return out.String()
	}
	const label = "description last line (the description above is truncated): "

	t.Run("a long description", func(t *testing.T) {
		for name, tail := range map[string]string{"no trailing break": "", "a trailing line feed": "\n", "trailing CRLF": "\r\n"} {
			out := render(t, body+"\n\n"+trailer+tail)
			if strings.Count(out, trailer) != 1 {
				t.Fatalf("%s: the output shows the closing line %d time(s), want once:\n%s", name, strings.Count(out, trailer), out)
			}
			if !strings.Contains(out, "\n"+label+trailer+"\n") {
				t.Errorf("%s: the closing line is not on its own labelled line:\n%s", name, out)
			}
			if !strings.Contains(out, "truncated]\n"+label) {
				t.Errorf("%s: the labelled line does not follow the cut description:\n%s", name, out)
			}
		}
	})

	t.Run("the last line is board text", func(t *testing.T) {
		out := render(t, body+"\nlast \x1b[2Jline")
		if strings.ContainsRune(out, 0x1b) {
			t.Errorf("the last line reached the terminal with its escape sequence intact: %q", out)
		}
		if !strings.Contains(out, label+"last ") {
			t.Errorf("the last line is missing: %q", out)
		}
		long := render(t, body+"\n"+strings.Repeat("z", 5000))
		if n := strings.Count(long, "z"); n > closingLineShowMaxRunes {
			t.Errorf("a long last line was printed past its cap (%d characters)", n)
		}
	})

	// A trailer at its limits is about 460 characters. The line exists to show
	// it, so it must come through whole.
	t.Run("a trailer at its limits is shown whole", func(t *testing.T) {
		full := "closed-by: " + strings.Repeat("c", 100) + " via forgectl tasks done 2026-01-02T03:04:05Z — " + strings.Repeat("e", 300)
		out := render(t, body+"\n\n"+full)
		if !strings.Contains(out, label+full+"\n") {
			t.Errorf("the closing line was cut:\n%s", out)
		}
	})

	t.Run("a short description is unchanged", func(t *testing.T) {
		out := render(t, "what the task is about\n\n"+trailer)
		if strings.Contains(out, "last line") {
			t.Errorf("a description shown whole got a last-line row:\n%s", out)
		}
		if want := "#1  t  [open]\n\nwhat the task is about\\n\\n" + trailer + "\n"; out != want {
			t.Errorf("output = %q, want %q", out, want)
		}
	})

	t.Run("a description at the limit is shown whole", func(t *testing.T) {
		out := render(t, strings.Repeat("d", textMaxRunes))
		if strings.Contains(out, "last line") || strings.Contains(out, "truncated") {
			t.Errorf("a description that fits was treated as cut:\n%s", out)
		}
	})

	// The cap counts what is printed, and a control character prints as
	// several runes. Whether the description was cut is not a question about
	// its own length.
	t.Run("the cut is measured on what is printed", func(t *testing.T) {
		for name, description := range map[string]string{
			"one rune over the limit":      strings.Repeat("d", textMaxRunes) + "\nend",
			"far over the limit":           strings.Repeat("d", 20_000) + "\nend",
			"short, and mostly escapes":    strings.Repeat("\x1b", textMaxRunes/2) + "\nend",
			"over the limit, CRLF between": strings.Repeat("d", textMaxRunes) + "\r\nend\r\n",
		} {
			out := render(t, description)
			if !strings.HasSuffix(out, label+"end\n") {
				t.Errorf("%s: the last line is not shown on its labelled line: %q", name, out[max(0, len(out)-120):])
			}
		}
		if descriptionProbeMaxRunes <= textMaxRunes {
			t.Errorf("descriptionProbeMaxRunes = %d must be larger than textMaxRunes = %d, or no description is ever seen as cut",
				descriptionProbeMaxRunes, textMaxRunes)
		}
	})
}

func TestTasksHelp_DescribesDoneAndTheSecondCredential(t *testing.T) {
	long := newTasksCmd(module.Deps{Theme: theme.Default()}).Long
	for _, want := range []string{
		"forgectl tasks done <id>",
		"--write-keychain-service",
		tasks.DefaultWriteKeychainService,
		tasks.DefaultKeychainService,
		"complete_task",
		"allowed_hosts",
	} {
		if !strings.Contains(long, want) {
			t.Errorf("the tasks help does not mention %q", want)
		}
	}
	for _, stale := range []string{"three read verbs over that data, and an MCP server", "also exposes create_task and\nadd_comment —"} {
		if strings.Contains(long, stale) {
			t.Errorf("the tasks help still says %q", stale)
		}
	}

	host, service := tasks.DefaultHost, tasks.DefaultKeychainService
	mcpLong := newTasksMCPCmd(module.Deps{}, &host, &service).Long
	if !strings.Contains(mcpLong, "allowed_hosts") || !strings.Contains(mcpLong, "--pin-ip") {
		t.Errorf("the mcp help does not say which transport the host list governs:\n%s", mcpLong)
	}
}
