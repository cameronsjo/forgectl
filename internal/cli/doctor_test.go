package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/bless"
	"github.com/cameronsjo/forgectl/internal/doctor"
	"github.com/cameronsjo/forgectl/internal/exec"
)

// runDoctor builds `doctor` over d and executes it with args, mirroring
// runUpdate's shape (update_test.go).
func runDoctor(t *testing.T, d doctor.Deps, args ...string) (stdout string, err error) {
	t.Helper()
	cmd := newDoctorCmdForDeps(d)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), err
}

// allOKDeps builds a doctor.Deps whose every seam reports a passing check —
// the baseline both tests below mutate one field of.
func allOKDeps(t *testing.T) doctor.Deps {
	t.Helper()
	redirectDoctorConfigDir(t)
	return doctor.Deps{
		Runner: &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return "ok", nil }},
		LookPath: func(name string) (string, error) {
			return "/usr/bin/" + name, nil
		},
		TrustedStore: func() (bless.Store, error) {
			return bless.Store{}, bless.ErrTrustStoreMissing
		},
		Prober: fakeDoctorProber{code: 200},
	}
}

type fakeDoctorProber struct{ code int }

func (f fakeDoctorProber) Probe(_ context.Context, _ string) (int, error) { return f.code, nil }

func redirectDoctorConfigDir(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("FORGECTL_CLAUDE_BIN", "")
}

func TestDoctor_AllChecksPass_ExitsZero(t *testing.T) {
	d := allOKDeps(t)
	stdout, err := runDoctor(t, d)
	if err != nil {
		t.Fatalf("Execute() = %v (exit %d), want nil; stdout=%q", err, ExitCode(err), stdout)
	}
	if !bytes.Contains([]byte(stdout), []byte("claude")) {
		t.Errorf("stdout missing the claude check line: %q", stdout)
	}
}

func TestDoctor_FailingCheck_ExitsOne(t *testing.T) {
	d := allOKDeps(t)
	// gh auth status fails → the "gh" check fails → Healthy() is false.
	d.Runner = &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "gh" {
			return "", &exec.CommandError{Name: "gh", Stderr: "not logged in"}
		}
		return "ok", nil
	}}

	stdout, err := runDoctor(t, d)
	if err == nil {
		t.Fatalf("Execute() = nil, want an error (gh check should fail); stdout=%q", stdout)
	}
	if ExitCode(err) != 1 {
		t.Errorf("ExitCode = %d, want 1", ExitCode(err))
	}
}

func TestDoctor_JSON_ReflectsHealthyFlag(t *testing.T) {
	d := allOKDeps(t)
	stdout, err := runDoctor(t, d, "--json")
	if err != nil {
		t.Fatalf("Execute() = %v, want nil; stdout=%q", err, stdout)
	}

	var report doctorReportJSON
	if jsonErr := json.Unmarshal([]byte(stdout), &report); jsonErr != nil {
		t.Fatalf("json.Unmarshal: %v; stdout=%q", jsonErr, stdout)
	}
	if !report.Healthy {
		t.Errorf("report.Healthy = false, want true: %+v", report)
	}
	if len(report.Checks) == 0 {
		t.Error("report.Checks is empty")
	}
}

// TestDoctorMark_CoversEveryState pins that every doctor.State has a
// distinct, non-empty glyph — a new State added to the package without a
// case here would otherwise silently fall into the StateSkip default.
func TestDoctorMark_CoversEveryState(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range []doctor.State{doctor.StateOK, doctor.StateWarn, doctor.StateFail, doctor.StateSkip} {
		mark := doctorMark(s)
		if mark == "" {
			t.Errorf("doctorMark(%s) is empty", s)
		}
		seen[mark] = true
	}
	if len(seen) != 4 {
		t.Errorf("doctorMark produced %d distinct glyphs across 4 states, want 4 (a state is aliasing another's mark)", len(seen))
	}
}

// bench.Prober compile-time assertion — keeps fakeDoctorProber honest if
// internal/bench's Prober interface ever grows a method.
var _ bench.Prober = fakeDoctorProber{}
