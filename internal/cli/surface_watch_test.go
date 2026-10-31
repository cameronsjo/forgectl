package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
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
		hc:               func() (workerwatch.CLI, error) { return workerwatch.CLI{}, nil },
		newCollector: func(opts watchOptions, entries []workerwatch.Entry, notes []string, _ workerwatch.CLI) *workerwatch.Collector {
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

type watchNoHC struct{}

func (watchNoHC) Get(context.Context, string) (workerwatch.Terminal, error) {
	return workerwatch.Terminal{}, workerwatch.ErrNoHC
}
func (watchNoHC) List(context.Context) ([]workerwatch.Terminal, error) {
	return nil, workerwatch.ErrNoHC
}
func (watchNoHC) Read(context.Context, string) (string, error) { return "", workerwatch.ErrNoHC }

func TestSurfaceWatchSnapshotExitsOneWithoutHC(t *testing.T) {
	var ran bool
	rt := fakeWatchRuntime(false, &ran)
	base := rt.newCollector
	rt.newCollector = func(opts watchOptions, entries []workerwatch.Entry, notes []string, hc workerwatch.CLI) *workerwatch.Collector {
		c := base(opts, entries, notes, hc)
		c.HC = watchNoHC{}
		return c
	}
	for _, args := range [][]string{{"--workers", writeWorkersFixture(t)}, {"--workers", writeWorkersFixture(t), "--json"}} {
		out, err := runWatch(t, rt, args...)
		if ExitCode(err) != exitFailed {
			t.Errorf("args %q: exit %d, want %d", args, ExitCode(err), exitFailed)
		}
		if !strings.Contains(out, "hc was not found on PATH") {
			t.Errorf("args %q: the snapshot does not say hc is missing\n%s", args, out)
		}
	}
}

// The production collector must count markers by the same [sweep] reviewers
// allowlist forgectl sweep uses.
func TestSurfaceWatchUsesTheSweepReviewers(t *testing.T) {
	deps := module.Deps{Runner: &exec.FakeRunner{}}
	deps.Cfg.Sweep.Reviewers = []string{"pr-reviewer", "security-reviewer"}
	c := productionWatchRuntime(deps).newCollector(watchOptions{GHTTL: workerwatch.MinGHTTL}, nil, nil, workerwatch.CLI{})
	if strings.Join(c.Reviewers, ",") != "pr-reviewer,security-reviewer" {
		t.Errorf("collector reviewers = %q, want the [sweep] reviewers", c.Reviewers)
	}
	if c.GH == nil || c.Git == nil || c.HC == nil || c.Reports == nil {
		t.Errorf("production collector is missing a source: %+v", c)
	}
}

// TestWatchPlainTellsGoneFromUnread: only hc's not_found is "gone"; any other
// hc failure is "unread" (the worker may still be running), and a finished
// terminal reads "exited", not its last self-reported status.
func TestWatchPlainTellsGoneFromUnread(t *testing.T) {
	snap := workerwatch.Snapshot{Workers: []workerwatch.Worker{
		{Name: "cos-gone", HCError: "no hc terminal by this name"},
		{Name: "cos-unread", HCError: "hc could not read this terminal"},
		{Name: "cos-exited", State: "finished", Status: workerwatch.StatusWorking},
	}}
	var b strings.Builder
	if err := writeWatchPlain(&b, snap); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	want := map[string]string{"cos-gone": "gone", "cos-unread": "unread", "cos-exited": "exited"}
	for _, l := range lines {
		f := strings.Split(l, "\t")
		if w, ok := want[f[0]]; ok {
			if !slices.Contains(f, w) {
				t.Errorf("%s: line %q has no %q", f[0], l, w)
			}
			delete(want, f[0])
		}
	}
	if len(want) != 0 {
		t.Errorf("rows missing from the snapshot: %v\n%s", want, b.String())
	}
}

// TestProductionWatchHC covers both ways the watch's hc client can fail to
// build: hc missing is not an error (the snapshot says hc_unavailable, and
// the client finds hc once it is installed), and an HC_SESSION the adapter
// refuses is.
func TestProductionWatchHC(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("HC_SESSION", "")
	c, err := productionWatchHC()
	if err != nil {
		t.Fatalf("hc missing: err %v; want no error", err)
	}
	if _, err := c.List(t.Context()); !errors.Is(err, workerwatch.ErrNoHC) {
		t.Fatalf("hc missing: List err = %v, want ErrNoHC", err)
	}

	// Installing hc while the client is open: the same client now reaches
	// it (the stub fails, which is not ErrNoHC).
	if err := os.WriteFile(filepath.Join(bin, "hc"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil { //nolint:gosec // G306: a test stub that must be executable
		t.Fatal(err)
	}
	if _, err := c.List(t.Context()); err == nil || errors.Is(err, workerwatch.ErrNoHC) {
		t.Fatalf("hc installed after start: List err = %v, want the stub's failure", err)
	}
	if c, err := productionWatchHC(); err != nil || c.Source == nil {
		t.Fatalf("hc present: CLI %+v, err %v; want a source", c, err)
	}
	t.Setenv("HC_SESSION", "not a session!")
	if _, err := productionWatchHC(); err == nil {
		t.Fatal("an HC_SESSION the adapter refuses built a watch client")
	}
}
