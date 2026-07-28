package update

import (
	"context"
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// argvOf renders a FakeRunner Call's name+args as a single string for
// concise comparison against the expected argv per step.
func argvOf(c exec.Call) []string {
	return append([]string{c.Name}, c.Args...)
}

func TestDefaultSteps_RosterNamesAndDestructiveness(t *testing.T) {
	want := map[string]bool{
		StepBrew:           true,
		StepSoftwareUpdate: false,
		StepGo:             true,
		StepNpm:            true,
	}
	steps := DefaultSteps()
	if len(steps) != len(want) {
		t.Fatalf("DefaultSteps() has %d steps, want %d", len(steps), len(want))
	}
	for _, s := range steps {
		destructive, ok := want[s.Name]
		if !ok {
			t.Errorf("unexpected step %q in roster", s.Name)
			continue
		}
		if s.Destructive != destructive {
			t.Errorf("step %q Destructive = %v, want %v", s.Name, s.Destructive, destructive)
		}
	}
}

func TestBrewStep_CheckRunsOutdated(t *testing.T) {
	fr := &exec.FakeRunner{}
	if _, err := brewStep().Check(context.Background(), fr); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(fr.Calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(fr.Calls))
	}
	got := argvOf(fr.Calls[0])
	want := []string{"brew", "outdated"}
	assertArgv(t, got, want)
}

func TestBrewStep_ApplyRunsUpdateUpgradeCleanupInOrder(t *testing.T) {
	fr := &exec.FakeRunner{}
	if _, err := brewStep().Apply(context.Background(), fr); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(fr.Calls) != 3 {
		t.Fatalf("got %d calls, want 3", len(fr.Calls))
	}
	assertArgv(t, argvOf(fr.Calls[0]), []string{"brew", "update"})
	// --formula excludes casks deliberately: a pkg-based cask upgrade can
	// invoke sudo or a GUI installer, and this step runs unattended
	// (--yes/cron) with no stdin wired to answer either.
	assertArgv(t, argvOf(fr.Calls[1]), []string{"brew", "upgrade", "--formula"})
	assertArgv(t, argvOf(fr.Calls[2]), []string{"brew", "cleanup"})
}

func TestBrewStep_ApplyStopsAtFirstFailure(t *testing.T) {
	fr := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "brew" && len(args) > 0 && args[0] == "upgrade" {
				return "", errors.New("upgrade failed")
			}
			return "ok", nil
		},
	}
	_, err := brewStep().Apply(context.Background(), fr)
	if err == nil {
		t.Fatal("expected an error from the failing upgrade step")
	}
	if len(fr.Calls) != 2 {
		t.Fatalf("got %d calls, want 2 (cleanup must not run after upgrade fails)", len(fr.Calls))
	}
	assertArgv(t, argvOf(fr.Calls[0]), []string{"brew", "update"})
	assertArgv(t, argvOf(fr.Calls[1]), []string{"brew", "upgrade", "--formula"})
}

func TestSoftwareUpdateStep_CheckOnly_NeverInstalls(t *testing.T) {
	s := softwareUpdateStep()
	if s.Destructive {
		t.Error("softwareupdate step must be Destructive: false — it never installs")
	}

	fr := &exec.FakeRunner{}
	if _, err := s.Check(context.Background(), fr); err != nil {
		t.Fatalf("Check: %v", err)
	}
	assertArgv(t, argvOf(fr.Calls[0]), []string{"softwareupdate", "-l"})

	fr2 := &exec.FakeRunner{}
	if _, err := s.Apply(context.Background(), fr2); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertArgv(t, argvOf(fr2.Calls[0]), []string{"softwareupdate", "-l"})

	for _, fr := range []*exec.FakeRunner{fr, fr2} {
		for _, c := range fr.Calls {
			if len(c.Args) > 0 && c.Args[0] == "-i" {
				t.Errorf("softwareupdate step invoked an install flag: %+v", c)
			}
		}
	}
}

func TestGoStep_CheckAndApply(t *testing.T) {
	fr := &exec.FakeRunner{}
	if _, err := goStep().Check(context.Background(), fr); err != nil {
		t.Fatalf("Check: %v", err)
	}
	assertArgv(t, argvOf(fr.Calls[0]), []string{"go", "version"})

	fr2 := &exec.FakeRunner{}
	if _, err := goStep().Apply(context.Background(), fr2); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertArgv(t, argvOf(fr2.Calls[0]), []string{"go", "clean", "-cache", "-modcache", "-testcache"})
}

func TestNpmStep_CheckAndApply(t *testing.T) {
	fr := &exec.FakeRunner{}
	if _, err := npmStep().Check(context.Background(), fr); err != nil {
		t.Fatalf("Check: %v", err)
	}
	assertArgv(t, argvOf(fr.Calls[0]), []string{"npm", "outdated", "-g"})

	fr2 := &exec.FakeRunner{}
	if _, err := npmStep().Apply(context.Background(), fr2); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertArgv(t, argvOf(fr2.Calls[0]), []string{"npm", "update", "-g"})
}

// TestNpmStep_CheckTreatsExitCode1AsSuccess pins the fix for a weekly cron
// alarming on a healthy machine: `npm outdated -g` exits 1 precisely when
// it found outdated packages — npm's normal "here's the finding" signal,
// not a broken check. Check must remap that one exit code to success and
// surface the captured table as its Result, not as an error.
func TestNpmStep_CheckTreatsExitCode1AsSuccess(t *testing.T) {
	fr := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return "", &exec.CommandError{
				Name: name, Args: args, ExitCode: 1,
				Output: "some-pkg  1.0.0  2.0.0  2.0.0  global",
				Err:    errors.New("exit status 1"),
			}
		},
	}
	out, err := npmStep().Check(context.Background(), fr)
	if err != nil {
		t.Fatalf("Check: %v, want nil (exit 1 is npm's normal finding signal)", err)
	}
	if out != "some-pkg  1.0.0  2.0.0  2.0.0  global" {
		t.Errorf("Check output = %q, want the outdated-packages table", out)
	}
}

// TestNpmStep_CheckStillFailsOnOtherExitCodes ensures the exit-1 remap
// stays narrow: any other exit code (npm genuinely broken) — or an error
// that isn't even a *exec.CommandError (npm not on PATH) — still surfaces
// as a real failure, never silently swallowed.
func TestNpmStep_CheckStillFailsOnOtherExitCodes(t *testing.T) {
	t.Run("exit code 2", func(t *testing.T) {
		fr := &exec.FakeRunner{
			RunFunc: func(name string, args []string) (string, error) {
				return "", &exec.CommandError{Name: name, Args: args, ExitCode: 2, Err: errors.New("exit status 2")}
			},
		}
		if _, err := npmStep().Check(context.Background(), fr); err == nil {
			t.Error("Check = nil error, want the exit-2 failure to surface")
		}
	})

	t.Run("non-CommandError error", func(t *testing.T) {
		boom := errors.New("exec: \"npm\": executable file not found in $PATH")
		fr := &exec.FakeRunner{
			RunFunc: func(string, []string) (string, error) { return "", boom },
		}
		if _, err := npmStep().Check(context.Background(), fr); !errors.Is(err, boom) {
			t.Errorf("Check error = %v, want it to wrap %v", err, boom)
		}
	})
}

func assertArgv(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("argv = %v, want %v", got, want)
		}
	}
}
