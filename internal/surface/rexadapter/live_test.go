// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package rexadapter

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// liveRexEnv gates the live test: it creates one Rex session, so it never
// runs by default.
const liveRexEnv = "FORGECTL_LIVE_REX"

// liveRexPathEnv names the rex binary; the default is where Rex Beta
// installs it.
const (
	liveRexPathEnv     = "FORGECTL_REX_PATH"
	defaultLiveRexPath = "/Applications/Rex Beta.app/Contents/Helpers/Rex Server.rex/Contents/MacOS/rex"
)

// recordingRunner runs commands for real and records which rex verb each was.
type recordingRunner struct {
	mu    sync.Mutex
	inner exec.SensitiveRunner
	verbs []string
}

func (r *recordingRunner) RunSensitive(ctx context.Context, cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
	r.mu.Lock()
	r.verbs = append(r.verbs, verb(cmd))
	r.mu.Unlock()
	return r.inner.RunSensitive(ctx, cmd)
}

// TestLiveRex drives the adapter against the running Rex server, with
// FORGECTL_LIVE_REX=1. It creates exactly one detached session, named
// forgectl-adapter-test-<5 random characters>, whose one block runs a
// harmless command through a real launch script: Start, Probe while it runs,
// Probe once it has exited 7, Close. It then confirms `rex ls` no longer lists
// that session, and kills that one session id itself if Close left it. It
// touches no other session.
func TestLiveRex(t *testing.T) {
	if os.Getenv(liveRexEnv) != "1" {
		t.Skipf("set %s=1 to run against a live Rex server", liveRexEnv)
	}
	rexPath := os.Getenv(liveRexPathEnv)
	if rexPath == "" {
		rexPath = defaultLiveRexPath
	}
	label := "forgectl-adapter-test-" + randomSuffix(t)
	run := &recordingRunner{inner: exec.NewOSSensitiveRunner()}
	a, err := New(run, rexPath)
	if err != nil {
		t.Fatal(err)
	}
	a.sessionLabel = func(backend.RecoveryTag) string { return label }

	// Every session this test may remove is found by its own label first;
	// nothing is removed by a guess.
	t.Cleanup(func() {
		for _, id := range liveSessionsLabeled(t, rexPath, label) {
			t.Logf("cleanup: rex kill %s (Close left it)", id)
			out, err := osexec.Command(rexPath, "--autostart=false", "kill", id).CombinedOutput() //nolint:gosec,noctx // G204: the test's own scratch session id
			if err != nil {
				t.Errorf("cleanup kill %s: %v: %s", id, err, out)
			}
		}
		if left := liveSessionsLabeled(t, rexPath, label); len(left) != 0 {
			t.Errorf("rex ls still lists %v after cleanup", left)
		}
	})

	ctx := t.Context()
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// A missing directory refuses before Rex is asked for anything.
	boot, err := backend.NewBootstrapCommand(exec.Opaque("'/bin/sh' '-c' 'sleep 3; exit 7'"))
	if err != nil {
		t.Fatal(err)
	}
	missing, err := backend.NewStartSpec(filepath.Join(cwd, "absent"), "forgectl adapter test", tag, boot)
	if err != nil {
		t.Fatal(err)
	}
	if res := a.Start(ctx, missing); res.Outcome() != backend.NotMutated {
		t.Fatalf("Start in a missing directory = %v, want not-mutated", res)
	}
	if left := liveSessionsLabeled(t, rexPath, label); len(left) != 0 {
		t.Fatalf("a refused Start left session %v", left)
	}

	spec, err := backend.NewStartSpec(cwd, "forgectl adapter test", tag, boot)
	if err != nil {
		t.Fatal(err)
	}
	res := a.Start(ctx, spec)
	ref, has := res.Ref()
	if !has || res.Failed() {
		c, _ := res.Cause()
		t.Fatalf("Start = %v (%v); calls %v", res, c, run.verbs)
	}
	id, _ := ref.RexIdentity()
	t.Logf("started %s in %s (created %s)", id.Block(), id.Session(), time.UnixMilli(id.CreatedMS()).UTC().Format(time.RFC3339Nano))
	if p := a.Probe(ctx, ref); p.State() != backend.ProbePresent {
		t.Fatalf("Probe while running = %v", p)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		p := a.Probe(ctx, ref)
		if code, finished := p.Finished(); finished {
			if code != 7 {
				t.Fatalf("finished with %d, want 7", code)
			}
			break
		}
		if p.State() != backend.ProbePresent || time.Now().After(deadline) {
			t.Fatalf("Probe = %v before the block finished", p)
		}
		time.Sleep(200 * time.Millisecond)
	}

	run.verbs = nil
	if c := a.Close(ctx, ref); c.State() != backend.CloseClosed {
		cause, _ := c.Cause()
		t.Fatalf("Close = %v (%v); calls %v", c, cause, run.verbs)
	}
	t.Logf("close ran: %v", run.verbs)
	if left := liveSessionsLabeled(t, rexPath, label); len(left) != 0 {
		t.Errorf("Close left session %v", left)
	}
	if p := a.Probe(ctx, ref); p.State() != backend.ProbeGone {
		t.Errorf("Probe after Close = %v, want gone", p)
	}
	if c := a.Close(ctx, ref); c.State() != backend.CloseAlreadyGone {
		t.Errorf("second Close = %v, want already-gone", c)
	}
}

// liveSessionsLabeled lists the ids of sessions carrying exactly label.
func liveSessionsLabeled(t *testing.T, rexPath, label string) []string {
	t.Helper()
	out, err := osexec.Command(rexPath, "--autostart=false", "--color=never", "ls", "--json").Output() //nolint:gosec,noctx // G204: the operator's rex binary, fixed arguments
	if err != nil {
		t.Fatalf("rex ls: %v", err)
	}
	var l struct {
		Sessions []struct {
			ID    string `json:"session_id"`
			Label string `json:"label"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(out, &l); err != nil {
		t.Fatalf("rex ls: %v", err)
	}
	var ids []string
	for _, s := range l.Sessions {
		if s.Label == label {
			ids = append(ids, s.ID)
		}
	}
	return ids
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
