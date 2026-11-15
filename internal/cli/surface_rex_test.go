// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
