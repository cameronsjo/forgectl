package cli

import (
	"crypto/ecdsa"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/bless"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/workflow"
)

// Compile-time proof that the real user-presence verifier satisfies the engine
// interface. It lives in the CLI layer, never in internal/bless — bless must not
// import internal/workflow (the verify path authenticates bytes before parsing,
// so it cannot depend on the parser it protects).
var _ workflow.Verifier = (*bless.Verifier)(nil)

// trustStorer is the trust-chain read seam the bless and trust-list verbs gate
// on: it returns the authenticated enrolled-key set (Verify's steps 1–3). It is
// distinct from verifierFactory because those verbs need the Store, not a
// blessing check. Anchor exposes the anchor-only leg (steps 1–2) for the
// `trust rebuild` verb, which must reach the authenticated anchor precisely when
// TrustedStore would fail on the missing store it is rebuilding.
type trustStorer interface {
	TrustedStore() (bless.Store, error)
	Anchor() (*ecdsa.PublicKey, string, error)
}

// trustStorerFactory builds the trust-chain reader. Like verifierFactory it is a
// package-level var purely as a TEST SEAM — never user-configurable (the anchor
// it roots on is compiled in). Production returns the real *bless.Verifier.
var trustStorerFactory = func() trustStorer { return bless.NewVerifier() }

// anchorStatPath is the path `trust init` checks for an existing anchor before
// bootstrapping. It defaults to the compiled-in bless.AnchorPath; it is a
// package var ONLY as a TEST SEAM (InstallAnchor always writes the real
// compiled-in path via sudo) and is never user-configurable.
var anchorStatPath = bless.AnchorPath

// blesserFactory builds the ceremony Blesser. It is an in-process package var —
// a TEST SEAM only — and deliberately NOT an environment variable: bless once
// honored FORGECTL_BLESS_HELPER, and because that override also served Enroll
// and PublicKey, an agent that wrote an export into a shell profile could have
// the human's own `trust init` enroll the AGENT's key as the root-owned anchor
// (no Touch ID prompt would even appear). A Go package var cannot be reached
// from outside the process — only test code linked into the test binary can
// swap it — so testability costs nothing here. Production always resolves the
// helper as a sibling of the running executable.
var blesserFactory = func(run exec.Runner) (bless.Blesser, error) {
	return bless.NewHelperBlesser(run)
}

// installAnchor is the privileged anchor write, injected as a package var so the
// trust-init FLOW (key → store → anchor last; resume after a cancelled sudo) can
// be tested without a root-owned /etc file. The argv construction and the
// post-install read-back it performs are covered by internal/bless's own tests.
// Test seam only — production always calls the real bless.InstallAnchor.
var installAnchor = bless.InstallAnchor

// newWorkflowBlessCmd builds `forgectl workflow bless <name>`: the human-presence
// ceremony that signs a user workflow file's exact bytes so `workflow run` will
// execute it. Built-ins are refused (they are part of the binary), and a file
// that references ${param} in any step's guarded field — the fields that drive
// execution or enforce a control — is refused before signing.
func newWorkflowBlessCmd(deps module.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "bless <name>",
		Short: "Approve a workflow file's exact bytes with a user-presence signature",
		Long: `bless signs a user workflow file's exact bytes with a Touch ID (or
account-password) presence ceremony, writing a *.blessing sidecar next to it.
` + "`workflow run`" + ` refuses any user workflow that is not blessed. Re-blessing
after any edit is the point — one changed byte invalidates the signature.

Built-in workflows are compiled into the binary and never need blessing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflowBless(cmd, deps, args[0])
		},
	}
}

// runWorkflowBless performs the eight-stage bless ceremony (ADR-0006 §bless).
func runWorkflowBless(cmd *cobra.Command, deps module.Deps, name string) error {
	out := cmd.OutOrStdout()
	ctx := cmd.Context()

	// 1. Load the bytes; a built-in is never blessed.
	src, err := workflow.Load(name)
	if err != nil {
		return err
	}
	if src.Builtin {
		return fmt.Errorf("workflow %q: built-in workflows are part of the binary and never need blessing", name)
	}

	// 2. Parse — never bless bytes that don't parse.
	wf, err := workflow.Parse(src.Data)
	if err != nil {
		return err
	}

	// 3. Map steps to StepChecks through the SAME merged registry the run path
	//    uses, so a verb this binary can't execute is refused rather than blessed
	//    — and so the guard's model of which fields are param-hostile comes from
	//    the same entry that maps the verb to its runner (a renamed or new
	//    arbitrary-exec verb cannot escape the guard by not being called "run").
	registry, err := workflow.NewRegistry(stepContributions(deps)...)
	if err != nil {
		return err
	}
	stepChecks := make([]bless.StepCheck, 0, len(wf.Steps))
	for i, s := range wf.Steps {
		def, ok := registry[s.Uses]
		if !ok {
			return fmt.Errorf("workflow %q: step %d uses unknown verb %q — this binary cannot execute it, so it will not be blessed", name, i, s.Uses)
		}
		guarded, err := workflow.GuardedValues(s, def.GuardedFields)
		if err != nil {
			// A registry Def naming a field that doesn't exist is a programming
			// error that would silently un-guard that field. Refuse loudly.
			return fmt.Errorf("workflow %q: step %d: %w", name, i, err)
		}
		stepChecks = append(stepChecks, bless.StepCheck{
			Uses:    s.Uses,
			Exports: def.Exports,
			Guarded: guarded,
		})
	}

	// 4. Refuse ${param} references in guarded fields (agent-controllable
	//    injection into approved bytes). Param names sorted for a deterministic
	//    error.
	paramNames := make([]string, 0, len(wf.Params))
	for p := range wf.Params {
		paramNames = append(paramNames, p)
	}
	sort.Strings(paramNames)
	if err := bless.CheckGuardedParamRefs(stepChecks, paramNames); err != nil {
		return fmt.Errorf("workflow %q: %w", name, err)
	}

	// 5. The trust chain must already be valid before we sign anything.
	store, err := trustStorerFactory().TrustedStore()
	if err != nil {
		return trustChainError(err)
	}

	// 6. This machine must hold a blessing key AND be enrolled in the store.
	blesser, err := blesserFactory(deps.Runner)
	if err != nil {
		return err
	}
	pubDER, err := blesser.PublicKey(ctx, bless.KeyLabel)
	if err != nil {
		if errors.Is(err, bless.ErrKeyNotFound) {
			return fmt.Errorf("this machine has no blessing key yet — run 'forgectl workflow trust init' first: %w", err)
		}
		return err
	}
	keyID := bless.Fingerprint(pubDER)
	if _, ok := store.Lookup(keyID); !ok {
		return fmt.Errorf("this machine's key %s is not enrolled in the trust store — run 'forgectl workflow trust init' first", keyID)
	}

	// 7. Sign the exact bytes — this fires the presence prompt.
	env, err := bless.SignEnvelope(ctx, blesser, bless.KeyLabel, keyID, bless.DomainWorkflow, src.Data, time.Now())
	if err != nil {
		return err
	}

	// 8. Write the sidecar next to the workflow file.
	encoded, err := bless.EncodeEnvelope(env)
	if err != nil {
		return err
	}
	sidecar := bless.SidecarPath(src.Path)
	if err := os.WriteFile(sidecar, encoded, 0o644); err != nil {
		return fmt.Errorf("write blessing sidecar %s: %w", sidecar, err)
	}
	fmt.Fprintf(out, "Blessed %q — wrote %s\n", name, sidecar)
	return nil
}

// newWorkflowVerifyCmd builds `forgectl workflow verify <name>`: a read-only
// preflight (CI, a pre-run check) that reports whether a workflow is blessed and
// its blessing valid, exiting non-zero on any failure.
func newWorkflowVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify <name>",
		Short: "Check that a workflow is blessed and its blessing is valid",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			out := cmd.OutOrStdout()

			src, err := workflow.Load(name)
			if err != nil {
				return err
			}
			if src.Builtin {
				fmt.Fprintf(out, "%s: built-in — exempt from blessing\n", name)
				return nil
			}
			if err := verifierFactory().Verify(src.Path, src.Data); err != nil {
				// Returning the error prints the typed reason and exits non-zero.
				return fmt.Errorf("workflow %q: %w", name, err)
			}
			fmt.Fprintf(out, "%s: blessed and valid\n", name)
			return nil
		},
	}
}

// newWorkflowTrustCmd builds the `forgectl workflow trust` parent: establishing
// and inspecting the blessing trust store. Peer enrollment (`trust add`) is a
// deferred follow-on.
func newWorkflowTrustCmd(deps module.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "Manage the workflow-blessing trust store",
	}
	cmd.AddCommand(
		newWorkflowTrustInitCmd(deps),
		newWorkflowTrustRebuildCmd(deps),
		newWorkflowTrustListCmd(),
	)
	return cmd
}

// newWorkflowTrustInitCmd builds `forgectl workflow trust init`: mint this
// machine's blessing key, sign a fresh trust store enrolling it, and install the
// root-owned anchor. Idempotent up to the anchor (which is refused if present).
func newWorkflowTrustInitCmd(deps module.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Establish this machine as the trust anchor and first blesser",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTrustInit(cmd, deps)
		},
	}
}

// runTrustInit performs the five-stage bootstrap (ADR-0006 §trust init). The
// anchor write is LAST so a cancelled sudo leaves the reusable key + store on
// disk and a re-run completes only the anchor leg.
func runTrustInit(cmd *cobra.Command, deps module.Deps) error {
	out := cmd.OutOrStdout()
	ctx := cmd.Context()

	// 1. Refuse if the anchor already exists — rotation is a manual
	//    re-establishment, never a silent overwrite.
	if _, err := os.Stat(anchorStatPath); err == nil {
		return fmt.Errorf("trust anchor already exists at %s; rotation is a manual re-establishment (see ADR-0006)", anchorStatPath)
	}

	// 2. Ensure this machine's blessing key (idempotent — reuses an existing
	//    key so a re-run after a cancelled sudo never wedges).
	blesser, err := blesserFactory(deps.Runner)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "Preparing this machine's blessing key…")
	pubDER, err := bless.EnsureKey(ctx, blesser, bless.KeyLabel)
	if err != nil {
		return fmt.Errorf("ensure blessing key: %w", err)
	}
	keyID := bless.Fingerprint(pubDER)
	fmt.Fprintf(out, "Blessing key ready: %s\n", keyID)

	// 3. Build the trust store: this key is both the anchor and the first
	//    enrolled blesser (the single-machine root of trust).
	store := bless.Store{
		Schema:      bless.StoreSchema,
		AnchorKeyID: keyID,
		Keys: []bless.TrustedKey{{
			KeyID:   keyID,
			Machine: shortHostname(),
			Pubkey:  base64.StdEncoding.EncodeToString(pubDER),
			AddedAt: time.Now().UTC().Format(time.RFC3339),
		}},
	}
	storeBytes, err := bless.EncodeStore(store)
	if err != nil {
		return err
	}

	// 4. Sign the store (presence prompt) and write it plus its sidecar.
	fmt.Fprintln(out, "Signing the trust store — approve the presence prompt…")
	storeEnv, err := bless.SignEnvelope(ctx, blesser, bless.KeyLabel, keyID, bless.DomainTrust, storeBytes, time.Now())
	if err != nil {
		return err
	}
	storeEnvBytes, err := bless.EncodeEnvelope(storeEnv)
	if err != nil {
		return err
	}
	storePath, err := config.TrustStorePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(storePath), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.WriteFile(storePath, storeBytes, 0o644); err != nil {
		return fmt.Errorf("write trust store %s: %w", storePath, err)
	}
	if err := os.WriteFile(bless.SidecarPath(storePath), storeEnvBytes, 0o644); err != nil {
		return fmt.Errorf("write trust store sidecar: %w", err)
	}
	fmt.Fprintf(out, "Trust store written: %s\n", storePath)

	// 5. Install the root-owned anchor LAST (interactive sudo).
	fmt.Fprintln(out, "Installing the root-owned trust anchor — sudo may prompt…")
	if err := installAnchor(ctx, deps.Runner, pubDER); err != nil {
		return fmt.Errorf("install trust anchor: %w", err)
	}
	fmt.Fprintf(out, "Trust anchor installed: %s\nThis machine is now the trust root and first blesser.\n", bless.AnchorPath)
	return nil
}

// newWorkflowTrustRebuildCmd builds `forgectl workflow trust rebuild`:
// reconstruct a deleted or corrupted trust STORE from the still-installed
// root-owned anchor, WITHOUT the two-sudo anchor rotation `trust init` forces.
// The anchor key never changes on a store loss, so re-establishing it is
// unnecessary ceremony; this verb rebuilds the store alone. It is a DISTINCT verb
// from init on purpose — init refuses when an anchor is present, whereas rebuild
// REQUIRES one.
func newWorkflowTrustRebuildCmd(deps module.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "rebuild",
		Short: "Rebuild the trust store from the installed anchor, enrolling only this machine",
		Long: `rebuild reconstructs a deleted or corrupted trust store from the
still-installed root-owned anchor, without touching the anchor (no sudo). Use it
to recover when trust.toml is lost but the anchor at ` + bless.AnchorPath + ` is
intact; use 'trust init' instead when the anchor itself is gone.

The rebuilt store enrolls ONLY this machine's presence-gated key, reconstructed
solely from the Secure Enclave — never read back out of the (now unauthenticated)
old store. Only the machine that holds the anchor key may rebuild.

WARNING: rebuild OVERWRITES the trust store. Any previously enrolled peer machine
is dropped and must be re-added afterward.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTrustRebuild(cmd, deps)
		},
	}
}

// runTrustRebuild reconstructs the trust store from the authenticated anchor
// without rotating it. The load-bearing security boundary: the enrolled set is
// reconstructed SOLELY from this machine's presence-gated key — never read back
// out of the existing store, which is unauthenticated the moment its sidecar is
// invalid. Reading Keys from it would let a same-UID agent pre-seed its own key
// into the store for the human to re-sign under one presence prompt.
func runTrustRebuild(cmd *cobra.Command, deps module.Deps) error {
	out := cmd.OutOrStdout()
	ctx := cmd.Context()

	// 1. The anchor is the ONLY authority a rebuild trusts. Read + ownership-check
	//    it directly via Anchor() — NOT TrustedStore(), which would fail on the
	//    very store we are rebuilding. A missing or non-root anchor means there is
	//    nothing to rebuild against: route the user to `trust init`.
	reader := trustStorerFactory()
	_, anchorFP, err := reader.Anchor()
	if err != nil {
		return trustChainError(err)
	}

	// 2. Fetch THIS machine's blessing key with PublicKey — NEVER EnsureKey. A
	//    rebuild must not MINT a key: no key means this machine never held the
	//    anchor, so it cannot be the rebuilder. A key that can sign without
	//    presence may have been planted, and must abort rather than be anointed.
	blesser, err := blesserFactory(deps.Runner)
	if err != nil {
		return err
	}
	pubDER, err := blesser.PublicKey(ctx, bless.KeyLabel)
	if err != nil {
		switch {
		case errors.Is(err, bless.ErrKeyNotFound):
			return fmt.Errorf("this machine has no blessing key — only the machine that holds the trust anchor can rebuild the store; run 'forgectl workflow trust init' on the anchor machine: %w", err)
		case errors.Is(err, bless.ErrKeyNotPresenceGated):
			return fmt.Errorf("this machine's blessing key is not presence-gated and may have been planted — refusing to rebuild the trust store around it: %w", err)
		default:
			return err
		}
	}
	keyID := bless.Fingerprint(pubDER)

	// 3. Only the anchor-holding machine may rebuild: this key must BE the anchor.
	//    A mismatch means a peer (or an impostor) is invoking rebuild — refuse.
	if keyID != anchorFP {
		return fmt.Errorf("this machine's key %s is not the trust anchor %s — only the anchor-holding machine may rebuild the store", keyID, anchorFP)
	}

	// 4. Forward-coupling guard (issue #86). Today `trust add` is unimplemented, so
	//    a valid store enrolls exactly this machine's key (== the anchor). Once peer
	//    enrollment ships, a rebuild would SILENTLY DROP any enrolled peer — so
	//    refuse the moment a currently-valid store holds a key that is not this
	//    machine's. A missing or invalid store is the normal recovery case and is
	//    tolerated (that is what we are here to rebuild).
	if store, serr := reader.TrustedStore(); serr == nil {
		for _, k := range store.Keys {
			if k.KeyID != keyID {
				return fmt.Errorf("the current trust store also enrolls %s (%s); rebuilding would silently drop it — remove that peer deliberately before rebuilding, or re-establish trust (issue #86)", k.KeyID, k.Machine)
			}
		}
	}

	fmt.Fprintln(out, "WARNING: rebuild OVERWRITES the trust store, enrolling ONLY this machine.")
	fmt.Fprintln(out, "Any previously enrolled peer machine will be dropped and must be re-added.")

	// 5. Reconstruct the store enrolling ONLY this machine's key — byte-identical
	//    to `trust init` stage 3. The enrolled set comes SOLELY from the Secure
	//    Enclave key fetched above; the old store's Keys are never read.
	store := bless.Store{
		Schema:      bless.StoreSchema,
		AnchorKeyID: anchorFP,
		Keys: []bless.TrustedKey{{
			KeyID:   keyID,
			Machine: shortHostname(),
			Pubkey:  base64.StdEncoding.EncodeToString(pubDER),
			AddedAt: time.Now().UTC().Format(time.RFC3339),
		}},
	}
	storeBytes, err := bless.EncodeStore(store)
	if err != nil {
		return err
	}

	// 6. Sign the store (ONE presence prompt) and write it plus its sidecar. No
	//    InstallAnchor, no sudo — the anchor is untouched.
	fmt.Fprintln(out, "Signing the rebuilt trust store — approve the presence prompt…")
	storeEnv, err := bless.SignEnvelope(ctx, blesser, bless.KeyLabel, anchorFP, bless.DomainTrust, storeBytes, time.Now())
	if err != nil {
		return err
	}
	storeEnvBytes, err := bless.EncodeEnvelope(storeEnv)
	if err != nil {
		return err
	}
	storePath, err := config.TrustStorePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(storePath), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.WriteFile(storePath, storeBytes, 0o644); err != nil {
		return fmt.Errorf("write trust store %s: %w", storePath, err)
	}
	if err := os.WriteFile(bless.SidecarPath(storePath), storeEnvBytes, 0o644); err != nil {
		return fmt.Errorf("write trust store sidecar: %w", err)
	}
	fmt.Fprintf(out, "Trust store rebuilt: %s\nEnrolled only this machine (%s). Any previously enrolled peer must be re-added.\n", storePath, keyID)
	return nil
}

// newWorkflowTrustListCmd builds `forgectl workflow trust list`: print the
// anchor key id and every enrolled machine key.
func newWorkflowTrustListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the trust anchor and enrolled machine keys",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			store, err := trustStorerFactory().TrustedStore()
			if err != nil {
				return trustChainError(err)
			}
			fmt.Fprintf(out, "anchor key: %s\n", store.AnchorKeyID)
			if len(store.Keys) == 0 {
				fmt.Fprintln(out, "no enrolled keys")
				return nil
			}
			fmt.Fprintln(out, "enrolled keys:")
			for _, k := range store.Keys {
				fmt.Fprintf(out, "  %s  %s  %s\n", k.KeyID, k.Machine, k.AddedAt)
			}
			return nil
		},
	}
}

// trustChainError decorates a TrustedStore failure with the actionable fix. A
// missing anchor or store both mean "run trust init"; anything else passes
// through with its typed reason intact.
func trustChainError(err error) error {
	switch {
	case errors.Is(err, bless.ErrNoAnchor):
		return fmt.Errorf("no trust anchor is installed — run 'forgectl workflow trust init' first: %w", err)
	case errors.Is(err, bless.ErrTrustStoreInvalid):
		return fmt.Errorf("the trust store is missing or invalid — run 'forgectl workflow trust init' first: %w", err)
	default:
		return err
	}
}

// shortHostname returns the machine's hostname with any domain suffix stripped,
// falling back to "unknown" when it can't be determined.
func shortHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	return h
}
