package bless

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// EnsureKey returns the PKIX DER of the key labelled label, minting it if it
// does not yet exist. Enroll is the happy path; an ErrLabelExists means a prior
// run already minted the key, so we fall back to fetching its public key. This
// makes enrollment idempotent — re-running `trust init` never wedges on an
// exit-3 "label exists".
func EnsureKey(ctx context.Context, b Blesser, label string) ([]byte, error) {
	der, err := b.Enroll(ctx, label)
	if err == nil {
		return der, nil
	}
	if errors.Is(err, ErrLabelExists) {
		slog.Debug("Key label already exists; reusing existing key.", "label", label)
		return b.PublicKey(ctx, label)
	}
	return nil, err
}

// SignEnvelope runs the signing ceremony for one domain-tagged blessing: it
// computes the tagged digest of data, asks the Blesser to sign it (the Touch ID
// prompt fires inside b.Sign), and assembles the Envelope. now is a parameter,
// never a hidden clock, so callers control the recorded signed_at and tests are
// deterministic.
func SignEnvelope(ctx context.Context, b Blesser, label, keyID string, d Domain, data []byte, now time.Time) (Envelope, error) {
	slog.Debug("Preparing to sign blessing.", "label", label, "keyID", keyID, "byteLength", len(data))
	digest := TaggedDigest(d, data)
	sig, err := b.Sign(ctx, label, digest)
	if err != nil {
		slog.Error("Failed to sign blessing.", "label", label, "error", err)
		return Envelope{}, err
	}
	slog.Debug("Successfully signed blessing.", "label", label, "keyID", keyID)
	return Envelope{
		Schema:    EnvelopeSchema,
		Algo:      AlgoECDSAP256SHA256,
		KeyID:     keyID,
		Signature: base64.StdEncoding.EncodeToString(sig),
		SignedAt:  now.UTC().Format(time.RFC3339),
	}, nil
}

// ErrAnchorExists reports that AnchorPath already holds an anchor. Installing
// never overwrites: replacing a live root of trust must be a deliberate,
// visible act (a `sudo rm` the human types), never a side effect of re-running
// `trust init`.
var ErrAnchorExists = errors.New("a trust anchor is already installed")

// anchorInstallScript is the privileged half of the anchor install, run under
// one interactive sudo. It is assembled at init from the AnchorPath constant
// alone — no caller input, and nothing an agent controls, ever reaches the
// script TEXT. The key arrives as the positional "$1".
//
// The shape is the whole security argument, so it is worth spelling out. The
// adversary is a same-UID local agent with ordinary file and exec access: it
// cannot become root, but it CAN write any file this user can write, at any
// moment — including while the human is typing a sudo password. So the anchor
// must never be staged anywhere the agent can reach:
//
//   - The key travels through ARGV, not a file. Argv is fixed at exec time;
//     there is no window in which its contents can be swapped. (The anchor is a
//     PUBLIC key — integrity is the only requirement, so its visibility in `ps`
//     costs nothing.)
//   - `set -C` (noclobber) turns the `>` redirect into an O_EXCL create. That
//     fails if the anchor already exists AND refuses to follow a symlink planted
//     at that path, so the write is atomic and cannot be redirected.
//   - The explicit `-e`/`-L` test in front gives that refusal a clean exit code
//     (3) instead of a generic redirect failure, so the CLI can report it
//     precisely. It tests `-L` as well because a DANGLING symlink is invisible to
//     `-e` — noclobber still refuses it, but with the generic code.
//   - A symlinked parent directory is refused outright (exit 4). Planting it
//     requires write access to /etc, i.e. root — out of scope — but the check is
//     free and closes the write-through-symlink shape completely.
var anchorInstallScript = "set -eu\n" +
	"umask 022\n" +
	"ANCHOR='" + AnchorPath + "'\n" +
	"DIR='" + filepath.Dir(AnchorPath) + "'\n" +
	`if [ -L "$DIR" ]; then exit 4; fi
if [ -e "$ANCHOR" ] || [ -L "$ANCHOR" ]; then exit 3; fi
mkdir -p "$DIR"
chown root:wheel "$DIR"
chmod 0755 "$DIR"
set -C
printf '%s\n' "$1" > "$ANCHOR"
chown root:wheel "$ANCHOR"
chmod 0644 "$ANCHOR"
`

// readAnchorFile reads the anchor back for the post-install check. It is a
// package var solely so same-package tests can point the read-back at a temp
// file; production always reads the compiled-in AnchorPath, and nothing outside
// this package (least of all an agent) can reassign it.
var readAnchorFile = func() ([]byte, error) { return os.ReadFile(AnchorPath) }

// InstallAnchor writes the anchor public key to the compiled-in AnchorPath as a
// single base64 line, root-owned and world-readable, through ONE interactive
// sudo leg (the human types the password) that carries the key in argv and
// creates the file atomically. See anchorInstallScript for why that shape is
// load-bearing. Installing never overwrites: an existing anchor is
// ErrAnchorExists.
//
// After the privileged leg returns, the anchor is read back and compared against
// the key we meant to install. The sudo leg is the only trusted step in the
// sequence; the read-back proves nothing raced us between the enrollment and the
// install, and turns any surprise into a loud refusal rather than a silently
// wrong root of trust.
func InstallAnchor(ctx context.Context, run exec.Runner, pubDER []byte) error {
	// Refuse to install anything that is not a key we would later accept — a
	// malformed anchor is unrecoverable without root.
	want, err := canonicalPubDER(pubDER)
	if err != nil {
		return fmt.Errorf("anchor key: %w", err)
	}
	b64 := base64.StdEncoding.EncodeToString(pubDER)

	slog.Debug("Preparing to install trust anchor.", "path", AnchorPath)
	if err := run.RunInteractive(ctx, "sudo", "sh", "-c", anchorInstallScript, "_", b64); err != nil {
		switch code, ok := exitCodeOf(err); {
		case ok && code == 3:
			return fmt.Errorf("%w at %s: remove it with 'sudo rm %s' before re-initializing", ErrAnchorExists, AnchorPath, AnchorPath)
		case ok && code == 4:
			return fmt.Errorf("install anchor: %s is a symlink; refusing to write the root of trust through it", filepath.Dir(AnchorPath))
		}
		return fmt.Errorf("install anchor %s: %w", AnchorPath, err)
	}

	got, err := readAnchorFile()
	if err != nil {
		return fmt.Errorf("verify installed anchor: read %s: %w", AnchorPath, err)
	}
	gotPub, err := ParseAnchorFile(got)
	if err != nil {
		return fmt.Errorf("verify installed anchor %s: %w", AnchorPath, err)
	}
	gotDER, err := EncodePublicKey(gotPub)
	if err != nil {
		return fmt.Errorf("verify installed anchor %s: %w", AnchorPath, err)
	}
	if !bytes.Equal(gotDER, want) {
		return fmt.Errorf(
			"the trust anchor now at %s is NOT the key that was just enrolled (installed %s, expected %s) — "+
				"something replaced it mid-install; remove it with 'sudo rm %s' and re-run trust init",
			AnchorPath, Fingerprint(gotDER), Fingerprint(want), AnchorPath)
	}

	slog.Debug("Successfully installed trust anchor.", "path", AnchorPath, "keyID", Fingerprint(want))
	return nil
}

// canonicalPubDER parses a PKIX DER public key and re-encodes it, yielding the
// canonical byte form. Comparing canonical forms means the post-install check
// answers "is this the same KEY" rather than "are these the same bytes".
func canonicalPubDER(der []byte) ([]byte, error) {
	pub, err := ParsePublicKey(der)
	if err != nil {
		return nil, err
	}
	return EncodePublicKey(pub)
}

// exitCodeOf walks the error chain for anything exposing ExitCode() int — the
// shape exec.CommandError unwraps to (os/exec's *ExitError), and the shape tests
// inject. It is how both the helper's and the install script's typed exit codes
// are read back.
func exitCodeOf(err error) (int, bool) {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode(), true
	}
	return 0, false
}

// StepCheck is the bless-time view of one workflow step, decoupled from
// internal/workflow so this package never imports the parser it protects. The
// CLI layer (PR 2) maps parsed steps into StepChecks. Uses selects the runner;
// Exports names the variables the step will contribute; Cmd/Args are the
// interpolation surface that CheckRunStepParamRefs guards for `run` steps.
type StepCheck struct {
	Uses    string
	Exports []string
	Cmd     string
	Args    []string
}

// CheckRunStepParamRefs is the bless-time param-injection refusal. A blessing
// covers file bytes, but `run` steps interpolate ${...} at run time — so a
// ${param} in a blessed run step would be an agent-controllable injection into
// approved bytes. The rule: in a `run` step, every ${name} in cmd or args must
// resolve to an export declared by an EARLIER step; anything else (a CLI param,
// or an export from a later step) is refused. Non-run steps are unrestricted.
//
// params is the workflow's declared param names. A param whose name collides
// with ANY step's export is refused outright: params and exports share one
// variable namespace at execution time, so a same-named param could ride an
// export name this guard trusts as step-produced (the executor also refuses
// the collision at plan time — this is the bless-time belt to that buckle, so
// the collision can never even be blessed into a file).
//
// The ${...} extraction mirrors internal/step.Context.Interpolate byte-for-byte:
// find "${", take the FIRST "}" after it, name is everything between (no charset
// restriction); an unterminated "${" is a fail-closed error. A drift test tying
// this to the interpolator lands in PR 2.
func CheckRunStepParamRefs(steps []StepCheck, params []string) error {
	exported := make(map[string]bool)
	for _, s := range steps {
		for _, exp := range s.Exports {
			exported[exp] = true
		}
	}
	for _, p := range params {
		if exported[p] {
			return fmt.Errorf("param %q collides with a step export of the same name: params and step exports share one namespace", p)
		}
	}

	allowed := make(map[string]bool)
	for i, s := range steps {
		if s.Uses == "run" {
			refs, err := extractRefs(s.Cmd)
			if err != nil {
				return fmt.Errorf("step %d cmd: %w", i, err)
			}
			for _, ref := range refs {
				if !allowed[ref] {
					return fmt.Errorf("step %d cmd references ${%s}: params are forbidden in blessed run steps; only exports from earlier steps are allowed", i, ref)
				}
			}
			for j, arg := range s.Args {
				refs, err := extractRefs(arg)
				if err != nil {
					return fmt.Errorf("step %d args[%d]: %w", i, j, err)
				}
				for _, ref := range refs {
					if !allowed[ref] {
						return fmt.Errorf("step %d args[%d] references ${%s}: params are forbidden in blessed run steps; only exports from earlier steps are allowed", i, j, ref)
					}
				}
			}
		}
		// Add this step's exports only AFTER checking it, so a step can never
		// reference its own (or a later step's) exports.
		for _, exp := range s.Exports {
			allowed[exp] = true
		}
	}
	return nil
}

// extractRefs returns the ${name} references in s, mirroring
// internal/step.Context.Interpolate's scan exactly (index of "${", first "}"
// after it, name between, no charset restriction). An unterminated "${" is an
// error — fail closed rather than let a malformed ref slip past the guard.
func extractRefs(s string) ([]string, error) {
	if !strings.Contains(s, "${") {
		return nil, nil
	}
	var refs []string
	i := 0
	for i < len(s) {
		start := strings.Index(s[i:], "${")
		if start == -1 {
			break
		}
		start += i
		end := strings.Index(s[start:], "}")
		if end == -1 {
			return nil, fmt.Errorf("unterminated ${...} in %q", s)
		}
		end += start
		refs = append(refs, s[start+2:end])
		i = end + 1
	}
	return refs, nil
}
