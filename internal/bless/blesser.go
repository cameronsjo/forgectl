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

// KeyLabel is the compiled-in label for this machine's blessing key — the one
// Secure Enclave key `workflow trust init` mints and every `workflow bless`
// signs under. A constant (not configurable): the ceremony key's identity is
// part of the trust surface, and a settable label would let an agent point the
// signer at a key of its choosing.
const KeyLabel = "forgectl-bless"

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
	// ErrKeyNotPresenceGated: the helper's presence probe found the key can sign
	// WITHOUT user presence. A planted, non-presence key at the compiled-in label
	// is exactly how an agent gets its own key anointed as the anchor during
	// bootstrap, so this must abort trust init and blessing — never be reused.
	ErrKeyNotPresenceGated = errors.New("key is not presence-gated and may have been planted")
)

// HelperBlesser drives forgectl-bless-helper over the exec.Runner seam. The
// helper does the actual Secure Enclave work; this side only frames the argv,
// pipes the base64 digest on stdin, and parses the JSON reply.
type HelperBlesser struct {
	run  exec.Runner
	path string
}

// helperName is the helper binary's filename. It is always looked up as a
// sibling of the running executable — exactly how the cask lays the two binaries
// out — and never anywhere else.
const helperName = "forgectl-bless-helper"

// resolveSelf returns the real path of the running executable, resolving any
// symlink (Homebrew links its binaries into bin/, so os.Executable() alone would
// point at the link and the sibling lookup would miss the Cellar/libexec dir
// where the helper actually sits).
//
// It is a package var solely so same-package tests can aim the sibling lookup at
// a temp dir. Nothing outside this package — and nothing an agent can influence
// at runtime — can reassign it.
var resolveSelf = func() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(self)
	if err != nil {
		return "", err
	}
	return real, nil
}

// NewHelperBlesser locates the helper and returns a HelperBlesser bound to it.
// The helper is ALWAYS a sibling of the running executable. If it is not there,
// NewHelperBlesser returns ErrNoBlesser so callers can degrade to a "no blessing
// backend" message rather than a raw exec failure.
//
// There is deliberately NO environment override for this path, and adding one
// back would be a root-of-trust compromise, not a convenience. The helper serves
// Enroll and PublicKey as well as Sign, and `trust init` calls Enroll at the one
// moment when no trust store exists yet — so whatever public key the helper
// hands back BECOMES the anchor. An agent that can set an env var (a line
// appended to a shell rc file is well within a same-UID agent's reach) could
// therefore have the human's own sudo install the AGENT's key as the root of
// trust, with no Touch ID prompt ever appearing: a fake helper simply returns a
// key. The env var would buy a dev seam that the build already provides — the
// verification scripts build forgectl and the helper into one directory as
// siblings — at the cost of the entire threat model.
func NewHelperBlesser(run exec.Runner) (*HelperBlesser, error) {
	self, err := resolveSelf()
	if err != nil {
		return nil, fmt.Errorf("%w: locate own executable: %v", ErrNoBlesser, err)
	}
	path := filepath.Join(filepath.Dir(self), helperName)
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

// mapHelperError translates the helper's typed exit codes into sentinels (see
// exitCodeOf for how the code is read off the error chain). A non-exit error, or
// an unmapped code, passes through unchanged.
func mapHelperError(err error) error {
	if code, ok := exitCodeOf(err); ok {
		switch code {
		case 2:
			return fmt.Errorf("%w: %v", ErrCancelled, err)
		case 3:
			return fmt.Errorf("%w: %v", ErrLabelExists, err)
		case 4:
			return fmt.Errorf("%w: %v", ErrKeyNotFound, err)
		case 5:
			return fmt.Errorf("%w: %v", ErrBadDigest, err)
		case 6:
			return fmt.Errorf("%w: %v", ErrKeyNotPresenceGated, err)
		}
	}
	return err
}
