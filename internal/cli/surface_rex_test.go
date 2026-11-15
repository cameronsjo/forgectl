// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

const rexTestPath = "/Applications/Rex Beta.app/Contents/Helpers/Rex Server.rex/Contents/MacOS/rex"

// TestNewRexAdapter_TakesTheLocationItsLocatorFinds: the adapter is built on
// whatever absolute path the locator returns, and a locator that finds
// nothing, or a relative path, is "unavailable" with a truly nil adapter.
func TestNewRexAdapter_TakesTheLocationItsLocatorFinds(t *testing.T) {
	a, err := newRexAdapter(func() (rexLocation, error) { return rexLocation{Path: rexTestPath}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind() != backend.KindRex {
		t.Errorf("Kind = %v", a.Kind())
	}
	if _, err := backend.RequireCapabilities(a); err != nil {
		t.Errorf("the rex adapter cannot clean up after itself: %v", err)
	}
	for name, locate := range map[string]rexLocator{
		"not found": func() (rexLocation, error) { return rexLocation{}, errors.New("no rex") },
		"relative":  func() (rexLocation, error) { return rexLocation{Path: "rex"}, nil },
	} {
		a, err := newRexAdapter(locate)
		if !errors.Is(err, errBackendUnavailable) {
			t.Errorf("%s: err = %v, want errBackendUnavailable", name, err)
		}
		if a != nil {
			t.Errorf("%s: a non-nil adapter came back with the error", name)
		}
	}
}

// TestVerifyingRunner_ChecksBeforeEveryCommand: the binary is re-checked
// before each command, and a failed check runs nothing.
func TestVerifyingRunner_ChecksBeforeEveryCommand(t *testing.T) {
	var inner exec.FakeSensitiveRunner
	verifies := 0
	var verifyErr error
	run := verifyingRunner{inner: &inner, verify: func(context.Context) error { verifies++; return verifyErr }}
	cmd := exec.SensitiveCommand{Kind: exec.KindRexReadiness, Path: exec.Secret(rexTestPath), Args: []exec.Arg{exec.MustFixed("whoami")},
		Env: []exec.EnvMutation{exec.UnsetLauncherSessionHandles()}, StdoutCap: 4096, StderrCap: 4096}

	for i := 1; i <= 3; i++ {
		if _, err := run.RunSensitive(t.Context(), cmd); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if verifies != i {
			t.Fatalf("after call %d verifies = %d, want %d", i, verifies, i)
		}
	}
	ran := len(inner.Calls())

	verifyErr = errors.New("swapped")
	if _, err := run.RunSensitive(t.Context(), cmd); err == nil {
		t.Fatal("a failed check still ran the command")
	}
	if got := len(inner.Calls()); got != ran {
		t.Errorf("inner runner saw %d calls after a failed check, want %d", got, ran)
	}
}

// rexTestSpec is a valid launch spec for tests that only watch what reaches
// the runner.
func rexTestSpec(t *testing.T) backend.StartSpec {
	t.Helper()
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	boot, err := backend.NewBootstrapCommand(exec.Opaque("'/bin/sh' '-c' 'true'"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := backend.NewStartSpec(cwd, "forgectl cli test", tag, boot)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// TestBuildRexAdapter_ReChecksBeforeAnyCommand: the adapter newRexAdapter
// builds goes through the verifying runner, so a failed check means rex is
// never run, and a passing one lets the command through (the positive control
// that makes the first assertion mean something).
func TestBuildRexAdapter_ReChecksBeforeAnyCommand(t *testing.T) {
	t.Run("failed check runs nothing", func(t *testing.T) {
		var base exec.FakeSensitiveRunner
		a, err := buildRexAdapter(rexLocation{Path: rexTestPath, Verify: func(context.Context) error { return errors.New("swapped") }}, &base)
		if err != nil {
			t.Fatal(err)
		}
		res := a.Start(t.Context(), rexTestSpec(t))
		if res.Outcome() != backend.NotMutated {
			t.Fatalf("Start = %v, want not-mutated", res)
		}
		if cause, _ := res.Cause(); cause.Class() != backend.FailureUnavailable {
			t.Errorf("a failed check classified as %v, want unavailable", cause.Class())
		}
		if got := len(base.Calls()); got != 0 {
			t.Fatalf("the runner saw %d calls after a failed check, want 0", got)
		}
	})
	t.Run("passing check lets the command through", func(t *testing.T) {
		var base exec.FakeSensitiveRunner
		a, err := buildRexAdapter(rexLocation{Path: rexTestPath, Verify: func(context.Context) error { return nil }}, &base)
		if err != nil {
			t.Fatal(err)
		}
		_ = a.Start(t.Context(), rexTestSpec(t))
		if len(base.Calls()) == 0 {
			t.Fatal("a passing check still stopped every command")
		}
	})
	t.Run("a cancel during the check is a cancel", func(t *testing.T) {
		var base exec.FakeSensitiveRunner
		a, err := buildRexAdapter(rexLocation{Path: rexTestPath, Verify: func(context.Context) error { return context.Canceled }}, &base)
		if err != nil {
			t.Fatal(err)
		}
		res := a.Start(t.Context(), rexTestSpec(t))
		if cause, _ := res.Cause(); cause.Class() != backend.FailureCanceled {
			t.Errorf("a cancel during Verify classified as %v, want canceled", cause.Class())
		}
	})
}

// TestResolveRexWith_RefusesAnUnreadableConfig: a config file that did not
// decode, or could not be located, is a refusal, never "no pin": the pin
// chooses an executable. resolveRex checks DecodeError and DecodeDegraded; a
// failed decode sets both, so each check covers the other and only removing
// both turns this red.
func TestResolveRexWith_RefusesAnUnreadableConfig(t *testing.T) {
	t.Run("does not decode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte("[surface.rex\nbinary_path = "), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := resolveRexWith(func() config.Config { return config.LoadPath(path) })
		if err == nil || !strings.Contains(err.Error(), "config file") {
			t.Fatalf("err = %v, want a config-file refusal", err)
		}
	})
	t.Run("cannot be located", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		cfg := config.Load()
		if !cfg.DecodeDegraded() && cfg.DecodeError() == nil {
			t.Skip("this platform locates a config path without HOME")
		}
		_, err := resolveRexWith(func() config.Config { return cfg })
		if err == nil {
			t.Fatal("resolveRexWith accepted a config it could not read")
		}
	})
}

// TestLiveResolveRex runs the real resolver with FORGECTL_LIVE_REX=1, with
// PATH emptied so the app bundle is the only way to find rex: it must find a
// bundle binary that passes its own re-check. It starts nothing.
func TestLiveResolveRex(t *testing.T) {
	if os.Getenv("FORGECTL_LIVE_REX") != "1" {
		t.Skip("set FORGECTL_LIVE_REX=1 to resolve the installed Rex")
	}
	t.Setenv("PATH", t.TempDir())
	loc, err := resolveRex()
	if err != nil {
		t.Fatalf("resolveRex with no PATH: %v", err)
	}
	if !strings.Contains(loc.Path, ".app/") {
		t.Errorf("resolved %q, want a path inside an app bundle", loc.Path)
	}
	if loc.Verify == nil {
		t.Fatal("a located rex carries no Verify")
	}
	if err := loc.Verify(context.Background()); err != nil {
		t.Errorf("Verify of the binary just resolved: %v", err)
	}
	t.Logf("resolved %s", loc.Path)
}

// TestLiveRexThroughTheCLISeam builds the adapter the way `--surface rex` does
// (the real resolver, the verifying runner) with FORGECTL_LIVE_REX=1 and PATH
// emptied, then starts, probes and closes one scratch block: the wiring, not
// just each piece. It touches only the session its own Start created, and
// closes it through the adapter, which checks identity first.
func TestLiveRexThroughTheCLISeam(t *testing.T) {
	if os.Getenv("FORGECTL_LIVE_REX") != "1" {
		t.Skip("set FORGECTL_LIVE_REX=1 to start a scratch block in the installed Rex")
	}
	t.Setenv("PATH", t.TempDir())
	adapter, err := newRexAdapter(locateRex)
	if err != nil {
		t.Fatalf("newRexAdapter with rex off PATH: %v", err)
	}
	a, err := backend.RequireCapabilities(adapter)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	boot, err := backend.NewBootstrapCommand(exec.Opaque("'/bin/sh' '-c' 'sleep 60'"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := backend.NewStartSpec(cwd, "forgectl cli seam test", tag, boot)
	if err != nil {
		t.Fatal(err)
	}
	res := a.Start(t.Context(), spec)
	ref, has := res.Ref()
	if has {
		t.Cleanup(func() {
			if c := a.Close(context.Background(), ref); c.State() != backend.CloseClosed && c.State() != backend.CloseAlreadyGone {
				cause, _ := c.Cause()
				t.Errorf("cleanup Close = %v (%v); remove the scratch Rex session named like forgectl by hand", c, cause)
			}
		})
	}
	if !has || res.Failed() {
		c, _ := res.Cause()
		t.Fatalf("Start = %v (%v)", res, c)
	}
	if p := a.Probe(t.Context(), ref); p.State() != backend.ProbePresent {
		t.Fatalf("Probe = %v, want present", p)
	}
	if c := a.Close(t.Context(), ref); c.State() != backend.CloseClosed {
		cause, _ := c.Cause()
		t.Fatalf("Close = %v (%v)", c, cause)
	}
	if p := a.Probe(t.Context(), ref); p.State() != backend.ProbeGone {
		t.Errorf("Probe after Close = %v, want gone", p)
	}
}
