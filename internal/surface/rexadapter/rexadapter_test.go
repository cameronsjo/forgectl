// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package rexadapter

import (
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// TestStartCreatesLabelsAndReturnsARef is the clean launch: identity and the
// cwd check first, then rex new, the rename, the read-back, and a reference
// that round-trips with the block id and the creation time it embeds.
func TestStartCreatesLabelsAndReturnsARef(t *testing.T) {
	f := newFake(t)
	f.secrets = []string{testBootstrap, testNonce}
	a := f.adapter()
	ref := start(t, f, a)

	if got, want := joinVerbs(f.verbs), "whoami test new block-rename block-ls"; got != want {
		t.Errorf("verbs = %q, want %q", got, want)
	}
	data, err := ref.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	back, err := backend.DecodeRef(data)
	if err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	id, err := back.RexIdentity()
	if err != nil || id.Session() != fxSession || id.Block() != fxBlock || id.CreatedMS() != fxBlockMS {
		t.Fatalf("identity = %+v, %v", id, err)
	}
	if a.Kind() != backend.KindRex {
		t.Errorf("Kind = %v", a.Kind())
	}
	if _, err := backend.RequireCapabilities(a); err != nil {
		t.Errorf("the adapter cannot clean up after itself: %v", err)
	}
}

// TestStartFlags pins the create's flags: --keep-open (a finished block stays
// readable), --shell=none, the ownership name as the session label, and no
// --autostart, so a missing server is never started by forgectl.
func TestStartFlags(t *testing.T) {
	f := newFake(t)
	start(t, f, f.adapter())
	var create exec.SensitiveCommand
	for _, c := range f.run.Calls() {
		if c.Kind == exec.KindRexCreate {
			create = c
		}
	}
	has := func(want exec.Arg) bool {
		return slices.ContainsFunc(create.Args, want.Equal)
	}
	for _, flag := range []string{"--keep-open", "--shell=none", "--json"} {
		if !has(exec.Opaque(flag)) && !hasFixed(create, flag) {
			t.Errorf("rex new lacks %s", flag)
		}
	}
	if !has(exec.Opaque(f.wantOwner)) {
		t.Error("rex new does not label the session with the ownership name")
	}
	for _, c := range f.run.Calls() {
		if c.Kind == exec.KindRexPreflight {
			continue
		}
		if !c.Args[0].Equal(exec.MustFixed("--autostart=false")) || !c.Args[1].Equal(exec.MustFixed("--color=never")) {
			t.Errorf("a %s call does not start with --autostart=false --color=never", c.Kind)
		}
	}
}

// hasFixed reports whether cmd carries the fixed flag. MustFixed takes only a
// constant, so each flag the tests look for is spelled out.
func hasFixed(cmd exec.SensitiveCommand, flag string) bool {
	known := map[string]exec.Arg{
		"--keep-open": exec.MustFixed("--keep-open"), "--shell=none": exec.MustFixed("--shell=none"),
		"--json": exec.MustFixed("--json"),
	}
	want, ok := known[flag]
	return ok && slices.ContainsFunc(cmd.Args, want.Equal)
}

// TestLaunchScriptLifecycle is DECISION 1's contract: the bootstrap is in a
// 0600 file in a 0700 directory when rex new runs, never in any argv the
// runner saw, and nothing of the script is left after Start, on success and
// on every failure path.
func TestLaunchScriptLifecycle(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		f := newFake(t)
		f.secrets = []string{testBootstrap, testNonce}
		start(t, f, f.adapter())
		if f.seenFileMode&^0o600 != 0 || f.seenDirMode != 0o700 {
			t.Errorf("script mode %o in directory mode %o, want at most 0600 in 0700", f.seenFileMode, f.seenDirMode)
		}
		if want := "/bin/rm -f -- \"$0\"\nexec " + testBootstrap + "\n"; f.seenScript != want {
			t.Errorf("script = %q, want %q", f.seenScript, want)
		}
		if left := scriptsLeft(t, f.scriptDir); len(left) != 0 {
			t.Errorf("Start left %v under the script dir", left)
		}
	})
	failures := map[string]func(*fakeRex){
		"rex new fails":        func(f *fakeRex) { f.newFail = &fail{stderr: "Error: boom\n", exit: 1} },
		"rex new is malformed": func(f *fakeRex) { f.newReply = `{"session_id":"nope"}` },
		"rename fails":         func(f *fakeRex) { f.fails["block-rename"] = fail{stderr: "Error: boom\n", exit: 1} },
		"read-back fails":      func(f *fakeRex) { f.fails["block-ls"] = fail{stderr: "Error: boom\n", exit: 1} },
		"never consumed":       func(f *fakeRex) { f.consume = false },
	}
	for name, mutate := range failures {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.secrets = []string{testBootstrap, testNonce}
			mutate(f)
			spec := testSpec(t)
			f.wantOwner = spec.OwnershipName()
			res := f.adapter().Start(t.Context(), spec)
			if !res.Failed() {
				t.Fatalf("Start = %v, want a failure", res)
			}
			if left := scriptsLeft(t, f.scriptDir); len(left) != 0 {
				t.Errorf("a failed Start left %v under the script dir", left)
			}
		})
	}
}

// TestStartRefusesBeforeCreating: a server on another API revision, a remote
// server, an unreadable whoami, and a missing target directory all refuse
// before rex new runs or a script is written.
func TestStartRefusesBeforeCreating(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*fakeRex)
		class  backend.StartFailureClass
	}{
		"api revision 13": {func(f *fakeRex) {
			f.whoami = strings.Replace(f.whoami, `"api_revision": 14`, `"api_revision": 13`, 1)
		}, backend.FailureIncompatible},
		"api revision 15": {func(f *fakeRex) {
			f.whoami = strings.Replace(f.whoami, `"api_revision": 14`, `"api_revision": 15`, 1)
		}, backend.FailureIncompatible},
		"no api revision": {func(f *fakeRex) {
			f.whoami = strings.Replace(f.whoami, `"api_revision": 14,`, ``, 1)
		}, backend.FailureMalformedResponse},
		"remote server": {func(f *fakeRex) {
			f.whoami = strings.Replace(f.whoami, `"unix:///Users`, `"tcp://100.64.0.1:7777/Users`, 1)
		}, backend.FailureIncompatible},
		"tailnet sharing enabled": {func(f *fakeRex) {
			f.whoami = strings.Replace(f.whoami, `"state": "disabled"`, `"state": "enabled"`, 1)
		}, backend.FailureIncompatible},
		"tailnet members allowed": {func(f *fakeRex) {
			f.whoami = strings.Replace(f.whoami, `"allow_tailnet_members": false`, `"allow_tailnet_members": true`, 1)
		}, backend.FailureIncompatible},
		"no tailscale report": {func(f *fakeRex) {
			f.whoami = strings.Replace(f.whoami, `"tailscale"`, `"x"`, 1)
		}, backend.FailureMalformedResponse},
		"no instance id": {func(f *fakeRex) {
			f.whoami = strings.Replace(f.whoami, `"instance_id"`, `"instance"`, 1)
		}, backend.FailureMalformedResponse},
		"no server": {func(f *fakeRex) {
			f.fails["whoami"] = fail{stderr: "Error: dial unix: connect: no such file or directory\n", exit: 1}
		}, backend.FailureUnavailable},
		"missing directory": {func(f *fakeRex) { f.dirMissing = true }, backend.FailureInternal},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			tc.mutate(f)
			spec := testSpec(t)
			f.wantOwner = spec.OwnershipName()
			res := f.adapter().Start(t.Context(), spec)
			cause, _ := res.Cause()
			if res.Outcome() != backend.NotMutated || cause.Class() != tc.class {
				t.Fatalf("Start = %v cause %v, want not-mutated %v", res, cause, tc.class)
			}
			if slices.Contains(f.verbs, "new") {
				t.Fatalf("verbs = %v: rex new ran after a refusal", f.verbs)
			}
			if left := scriptsLeft(t, f.scriptDir); len(left) != 0 {
				t.Errorf("a refusal left %v under the script dir", left)
			}
		})
	}
}

// TestStartReconciles covers every outcome of the listing that settles a
// create Rex did not report cleanly.
func TestStartReconciles(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate  func(*fakeRex)
		outcome backend.MutationOutcome
	}{
		"failed create, no session": {func(f *fakeRex) { f.newFail = &fail{stderr: "Error: boom\n", exit: 1} }, backend.NotMutated},
		// The create landed but its reply was lost, so the rename never ran:
		// the session is ours by its label, its block still has Rex's
		// default label, and nothing proves the block is the launch's.
		"reply lost after the create": {func(f *fakeRex) {
			f.newFail, f.newCreates = &fail{stderr: "Error: boom\n", exit: 1}, true
		}, backend.OutcomeUnknown},
		// The rename failed, so the block still has Rex's default label: the
		// session is ours by its label, but the block cannot be proven so.
		"rename failed": {func(f *fakeRex) { f.fails["block-rename"] = fail{stderr: "Error: boom\n", exit: 1} }, backend.OutcomeUnknown},
		"listing fails": {func(f *fakeRex) {
			f.newFail = &fail{stderr: "Error: boom\n", exit: 1}
			f.fails["ls"] = fail{stderr: "Error: boom\n", exit: 1}
		}, backend.OutcomeUnknown},
		"two sessions under the name": {func(f *fakeRex) {
			f.newFail, f.newCreates = &fail{stderr: "Error: boom\n", exit: 1}, true
			f.sessions = []*fakeSession{{id: fxSession2, label: "", blocks: nil}}
		}, backend.OutcomeUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			tc.mutate(f)
			spec := testSpec(t)
			f.wantOwner = spec.OwnershipName()
			for _, s := range f.sessions {
				if s.label == "" {
					s.label = spec.OwnershipName()
				}
			}
			res := f.adapter().Start(t.Context(), spec)
			if res.Outcome() != tc.outcome {
				t.Fatalf("outcome = %v, want %v (verbs %v)", res.Outcome(), tc.outcome, f.verbs)
			}
			if !res.Failed() {
				t.Error("a reconciled start must carry the create's cause")
			}
		})
	}
}

// TestReconcileFindsALabeledBlock: one session under the ownership name whose
// one block carries the owner label is RefKnown with the create's cause, so
// the service closes exactly that block.
func TestReconcileFindsALabeledBlock(t *testing.T) {
	f := newFake(t)
	spec := testSpec(t)
	f.wantOwner = spec.OwnershipName()
	f.sessions = []*fakeSession{{id: fxSession, label: spec.OwnershipName(),
		blocks: []*fakeBlock{{id: fxBlock, label: ownerLabelKey + spec.OwnershipName()}}}}
	srv := fingerprint(t, f)
	res := f.adapter().reconcile(t.Context(), spec.Tag(), srv, backend.NewStartCause(backend.FailureTimeout, nil))
	ref, has := res.Ref()
	if res.Outcome() != backend.RefKnown || !has || !res.Failed() {
		t.Fatalf("reconcile = %v, want RefKnown with a cause", res)
	}
	if id, _ := ref.RexIdentity(); id.Block() != fxBlock {
		t.Errorf("reconciled block = %q", id.Block())
	}
}

// fingerprint is the server id the fake's whoami reply fingerprints to.
func fingerprint(t *testing.T, f *fakeRex) backend.ServerID {
	t.Helper()
	srv, cause := f.adapter().readiness(t.Context())
	if cause != nil {
		t.Fatal(*cause)
	}
	return srv
}

// TestProbeStates is DECISION 2: a running block is present, a finished one
// is gone with its exit code, and an absent one is gone, whether its block or
// its whole session is missing.
func TestProbeStates(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	ref := start(t, f, a)
	if res := a.Probe(t.Context(), ref); res.State() != backend.ProbePresent {
		t.Fatalf("running Probe = %v", res)
	}
	three := 3
	f.sessions[0].blocks[0].exit = &three
	res := a.Probe(t.Context(), ref)
	code, finished := res.Finished()
	if res.State() != backend.ProbeGone || !finished || code != 3 {
		t.Fatalf("finished Probe = %v (finished %v code %d), want gone, finished, 3", res, finished, code)
	}
	f.processReply = fixture(t, "process_exit7.json")
	if code, _ := a.Probe(t.Context(), ref).Finished(); code != 7 {
		t.Errorf("recorded exit-7 reply read as %d", code)
	}
	f.processReply = ""
	f.sessions[0].blocks = append(f.sessions[0].blocks[:0], &fakeBlock{id: fxBlock2, label: "shell"})
	res = a.Probe(t.Context(), ref)
	if _, finished := res.Finished(); res.State() != backend.ProbeGone || finished {
		t.Fatalf("absent block Probe = %v, want gone and not finished", res)
	}
	f.sessions = nil
	if res := a.Probe(t.Context(), ref); res.State() != backend.ProbeGone {
		t.Fatalf("absent session Probe = %v, want gone", res)
	}
}

// TestUnreadableIsNeverGone: every failure the parsers or the not-found rule
// cannot positively read is unreadable. rc 3 with any text but the exact
// not-found line, the right text with another exit code, a not-found naming
// another id, a timeout, and replies that do not parse.
func TestUnreadableIsNeverGone(t *testing.T) {
	cases := map[string]func(t *testing.T, f *fakeRex){
		"ambiguous at rc 3": func(t *testing.T, f *fakeRex) {
			f.fails["block-call"] = fail{stderr: fixture(t, "stderr_ambiguous.txt"), exit: 3}
		},
		"not found at rc 1": func(_ *testing.T, f *fakeRex) {
			f.fails["block-call"] = fail{stderr: "Error: block target \"" + fxBlock + "\" not found\n", exit: 1}
		},
		"not found for another id": func(t *testing.T, f *fakeRex) {
			f.fails["block-call"] = fail{stderr: fixture(t, "stderr_block_not_found.txt"), exit: 3}
		},
		"recorded session not found for another target": func(t *testing.T, f *fakeRex) {
			f.fails["block-ls"] = fail{stderr: fixture(t, "stderr_session_not_found.txt"), exit: 3}
		},
		"timeout at rc 1": func(t *testing.T, f *fakeRex) {
			f.fails["block-call"] = fail{stderr: fixture(t, "stderr_wait_timeout.txt"), exit: 1}
		},
		"unknown method": func(t *testing.T, f *fakeRex) {
			f.fails["block-call"] = fail{stderr: fixture(t, "stderr_unknown_method.txt"), exit: 1}
		},
		"no last_exit": func(_ *testing.T, f *fakeRex) { f.processReply = `{"home":"/Users/operator"}` },
		"exit without a code": func(_ *testing.T, f *fakeRex) {
			f.processReply = `{"last_exit":{"error":"exit status 1"}}`
		},
		"process garbage": func(_ *testing.T, f *fakeRex) { f.processReply = `not json` },
		"block ls row unreadable": func(_ *testing.T, f *fakeRex) {
			f.sessions[0].blocks = append(f.sessions[0].blocks, &fakeBlock{id: "block:not-a-uuid", label: "x"})
		},
		"whoami unreadable": func(_ *testing.T, f *fakeRex) { f.whoami = `{"api_revision":14}` },
		"api revision moved": func(_ *testing.T, f *fakeRex) {
			f.whoami = strings.Replace(f.whoami, `"api_revision": 14`, `"api_revision": 15`, 1)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			a := f.adapter()
			ref := start(t, f, a)
			mutate(t, f)
			if res := a.Probe(t.Context(), ref); res.State() != backend.ProbeUnreadable {
				t.Fatalf("Probe = %v, want unreadable", res)
			}
			f.verbs = nil
			if res := a.Close(t.Context(), ref); res.State() == backend.CloseAlreadyGone || res.State() == backend.CloseClosed && !closeRan(f) {
				t.Fatalf("Close = %v without closing anything; an unreadable block is not gone", res)
			}
		})
	}
}

func closeRan(f *fakeRex) bool { return slices.Contains(f.verbs, "block-close") }

// TestIdentityMismatchActsOnNothing: a restarted server (new instance_id),
// another host, a relabeled block, and a foreign block under the right label
// at another id are all identity mismatches, and Close runs no block close
// or kill on any of them.
func TestIdentityMismatchActsOnNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*fakeRex, backend.Ref)
		want   backend.ProbeState
	}{
		"server restarted": {func(f *fakeRex, _ backend.Ref) {
			f.whoami = strings.Replace(f.whoami, "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-0000000000ff", 1)
		}, backend.ProbeIdentityMismatch},
		"another host": {func(f *fakeRex, _ backend.Ref) {
			f.whoami = strings.Replace(f.whoami, "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-0000000000fe", 1)
		}, backend.ProbeIdentityMismatch},
		"another endpoint": {func(f *fakeRex, _ backend.Ref) {
			f.whoami = strings.Replace(f.whoami, "server.sock", "other.sock", 1)
		}, backend.ProbeIdentityMismatch},
		"block relabeled": {func(f *fakeRex, _ backend.Ref) {
			f.sessions[0].blocks[0].label = "shell"
		}, backend.ProbeIdentityMismatch},
		"label of another launch": {func(f *fakeRex, _ backend.Ref) {
			f.sessions[0].blocks[0].label = ownerLabelKey + "fc-surface-" + strings.Repeat("0", 32)
		}, backend.ProbeIdentityMismatch},
		// The owner label moved to another block: the reference's own block
		// is gone, and the labeled one is not it.
		"label on another block": {func(f *fakeRex, ref backend.Ref) {
			f.sessions[0].blocks = []*fakeBlock{{id: fxBlock2, label: ownerLabelKey + ref.OwnershipName()}}
		}, backend.ProbeGone},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			a := f.adapter()
			ref := start(t, f, a)
			tc.mutate(f, ref)
			if res := a.Probe(t.Context(), ref); res.State() != tc.want {
				t.Fatalf("Probe = %v, want %v", res, tc.want)
			}
			f.verbs = nil
			res := a.Close(t.Context(), ref)
			if tc.want == backend.ProbeIdentityMismatch && res.State() != backend.CloseIdentityMismatch {
				t.Fatalf("Close = %v, want identity-mismatch", res)
			}
			if slices.Contains(f.verbs, "block-close") || slices.Contains(f.verbs, "kill") {
				t.Fatalf("verbs = %v: a block that is not provably the reference's was acted on", f.verbs)
			}
		})
	}
}

// TestCloseRetiresOnlyItsOwnEmptySession: closing the worker's block kills
// the session when it is the one Start created and now holds nothing; a
// session holding another block, or relabeled, is left alone.
func TestCloseRetiresOnlyItsOwnEmptySession(t *testing.T) {
	t.Run("own and empty", func(t *testing.T) {
		f := newFake(t)
		a := f.adapter()
		ref := start(t, f, a)
		f.verbs = nil
		if res := a.Close(t.Context(), ref); res.State() != backend.CloseClosed {
			t.Fatalf("Close = %v", res)
		}
		if got, want := joinVerbs(f.verbs), "whoami block-ls block-close block-ls ls block-ls kill"; got != want {
			t.Errorf("verbs = %q, want %q", got, want)
		}
		if len(f.sessions) != 0 {
			t.Error("the session Start created is still there")
		}
		f.verbs = nil
		if res := a.Close(t.Context(), ref); res.State() != backend.CloseAlreadyGone {
			t.Fatalf("second Close = %v, want already-gone", res)
		}
	})
	t.Run("another block in it", func(t *testing.T) {
		f := newFake(t)
		a := f.adapter()
		ref := start(t, f, a)
		f.sessions[0].blocks = append(f.sessions[0].blocks, &fakeBlock{id: fxBlock2, label: "notes"})
		if res := a.Close(t.Context(), ref); res.State() != backend.CloseClosed {
			t.Fatalf("Close = %v", res)
		}
		if slices.Contains(f.verbs, "kill") || len(f.sessions) != 1 || len(f.sessions[0].blocks) != 1 {
			t.Errorf("verbs %v: a session holding a person's block was killed or emptied", f.verbs)
		}
	})
	t.Run("relabeled session", func(t *testing.T) {
		f := newFake(t)
		a := f.adapter()
		ref := start(t, f, a)
		f.sessions[0].label = "my-work"
		if res := a.Close(t.Context(), ref); res.State() != backend.CloseClosed {
			t.Fatalf("Close = %v", res)
		}
		if slices.Contains(f.verbs, "kill") {
			t.Errorf("verbs %v: a session not labeled with the ownership name was killed", f.verbs)
		}
	})
	t.Run("still listed after close", func(t *testing.T) {
		f := newFake(t)
		a := f.adapter()
		ref := start(t, f, a)
		// rex reports the close done but still lists the block.
		f.run.RunFunc = func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
			if verb(cmd) == "block-close" {
				f.verbs = append(f.verbs, "block-close")
				return ok(""), nil
			}
			return f.handle(cmd)
		}
		if res := a.Close(t.Context(), ref); res.State() != backend.CloseFailed {
			t.Fatalf("Close = %v, want failed while rex still lists the block", res)
		}
	})
}

// TestEveryRexCallDropsTheLauncherHandles: every call, the cwd check
// included, carries the mutation that drops the launching session's handles.
func TestEveryRexCallDropsTheLauncherHandles(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	ref := start(t, f, a)
	a.Probe(t.Context(), ref)
	a.Close(t.Context(), ref)
	calls := f.run.Calls()
	if len(calls) < 10 {
		t.Fatalf("only %d calls recorded", len(calls))
	}
	want := exec.UnsetLauncherSessionHandles()
	for i, c := range calls {
		if len(c.Env) != 1 || !c.Env[0].Equal(want) {
			t.Errorf("call %d (%s) does not drop the launcher's session handles", i, c.Kind)
		}
	}
}

// TestNewRefuses: a relative rex path, a missing runner, and a relative
// script directory refuse.
func TestNewRefuses(t *testing.T) {
	if _, err := New(&exec.FakeSensitiveRunner{}, "rex"); err == nil {
		t.Error("a relative rex path was accepted")
	}
	if _, err := New(nil, "/rex"); err == nil {
		t.Error("a nil runner was accepted")
	}
	if _, err := New(&exec.FakeSensitiveRunner{}, "/rex", WithScriptDir("tmp")); err == nil {
		t.Error("a relative script directory was accepted")
	}
}
