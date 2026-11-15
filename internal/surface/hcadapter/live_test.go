// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package hcadapter

import (
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
// FORGECTL_LIVE_HC=1. It creates exactly one background terminal named
// forgectl-live-<5 random characters> whose program is a short harmless
// command: Start, Probe while it runs, Probe once it has finished, Close, then
// Probe and a second Close to see it gone. It touches no other terminal, and
// removes the one it made if Close left it. Run it on the hc version the
// package doc names; the version the run saw is logged.
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
	ctx := t.Context()
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	boot, err := backend.NewBootstrapCommand(exec.Opaque("'/bin/sh' '-c' 'sleep 3; exit 7'"))
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

	res := a.Start(ctx, spec)
	ref, has := res.Ref()
	if !has || res.Failed() {
		c, _ := res.Cause()
		t.Fatalf("Start = %v (%v)", res, c)
	}
	id, err := ref.HcIdentity()
	if err != nil {
		t.Fatal(err)
	}
	address := id.Terminal()
	t.Logf("started %s (created %s)", address, time.UnixMilli(id.CreatedMS()).UTC().Format(time.RFC3339Nano))
	// Only the terminal this test made is ever removed, found by the address
	// Start returned; nothing is removed by a guess.
	t.Cleanup(func() {
		for _, verb := range []string{"kill", "rm"} {
			out, err := osexec.Command(hcPath, verb, address).CombinedOutput() //nolint:gosec,noctx // G204: the test's own scratch terminal
			t.Logf("cleanup: hc %s %s: err=%v out=%s", verb, address, err, out)
		}
	})

	if p := a.Probe(ctx, ref); p.State() != backend.ProbePresent {
		t.Fatalf("Probe while running = %v", p)
	}
	// Let the program finish, then record what Probe says of a finished
	// terminal; it is logged, not asserted, because it is what this run
	// measures.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if p := a.Probe(ctx, ref); p.State() != backend.ProbePresent {
			t.Logf("Probe after the program finished = %v", p)
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	if c := a.Close(ctx, ref); c.State() != backend.CloseClosed {
		cause, _ := c.Cause()
		t.Fatalf("Close = %v (%v)", c, cause)
	}
	if p := a.Probe(ctx, ref); p.State() != backend.ProbeGone {
		t.Errorf("Probe after Close = %v, want gone", p)
	}
	if c := a.Close(ctx, ref); c.State() != backend.CloseAlreadyGone {
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
