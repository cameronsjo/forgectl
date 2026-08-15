package pr

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// Breadcrumb is the on-disk record of one clean-room review session, written
// under the forgectl session-state dir (config.PrSessionsDir). It is the sole
// bridge between `forgectl pr <ref>` and the later list/attach/teardown verbs
// — and it is HOSTILE INPUT on the way back in: a malicious breadcrumb must
// not be able to steer a `git -C <workspace>` at an arbitrary path, so
// LoadBreadcrumb validates both its LOCATION and its CONTENT before any caller
// touches Workspace.
type Breadcrumb struct {
	Workspace string    `json:"workspace"`
	Ref       string    `json:"ref"` // canonical "owner/repo#N"
	Agent     string    `json:"agent"`
	CreatedAt time.Time `json:"createdAt"`
	// Local persists Ref.local, which Ref's own string form cannot carry.
	// Omitted when false so a remote session's breadcrumb is byte-identical
	// to what earlier versions wrote.
	Local bool `json:"local,omitempty"`
	// Provenance persists who WROTE the reviewed code (forgectl#232), so a
	// reconstructed session can answer the question that gates CodexExec
	// without re-entering preparation.
	//
	// IT IS NEVER READ ON ITS OWN. This is the single most attractive field in
	// the file to an attacker who can write here: flipping it to
	// "operator-authored" is a one-word edit that would otherwise buy an
	// unconfined shell over content of their choosing. provenanceFromRecord
	// validates it JOINTLY with the record's canonical local shape, so the
	// string can only mean what the rest of the breadcrumb corroborates.
	//
	// Omitted when unknown, which keeps a legacy breadcrumb and a freshly
	// written unasserted one byte-identical rather than inventing a second
	// spelling for the same state.
	Provenance string `json:"provenance,omitempty"`
}

// provenanceFromRecord resolves a breadcrumb's EFFECTIVE provenance, and it is
// the one place a persisted authorship claim is ever believed.
//
// The rule it enforces: a positive claim is only valid when the record has the
// CANONICAL LOCAL SHAPE that could have produced it. Only PrepareLocal writes
// operator-authored, and it always writes Local:true alongside — which
// validateBreadcrumbRecord in turn ties to the localOwnerSentinel owner, so the
// three facts corroborate each other. A record missing any of them did not come
// from that writer.
//
// This is validation of application state, not a cryptographic seal, and the
// distinction is honest: an actor with arbitrary write access to the 0700
// session-state dir can author a fully canonical local breadcrumb, and this
// check will accept it. What it stops is the CHEAP attack — self-labelling an
// existing remote-shaped record into eligibility by editing one field — which is
// the difference between a boundary and a speed bump.
//
// It NORMALIZES rather than rejects. A contradictory record stays loadable, so
// Claude, `pr list`, `pr attach`, and `pr teardown` keep working on it exactly
// as before; only Codex eligibility is denied. Rejecting outright would turn a
// security downgrade into a availability failure for verbs that were never at
// risk.
//
// Where the record's remote origin is KNOWN it degrades to third-party; where
// nothing is established it degrades to unknown. Both refuse Codex identically.
func provenanceFromRecord(bc Breadcrumb) ReviewProvenance {
	declared := ParseReviewProvenance(bc.Provenance)
	if declared == ReviewProvenanceOperatorAuthored && !bc.Local {
		// A remote-shaped record claiming authorship. Its origin is known, so
		// say so rather than hiding behind unknown.
		//
		// Workspace is logged unwrapped, which was CHECKED rather than assumed:
		// this branch fires when a record looks tampered with, so the pathname is
		// attacker-influenced and could carry ANSI or bidi controls. The stdlib
		// slog.TextHandler this binary installs (internal/config) quotes any value
		// containing control characters, so they reach the terminal escaped and a
		// termsafe.QuotePath here would be redundant. That redundancy becomes
		// necessary if the handler is ever swapped for one that does not quote.
		slog.Warn("Breadcrumb claims operator-authored provenance but does not have the canonical local shape; "+
			"treating it as third-party. The unconfined Codex reviewer is refused for this session.",
			"ref", bc.Ref, "workspace", bc.Workspace)
		return ReviewProvenanceThirdParty
	}
	return declared
}

// breadcrumbFilename derives a stable, filesystem-safe name from the ref and
// creation time. Owner/repo are already constrained to [A-Za-z0-9._-] by
// ParseRef, so no separator collision or path segment can appear.
func breadcrumbFilename(ref Ref, createdAt time.Time) string {
	return fmt.Sprintf("%s-%s-%d-%d.json", ref.Owner, ref.Repo, ref.Number, createdAt.UnixNano())
}

// writeBreadcrumb is the client-owned write path: it takes the same mutex
// teardown holds, so one Client cannot replace a breadcrumb while another of
// its own operations is reading and verifying it. Breadcrumb names are already
// unique by ref and creation nanosecond, so this rarely contends — having a
// single ownership point is what makes the invariant explicit rather than
// incidental.
func (c *Client) writeBreadcrumb(ref Ref, bc Breadcrumb) (string, error) {
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	return writeBreadcrumb(c.sessionsDir, ref, bc)
}

// writeBreadcrumb writes bc into sessionsDir and returns the file path. The
// directory is created if absent (0700 — session state is private).
func writeBreadcrumb(sessionsDir string, ref Ref, bc Breadcrumb) (string, error) {
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		return "", fmt.Errorf("create pr sessions dir: %w", err)
	}
	data, err := json.MarshalIndent(bc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal breadcrumb: %w", err)
	}
	path := filepath.Join(sessionsDir, breadcrumbFilename(ref, bc.CreatedAt))
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write breadcrumb %s: %w", path, err)
	}
	slog.Debug("Wrote pr session breadcrumb.", "path", path, "ref", bc.Ref)
	return path, nil
}

// LoadBreadcrumb validates and loads the breadcrumb at path. It resolves the
// canonical session-state dir (config.PrSessionsDir) itself; see loadBreadcrumb
// for the injected-dir core used by the Client and tests.
//
// This is the LIVE/ACTIONABLE loader and keeps its full historical contract: a
// breadcrumb whose workspace is not a live sandbox directory is an error here.
// Callers that only need the RECORD (list rows, stale teardown) use
// loadBreadcrumbRecord instead.
func LoadBreadcrumb(path string) (Breadcrumb, error) {
	dir, err := config.PrSessionsDir()
	if err != nil {
		return Breadcrumb{}, fmt.Errorf("resolve pr sessions dir: %w", err)
	}
	return loadBreadcrumb(path, dir)
}

// decodeBreadcrumb is the ONE decoder. Every consumer — strict live loader,
// list rows, stale teardown — decodes through here, so no second schema can
// drift away from this one.
//
// GRAMMAR: a breadcrumb file is EXACTLY ONE JSON record. DisallowUnknownFields
// refuses smuggled keys inside the record, and the trailing-token check refuses
// anything after it (forgectl#289).
//
// Both halves answer the same question — can this file mean something other
// than what a reader took from it? While trailing bytes were ignored, a file
// could carry a second record that no consumer here ever saw, and which
// document a reader ended up acting on was a property of where its parser
// happened to stop rather than of the file. Trailing content is also positive
// evidence something other than writeBreadcrumb has written to this path, which
// is worth refusing on its own.
//
// Trailing WHITESPACE stays accepted, and that is what keeps this a hardening
// rather than a migration: writeBreadcrumb terminates every file with "\n", so
// every breadcrumb forgectl has ever written still decodes. Only bytes forgectl
// never wrote are newly rejected.
func decodeBreadcrumb(data []byte) (Breadcrumb, error) {
	var bc Breadcrumb
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&bc); err != nil {
		return Breadcrumb{}, err
	}
	// Token reports the next token in the stream, skipping whitespace, and
	// returns io.EOF only when nothing but whitespace remains. Anything else —
	// a second document, a truncated one, a stray delimiter, NUL padding —
	// yields a value or a syntax error, and both mean this file is not one
	// record. The error names no file content, so hostile bytes never reach a
	// log or a terminal through it.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		// The remedy is named because no forgectl verb can reach this record:
		// `pr list` skips it and `pr teardown` refuses it at this same decode,
		// so an operator who is not told to remove it by hand has no next move.
		// The path is deliberately NOT interpolated — decodeBreadcrumbRecord's
		// wrapper already carries it, and keeping this message a CONSTANT is
		// what stops the file's own bytes riding the refusal to a terminal.
		// The clean room is named too: the first document may be a perfectly
		// good record for a LIVE sandbox — a stray writer appending bytes does
		// not make the recorded workspace fake — so removing only the
		// breadcrumb discards the last pointer to that directory.
		return Breadcrumb{}, fmt.Errorf("trailing content after the breadcrumb record; " +
			"a breadcrumb file must contain exactly one JSON record, and no forgectl verb can " +
			"read or discard this one — remove the file by hand, and check whether its clean " +
			"room under the temp dir needs removing with it")
	}
	return bc, nil
}

// loadBreadcrumbRecord is the RECORD loader: location guard, read, decode, and
// non-filesystem record validation — and nothing else. It performs NO
// workspace filesystem access and therefore grants NO action authority.
//
// It returns the exact bytes it validated alongside the decoded record, so a
// caller that must later prove the file has not changed underneath it (the
// stale-unlink protocol in teardown.go) can compare against precisely what it
// authorized rather than re-reading and re-deriving.
//
// The location-first boundary is unchanged: membership inside sessionsDir is
// settled BEFORE the file is read.
func loadBreadcrumbRecord(path, sessionsDir string) (Breadcrumb, []byte, error) {
	// (1) LOCATION — reject anything not inside the forgectl-owned dir first.
	if !sandbox.WithinWorkspace(sessionsDir, path) {
		slog.Error("Breadcrumb path escapes session-state dir; refusing.", "path", path, "sessionsDir", sessionsDir)
		return Breadcrumb{}, nil, fmt.Errorf("breadcrumb %q is not inside the forgectl session-state dir", path)
	}

	// (2) CONTENT — only now read and decode.
	data, err := os.ReadFile(path) //nolint:gosec // path was location-validated above
	if err != nil {
		return Breadcrumb{}, nil, fmt.Errorf("read breadcrumb %s: %w", path, err)
	}
	bc, err := decodeBreadcrumbRecord(data, path)
	if err != nil {
		return Breadcrumb{}, nil, err
	}
	return bc, data, nil
}

// decodeBreadcrumbRecord is step (2) of loadBreadcrumbRecord split out for a
// caller that already holds the bytes and has settled location by construction
// rather than by pathname — the stale-unlink protocol reads through a pinned
// directory handle, so there is no path left to location-check.
//
// path names the file only for error messages; it is never opened here. It is
// interpolated RAW, as every sibling error site in this package does — the
// breadcrumb filename is attacker-chosen and reaches a terminal unescaped from
// roughly a dozen sites here, in manage.go, member.go, and teardown.go, and
// escaping this one pair while the rest stay raw buys an operator nothing.
// Converging the cluster is forgectl#309.
func decodeBreadcrumbRecord(data []byte, path string) (Breadcrumb, error) {
	bc, err := decodeBreadcrumb(data)
	if err != nil {
		return Breadcrumb{}, fmt.Errorf("decode breadcrumb %s: %w", path, err)
	}
	if err := validateBreadcrumbRecord(bc); err != nil {
		return Breadcrumb{}, fmt.Errorf("invalid breadcrumb %s: %w", path, err)
	}
	return bc, nil
}

// loadBreadcrumb is the STRICT LIVE loader: the record load above, plus the
// workspace classifier. It enforces, IN ORDER and BEFORE any caller can act on
// Workspace:
//
//  1. LOCATION — path, after EvalSymlinks, must resolve to inside sessionsDir.
//     A path outside the dir, or a symlink inside it that points outside, is
//     rejected before the file is even read.
//  2. RECORD/SCHEMA — valid JSON, all required fields, a re-parseable complete
//     ref, agreeing locality, nonzero timestamp, absolute workspace.
//  3. WORKSPACE ACTIONABILITY — Workspace must classify LIVE: an existing
//     directory carrying the "forgectl-workflow-" sandbox prefix (a real
//     sandbox), so no arbitrary path can be smuggled in for a later `git -C`.
//     This is content identity, not location — it does not require Workspace
//     to sit under the current $TMPDIR, which can differ from the one in
//     effect when the sandbox was created.
//
// A LEXICALLY MISSING workspace returns a *workspaceMissingError, which
// callers detect with errors.As to offer `pr teardown` remediation. Every
// other failure returns an ordinary error — see classifyWorkspace for why the
// distinction cannot be made with a bare errors.Is(fs.ErrNotExist).
func loadBreadcrumb(path, sessionsDir string) (Breadcrumb, error) {
	bc, _, err := loadBreadcrumbRecord(path, sessionsDir)
	if err != nil {
		return Breadcrumb{}, err
	}
	switch avail, err := classifyWorkspace(bc.Workspace); avail {
	case workspaceAvailabilityLive:
		return bc, nil
	case workspaceAvailabilityMissing:
		return Breadcrumb{}, fmt.Errorf("breadcrumb %s: %w", path, err)
	default:
		return Breadcrumb{}, fmt.Errorf("invalid breadcrumb %s: %w", path, err)
	}
}

// validateBreadcrumbRecord enforces the RECORD schema — and only the record
// schema. It touches the filesystem NOWHERE: required fields present, a
// re-parseable complete ref, agreement between the two representations of
// locality, a nonzero timestamp, and an absolute workspace pathname.
//
// It deliberately does NOT require the workspace to exist, to be a directory,
// or to carry the sandbox prefix. Those are ACTIONABILITY questions answered
// by classifyWorkspace, because a record whose workspace was deleted is still
// a perfectly valid record — that is the whole premise of #212. Nor does it
// require a nonempty Agent, which legacy breadcrumbs omit.
func validateBreadcrumbRecord(bc Breadcrumb) error {
	if bc.Workspace == "" {
		return fmt.Errorf("missing workspace")
	}
	if bc.Ref == "" {
		return fmt.Errorf("missing ref")
	}
	ref, err := ParseRef(bc.Ref)
	if err != nil {
		return fmt.Errorf("malformed ref %q: %w", bc.Ref, err)
	}
	// A bare number parses (ParseRef's third form) but leaves Owner/Repo empty,
	// which would yield a Session whose Slug() is "/" and make the locality
	// cross-check below read an empty Owner. A breadcrumb always records a
	// resolved ref, so require one.
	if !ref.Complete() {
		return fmt.Errorf("ref %q is not a complete owner/repo#N reference", bc.Ref)
	}
	// CROSS-REPRESENTATION CHECK. Locality is recorded twice — as the Local
	// flag (authoritative) and as the ref's display owner — and the only
	// writer of Local:true is PrepareLocal, which always stamps
	// localOwnerSentinel. A breadcrumb that claims locality while naming a
	// real-looking owner therefore cannot have been written by this package:
	// refuse it, so forged locality cannot hide behind a plausible remote ref.
	//
	// Deliberately one-directional. The converse — owner "local" with the flag
	// unset — is the legitimate case this whole change exists to permit: a real
	// forge repo named local/… (git.sjo.lol/local/tools), and equally a
	// pre-upgrade local breadcrumb written before the flag existed.
	if bc.Local && ref.Owner != localOwnerSentinel {
		return fmt.Errorf(
			"breadcrumb claims a local session but its ref names owner %q, not %q",
			ref.Owner, localOwnerSentinel,
		)
	}
	if bc.CreatedAt.IsZero() {
		return fmt.Errorf("missing createdAt")
	}
	// The pathname shape is a RECORD property (it constrains what the string
	// can ever mean); whether that path exists is an actionability question.
	if !filepath.IsAbs(bc.Workspace) {
		return fmt.Errorf("workspace %q must be an absolute path", bc.Workspace)
	}
	return nil
}

// validateWorkspace confirms workspace is an existing directory whose base
// name carries the forgectl sandbox prefix. This is the gate that stops a
// breadcrumb from pointing a later `git -C` at, say, / or $HOME.
//
// It deliberately does NOT require workspace to sit under the current
// os.TempDir(): a workspace is created once, under whatever $TMPDIR was in
// effect at that time, and its breadcrumb can be loaded much later under a
// different $TMPDIR (a shell restart, a changed env, a different session).
// Gating on the current temp root made every `pr list`/`attach`/`teardown`
// go blind to a pre-existing session the moment $TMPDIR changed — and it was
// never an adversarial boundary to begin with: a same-uid attacker can just
// call os.MkdirTemp("", "forgectl-workflow-evil-*") and pass it. Identity
// comes from the sandbox prefix alone.
//
// Static missing paths and dangling symlinks normally fail at the preceding
// Stat. If resolution fails later because the path changed or the filesystem
// has special resolution semantics, refuse it rather than judging the literal
// base name.
//
// LOAD-BEARING — VALIDATE RESOLVED, ACT UNRESOLVED. This function checks the
// prefix on filepath.EvalSymlinks(workspace), but every caller acts on the
// UNRESOLVED string — sandbox.Teardown hands it to os.RemoveAll after its own
// resolved-prefix gate.
// That split is deliberate and is what keeps a symlink NAMED without the
// prefix but POINTING at a prefixed directory harmless: it validates here,
// and RemoveAll then unlinks the link itself rather than following it to the
// target. Do NOT "tidy" a caller to act on the resolved path on the theory
// that it should match what was validated — that turns those cases into real
// deletions of directories outside any sandbox. See the matching note on
// sandbox.Teardown.
//
// KNOWN, ACCEPTED: a symlinked PARENT component IS followed. RemoveAll only
// refuses to follow the FINAL component, so a workspace recorded as
// /tmp/plink/forgectl-workflow-x with plink -> $HOME/real deletes
// $HOME/real/forgectl-workflow-x. The retired temp-root check rejected that
// shape specifically, because sandbox.WithinWorkspace resolves symlinks on
// both sides. Reaching it still requires writing a breadcrumb into the 0700
// session-state dir under $HOME — same-uid arbitrary write — an actor who
// can delete the target outright without forgectl.
func validateWorkspace(workspace string) error {
	if !filepath.IsAbs(workspace) {
		return fmt.Errorf("workspace %q must be an absolute path", workspace)
	}
	info, err := fsStat(workspace)
	if err != nil {
		return fmt.Errorf("workspace %q does not exist: %w", workspace, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace %q is not a directory", workspace)
	}
	resolved, err := fsEvalSymlinks(workspace)
	if err != nil {
		return fmt.Errorf("workspace %q could not be resolved: %w", workspace, err)
	}
	if !strings.HasPrefix(filepath.Base(resolved), sandboxPrefix) {
		return fmt.Errorf("workspace %q lacks the %q sandbox prefix", workspace, sandboxPrefix)
	}
	return nil
}
