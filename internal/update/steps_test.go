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
	assertArgv(t, argvOf(fr.Calls[1]), []string{"brew", "upgrade"})
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
	assertArgv(t, argvOf(fr.Calls[1]), []string{"brew", "upgrade"})
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
