// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package hcadapter

import (
	"context"
	"crypto/rand"
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// liveHcEnv gates the live test: it creates one hc terminal, so it never runs
// by default.
const liveHcEnv = "FORGECTL_LIVE_HC"

// liveHcPathEnv names the hc binary; the default is the one on PATH.
const liveHcPathEnv = "FORGECTL_HC_PATH"

// TestLiveHc drives the adapter against the running herdr-core, with
// FORGECTL_LIVE_HC=1. Two phases, each with one background terminal named
// forgectl-live-<5 random characters>: a short command that finishes before
// Close (the rm path), and a long one that is still running when Close is
// called (the kill path). Each runs Start, Probe, Close, then Probe and a
// second Close to see it gone. It touches no other terminal. If a phase fails
// before Close, its terminal is closed through the adapter, which checks the
// terminal's identity first; nothing is ever killed or removed by a bare
// address. The hc version the run saw is logged.
func TestLiveHc(t *testing.T) {
	if os.Getenv(liveHcEnv) != "1" {
		t.Skipf("set %s=1 to run against a live herdr-core", liveHcEnv)
	}
	hcPath := os.Getenv(liveHcPathEnv)
	if hcPath == "" {
		p, err := osexec.LookPath("hc")
		if err != nil {
			t.Skipf("no hc on PATH and %s unset", liveHcPathEnv)
		}
		hcPath = p
	}
	hcPath, err := filepath.Abs(hcPath)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := osexec.Command(hcPath, "--version").Output(); err == nil { //nolint:gosec,noctx // G204: the operator's hc binary, fixed argument
		t.Logf("hc version: %s", string(v))
	}
	a, err := New(exec.NewOSSensitiveRunner(), hcPath, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("finished before close", func(t *testing.T) {
		ref := liveStart(t, a, "'/bin/sh' '-c' 'sleep 2; exit 7'")
		if p := a.Probe(t.Context(), ref); p.State() != backend.ProbePresent {
			t.Fatalf("Probe while running = %v", p)
		}
		// The program ends after two seconds. What Probe then says of a
		// finished terminal is what this phase measures, so it is logged and
		// must be a definite answer, never unreadable.
		deadline := time.Now().Add(20 * time.Second)
		for {
			p := a.Probe(t.Context(), ref)
			if p.State() == backend.ProbeUnreadable {
				t.Fatalf("Probe of a finished terminal = %v, want a definite answer", p)
			}
			if p.State() != backend.ProbePresent {
				t.Logf("Probe after the program finished = %v", p)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("terminal still present 20s after a 2s program")
			}
			time.Sleep(250 * time.Millisecond)
		}
		liveClose(t, a, ref)
	})

	t.Run("running at close", func(t *testing.T) {
		ref := liveStart(t, a, "'/bin/sh' '-c' 'sleep 120'")
		if p := a.Probe(t.Context(), ref); p.State() != backend.ProbePresent {
			t.Fatalf("Probe while running = %v", p)
		}
		liveClose(t, a, ref)
	})
}

// liveStart starts one scratch terminal running program and registers a
// cleanup that closes it through the adapter if the phase did not.
func liveStart(t *testing.T, a *Adapter, program string) backend.Ref {
	t.Helper()
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	boot, err := backend.NewBootstrapCommand(exec.Opaque(program))
	if err != nil {
		t.Fatal(err)
	}
	name := "forgectl-live-" + randomSuffix(t)
	if err := CheckName(name); err != nil {
		t.Fatal(err)
	}
	spec, err := backend.NewStartSpec(cwd, name, tag, boot)
	if err != nil {
		t.Fatal(err)
	}
	res := a.Start(t.Context(), spec)
	ref, has := res.Ref()
	if !has || res.Failed() {
		c, _ := res.Cause()
		// A terminal named like this may exist; say so, so a person can
		// remove it by name. Nothing here removes it by a guess.
		t.Fatalf("Start of %q = %v (%v); if a terminal with that name exists, remove it with hc", name, res, c)
	}
	id, err := ref.HcIdentity()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("started %s as %s (created %s)", id.Terminal(), name, time.UnixMilli(id.CreatedMS()).UTC().Format(time.RFC3339Nano))
	t.Cleanup(func() {
		// Close checks the terminal's identity before it kills or removes
		// anything, so a reused address is refused rather than hit. It is
		// idempotent: already-gone is the normal answer after a passing phase.
		if c := a.Close(context.Background(), ref); c.State() != backend.CloseClosed && c.State() != backend.CloseAlreadyGone {
			cause, _ := c.Cause()
			t.Errorf("cleanup Close of %s = %v (%v); remove the terminal named %s with hc", id.Terminal(), c, cause, name)
		}
	})
	return ref
}

// liveClose closes ref and checks it is gone, and that a second Close agrees.
func liveClose(t *testing.T, a *Adapter, ref backend.Ref) {
	t.Helper()
	if c := a.Close(t.Context(), ref); c.State() != backend.CloseClosed {
		cause, _ := c.Cause()
		t.Fatalf("Close = %v (%v)", c, cause)
	}
	if p := a.Probe(t.Context(), ref); p.State() != backend.ProbeGone {
		t.Errorf("Probe after Close = %v, want gone", p)
	}
	if c := a.Close(t.Context(), ref); c.State() != backend.CloseAlreadyGone {
		t.Errorf("second Close = %v, want already-gone", c)
	}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
