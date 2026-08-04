package pr

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/sandbox"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// reviewPrompt seeds the inline (agent A) review. It is intentionally
// read-only in intent: the agent inspects and drafts, it never posts. Posting
// is gated exclusively by PostReview's human approval gate.
const reviewPrompt = "Review this pull request as a clean-room reviewer. " +
	"Inspect the diff and the checked-out tree, then report findings by severity " +
	"(Critical / Important / Nit) with file:line and a concrete fix. " +
	"Do NOT post, comment, merge, or push anything — output the review only."

// localReviewPrompt seeds a local (offline) review — there is no PR to post
// to, so it directs findings to the writable escape-hatch dir (named
// explicitly, since that path is the one thing tying the prompt to the
// scoped Write(findingsDir/**) allowlist grant) instead of a PostReview
// approval gate.
//
// writesEnforced says whether the harness actually confines writes to
// findingsDir. Under agent A it does: the workspace allowlist grants exactly
// one Write(findingsDir/**) rule, and --add-dir is what makes that grant
// reachable at all — so the prompt may state the restriction as fact. Under
// Codex it does not: --add-dir adds a writable root ALONGSIDE an
// already-writable workspace, and approval_policy="never" removes the prompt
// that would otherwise surface a stray write. There the sentence is an
// instruction, not a control, and it says so. Asserting an enforcement that
// is not there is worse than asserting nothing — it invites a reader to trust
// prose as a boundary.
func localReviewPrompt(findingsDir string, writesEnforced bool) string {
	scope := "Write your findings to a file under " + findingsDir + ", and write " +
		"nothing anywhere else — nothing in this instruction restricts you, so treat " +
		"it as a requirement of the task."
	if writesEnforced {
		scope = "Write your findings to a file under " + findingsDir +
			" — the only directory you may write to."
	}
	return "Review the committed changes in this working tree as a clean-room " +
		"reviewer. Inspect the diff and the checked-out tree, then report your " +
		"findings by severity (Critical / Important / Nit, with file:line and a " +
		"concrete fix). " + scope + " Do NOT post, comment, merge, or push " +
		"anything, and do not attempt any network access — output the review only, " +
		"to that file."
}

// windowName is the tmux window name for a review session:
// "<reviewWindowPrefix><owner>-<sanitized repo>-<N>" — built off
// reviewWindowPrefix (admission.go), not a re-hardcoded literal, so the
// concurrency gate's window count can never silently drift out of sync with
// what a review launch actually names its window. Owner is included, not
// just Number: a
// local-mode Ref (Owner "local", Number derived from a hex oid prefix) and a
// real PR-mode Ref can otherwise land on the identical "pr-<N>" name whenever
// the derived number happens to match a live PR number — Number alone is not
// unique across the two Ref kinds. Repo is included too: two repos under the
// same owner (o/a#42 and o/b#42) still collide on "pr-<owner>-<N>" — Owner
// alone is not unique across repos.
//
// The repo component has its dots replaced with hyphens. Empirically, tmux
// target strings split on "." as the window.pane separator: a window
// literally named "pr-o-foo.bar-42" mis-resolves `select-window -t
// sess:pr-o-foo.bar-42` to window="pr-o-foo", pane="bar-42" instead of
// matching (or cleanly failing to match) the window — functional breakage
// for a legal GitHub repo name. Sanitizing accepts a narrower, purely
// cosmetic recollision (repos "a.b" and "a-b" under the same owner sharing
// a window) in exchange for correct targeting, which is the greater good.
func windowName(ref Ref) string {
	repo := strings.ReplaceAll(ref.Repo, ".", "-")
	return reviewWindowPrefix + fmt.Sprintf("%s-%s-%d", ref.Owner, repo, ref.Number)
}

// exactSessionTarget is the tmux -t form that resolves ONLY a session
// literally named c.tmuxSession — never tmux's own -t fallback resolution
// (exact name, then fnmatch, then PREFIX). Both the leading "=" (tmux's
// exact-match modifier) and the trailing ":" are load-bearing: live-verified
// on an isolated tmux socket (tmux 3.7b) that "=forgectl" WITHOUT the
// trailing colon still falls through to prefix-matching an unrelated
// session like "forgectl-review" for a `new-window -t` session target — only
// "=forgectl:" refuses correctly when the exact session is absent, and lands
// correctly when it exists. has-session doesn't need the trailing colon (its
// target is always session-only), but including it there too costs nothing
// and keeps every caller using one form.
func (c *Client) exactSessionTarget() string {
	return "=" + c.tmuxSession + ":"
}

// windowTarget is the tmux target "<exact session>:pr-<owner>-<sanitized
// repo>-<N>" for select/attach/kill. Built off exactSessionTarget for the
// same reason new-window is: a bare or "="-without-colon session prefix on
// a session:window target ALSO fuzzy-prefix-matches the session part
// (verified live), so a stale leaked window under a wrongly-resolved
// sibling session could otherwise be selected or killed as if it were the
// real one.
func (c *Client) windowTarget(ref Ref) string {
	return c.exactSessionTarget() + windowName(ref)
}

// ensureSession creates the client's tmux session explicitly if a session
// literally named c.tmuxSession doesn't already exist. Before
// exactSessionTarget, a review's FIRST-EVER new-window call implicitly
// depended on tmux's own fuzzy -t resolution to land the window somewhere —
// in a sibling session if one happened to prefix-match, or fail outright
// with "can't find window" if nothing did (verified live: with no session at
// all, or only unrelated sessions, the bare `-t c.tmuxSession` form used to
// fail the exact same way exact targeting does now). That was never a real
// bootstrap mechanism, just an accident of tmux's resolution order — now
// that new-window targets the session exactly, dispatch must create the
// session itself rather than rely on it.
func (c *Client) ensureSession(ctx context.Context) error {
	t := tmux.New(c.run)
	if t.HasSession(ctx, "="+c.tmuxSession) {
		return nil
	}
	if _, err := c.run.Run(ctx, "tmux", "new-session", "-d", "-s", c.tmuxSession); err != nil {
		return fmt.Errorf("create review session %q: %w", c.tmuxSession, err)
	}
	return nil
}

// Launch dispatches the review agent for sess into a fresh tmux window under
// the client's session. Agent A (InlineSeeded) runs `claude -p <reviewPrompt>`
// with the profile-resolved posture; agent B (BareTUIEscalation) is NOT YET
// WIRED and returns a clear error here rather than dispatching. Launch never
// posts a review — that is PostReview's job, behind the approval gate.
func (c *Client) Launch(ctx context.Context, sess Session, cfg config.Config) error {
	if sess.Workspace == "" {
		return fmt.Errorf("cannot launch: session has no workspace (dry-run?)")
	}
	// Authoritative use-based guard. Prepare refuses the same pairing earlier so
	// nothing is fetched, but this is the one every route reaches — including a
	// Session reconstituted from a breadcrumb, which never re-enters Prepare.
	if err := CheckAgentForRef(sess.Agent, sess.Ref); err != nil {
		return err
	}
	// FindingsDir is deliberately NOT persisted (see Session), so loadSession
	// leaves it zero. A local dispatch on a reload would then pass --add-dir ""
	// and seed a prompt reading "…to a file under , and write nothing anywhere
	// else" — a silently misconfigured review rather than a failed one. Nothing
	// calls Launch on a reload today; this makes the future verb that does fail
	// loudly instead.
	if sess.Ref.IsLocal() && sess.FindingsDir == "" {
		return fmt.Errorf(
			"local review session %s has no findings directory: it is not persisted in the breadcrumb, "+
				"so a reloaded session cannot be dispatched — re-run `forgectl pr local`",
			sess.Ref.String(),
		)
	}
	switch path := LaunchPathFor(sess.Agent); path {
	case InlineSeeded:
		return c.launchInline(ctx, sess, cfg)
	case BareTUIEscalation:
		return fmt.Errorf("agent %q (bare-TUI escalation) is not yet wired", sess.Agent)
	case CodexExec:
		return c.launchCodex(ctx, sess, cfg)
	default:
		return fmt.Errorf("unknown launch path %v for agent %q", path, sess.Agent)
	}
}

// launchCodex dispatches `codex exec` under Codex's native sandbox. Remote PR
// reviews are read-only. Local reviews use workspace-write plus the dedicated
// findings directory as an additional writable root.
//
// This posture bounds WRITES and network egress, not command execution: the
// reviewer can run arbitrary shell and read the whole host filesystem. See
// CodexExec in agent.go for the measured delta against agent A's allowlist and
// why it is not closable from `codex exec` today.
//
// No MCP counterpart to launchInline's profile.StrictMCP is set here, and that
// is not an oversight: `codex exec --help` exposes no MCP flag at all, so there
// is no harness-boundary control to force.
//
// What bounds the exposure, stated exactly: CheckAgentForRef refuses Codex for
// remote PR heads, so this path only ever runs against a workspace on a path
// the operator owns. That is all sess.Ref.IsLocal() proves — PATH ownership,
// not COMMIT provenance. PrepareLocal accepts a detached HEAD and only warns,
// so an ordinary `gh pr checkout` of a third-party branch reaches this path
// with the contributor's tree in it, repo-supplied MCP config included. The
// content under review is therefore not necessarily the operator's own; what
// holds this at Tier 2 is that the operator selected that checkout deliberately
// and can inspect it before dispatch, not that the bytes are theirs. Revisit if
// Codex gains a strict-config flag, if the remote-head refusal is relaxed, or
// if detached heads are ever rejected before dispatch.
func (c *Client) launchCodex(ctx context.Context, sess Session, cfg config.Config) error {
	codexPath, err := launch.CodexPath(cfg.Launch.Defaults)
	if err != nil {
		return fmt.Errorf("resolve codex binary: %w", err)
	}
	resolved := launch.Resolve(cfg.Launch, sess.Workspace)
	profile := launch.Profile{
		Harness:        "codex",
		ApprovalPolicy: "never",
		Sandbox:        "read-only",
	}
	if resolved.Harness == "codex" {
		profile.Model = resolved.Model
	}
	// [pr] model/effort is deliberately NOT applied here, and its absence is
	// the correct behavior rather than an oversight. Both keys are Claude-side:
	// effort maps to --effort, which `codex exec` has no equivalent for, and a
	// model set for the Claude reviewer is a Claude id that would reach `codex
	// --model`. That is the exact cross-harness mispairing the resolved.Harness
	// guard above already refuses; honoring [pr] model here would reintroduce it
	// from a different direction.
	prompt := reviewPrompt
	if sess.Ref.IsLocal() {
		profile.Sandbox = "workspace-write"
		profile.AddDir = []string{sess.FindingsDir}
		// writesEnforced=false: Codex's --add-dir widens an already-writable
		// workspace rather than being the sole grant, so the findings-dir scope
		// is an instruction here, not a control.
		prompt = localReviewPrompt(sess.FindingsDir, false)
	}
	if err := profile.Validate(); err != nil {
		return fmt.Errorf("codex review profile invalid: %w", err)
	}
	codexArgs := launch.CodexExecArgs(profile, []string{prompt})
	if err := c.ensureSession(ctx); err != nil {
		return err
	}
	args := []string{
		"new-window",
		"-t", c.exactSessionTarget(),
		"-n", windowName(sess.Ref),
		"-c", sess.Workspace,
		"--", codexPath,
	}
	args = append(args, codexArgs...)
	if _, err := c.run.Run(ctx, "tmux", args...); err != nil {
		return fmt.Errorf("open Codex review window: %w", err)
	}
	slog.Info("Successfully dispatched Codex clean-room review.", "ref", sess.Ref.String(), "window", c.windowTarget(sess.Ref))
	return nil
}

// launchInline composes the claude argv and opens it in a tmux window rooted
// at the workspace. It uses launch.ClaudePath/Resolve/BuilderArgs — never
// launch.Exec (which would replace this process); the review runs in its own
// tmux window via the Runner.
func (c *Client) launchInline(ctx context.Context, sess Session, cfg config.Config) error {
	claudePath, err := launch.ClaudePath(cfg.Launch.Defaults)
	if err != nil {
		return fmt.Errorf("resolve claude binary: %w", err)
	}
	// Clean-room review runs under a HARDENED posture regardless of the user's
	// ambient launch profile: never --allow-dangerously-skip-permissions, always
	// plan mode. Inheriting a permissive config (AllowDanger, a bypass permission
	// mode) would let the review agent ignore the deny-by-default workspace
	// allowlist — the whole clean-room control. Force the safe posture here.
	profile := launch.Resolve(cfg.Launch, sess.Workspace)
	profile.AllowDanger = false
	profile.PermissionMode = "plan"
	// Refuse every DISCOVERED MCP configuration. The workspace is a third
	// party's checkout, and Claude Code spawns a discovered server's `command` +
	// `args` at session START, before the agent invokes any tool — so both
	// controls above sit downstream of the boundary an `.mcp.json` in the head
	// already crossed. quarantine.DefaultTargets renames that file aside as the
	// backstop; this is the control at the harness boundary, and unlike a
	// filename list it does not go stale when Claude Code learns a new config
	// path. Measured on 2.1.220: with the flag, a planted carrier did not spawn;
	// without it, the same carrier did.
	profile.StrictMCP = true
	// The resolved profile is the user's ambient launch config, which may well
	// be a Codex one — `harness = "codex"` in [launch.defaults] is exactly the
	// config the Codex work targets. This dispatch is `claude` regardless, and
	// BuilderArgs passes Model unconditionally, so a Codex model id would reach
	// `claude --model <that>`. Force the harness and re-derive the model.
	// launchCodex guards the mirror case (it adopts resolved.Model only when
	// the resolved harness is codex); this is the other half.
	//
	// Effort is re-derived alongside the model for the same reason it is in
	// launch.resolve: it is a property OF the model, so carrying a level
	// chosen under the old one across a model swap is the mispairing, not the
	// fix. A Codex profile's own [launch.defaults] effort is doubly wrong here
	// — Codex has no --effort, so any level set there was inert until this
	// forced harness switch made it reachable.
	if profile.Harness != "claude" {
		profile.Harness = "claude"
		profile.Model = launch.DefaultModelFor("claude")
		profile.Effort = launch.EffortForModel(profile.Model)
	}
	// Drop the operator's ambient add_dir. launch.Resolve returns
	// [launch.defaults].add_dir plus any project block matching the workspace,
	// and BuilderArgs emits --add-dir for each — so `add_dir = ["~/notes"]`
	// would hand the reviewer of a remote hostile head a root outside the clean
	// room, defeating the deny-by-default workspace scoping for those paths.
	// The local branch below re-adds exactly one entry, the findings dir.
	// launchCodex builds a fresh Profile and inherits none; this is the
	// inline half.
	profile.AddDir = nil

	// [pr] model/effort is its own posture, not a patch on the ambient one.
	// Setting model DISCARDS the ambient repo's effort and re-derives, because
	// filling only when empty would be a silent no-op: Effort is already
	// non-empty by the time this runs (launch.Resolve derives it), so an
	// ambient sonnet repo plus `[pr] model = "opus"` would ship
	// `--model opus --effort high` — the exact pairing this knob exists to
	// prevent.
	if cfg.Pr.Model != "" {
		profile.Model = cfg.Pr.Model
		profile.Effort = launch.EffortForModel(cfg.Pr.Model)
	}
	if cfg.Pr.Effort != "" {
		profile.Effort = cfg.Pr.Effort
	}

	// Guard the FINAL values, not just the [pr]-sourced ones. Both land as the
	// operands of --model and --effort in an argv whose neighbours are the
	// clean room's security flags, and a leading '-' there relies on the
	// parser swallowing the token as a value rather than reading it as a flag
	// in its own right. Which config layer supplied the value does not change
	// that, so guarding only the newest source would leave the ambient
	// [launch] half — reachable on every review where [pr] is unset, i.e. the
	// default — travelling the identical path unchecked.
	//
	// There is no shell anywhere below (exec.CommandContext with separate
	// args), so word-splitting and metacharacters are already off the table;
	// this closes the one residual.
	if err := sandbox.RejectOptionLike("reviewer model", profile.Model); err != nil {
		return err
	}
	if err := sandbox.RejectOptionLike("reviewer effort", profile.Effort); err != nil {
		return err
	}

	// Validate the assembled posture before dispatch. This path needs it more
	// than `forgectl launch` does, not less: a launch execs into the operator's
	// own terminal, so a value claude rejects prints its error where they are
	// looking. A review is dispatched into a DETACHED tmux window, where the
	// same rejection is an empty pane and no error anywhere — a silent failure
	// that reads as "the review is still thinking". launchCodex has validated
	// since it shipped; this is the inline half.
	if err := profile.Validate(); err != nil {
		return fmt.Errorf("clean-room review profile invalid: %w", err)
	}

	prompt := reviewPrompt
	if sess.Ref.IsLocal() {
		// Grant --add-dir for the escape-hatch findings dir. Without this, the
		// permission-scoped Write(<dir>/**) allowlist rule is moot — Claude Code
		// won't expose a path outside the launch cwd at all.
		profile.AddDir = append(profile.AddDir, sess.FindingsDir)
		prompt = localReviewPrompt(sess.FindingsDir, true)
	}
	claudeArgs := launch.BuilderArgs(profile, []string{"-p", prompt})

	if err := c.ensureSession(ctx); err != nil {
		return err
	}
	args := []string{
		"new-window",
		"-t", c.exactSessionTarget(),
		"-n", windowName(sess.Ref),
		"-c", sess.Workspace,
		"--", claudePath,
	}
	args = append(args, claudeArgs...)

	slog.Debug("Preparing to dispatch review into tmux window.", "target", c.windowTarget(sess.Ref), "workspace", sess.Workspace)
	if _, err := c.run.Run(ctx, "tmux", args...); err != nil {
		return fmt.Errorf("open review window: %w", err)
	}
	slog.Info("Successfully dispatched clean-room review.", "ref", sess.Ref.String(), "window", c.windowTarget(sess.Ref))
	return nil
}

// PostReview posts review to the PR — but ONLY after the human approval gate
// passes. SECURITY INVARIANT: the `gh pr review` argv that follows is
// textually the sole reachable post path, and it is unreachable unless approve
// returns true. In headless / non-interactive mode the gate is not shown at
// all: the review is staged (returned as not-posted), never auto-posted.
//
// A local (offline) review session is refused outright: there is no PR to
// post to, and sess.Ref.Slug() for a local session resolves to the synthetic
// "local/<oid>" identity — posting against it would fire an unintended
// `gh pr review` network call, breaking the offline guarantee `pr local`
// exists to provide. Ref.IsLocal() catches a reload-reconstituted Session
// (e.g. from a future verb built on the loadSession pattern) because
// loadSession restores locality from the breadcrumb's local flag — but only
// for breadcrumbs written by this version FORWARD. A pre-#185 local
// breadcrumb has no local key and reloads NON-local; see the tripwire on the
// refusal below.
//
// It returns whether a post actually fired.
func (c *Client) PostReview(ctx context.Context, sess Session, review string, headless bool) (posted bool, err error) {
	// TRIPWIRE for a future caller. This refusal is reload-safe only for
	// breadcrumbs carrying the local flag (this version forward). A synthetic
	// session recorded before #185 reloads NON-local and would fall straight
	// through to the post path below — so IsLocal alone is not sufficient to
	// exclude synthetic sessions from a loadSession result.
	//
	// There is NO other persisted field that closes the gap. FindingsDir is
	// unpersisted for remote sessions too (see Prepare), so on any reload it is
	// empty for every session — Launch's guard above only works because it is
	// CONJUNCTIVE with IsLocal(), which is the exact bit a pre-#185 breadcrumb
	// lacks. Nor is the ref's shape a discriminator: owner "local" with a
	// short-oid repo is precisely what a real forge repo can spell, which is
	// the bug this change fixed.
	//
	// So a future verb that calls PostReview on a loadSession result cannot
	// distinguish a legacy synthetic session at all. The operational answer is
	// migration, not detection: tear down pre-#185 local sessions on upgrade.
	// PostReview has no such caller today, which is the only reason the window
	// is harmless.
	if sess.Ref.IsLocal() {
		return false, fmt.Errorf("cannot post a review for a local session %q: there is no PR to post to", sess.Ref.String())
	}
	if headless || !c.isTTY() {
		slog.Info("Non-interactive/headless: staging review, not posting.", "ref", sess.Ref.String())
		return false, nil
	}

	approved, err := c.approve(review)
	if err != nil {
		return false, fmt.Errorf("approval gate: %w", err)
	}
	if !approved {
		slog.Info("Review post declined at approval gate.", "ref", sess.Ref.String())
		return false, nil
	}

	// --- Past this point, and ONLY past this point, a post argv reaches the
	// Runner. No other code path in this package invokes `gh pr review`. ---
	if _, err := c.run.Run(ctx, "gh", "pr", "review", fmt.Sprintf("%d", sess.Ref.Number),
		"--repo", sess.Ref.Slug(), "--comment", "--body", review); err != nil {
		return false, fmt.Errorf("post review: %w", err)
	}
	slog.Info("Posted approved review.", "ref", sess.Ref.String())
	return true, nil
}

// confirmReview is the default human approval gate: it surfaces the drafted
// review and asks for an explicit yes/no. It requires a TTY (huh renders an
// interactive form); PostReview only calls it when isTTY reports true.
func confirmReview(review string) (bool, error) {
	ok := false
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title("Drafted review — approve before posting?").
				Description(review),
			huh.NewConfirm().
				Title("Post this review to the PR?").
				Affirmative("Post").
				Negative("Cancel").
				Value(&ok),
		),
	).WithTheme(huh.ThemeCharm()).Run()
	return ok, err
}
