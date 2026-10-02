// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/tasks"
)

// A host is whatever follows --host. One holding a line break must still
// produce one line in the close log, with the host inside its own field.
func TestTasksVerbs_ARefusedHostWithALineBreakIsOneLine(t *testing.T) {
	for _, host := range []string{
		"other.example\n{\"time\":\"2026-01-01T00:00:00Z\",\"event\":\"host_refused\",\"verb\":\"forged\"}",
		"other.example\r\nsecond line",
	} {
		t.Run(fmt.Sprintf("%q", host), func(t *testing.T) {
			rig := newDoneRig(t, doneTask(nil))
			_, _, err := rig.run("ls", "--host", host)
			if got := ExitCode(err); got != exitTasksHostRefused {
				t.Fatalf("ExitCode = %d, want %d: %v", got, exitTasksHostRefused, err)
			}
			requireNoSubprocess(t, rig.runner, "a refused host")

			log := rig.closeLog()
			if strings.Count(log, "\n") != 1 || !strings.HasSuffix(log, "\n") {
				t.Fatalf("the close log is not exactly one line:\n%q", log)
			}
			refusals := rig.refusalLines()
			if len(refusals) != 1 {
				t.Fatalf("want one refusal line, got %d:\n%q", len(refusals), log)
			}
			// A host with a line break is not a plain hostname, so the line
			// carries the marker and none of the text.
			if refusals[0]["host"] != "[not a plain hostname]" {
				t.Errorf("host = %q, want the marker for a value that is not a plain hostname", refusals[0]["host"])
			}
			if strings.Contains(log, "second line") || strings.Contains(log, "forged") {
				t.Errorf("text typed after --host reached the close log: %q", log)
			}
			if refusals[0]["verb"] != "ls" {
				t.Errorf("verb = %v, want ls: the host reached another field", refusals[0]["verb"])
			}
		})
	}
}

// The service name is recorded only when it is one this client would read.
func TestTasksVerbs_ARefusedHostRecordsNoUnusableServiceName(t *testing.T) {
	rig := newDoneRig(t, doneTask(nil))
	_, _, err := rig.run("ls", "--host", "other.example", "--keychain-service", "a;b "+doneWriteToken)
	if got := ExitCode(err); got != exitTasksHostRefused {
		t.Fatalf("ExitCode = %d, want %d: %v", got, exitTasksHostRefused, err)
	}
	refusals := rig.refusalLines()
	if len(refusals) != 1 || refusals[0]["credential"] != "" {
		t.Errorf("refusal lines = %v, want one with a blank credential", refusals)
	}
	if strings.Contains(rig.closeLog(), doneWriteToken) {
		t.Error("text typed after --keychain-service reached the close log")
	}
}

// A refusal that cannot be recorded is still a refusal: the exit code and the
// error are unchanged, and one plain line on stderr says the record is missing.
func TestTasksVerbs_AHostRefusalThatCannotBeRecordedStillExitsFour(t *testing.T) {
	blockers := map[string]func(t *testing.T, logPath string){
		"the log directory cannot be written": func(t *testing.T, logPath string) {
			if os.Geteuid() == 0 {
				t.Skip("root writes into a read-only directory")
			}
			dir := filepath.Dir(logPath)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // G302: a directory, made read-only on purpose
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // G302: restore the test's own temp dir so it can be removed
		},
		"a directory sits where the log belongs": func(t *testing.T, logPath string) {
			if err := os.MkdirAll(logPath, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, block := range blockers {
		for _, args := range [][]string{
			{"ls"},
			{"done", "1", "--evidence", "x"},
			{"done", "1", "--evidence", "x", "--json"},
		} {
			t.Run(name+"/"+strings.Join(args, " "), func(t *testing.T) {
				rig := newDoneRig(t, doneTask(nil))
				logPath, err := config.TasksCloseLogPath()
				if err != nil {
					t.Fatal(err)
				}
				block(t, logPath)

				_, stderr, err := rig.run(append(args, "--host", "other.example")...)
				if got := ExitCode(err); got != exitTasksHostRefused {
					t.Fatalf("ExitCode = %d, want %d: %v", got, exitTasksHostRefused, err)
				}
				asJSON := args[len(args)-1] == "--json"
				// Under --json the refusal has already been written to stderr
				// as the failure object, and the returned error carries only
				// the exit code.
				if !asJSON && (!tasks.IsHostRefused(err) || !strings.Contains(err.Error(), tasks.AllowedHostsConfigKey)) {
					t.Errorf("the failed record hid the refusal: %v", err)
				}
				requireNoSubprocess(t, rig.runner, "a refused host")

				lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
				note := lines[0]
				if !strings.Contains(note, "could not be recorded") || !strings.Contains(note, "tasks-closes.jsonl") {
					t.Errorf("stderr's first line %q does not say the refusal could not be recorded in the close log", note)
				}
				if strings.HasPrefix(note, "{") {
					t.Errorf("the note is not a plain line: %q", note)
				}
				if n := strings.Count(stderr, "could not be recorded"); n != 1 {
					t.Errorf("the note appears %d time(s), want once:\n%s", n, stderr)
				}
				if asJSON {
					var failure jsonFailureObject
					if jsonErr := json.Unmarshal([]byte(strings.Join(lines[1:], "\n")), &failure); jsonErr != nil {
						t.Fatalf("stderr after the note is not the failure object: %v\n%s", jsonErr, stderr)
					}
					if failure.Code != jsonCodeFailed {
						t.Errorf("code = %q, want %q", failure.Code, jsonCodeFailed)
					}
					if !strings.Contains(failure.Error, tasks.AllowedHostsConfigKey) {
						t.Errorf("the failed record hid the refusal: %q", failure.Error)
					}
				} else if len(lines) != 1 {
					t.Errorf("stderr has %d lines, want the one note:\n%s", len(lines), stderr)
				}
			})
		}
	}
}

// closeLogFile points the config directory at a temp dir and returns the close
// log's path with its directory created.
func closeLogFile(t *testing.T) string {
	t.Helper()
	isolateTasksConfigDir(t)
	path, err := config.TasksCloseLogPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCloseLogWriter_AppendsToAnOwnerOnlyFile(t *testing.T) {
	path := closeLogFile(t)
	for _, line := range []string{"first\n", "second\n"} {
		if n, err := (closeLogWriter{}).Write([]byte(line)); err != nil || n != len(line) {
			t.Fatalf("Write(%q) = (%d, %v), want (%d, nil)", line, n, err, len(line))
		}
	}
	got, err := os.ReadFile(path) //nolint:gosec // G304: a path under the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first\nsecond\n" {
		t.Errorf("the close log holds %q, want both lines in order", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the close log was created mode %04o, want 0600", mode)
	}
}

// A symlink where the log belongs would send every record to whatever it
// points at. The write is refused and the target is left alone.
func TestCloseLogWriter_RefusesASymlink(t *testing.T) {
	path := closeLogFile(t)
	for name, target := range map[string]string{
		"to a file":  filepath.Join(t.TempDir(), "elsewhere.jsonl"),
		"to nothing": filepath.Join(t.TempDir(), "absent.jsonl"),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "to a file" {
				if err := os.WriteFile(target, []byte("kept\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(path) })

			n, err := (closeLogWriter{}).Write([]byte("a record\n"))
			if err == nil || n != 0 {
				t.Fatalf("Write through a symlink = (%d, %v), want a refusal", n, err)
			}
			if !strings.Contains(err.Error(), "tasks-closes.jsonl") || !strings.Contains(err.Error(), "not a regular file") {
				t.Errorf("the refusal %q does not name the path and say it is not a regular file", err)
			}
			got, readErr := os.ReadFile(target) //nolint:gosec // G304: a path under the test's own temp dir
			if name == "to a file" {
				if readErr != nil || string(got) != "kept\n" {
					t.Errorf("the symlink's target was written: %q (%v)", got, readErr)
				}
			} else if !os.IsNotExist(readErr) {
				t.Errorf("the write created the symlink's target (read err %v)", readErr)
			}
		})
	}
}

// A log that group or other can read or write is refused, not appended to and
// not quietly narrowed: the lines already in it were readable, and the
// operator should know.
func TestCloseLogWriter_RefusesAFileOpenToGroupOrOther(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o602} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			path := closeLogFile(t)
			if err := os.WriteFile(path, []byte("earlier\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil { //nolint:gosec // G302: the wide mode is what the test is about
				t.Fatal(err)
			}
			n, err := (closeLogWriter{}).Write([]byte("a record\n"))
			if err == nil || n != 0 {
				t.Fatalf("Write to a mode %04o file = (%d, %v), want a refusal", mode, n, err)
			}
			if !strings.Contains(err.Error(), "tasks-closes.jsonl") || !strings.Contains(err.Error(), fmt.Sprintf("%04o", mode)) {
				t.Errorf("the refusal %q does not name the path and the mode", err)
			}
			got, readErr := os.ReadFile(path) //nolint:gosec // G304: a path under the test's own temp dir
			if readErr != nil || string(got) != "earlier\n" {
				t.Errorf("the refused file was changed: %q (%v)", got, readErr)
			}
			info, statErr := os.Stat(path)
			if statErr != nil || info.Mode().Perm() != mode {
				t.Errorf("the refused file's mode was changed (%v)", statErr)
			}
		})
	}
}

// Through the verb: the close still succeeds, the record still reaches stderr,
// and the caller is told the file did not get it.
func TestTasksDone_ASymlinkedCloseLogGetsNoRecord(t *testing.T) {
	rig := newDoneRig(t, doneTask(nil))
	path, err := config.TasksCloseLogPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := rig.done()
	if err != nil || !strings.Contains(stdout, "closed") {
		t.Fatalf("done = %q, %v, want the close to succeed", stdout, err)
	}
	records, rest := splitRecords(t, stderr)
	if len(records) != 1 {
		t.Errorf("want the record on stderr, got %d", len(records))
	}
	if !strings.Contains(rest, "close record") || !strings.Contains(rest, "not a regular file") {
		t.Errorf("stderr does not say the record could not be appended: %q", rest)
	}
	if got, readErr := os.ReadFile(target); readErr != nil || len(got) != 0 { //nolint:gosec // G304: a path under the test's own temp dir
		t.Errorf("the record was written through the symlink: %q (%v)", got, readErr)
	}
}

// `done` applies the host rule before it checks its other arguments. Checked
// after them, a command with an unlisted host and one other mistake would exit
// on the mistake and leave no record that a credential was asked to go
// elsewhere.
func TestTasksDone_ARefusedHostIsRecordedEvenWhenAnotherArgumentIsWrong(t *testing.T) {
	for name, args := range map[string][]string{
		"no evidence": {"done", "1", "--host", "other.example"},
		"a bad id":    {"done", "abc", "--host", "other.example", "--evidence", "owner/repo#1"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDoneRig(t, doneTask(nil))
			_, _, err := rig.run(args...)
			if got := ExitCode(err); got != exitTasksHostRefused {
				t.Fatalf("ExitCode = %d, want %d: %v", got, exitTasksHostRefused, err)
			}
			refusals := rig.refusalLines()
			if len(refusals) != 1 || refusals[0]["verb"] != "done" || refusals[0]["host"] != "other.example" {
				t.Errorf("refusal lines = %v, want one for done and other.example", refusals)
			}
			requireNoSubprocess(t, rig.runner, "a refused host on done")
		})
	}
}
