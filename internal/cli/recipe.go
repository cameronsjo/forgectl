package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

const (
	herdrPaneIDEnv       = "HERDR_PANE_ID"
	herdrActivePaneIDEnv = "HERDR_ACTIVE_PANE_ID"
)

// recipeCompactMarkers are the substrings that prove /compact actually ran.
// herdr's CLI exits 0 for any non-error response, so a zero exit from the
// submission says the request was accepted, never that the text landed and ran.
// Measured 2026-09-06: the pane renders "Compacting conversation…" while it
// works. Anything here must be text the payload itself cannot contain, or the
// check matches the command echoing in the input box and can never fail.
var recipeCompactMarkers = []string{"Compacting", "Compacted", "Compact summary"}

// herdrTargetEnvKeys are the fallbacks for CHOOSING a target, most specific
// first. HERDR_ACTIVE_PANE_ID belongs here and nowhere else: herdr sets it from
// the workspace's FOCUSED pane, not the caller's, so it is a reasonable default
// target and a wrong answer to "is this me".
var herdrTargetEnvKeys = []string{herdrPaneIDEnv, herdrActivePaneIDEnv}

// maxRecipeRenameRunes bounds a name that is pasted into another pane's input
// in a single submission.
const maxRecipeRenameRunes = 128

// recipeReadLines is how much of the pane the receipt check reads back. Wide
// enough that a compaction line is not scrolled past between polls, narrow
// enough that the read stays cheap.
const recipeReadLines = "60"

var recipeModule = module.Manifest{
	Name:         "recipe",
	Tier:         module.TierExtension,
	ConfigKey:    "",
	GroupAliases: []string{"r"},
	New:          newRecipeCmd,
}

var (
	lookupRecipeEnv = os.LookupEnv
	recipeSleep     = time.Sleep
)

type recipeAfkOptions struct {
	Target      string
	Rename      string
	CompactOnly bool
	SkipReceipt bool
}

// herdrStep is one herdr invocation the recipe intends to make. Building the
// whole list before running any of it keeps the ordering decisions pure and
// table-testable, and lets the preflight refuse before the first side effect.
type herdrStep struct {
	Args []string
	// Describes the step in an error, e.g. "run /journal through herdr agent prompt".
	Describe string
}

func newRecipeCmd(deps module.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "recipe",
		Aliases: []string{"r"},
		Short:   "Run small built-in workbench recipes",
	}
	cmd.AddCommand(newRecipeAfkCmd(deps))
	return cmd
}

func newRecipeAfkCmd(deps module.Deps) *cobra.Command {
	var opts recipeAfkOptions
	cmd := &cobra.Command{
		Use:   "afk",
		Short: "Journal and compact the current Herdr agent pane",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRecipeAfk(cmd.Context(), deps.Runner, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Target, "target", "", "Herdr agent name or pane id (defaults to HERDR_PANE_ID, then HERDR_ACTIVE_PANE_ID)")
	cmd.Flags().StringVar(&opts.Rename, "rename", "", "Rename the session with /rename <name> before compacting")
	cmd.Flags().BoolVar(&opts.CompactOnly, "compact-only", false, "Skip /journal and run /compact alone (for a caller that already journaled)")
	cmd.Flags().BoolVar(&opts.SkipReceipt, "skip-receipt", false, "Do not read the pane back to confirm /compact ran")
	return cmd
}

func runRecipeAfk(ctx context.Context, runner exec.Runner, opts recipeAfkOptions) error {
	target, ok := resolveRecipeHerdrTarget(opts.Target)
	if !ok {
		return WithExitCode(fmt.Errorf("no Herdr target found; pass --target or run inside a Herdr pane with %s or %s set", herdrPaneIDEnv, herdrActivePaneIDEnv), 2)
	}
	if err := validateRecipeHerdrTarget(target); err != nil {
		return WithExitCode(err, 2)
	}
	if opts.Rename != "" {
		if err := validateRecipeRename(opts.Rename); err != nil {
			return WithExitCode(err, 2)
		}
	}

	// Preflight before the first side effect. `agent get` is the honest probe:
	// it proves herdr is installed, the server is answering, AND the target
	// resolves — all three of which otherwise surface only after /journal has
	// already spent a model call. A `--help` probe would catch none of them.
	preflight, err := runner.Run(ctx, "herdr", "agent", "get", target)
	if err != nil {
		return fmt.Errorf("preflight: resolve Herdr target %q with herdr agent get: %w", target, err)
	}

	isSelf := targetIsSelf(target, preflight)

	// Snapshot the pane BEFORE submitting. A marker already on screen from an
	// earlier compaction would otherwise satisfy the receipt check without this
	// run's /compact doing anything, so the check has to be for a marker that is
	// NEW — matching text that was already there is a check that cannot fail.
	var before string
	wantReceipt := !opts.SkipReceipt && !isSelf
	if wantReceipt {
		if out, err := runner.Run(ctx, "herdr", "agent", "read", target, "--source", "visible", "--lines", recipeReadLines); err == nil {
			before = out
		}
	}

	for _, step := range recipeAfkSteps(opts, target, isSelf) {
		if _, err := runner.Run(ctx, "herdr", step.Args...); err != nil {
			return fmt.Errorf("%s: %w", step.Describe, err)
		}
	}

	if !wantReceipt {
		return nil
	}
	return confirmRecipeCompact(ctx, runner, target, before)
}

// recipeAfkSteps is the pure ordering decision: which herdr calls, in what
// order, with which flags.
//
// `--wait` is dropped when the target is the caller's own pane. `agent prompt
// --wait` waits for the agent to reach a settled state, and an agent running
// this very command is `working` and cannot settle until the command returns —
// so a self-targeted --wait deadlocks the caller against itself. Self-targeting
// is not an error to refuse: a session compacting itself before its usage block
// expires is the whole point of the recipe.
//
// The cost of dropping it is real and worth naming: on a self-target the steps
// are submitted back to back with nothing sequencing them, so their ordering
// rests entirely on the pane's own prompt queue rather than on anything this
// command observes. That is also why a self-targeted run does no receipt check —
// /compact cannot run until this process's turn ends, so any read taken here
// necessarily predates it. Every other target sequences each step with --wait.
func recipeAfkSteps(opts recipeAfkOptions, target string, isSelf bool) []herdrStep {
	steps := make([]herdrStep, 0, 3)
	withWait := func(args []string) []string {
		if isSelf {
			return args
		}
		return append(args, "--wait")
	}

	if !opts.CompactOnly {
		steps = append(steps, herdrStep{
			Args:     withWait([]string{"agent", "prompt", target, "/journal"}),
			Describe: "run /journal through herdr agent prompt",
		})
	}

	if opts.Rename != "" {
		steps = append(steps, herdrStep{
			Args:     withWait([]string{"agent", "prompt", target, "/rename " + opts.Rename}),
			Describe: fmt.Sprintf("run /rename %s through herdr agent prompt", opts.Rename),
		})
	}

	steps = append(steps, herdrStep{
		Args:     []string{"agent", "prompt", target, "/compact"},
		Describe: "run /compact through herdr agent prompt",
	})
	return steps
}

// confirmRecipeCompact reads the pane back and looks for evidence that /compact
// ran. Without this the recipe reports success whenever herdr accepted the
// request, which is not the same claim.
//
// `before` is the pane as it looked prior to submission: a marker is evidence
// only if it was NOT already there. A pane compacted ten minutes ago still shows
// "Compacted" in its visible window, so matching the raw text would pass on
// every second run regardless of what this one did.
func confirmRecipeCompact(ctx context.Context, runner exec.Runner, target, before string) error {
	const (
		attempts = 10
		interval = 2 * time.Second
	)
	var lastErr error
	for range attempts {
		// Sleep first, including on the opening attempt: reading the instant
		// after submission catches the pane before the agent could have acted,
		// which only ever matches pre-existing text.
		recipeSleep(interval)
		out, err := runner.Run(ctx, "herdr", "agent", "read", target, "--source", "visible", "--lines", recipeReadLines)
		if err != nil {
			lastErr = err
			continue
		}
		lastErr = nil // a later success must not be reported as the earlier read failure
		if newRecipeMarker(before, out, recipeCompactMarkers) != "" {
			return nil
		}
	}
	if lastErr != nil {
		return fmt.Errorf("confirm /compact ran: reading pane %q failed: %w", target, lastErr)
	}
	return fmt.Errorf("confirm /compact ran: submitted to %q and herdr accepted it, but no new %v appeared in the pane; the text may have landed without running", target, recipeCompactMarkers)
}

// newRecipeMarker returns the first marker present in after and absent from
// before, or "" when none is newly present.
func newRecipeMarker(before, after string, markers []string) string {
	for _, marker := range markers {
		if strings.Contains(after, marker) && !strings.Contains(before, marker) {
			return marker
		}
	}
	return ""
}

// targetIsSelf reports whether target names the pane this process runs in.
//
// The answer is taken from the preflight response, not from comparing the two
// raw strings, because this one bit disables both safety mechanisms at once:
// a true drops --wait from every step AND skips the receipt check. Getting it
// wrong in the false-positive direction submits an unsequenced, unverified
// /compact into somebody else's live session and destroys its context.
//
// Two rules make that safe. Only HERDR_PANE_ID can answer it — herdr sets
// HERDR_ACTIVE_PANE_ID from the workspace's FOCUSED pane, so a forgectl spawned
// from a hook, a workflow, or any process holding a stale environment would
// otherwise call a stranger's pane "self". And an agent NAME is compared through
// the canonical pane id herdr just reported for it, so `--target reviewer`
// naming our own pane is recognised rather than deadlocking against itself.
func targetIsSelf(target, preflight string) bool {
	ownPane, ok := lookupRecipeEnv(herdrPaneIDEnv)
	if !ok || ownPane == "" {
		return false
	}
	if target == ownPane {
		return true
	}
	var resp struct {
		Result struct {
			Agent struct {
				PaneID string `json:"pane_id"`
			} `json:"agent"`
		} `json:"result"`
	}
	// A response that does not parse leaves this false: refusing to claim
	// self-ness keeps --wait and the receipt check on, which is the safe
	// direction — the cost is a deadlock the operator sees, not a stranger's
	// context silently destroyed.
	if json.Unmarshal([]byte(preflight), &resp) != nil {
		return false
	}
	return resp.Result.Agent.PaneID != "" && resp.Result.Agent.PaneID == ownPane
}

// validateRecipeHerdrTarget rejects a target herdr would misread. `agent
// prompt` takes its target positionally and never parses it as a flag, so this
// is about a legible error rather than an injection: a leading dash or embedded
// whitespace produces a confusing herdr-side failure well after the journal
// step has already run.
func validateRecipeHerdrTarget(target string) error {
	if strings.HasPrefix(target, "-") {
		return fmt.Errorf("invalid Herdr target %q: must not start with %q", target, "-")
	}
	if bad, found := firstDisallowedRune(target); found {
		return fmt.Errorf("invalid Herdr target %q: contains %q; allowed are letters, digits, and %q", target, bad, "._-:/@")
	}
	return nil
}

// firstDisallowedRune enforces an ALLOWLIST rather than naming characters to
// reject. The values here are written onto another pane's pty and joined into
// operator-facing error text, so the set that must be excluded is "everything
// nobody vouched for" — a denylist of the control bytes known today is one
// unlisted escape sequence away from failing open.
func firstDisallowedRune(value string) (rune, bool) {
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("._-:/@", r):
		default:
			return r, true
		}
	}
	return 0, false
}

// validateRecipeRename allowlists the name. herdr writes the payload to the
// target pane's pty verbatim — bracket-wrapped when the pane has bracketed paste
// on, raw otherwise — and nothing downstream escapes it. So the receiving
// surface is a terminal parsing bytes, and a name is not "just a label": an
// embedded ESC cancels the input box, \x03 interrupts the agent, and the literal
// end-of-paste marker closes the paste region early so every following byte
// arrives as keystrokes. Enumerating those is the losing game; allowlist instead.
func validateRecipeRename(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("invalid --rename %q: must not be blank", name)
	}
	if n := len([]rune(name)); n > maxRecipeRenameRunes {
		return fmt.Errorf("invalid --rename: %d runes exceeds the %d-rune limit", n, maxRecipeRenameRunes)
	}
	for _, r := range name {
		ok := r == ' ' || strings.ContainsRune("._-", r) ||
			(r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)))
		if !ok {
			return fmt.Errorf("invalid --rename %q: contains %q; allowed are ASCII letters, digits, space, and %q", name, r, "._-")
		}
	}
	// The receipt check proves /compact ran by finding a compaction marker that
	// was not on screen before. A name carrying one puts that text on screen
	// itself — the input-box echo and the new title both render it — so the
	// receipt would pass without /compact doing anything. Refusing the overlap
	// keeps the two mechanisms independent.
	for _, marker := range recipeCompactMarkers {
		if strings.Contains(name, marker) {
			return fmt.Errorf("invalid --rename %q: must not contain %q, which the /compact receipt check looks for", name, marker)
		}
	}
	return nil
}

func resolveRecipeHerdrTarget(explicit string) (string, bool) {
	if explicit != "" {
		return explicit, true
	}
	for _, key := range herdrTargetEnvKeys {
		if value, ok := lookupRecipeEnv(key); ok && value != "" {
			return value, true
		}
	}
	return "", false
}
