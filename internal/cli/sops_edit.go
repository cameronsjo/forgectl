package cli

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	sopspkg "github.com/cameronsjo/forgectl/internal/sops"
)

// The fixed file names inside the work directory. Only the directory's path
// travels in the child's environment; every file within it is at a name both
// sides already know, so there is nothing an attacker could redirect by
// setting a variable.
const (
	sopsNonceFile  = "nonce"
	sopsValueFile  = "value"
	sopsResultFile = "result"
	sopsCountFile  = "count"
)

// Environment variables the driver sets and this command reads. Mirrored from
// internal/exec's mutation constructors, which own the write side.
const (
	sopsWorkdirEnv = "FORGECTL_SOPS_WORKDIR"
	sopsPathEnv    = "FORGECTL_SOPS_PATH"
	sopsNonceEnv   = "FORGECTL_SOPS_NONCE"
)

// newSopsEditCmd builds the hidden `__sops-edit` subcommand: the editor sops
// invokes, which is forgectl re-invoking itself.
//
// # Why an editor at all
//
// `sops set file '["a"]["b"]' '"value"'` puts the plaintext in argv, visible
// in `ps` and left in shell history — the exposure this feature exists to
// close. `sops <file>` instead decrypts to a temp file, runs $EDITOR on it,
// and re-encrypts whatever comes back. So the value can travel by file while
// the key path travels by environment, and no process ever takes the secret
// as an argument.
//
// # Three guards, and what each one actually bounds
//
//  1. A nonce, checked against a file in the work directory. This bounds a
//     STRAY invocation — sops re-running the editor after a run's files are
//     gone, a replay from a stale environment, a hand-typed invocation that
//     forgot the protocol. It is deliberately NOT described as a privilege
//     boundary, because it is not one: a caller who can set this process's
//     environment can also create the directory and nonce file it names, so
//     the nonce buys nothing against them. Nor does it need to — a caller who
//     can exec forgectl can already write YAML with a shell, so this
//     subcommand grants no capability its invoker lacked. `Hidden: true` is
//     presentation, not a control, for the same reason.
//  2. Output validation. It parses its own result and exits non-zero rather
//     than handing sops a document sops cannot read. This one IS load-bearing:
//     it is what keeps sops out of its unbounded editor re-invocation loop
//     (measured on 3.13.3: 36,851 invocations and 8.4 MB of stderr in three
//     minutes). A non-zero editor exit gives a clean rc=201 with the
//     encrypted file byte-identical.
//  3. Once only. A counter file created O_EXCL means a second invocation in
//     one run refuses, so any loop that does start terminates on its first
//     retry — a second brake on the same failure as guard 2, arriving from a
//     different direction.
func newSopsEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__sops-edit FILE",
		Short:  "Internal: the editor sops invokes; not for direct use",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		// The parent's error rendering is fine, but usage on failure would
		// put this command's spelling in front of a user who never typed it.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			err := runSopsEdit(args[0])
			// Relay the reason to the driver, which cannot read it any other
			// way: this process's stderr is sops' stderr, and the driver
			// refuses to surface that stream because a sops parse error
			// quotes the line holding the value.
			//
			// Only THESE messages are relayed, and that is what makes it safe.
			// Every one of them originates in forgectl and names a rule, never
			// an argument or a value — a property the sops package's own tests
			// assert. Without the relay, a typo'd block name (the commonest
			// mistake there is) reaches the operator as "sops refused the
			// edit", with the actionable part in a temp file.
			recordSopsError(err)
			return err
		},
	}
}

// sopsErrorFile is where the editor leaves its refusal for the driver.
const sopsErrorFile = "error"

// workdirPrefix is the basename prefix the driver's work directory carries.
const workdirPrefix = ".forgectl-sops-"

// resolveWorkdir reads and constrains the work directory.
//
// The constraints are narrow but real: absolute, and a basename the driver
// itself generates. They do not make the environment a trust boundary — a
// caller who can set it can create a matching directory — and they are not
// claimed to. What they do is stop this subcommand from being pointed at an
// arbitrary EXISTING directory, so a stray or replayed invocation fails
// loudly instead of reading four files out of somewhere unrelated.
func resolveWorkdir() (string, error) {
	raw := os.Getenv(sopsWorkdirEnv)
	if raw == "" {
		return "", errors.New("this command is invoked by forgectl, not directly")
	}
	clean := filepath.Clean(raw)
	if !filepath.IsAbs(clean) || !strings.HasPrefix(filepath.Base(clean), workdirPrefix) {
		return "", errors.New("this command is invoked by forgectl, not directly")
	}
	return clean, nil
}

// recordSopsError writes err's message into the work directory. Failures here
// are dropped deliberately: the relay is a diagnostic improvement, and losing
// it must not change the outcome the operator sees.
func recordSopsError(err error) {
	if err == nil {
		return
	}
	// Through the same constraint as the read path, not a raw Getenv: two
	// notions of "the work directory" in one file is how they drift apart.
	workdir, wdErr := resolveWorkdir()
	if wdErr != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(workdir, sopsErrorFile), []byte(err.Error()), 0o600) //nolint:gosec // G304/G703: a fixed filename under the directory resolveWorkdir already constrained; the environment is not a trust boundary here (see newSopsEditCmd guard 1)
}

// runSopsEdit performs the one edit, in the order the guards require: prove
// the caller is us, claim the single invocation, then touch the document.
func runSopsEdit(tempPath string) error {
	workdir, err := resolveWorkdir()
	if err != nil {
		return err
	}

	if err := checkSopsNonce(workdir); err != nil {
		return err
	}
	// Claimed BEFORE the edit, not after. A counter incremented on success
	// would let a failing edit be retried forever, which is the loop the
	// counter exists to stop.
	if err := claimSopsInvocation(workdir); err != nil {
		return err
	}

	path, err := sopspkg.ParsePath(os.Getenv(sopsPathEnv))
	if err != nil {
		return err
	}

	valueBytes, err := os.ReadFile(filepath.Join(workdir, sopsValueFile)) //nolint:gosec // G304/G703: a fixed filename under the directory resolveWorkdir already constrained; the environment is not a trust boundary here (see newSopsEditCmd guard 1)
	if err != nil {
		return errors.New("the value file is unreadable")
	}
	value, err := sopspkg.NormalizeValue(string(valueBytes))
	if err != nil {
		return err
	}

	doc, err := os.ReadFile(tempPath) //nolint:gosec // G304: the path sops passed us as its editor argument
	if err != nil {
		return errors.New("the document sops provided is unreadable")
	}

	edited, outcome, err := sopspkg.SetScalar(doc, path, value)
	if err != nil {
		return err
	}

	// Parse what we are about to hand back. sops answers an unparseable
	// document by re-invoking its editor, forever; refusing here converts
	// that into one clean failure with the encrypted file untouched.
	var probe map[string]any
	if err := yaml.Unmarshal(edited, &probe); err != nil {
		return errors.New("the edited document does not parse as YAML; refusing to hand it back")
	}

	// The result is recorded before the document is written, so the driver
	// can distinguish "edited and reported" from "wrote something and died".
	if err := os.WriteFile(filepath.Join(workdir, sopsResultFile), []byte(outcome.String()), 0o600); err != nil { //nolint:gosec // G304/G703: a fixed filename under the directory resolveWorkdir already constrained; the environment is not a trust boundary here (see newSopsEditCmd guard 1)
		return errors.New("could not record the outcome")
	}

	// 0600 matches what sops created the temp file as; the mode is restated
	// rather than inherited so a umask cannot widen it.
	if err := os.WriteFile(tempPath, edited, 0o600); err != nil { //nolint:gosec // G306: 0600, the mode sops itself used
		return errors.New("could not write the edited document")
	}
	return nil
}

// checkSopsNonce requires the environment's nonce to equal the one in the
// work directory.
//
// subtle.ConstantTimeCompare is not about timing — see the type comment for
// why this is not a privilege boundary — it is about not writing a comparison
// a later reader has to reason about. The refusal names neither value.
//
//nolint:gosec // G703/G304: workdir comes from this process's own environment, which is not a trust boundary; see newSopsEditCmd's guard 1.
func checkSopsNonce(workdir string) error {
	want, err := os.ReadFile(filepath.Join(workdir, sopsNonceFile)) //nolint:gosec // G304/G703: a fixed filename under the directory resolveWorkdir already constrained; the environment is not a trust boundary here (see newSopsEditCmd guard 1)
	if err != nil {
		return errors.New("this command is invoked by forgectl, not directly")
	}
	got := os.Getenv(sopsNonceEnv)
	if len(got) == 0 || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
		return errors.New("this command is invoked by forgectl, not directly")
	}
	return nil
}

// claimSopsInvocation creates the counter file exclusively, so exactly one
// invocation per run can proceed.
func claimSopsInvocation(workdir string) error {
	f, err := os.OpenFile(filepath.Join(workdir, sopsCountFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304/G703: a fixed filename under the directory resolveWorkdir already constrained; the environment is not a trust boundary here (see newSopsEditCmd guard 1)
	if err != nil {
		if os.IsExist(err) {
			return errors.New("the editor was invoked more than once in one run; refusing")
		}
		return fmt.Errorf("could not claim the invocation: %w", err)
	}
	return f.Close()
}
