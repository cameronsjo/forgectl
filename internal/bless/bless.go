package bless

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
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

// InstallAnchor writes the anchor public key to the compiled-in AnchorPath as a
// single base64 line, root-owned and non-writable, via two interactive sudo
// legs (the human types the password): first `install -d` to create the parent
// directory, then `install` to place the file. It never removes or overwrites —
// the refuse-if-exists guard lives in the CLI layer, so InstallAnchor is a pure
// write and the temp file is always cleaned up.
func InstallAnchor(ctx context.Context, run exec.Runner, pubDER []byte) error {
	line := base64.StdEncoding.EncodeToString(pubDER) + "\n"

	tmp, err := os.CreateTemp("", "forgectl-anchor-*.pub")
	if err != nil {
		return fmt.Errorf("create temp anchor file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.WriteString(line); err != nil {
		tmp.Close() //nolint:errcheck
		return fmt.Errorf("write temp anchor file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp anchor file: %w", err)
	}

	anchorDir := filepath.Dir(AnchorPath)
	slog.Debug("Preparing to install trust anchor.", "path", AnchorPath)
	if err := run.RunInteractive(ctx, "sudo", "install", "-d", "-o", "root", "-g", "wheel", "-m", "0755", anchorDir); err != nil {
		return fmt.Errorf("create anchor directory %s: %w", anchorDir, err)
	}
	if err := run.RunInteractive(ctx, "sudo", "install", "-o", "root", "-g", "wheel", "-m", "0644", tmpPath, AnchorPath); err != nil {
		return fmt.Errorf("install anchor %s: %w", AnchorPath, err)
	}
	slog.Debug("Successfully installed trust anchor.", "path", AnchorPath)
	return nil
}

// StepCheck is the bless-time view of one workflow step, decoupled from
// internal/workflow so this package never imports the parser it protects. The
// CLI layer maps parsed steps into StepChecks through the same merged step
// registry the executor runs.
//
// Exports names the variables the step will contribute. Guarded holds the step
// verb's param-hostile fields (step.Def.GuardedFields) keyed by field name, with
// each field's string value(s) — a scalar field contributes one element, a slice
// field (Args, Globs) contributes all of them. It is the registry, not this
// package, that decides which fields are guarded: the guard's model of danger
// must come from the same place that maps a verb to its runner, or a renamed or
// newly-added exec verb would silently escape it.
//
// Uses is carried for error messages only — no rule keys on it.
type StepCheck struct {
	Uses    string
	Exports []string
	Guarded map[string][]string
}

// CheckGuardedParamRefs is the bless-time param-injection refusal. A blessing
// covers file BYTES, but ${...} references interpolate at RUN time — so a
// ${param} in a step's guarded field (see step.Def.GuardedFields: what runs,
// what a launched agent does, what a security control covers) would be an
// agent-controllable injection into human-approved bytes. The rule: in a guarded
// field, every ${name} must resolve to an export declared by an EARLIER step;
// anything else (a CLI param, or an export from a later step) is refused.
// Non-guarded fields — a worktree step's repo/ref — are unrestricted, because
// naming the data a blessed pattern operates on is the intended parameterization.
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
// restriction); an unterminated "${" is a fail-closed error.
// TestBlessRefExtractionMatchesInterpolate pins the two scanners together.
func CheckGuardedParamRefs(steps []StepCheck, params []string) error {
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
		// Field names sorted so a step with several offending fields always
		// reports the same one.
		fields := make([]string, 0, len(s.Guarded))
		for f := range s.Guarded {
			fields = append(fields, f)
		}
		sort.Strings(fields)

		for _, field := range fields {
			for _, value := range s.Guarded[field] {
				refs, err := extractRefs(value)
				if err != nil {
					return fmt.Errorf("step %d (%s) field %s: %w", i, s.Uses, field, err)
				}
				for _, ref := range refs {
					if !allowed[ref] {
						return fmt.Errorf("step %d (%s) field %s references ${%s}: params are forbidden in a blessed step's guarded fields; only exports from earlier steps are allowed", i, s.Uses, field, ref)
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
