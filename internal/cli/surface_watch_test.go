package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tui"
	"github.com/cameronsjo/forgectl/internal/workerwatch"
)

type watchFakeHC struct{}

func (watchFakeHC) Get(_ context.Context, name string) (workerwatch.Terminal, error) {
	if name == "cos-b" {
		return workerwatch.Terminal{Address: "t2", Name: name, State: "live", Status: workerwatch.StatusBlocked, Model: "opus"}, nil
	}
	return workerwatch.Terminal{}, workerwatch.ErrGone
}

func (watchFakeHC) List(context.Context) ([]workerwatch.Terminal, error) { return nil, nil }

func (watchFakeHC) Read(context.Context, string) (string, error) { return "", nil }

// fakeWatchRuntime collects through a Collector over fakes; it runs no hc,
// git or gh.
func fakeWatchRuntime(tty bool, ran *bool) watchRuntime {
	return watchRuntime{
		stdoutIsTerminal: func(io.Writer) bool { return tty },
		stdinIsTerminal:  func(io.Reader) bool { return tty },
		hc:               func() workerwatch.CLI { return workerwatch.CLI{} },
		newCollector: func(opts watchOptions, entries []workerwatch.Entry, notes []string) *workerwatch.Collector {
			return &workerwatch.Collector{
				HC: watchFakeHC{}, Entries: entries, Notes: notes, Cos: opts.Cos, GHTTL: opts.GHTTL,
				Reports: func(string) workerwatch.Reports {
					return workerwatch.Reports{Files: []workerwatch.ReportFile{{Name: "report.md", Complete: true}}}
				},
				Now: func() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) },
			}
		},
		run: func(_ context.Context, opts tui.WorkersOptions) error {
			*ran = true
			if opts.Collect == nil || opts.Read == nil || opts.Events == nil || opts.Poll != defaultWatchInterval {
				panic("the TUI was not given its collect, read, events and poll interval")
			}
			return nil
		},
	}
}

func writeWorkersFixture(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "workers.txt")
	body := "cos-a|/wt/a|/briefs/a\ncos-b|/wt/b|/briefs/b\nnot a line\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runWatch(t *testing.T, rt watchRuntime, args ...string) (string, error) {
	t.Helper()
	cmd := newSurfaceWatchCmdWith(theme.Default(), rt)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestSurfaceWatchJSONSnapshot(t *testing.T) {
	var ran bool
	out, err := runWatch(t, fakeWatchRuntime(true, &ran), "--workers", writeWorkersFixture(t), "--json")
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Error("--json opened the TUI")
	}
	var snap struct {
		Workers []struct {
			Name    string `json:"name"`
			Address string `json:"address"`
			Status  string `json:"status"`
			HCError string `json:"hc_error"`
			Reports struct {
				Files []struct {
					Name     string `json:"name"`
					Complete bool   `json:"complete"`
				} `json:"files"`
			} `json:"reports"`
		} `json:"workers"`
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(snap.Workers) != 2 || snap.Workers[1].Status != "blocked" || snap.Workers[1].Address != "t2" || snap.Workers[0].HCError == "" {
		t.Errorf("workers = %+v", snap.Workers)
	}
	if !snap.Workers[0].Reports.Files[0].Complete {
		t.Error("report completeness missing from JSON")
	}
	if len(snap.Notes) != 1 || !strings.Contains(snap.Notes[0], "line 3") {
		t.Errorf("notes = %q, want the bad line 3", snap.Notes)
	}
}

func TestSurfaceWatchPlainOffTerminal(t *testing.T) {
	var ran bool
	out, err := runWatch(t, fakeWatchRuntime(false, &ran), "--workers", writeWorkersFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Error("a pipe opened the TUI")
	}
	for _, want := range []string{"cos-a\t-\tgone", "cos-b\tt2\tblocked NEEDS-YOU\topus", "reports: 1 (1 complete) · newest report.md REPORT-COMPLETE", "note: line 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("plain snapshot is missing %q\n%s", want, out)
		}
	}
}

func TestSurfaceWatchOpensTUIOnTerminal(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	var ran bool
	if _, err := runWatch(t, fakeWatchRuntime(true, &ran), "--cos"); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("a terminal did not open the TUI")
	}
}

func TestSurfaceWatchUsageErrors(t *testing.T) {
	var ran bool
	rt := fakeWatchRuntime(true, &ran)
	for _, args := range [][]string{
		{},
		{"--cos", "--interval", "1s"},
		{"--cos", "--gh-ttl", "10s"},
		{"--workers", filepath.Join(t.TempDir(), "missing.txt")},
		{"--cos", "extra-arg"},
	} {
		_, err := runWatch(t, rt, args...)
		if err == nil {
			t.Errorf("args %q: no error", args)
			continue
		}
		if code := ExitCode(err); code != exitUsage && len(args) > 0 && args[len(args)-1] != "extra-arg" {
			t.Errorf("args %q: exit %d, want %d (%v)", args, code, exitUsage, err)
		}
	}
	if ran {
		t.Error("a usage error opened the TUI")
	}
}
