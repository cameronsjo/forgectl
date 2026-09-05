package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/bless"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/doctor"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
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
//
// checkClaude does NOT consult Deps.LookPath — it goes through
// launch.ClaudePath, which resolves $FORGECTL_CLAUDE_BIN, then
// [launch.defaults].binary_path, then a REAL os/exec.LookPath("claude") as
// its last resort. Stubbing Deps.LookPath alone leaves that PATH lookup
// live, so a "checks all pass" test would only pass on a machine that
// happens to have claude installed — exactly the bug this comment now
// guards against (CI has no claude on PATH; this test was green locally by
// environmental luck, not by construction). Pointing binary_path at a fake
// executable makes checkClaude deterministic the same way TestCheckClaude
// (doctor package) already does, instead of depending on the runner's PATH.
func allOKDeps(t *testing.T) doctor.Deps {
	t.Helper()
	redirectDoctorConfigDir(t)

	fakeClaude := t.TempDir() + "/claude"
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	return doctor.Deps{
		Cfg:    config.Config{Launch: config.LaunchConfig{Defaults: config.LaunchDefaults{BinaryPath: fakeClaude}}},
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

func TestDoctor_OutputOmitsRetiredBoard(t *testing.T) {
	stdout, err := runDoctor(t, allOKDeps(t))
	if err != nil {
		t.Fatalf("Execute() = %v, want nil; stdout=%q", err, stdout)
	}
	if strings.Contains(strings.ToLower(stdout), "fl"+"ux") {
		t.Errorf("doctor output names retired board component; got:\n%s", stdout)
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

// TestPrintDoctorReport_SanitizesForgedControlBytes pins the fix for output
// forging: Detail/Hint carry captured stdout/stderr from external commands
// (an untrusted tap, a compromised gh session), so a newline or an ANSI CSI
// sequence embedded there could forge an extra well-formed check line, or
// rewrite a real failure's line via a cursor-control escape (the exact
// pr_prs.go/forgectl#162 defect class, reused here at doctor's own display
// boundary). Before the terminal boundary was applied, this forged detail
// would render as two lines, the second indistinguishable from a real
// passing "gh" check.
func TestPrintDoctorReport_SanitizesForgedControlBytes(t *testing.T) {
	forged := "not logged in\nok    gh                 authenticated"
	report := doctor.Report{Checks: []doctor.Check{
		{Name: "gh", State: doctor.StateFail, Detail: forged, Hint: "run `gh auth login`\x1b[2Kforged hint line"},
	}}

	var out bytes.Buffer
	if err := printDoctorReport(&out, report); err != nil {
		t.Fatal(err)
	}
	rendered := out.String()

	if bytes.Count([]byte(rendered), []byte("\n")) != 2 {
		t.Errorf("rendered = %q, want exactly 2 newlines (one detail line + one hint line) — a forged \\n produced an extra line", rendered)
	}

	// The forged CSI must reach the terminal only in escaped, inert form.
	//
	// This was "no ESC byte anywhere" before the charm v2 migration, which
	// passed for the wrong reason: lipgloss v1 resolved its colour profile from
	// a global that a test binary left unset, so the status marks rendered plain
	// and the only ESC that could appear was a forged one. v2 renders truecolor
	// unconditionally and downgrades at the writer, so the mark now carries a
	// legitimate SGR sequence and the blanket assertion fires on forgectl's own
	// styling.
	//
	// The replacement is the SHARED contract, not a local rewrite. An earlier
	// draft of this test stripped SGR with its own `\x1b\[[0-9;]*m` regexp,
	// which was a second and weaker definition of "forgectl's own styling" than
	// termsafetest's: it accepted conceal and blink, and accepted a truncated or
	// out-of-range extended-colour selector — exactly the shapes an injection
	// takes. Two notions of safe in one package is the drift termsafetest exists
	// to prevent.
	if !strings.Contains(rendered, `\x1b[2K`) {
		t.Errorf("rendered = %q, want the forged CSI present in ESCAPED form — its absence means the assertion below can no longer go red", rendered)
	}
	termsafetest.AssertInert(t, "doctor report", rendered)
}
