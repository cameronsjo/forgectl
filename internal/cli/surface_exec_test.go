package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// validNonce is a well-formed 256-bit rendezvous nonce: 64 lowercase hex
// characters. Distinctive rather than random so a test asserting it did not
// leak can grep for it.
const validNonce = "beefcafe0123456789abcdef0123456789abcdef0123456789abcdef0beefcaf"

const validSocket = "/tmp/forgectl-surface-abc/sock"

// validBootstrapArgs is the one argv shape the classifier accepts.
func validBootstrapArgs() []string {
	return []string{
		"surface", "_exec",
		"--protocol", bootstrapProtocol,
		"--socket", validSocket,
		"--nonce", validNonce,
	}
}

// recordingTrampoline captures the request it was handed, so a test can assert
// the classifier parsed rather than merely accepted.
type recordingTrampoline struct {
	calls int
	got   bootstrapRequest
	err   error
}

func (r *recordingTrampoline) Run(_ context.Context, req bootstrapRequest) error {
	r.calls++
	r.got = req
	return r.err
}

// failIfCalledTrampoline fails the test if the classifier reaches the runtime.
type failIfCalledTrampoline struct{ t *testing.T }

func (f failIfCalledTrampoline) Run(context.Context, bootstrapRequest) error {
	f.t.Helper()
	f.t.Error("the trampoline runtime ran for an invocation that should have been refused")
	return nil
}

// TestTrySurfaceExec_AcceptsOnlyTheExactForm proves the happy path parses into
// the runtime, with the socket and nonce recovered exactly.
func TestTrySurfaceExec_AcceptsOnlyTheExactForm(t *testing.T) {
	rt := &recordingTrampoline{}

	handled, err := trySurfaceExec(context.Background(), validBootstrapArgs(), rt)
	if !handled {
		t.Fatal("a well-formed bootstrap was not claimed")
	}
	if err != nil {
		t.Fatalf("trySurfaceExec: %v", err)
	}
	if rt.calls != 1 {
		t.Fatalf("trampoline ran %d times, want exactly 1", rt.calls)
	}
	// Compared through SecretArg.Equal rather than a reveal accessor: the
	// request deliberately exposes no way to read its payload back out, so the
	// test asserts equality the same way production code would have to.
	if !rt.got.socket.Equal(exec.Secret(validSocket)) {
		t.Errorf("socket did not round-trip; want %q", validSocket)
	}
	if !rt.got.nonce.Equal(exec.Secret(validNonce)) {
		t.Errorf("nonce did not round-trip; want %q", validNonce)
	}
	// And a wrong value must not compare equal, or the two assertions above
	// would pass over a request that parsed nothing at all.
	if rt.got.nonce.Equal(exec.Secret(strings.Repeat("a", 64))) {
		t.Error("nonce compared equal to a value it was never given")
	}
}

// TestTrySurfaceExec_ClaimsAndRefusesEveryMalformedCandidate is the
// fall-through guard. A hand-crafted `surface _exec` must be claimed and
// refused, never allowed to continue into normal startup — because normal
// startup captures config, prepares the legacy boundary, builds the module
// registry, and (before this change) logged raw argv, which for a bootstrap
// candidate means logging a socket path and a rendezvous nonce.
//
// Each row asserts BOTH halves: claimed (handled) and refused (error). A row
// that returned handled=false would silently be the exact defect this exists
// to prevent.
func TestTrySurfaceExec_ClaimsAndRefusesEveryMalformedCandidate(t *testing.T) {
	tests := []struct {
		name string
		argv []string
	}{
		{"bare prefix, no flags", []string{"surface", "_exec"}},
		{"missing nonce", []string{"surface", "_exec", "--protocol", bootstrapProtocol, "--socket", validSocket}},
		{"flags out of order", []string{"surface", "_exec", "--socket", validSocket, "--protocol", bootstrapProtocol, "--nonce", validNonce}},
		{"equals form", []string{"surface", "_exec", "--protocol=" + bootstrapProtocol, "--socket=" + validSocket, "--nonce=" + validNonce}},
		{"abbreviated flag", []string{"surface", "_exec", "--proto", bootstrapProtocol, "--socket", validSocket, "--nonce", validNonce}},
		{"duplicate field", append(validBootstrapArgs(), "--nonce", validNonce)},
		{"positional extra", append(validBootstrapArgs(), "extra")},
		{"leading global flag", []string{"surface", "--no-icons", "_exec", "--protocol", bootstrapProtocol, "--socket", validSocket, "--nonce", validNonce}},
		{"unsupported protocol", []string{"surface", "_exec", "--protocol", "99", "--socket", validSocket, "--nonce", validNonce}},
		{"relative socket", []string{"surface", "_exec", "--protocol", bootstrapProtocol, "--socket", "sock", "--nonce", validNonce}},
		{"empty socket", []string{"surface", "_exec", "--protocol", bootstrapProtocol, "--socket", "", "--nonce", validNonce}},
		{"short nonce", []string{"surface", "_exec", "--protocol", bootstrapProtocol, "--socket", validSocket, "--nonce", "abc"}},
		{"uppercase nonce", []string{"surface", "_exec", "--protocol", bootstrapProtocol, "--socket", validSocket, "--nonce", strings.ToUpper(validNonce)}},
		{"non-hex nonce", []string{"surface", "_exec", "--protocol", bootstrapProtocol, "--socket", validSocket, "--nonce", strings.Repeat("z", 64)}},
		{"_exec after a surface subcommand", []string{"surface", "launch", "_exec"}},

		// The forms every LATER stage of the pipeline accepts, which is what
		// makes them the ones the classifier must not miss. A leading
		// --no-icons is skipped by both the launch intercept and the extension
		// rung — and the extension rung would hand an unregistered `surface`
		// verb, nonce and all, to a PATH-resolved forgectl-surface binary. A
		// forgiving spelling is rewritten to the canonical module name by argv
		// normalization once a surface module exists.
		{"inert global flag before surface", []string{"--no-icons", "surface", "_exec", "--protocol", bootstrapProtocol, "--socket", validSocket, "--nonce", validNonce}},
		{"inert global flag in = form", []string{"--no-icons=true", "surface", "_exec", "--protocol", bootstrapProtocol, "--socket", validSocket, "--nonce", validNonce}},
		{"non-inert global flag before surface", []string{"--version", "surface", "_exec", "--protocol", bootstrapProtocol, "--socket", validSocket, "--nonce", validNonce}},
		{"forgiving spelling of surface", []string{"Surface.", "_exec", "--protocol", bootstrapProtocol, "--socket", validSocket, "--nonce", validNonce}},
		{"uppercase surface", []string{"SURFACE", "_exec", "--protocol", bootstrapProtocol, "--socket", validSocket, "--nonce", validNonce}},
		{"flag and forgiving spelling together", []string{"--no-icons", "surface,", "_exec", "--protocol", bootstrapProtocol, "--socket", validSocket, "--nonce", validNonce}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handled, err := trySurfaceExec(context.Background(), tc.argv, failIfCalledTrampoline{t})
			if !handled {
				t.Fatalf("argv %q fell through to normal startup; a bootstrap candidate must be claimed", tc.argv)
			}
			if err == nil {
				t.Fatal("claimed the candidate but returned no error")
			}
		})
	}
}

// TestTrySurfaceExec_RefusalNeverEchoesValues pins the bounded,
// category-only error contract. The refusal path handles a socket path and a
// rendezvous nonce; an error that quoted the offending token would print the
// nonce to stderr, which is the leak the whole early seam exists to avoid.
func TestTrySurfaceExec_RefusalNeverEchoesValues(t *testing.T) {
	secrets := []string{validNonce, validSocket, "supersecretvalue"}

	argvs := [][]string{
		{"surface", "_exec", "--protocol", "99", "--socket", validSocket, "--nonce", validNonce},
		{"surface", "_exec", "--protocol", bootstrapProtocol, "--socket", validSocket, "--nonce", "supersecretvalue"},
		append(validBootstrapArgs(), "supersecretvalue"),
		{"surface", "--no-icons", "_exec", "--socket", validSocket, "--nonce", validNonce},
	}

	for _, argv := range argvs {
		handled, err := trySurfaceExec(context.Background(), argv, failIfCalledTrampoline{t})
		if !handled || err == nil {
			t.Fatalf("argv %q: handled=%v err=%v, want claimed and refused", argv, handled, err)
		}
		for _, secret := range secrets {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error %q echoes %q", err.Error(), secret)
			}
		}
	}
}

// TestTrySurfaceExec_LeavesOrdinaryInvocationsAlone is the other side of the
// claim boundary. The classifier runs before everything, so an over-broad
// candidate test would break unrelated commands — and the launch passthrough
// is the one that matters: it forwards its args to claude byte-clean, so an
// operator prompt containing the literal `_exec` must not be intercepted.
func TestTrySurfaceExec_LeavesOrdinaryInvocationsAlone(t *testing.T) {
	tests := [][]string{
		nil,
		{},
		{"--version"},
		{"tmux", "list"},
		{"launch"},
		{"launch", "-p", "_exec"},
		{"launch", "_exec"},
		{"cl", "-p", "explain _exec to me"},
		{"surface"},
		{"surface", "launch", "--name", "forgectl"},
		{"_exec"},
		{"_exec", "--protocol", bootstrapProtocol},
	}

	for _, argv := range tests {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			handled, err := trySurfaceExec(context.Background(), argv, failIfCalledTrampoline{t})
			if handled {
				t.Errorf("argv %q was claimed as a bootstrap candidate; it must fall through", argv)
			}
			if err != nil {
				t.Errorf("unclaimed argv returned err = %v, want nil", err)
			}
		})
	}
}

// TestBootstrapRequest_RendersRedacted keeps the socket and nonce out of every
// formatting verb. A refusal is bounded by construction, but a future debug
// line or a wrapped error would otherwise be one %v away from printing the
// nonce.
func TestBootstrapRequest_RendersRedacted(t *testing.T) {
	rt := &recordingTrampoline{}
	if _, err := trySurfaceExec(context.Background(), validBootstrapArgs(), rt); err != nil {
		t.Fatalf("trySurfaceExec: %v", err)
	}

	renders := map[string]string{
		"%v":  fmt.Sprintf("%v", rt.got),
		"%+v": fmt.Sprintf("%+v", rt.got),
		"%#v": fmt.Sprintf("%#v", rt.got),
		// The %s verb is the point of the row, not an accidental Sprintf — the
		// test exists to prove every verb redacts, including the one a String
		// method makes "redundant".
		"%s": fmt.Sprintf("%s", rt.got), //nolint:staticcheck // exercising the verb is the assertion
		"%q": fmt.Sprintf("%q", rt.got),
	}
	for verb, rendered := range renders {
		if rendered == "" {
			t.Errorf("%s rendered nothing; this test would pass vacuously", verb)
		}
		for _, secret := range []string{validNonce, validSocket} {
			if strings.Contains(rendered, secret) {
				t.Errorf("%s rendered the %s: %s", verb, secretName(secret), rendered)
			}
		}
	}
}

func secretName(s string) string {
	if s == validNonce {
		return "nonce"
	}
	return "socket path"
}

// TestProductionTrampolineRuntime_ReceivesAValidBootstrap pins the handoff.
//
// The fixture names a socket that does not exist, so the runtime refuses at its
// first check — and that refusal is the evidence, because it can only come from
// the trampoline. A nil return here would mean a valid bootstrap was claimed and
// then quietly did nothing.
func TestProductionTrampolineRuntime_ReceivesAValidBootstrap(t *testing.T) {
	handled, err := trySurfaceExec(context.Background(), validBootstrapArgs(), productionTrampolineRuntime())
	if !handled {
		t.Fatal("a well-formed bootstrap was not claimed")
	}
	if !errors.Is(err, errSocketUnsafe) {
		t.Fatalf("err = %v, want errSocketUnsafe", err)
	}
}
