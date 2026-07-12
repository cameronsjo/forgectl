package bless

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Blesser is the user-presence signing seam. Enroll mints a new presence-gated
// key, PublicKey fetches an existing one, and Sign produces a signature over a
// pre-computed tagged digest — the blesser never sees the file, only the digest,
// so domain separation is enforced entirely on the Go side. Pubkeys and
// signatures cross the seam as DER bytes.
//
// On macOS the concrete implementation shells out to forgectl-bless-helper,
// where Sign triggers a Touch ID prompt. A non-Touch-ID environment (headless,
// Linux) has no Blesser — verification still works, blessing does not, which is
// the intended asymmetry.
type Blesser interface {
	Enroll(ctx context.Context, label string) ([]byte, error)
	PublicKey(ctx context.Context, label string) ([]byte, error)
	Sign(ctx context.Context, label string, digest [32]byte) ([]byte, error)
}

// Typed sentinels for the ceremony. ErrCancelled distinguishes a human
// declining Touch ID (a normal outcome) from a real failure; the rest map the
// helper's typed exit codes so callers can react (EnsureKey falls back to
// PublicKey on ErrLabelExists, for instance).
var (
	// ErrCancelled: the human declined the Touch ID / presence prompt.
	ErrCancelled = errors.New("blessing ceremony cancelled by the user")
	// ErrLabelExists: enroll was asked to mint a key whose label is taken.
	ErrLabelExists = errors.New("a key with this label already exists")
	// ErrKeyNotFound: no key exists for the requested label.
	ErrKeyNotFound = errors.New("no key found for this label")
	// ErrBadDigest: the helper rejected the digest (not 32 bytes, unparseable).
	ErrBadDigest = errors.New("helper rejected the digest")
	// ErrNoBlesser: no helper binary is available on this machine.
	ErrNoBlesser = errors.New("blessing helper not found")
)

// HelperBlesser drives forgectl-bless-helper over the exec.Runner seam. The
// helper does the actual Secure Enclave work; this side only frames the argv,
// pipes the base64 digest on stdin, and parses the JSON reply.
type HelperBlesser struct {
	run  exec.Runner
	path string
}

// NewHelperBlesser locates the helper and returns a HelperBlesser bound to it.
// Discovery: FORGECTL_BLESS_HELPER wins (the dev seam — point it at a locally
// built helper); otherwise the helper is expected next to the running binary
// (filepath.Dir(os.Executable())). If the resolved path does not exist on disk,
// it returns ErrNoBlesser so callers can degrade to a "no blessing backend"
// message rather than a raw exec failure.
//
// FORGECTL_BLESS_HELPER being agent-settable is deliberate and safe: it
// redirects only the SIGN side. A rogue helper can mint signatures, but only
// under keys absent from the anchor-signed trust store — the verify path
// rejects them with ErrUnknownKey — and it can never satisfy userPresence for
// a genuinely enrolled key. The verify path reads no environment at all.
func NewHelperBlesser(run exec.Runner) (*HelperBlesser, error) {
	path := os.Getenv("FORGECTL_BLESS_HELPER")
	if path == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("%w: locate own executable: %v", ErrNoBlesser, err)
		}
		path = filepath.Join(filepath.Dir(self), "forgectl-bless-helper")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrNoBlesser, path, err)
	}
	return &HelperBlesser{run: run, path: path}, nil
}

// pubkeyReply and signatureReply are the frozen JSON shapes the helper emits.
// Decoded strictly (DisallowUnknownFields) so a drifted helper reply is a loud
// error, not a silent misread.
type (
	pubkeyReply struct {
		Pubkey string `json:"pubkey"`
	}
	signatureReply struct {
		Signature string `json:"signature"`
	}
)

// Enroll mints a new presence-gated key labelled label and returns its PKIX DER.
func (h *HelperBlesser) Enroll(ctx context.Context, label string) ([]byte, error) {
	out, err := h.run.Run(ctx, h.path, "enroll", "--label", label)
	if err != nil {
		return nil, mapHelperError(err)
	}
	return decodePubkeyReply(out)
}

// PublicKey returns the PKIX DER of the existing key labelled label.
func (h *HelperBlesser) PublicKey(ctx context.Context, label string) ([]byte, error) {
	out, err := h.run.Run(ctx, h.path, "pubkey", "--label", label)
	if err != nil {
		return nil, mapHelperError(err)
	}
	return decodePubkeyReply(out)
}

// Sign pipes the base64-std of the 32 digest bytes (plus a trailing newline, per
// the frozen contract) to the helper and returns the ASN.1/DER ECDSA signature.
// This is the call that triggers the Touch ID prompt.
func (h *HelperBlesser) Sign(ctx context.Context, label string, digest [32]byte) ([]byte, error) {
	stdin := base64.StdEncoding.EncodeToString(digest[:]) + "\n"
	out, err := h.run.RunWithInput(ctx, stdin, h.path, "sign", "--label", label)
	if err != nil {
		return nil, mapHelperError(err)
	}
	var reply signatureReply
	if err := decodeStrictJSON(out, &reply); err != nil {
		return nil, fmt.Errorf("decode helper sign reply: %w", err)
	}
	if reply.Signature == "" {
		return nil, fmt.Errorf("helper sign reply has empty signature")
	}
	der, err := base64.StdEncoding.DecodeString(reply.Signature)
	if err != nil {
		return nil, fmt.Errorf("helper signature is not valid base64: %w", err)
	}
	return der, nil
}

// decodePubkeyReply parses a {"pubkey":...} reply and returns the decoded DER.
func decodePubkeyReply(out string) ([]byte, error) {
	var reply pubkeyReply
	if err := decodeStrictJSON(out, &reply); err != nil {
		return nil, fmt.Errorf("decode helper pubkey reply: %w", err)
	}
	if reply.Pubkey == "" {
		return nil, fmt.Errorf("helper pubkey reply has empty pubkey")
	}
	der, err := base64.StdEncoding.DecodeString(reply.Pubkey)
	if err != nil {
		return nil, fmt.Errorf("helper pubkey is not valid base64: %w", err)
	}
	return der, nil
}

// decodeStrictJSON decodes one JSON object into v, rejecting unknown fields so a
// helper reply that drifted from the frozen contract fails loudly.
func decodeStrictJSON(s string, v any) error {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// mapHelperError translates the helper's typed exit codes into sentinels by
// walking the error chain for anything exposing ExitCode() int. exec.CommandError
// unwraps to *exec.ExitError (os/exec), whose promoted ExitCode satisfies this;
// tests inject any type with an ExitCode method. A non-exit error (or an
// unmapped code) passes through unchanged.
func mapHelperError(err error) error {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		switch coded.ExitCode() {
		case 2:
			return fmt.Errorf("%w: %v", ErrCancelled, err)
		case 3:
			return fmt.Errorf("%w: %v", ErrLabelExists, err)
		case 4:
			return fmt.Errorf("%w: %v", ErrKeyNotFound, err)
		case 5:
			return fmt.Errorf("%w: %v", ErrBadDigest, err)
		}
	}
	return err
}
