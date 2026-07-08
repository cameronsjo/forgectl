package bench

import (
	"context"
	"errors"
	"io"
	"runtime"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestUp_BothConfigured_DelegatesToEntrypoints(t *testing.T) {
	t.Setenv("HEARTH_DIR", "")
	t.Setenv("CHRONICLE_DIR", "")
	cfg := config.Config{Bench: config.BenchConfig{
		HearthDir:    "/x/hearth",
		ChronicleDir: "/x/chronicle",
	}}
	runner := &exec.FakeRunner{}

	if err := Up(context.Background(), cfg, runner, io.Discard); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(runner.Calls) != 2 {
		t.Fatalf("got %d calls, want 2: %+v", len(runner.Calls), runner.Calls)
	}
	if got := runner.Calls[0]; got.Name != "/x/hearth/scripts/start.sh" || !got.Interactive {
		t.Errorf("hearth call = %+v, want interactive /x/hearth/scripts/start.sh", got)
	}
	if got := runner.Calls[1]; got.Name != "make" || !equalStr(got.Args, []string{"-C", "/x/chronicle", "sync"}) || !got.Interactive {
		t.Errorf("chronicle call = %+v, want interactive make -C /x/chronicle sync", got)
	}
}

func TestUp_HearthOnly_SkipsChronicle(t *testing.T) {
	t.Setenv("HEARTH_DIR", "")
	t.Setenv("CHRONICLE_DIR", "")
	cfg := config.Config{Bench: config.BenchConfig{HearthDir: "/x/hearth"}}
	runner := &exec.FakeRunner{}

	if err := Up(context.Background(), cfg, runner, io.Discard); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(runner.Calls) != 1 || runner.Calls[0].Name != "/x/hearth/scripts/start.sh" {
		t.Errorf("calls = %+v, want only the hearth start", runner.Calls)
	}
}

func TestUp_NothingConfigured_Errors(t *testing.T) {
	t.Setenv("HEARTH_DIR", "")
	t.Setenv("CHRONICLE_DIR", "")
	runner := &exec.FakeRunner{}

	err := Up(context.Background(), config.Config{}, runner, io.Discard)
	if err == nil {
		t.Fatal("Up with nothing configured = nil error, want an error")
	}
	if len(runner.Calls) != 0 {
		t.Errorf("expected no delegated calls, got %+v", runner.Calls)
	}
}

func TestUp_HearthStartFails_Errors(t *testing.T) {
	t.Setenv("HEARTH_DIR", "")
	t.Setenv("CHRONICLE_DIR", "")
	cfg := config.Config{Bench: config.BenchConfig{HearthDir: "/x/hearth", ChronicleDir: "/x/chronicle"}}
	runner := &exec.FakeRunner{InteractiveErr: errors.New("start.sh exited 1")}

	if err := Up(context.Background(), cfg, runner, io.Discard); err == nil {
		t.Fatal("Up with a failing hearth start = nil error, want an error")
	}
	// Must stop at hearth — chronicle is never reached.
	if len(runner.Calls) != 1 {
		t.Errorf("got %d calls, want 1 (stop after hearth failure)", len(runner.Calls))
	}
}

func TestOpen_TargetToURL(t *testing.T) {
	cases := map[string]string{
		"":        "http://hearth.localhost",
		"hearth":  "http://hearth.localhost",
		"grafana": "http://grafana.localhost",
	}
	for target, wantURL := range cases {
		t.Run("target="+target, func(t *testing.T) {
			runner := &exec.FakeRunner{}
			if err := Open(context.Background(), config.Config{}, runner, target); err != nil {
				t.Fatalf("Open(%q): %v", target, err)
			}
			call := runner.Calls[0]
			if !call.Interactive {
				t.Errorf("open call should be interactive: %+v", call)
			}
			if call.Name != openCommand() {
				t.Errorf("open command = %q, want %q", call.Name, openCommand())
			}
			if !equalStr(call.Args, []string{wantURL}) {
				t.Errorf("open args = %v, want [%s]", call.Args, wantURL)
			}
		})
	}
}

func TestOpen_UnknownTarget_Errors(t *testing.T) {
	runner := &exec.FakeRunner{}
	if err := Open(context.Background(), config.Config{}, runner, "nope"); err == nil {
		t.Error("Open with an unknown target = nil error, want an error")
	}
	if len(runner.Calls) != 0 {
		t.Errorf("unknown target should not shell out, got %+v", runner.Calls)
	}
}

func TestOpenCommand_MatchesGOOS(t *testing.T) {
	want := "xdg-open"
	if runtime.GOOS == "darwin" {
		want = "open"
	}
	if got := openCommand(); got != want {
		t.Errorf("openCommand() = %q, want %q on %s", got, want, runtime.GOOS)
	}
}
