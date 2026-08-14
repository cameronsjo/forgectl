package pr

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
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

var dispatchWindowIDPattern = regexp.MustCompile(`^@[0-9]+$`)

// Dispatch is the generation-qualified identity returned by one successful
// detached review launch. WindowID is opaque outside this package.
type Dispatch struct {
	Ref      Ref
	WindowID string
}

// newDispatch parses the `new-window -P -F tmux.IdentityFormat` result into a
// generation-qualified identity.
//
// It splits with tmux.SplitFields, not a bare strings.Split on tmux.FieldSep,
// because tmux 3.5a and older hand the separator back octal-escaped (see
// escapedFieldSep in internal/tmux/format.go). Every field is still validated
// exactly as before — pid and start time numeric, window id ^@[0-9]+$ — and
// WindowID is rejoined on the RAW separator, so the in-memory identity is one
// canonical form regardless of which tmux produced it. That is what keeps it
// comparable to the ListWindows rows VerifyDispatched matches against, which
// normalize the same way.
func newDispatch(ref Ref, output string) (Dispatch, error) {
	fields := tmux.SplitFields(output)
	if len(fields) != 3 {
		return Dispatch{}, fmt.Errorf("tmux dispatch identity has %d fields, want 3 (raw %q)", len(fields), output)
	}
	pid, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil || pid == 0 {
		return Dispatch{}, fmt.Errorf("invalid tmux server pid %q", fields[0])
	}
	if _, err := strconv.ParseUint(fields[1], 10, 64); err != nil {
		return Dispatch{}, fmt.Errorf("invalid tmux server start time %q", fields[1])
	}
	if !dispatchWindowIDPattern.MatchString(fields[2]) {
		return Dispatch{}, fmt.Errorf("invalid tmux window id %q", fields[2])
	}
	return Dispatch{Ref: ref, WindowID: strings.Join(fields, tmux.FieldSep)}, nil
}

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

// ReviewWindowName is the tmux window name for ref's review window, derived from
// ref's logical key (sessionkey.go) through the name codec (sessionname.go).
//
// It reads Ref.IsLocal() rather than the owner spelling, which is the whole of
// forgectl#218: the predecessor assembled "pr-<owner>-<repo>-<N>" from display
// parts, so a synthetic local session (owner "local", repo a short oid, number
// derived from that oid) and a genuine PR under a forge owner literally named
// "local" could spell one name — and tmux's first-match targeting then sent
// `pr attach` and `pr teardown` at whichever window it found first. Two
// different things now have two different keys, so they cannot share a name.
//
// It also retires the dot-to-hyphen rewrite the predecessor needed. tmux splits
// a target on "." as the window.pane separator, so a window named
// "pr-o-foo.bar-42" mis-resolved to window="pr-o-foo", pane="bar-42"; the
// rewrite fixed that at the cost of a real recollision between repos "a.b" and
// "a-b". The generated grammar excludes "." outright, and the digest is taken
// over the key rather than the display spelling, so both problems are gone
// rather than traded.
//
// It returns an error rather than a fallback name: a name with no key behind it
// is exactly the unbacked authority #218 removes, so every caller fails closed.
func ReviewWindowName(ref Ref) (string, error) {
	return refWindowName(ref, roleReview)
}

// shellWindowName is the name for the non-authoritative shell window `pr open`
// puts in the clean room. It is a distinct identity, not the review name with a
// suffix — the role lives in the digest, so it cannot be lost to truncation and
// a shell window can never be mistaken for the review it sits beside.
func shellWindowName(ref Ref) (string, error) {
	return refWindowName(ref, roleShell)
}

func refWindowName(ref Ref, role nameRole) (string, error) {
	key, err := sessionKeyForRef(ref)
	if err != nil {
		return "", fmt.Errorf("derive review session identity for %s: %w", ref.String(), err)
	}
	name, err := key.encodedName(sessionLabelForRef(ref), role)
	if err != nil {
		return "", fmt.Errorf("derive review window name for %s: %w", ref.String(), err)
	}
	return name, nil
}

// ensureSession resolves the review session by exact name, creating it if it
// does not exist, and returns its generation-qualified identity.
//
// The name never reaches a `-t` operand — every window command below targets
// the returned native id. The predecessor here built `=forgectl:` by hand and
// relied on that spelling being exactly right; the "=" alone was not enough
// (see internal/tmux/target.go for the measured resolution table), and getting
// it wrong deposited review windows into whatever session prefix-matched.
func (c *Client) ensureSession(ctx context.Context) (tmux.SessionIdentity, error) {
	session, err := tmux.New(c.run).EnsureSession(ctx, c.tmuxSession, "")
	if err != nil {
		return tmux.SessionIdentity{}, fmt.Errorf("resolve review session %q: %w", c.tmuxSession, err)
	}
	return session, nil
}

// newWindowTarget renders the `new-window -t` destination for the review
// session. Split out so both launch paths and `pr open` share one spelling.
func newWindowTarget(session tmux.SessionIdentity) (string, error) {
	return tmux.NewWindowSessionTarget(session.ID)
}

// resolveReviewWindow finds the review window for ref inside the review
// session, by exact name under that session's native id.
//
// Resolution happens on use rather than from the breadcrumb: breadcrumbs
// predate native ids and carry none, and an id persisted across a tmux server
// restart would name a different window anyway. What the breadcrumb supplies is
// the NAME to look for; the identity is rebuilt from the live server every time.
func (c *Client) resolveReviewWindow(ctx context.Context, ref Ref) (tmux.WindowIdentity, error) {
	name, err := ReviewWindowName(ref)
	if err != nil {
		return tmux.WindowIdentity{}, err
	}
	t := tmux.New(c.run)
	session, err := t.ResolveSessionExact(ctx, c.tmuxSession)
	if err != nil {
		return tmux.WindowIdentity{}, fmt.Errorf("resolve review session %q: %w", c.tmuxSession, err)
	}
	return t.ResolveWindowExact(ctx, session, name)
}

// Launch dispatches the review agent for sess into a fresh tmux window under
// the client's session. Agent A (InlineSeeded) runs `claude -p <reviewPrompt>`
// with the profile-resolved posture; agent B (BareTUIEscalation) is NOT YET
// WIRED and returns a clear error here rather than dispatching. Launch never
// posts a review — that is PostReview's job, behind the approval gate.
func (c *Client) Launch(ctx context.Context, sess Session, cfg config.Config) (Dispatch, error) {
	if sess.Workspace == "" {
		return Dispatch{}, fmt.Errorf("cannot launch: session has no workspace (dry-run?)")
	}
	// Authoritative use-based guard. Prepare refuses the same pairing earlier so
	// nothing is fetched, but this is the one every route reaches — including a
	// Session reconstituted from a breadcrumb, which never re-enters Prepare.
	if err := CheckAgentForRef(sess.Agent, sess.Ref); err != nil {
		return Dispatch{}, err
	}
	// FindingsDir is deliberately NOT persisted (see Session), so loadSession
	// leaves it zero. A local dispatch on a reload would then pass --add-dir ""
	// and seed a prompt reading "…to a file under , and write nothing anywhere
	// else" — a silently misconfigured review rather than a failed one. Nothing
	// calls Launch on a reload today; this makes the future verb that does fail
	// loudly instead.
	if sess.Ref.IsLocal() && sess.FindingsDir == "" {
		return Dispatch{}, fmt.Errorf(
			"local review session %s has no findings directory: it is not persisted in the breadcrumb, "+
				"so a reloaded session cannot be dispatched — re-run `forgectl pr local`",
			sess.Ref.String(),
		)
	}
	switch path := LaunchPathFor(sess.Agent); path {
	case InlineSeeded:
		return c.launchInline(ctx, sess, cfg)
	case BareTUIEscalation:
		return Dispatch{}, fmt.Errorf("agent %q (bare-TUI escalation) is not yet wired", sess.Agent)
	case CodexExec:
		return c.launchCodex(ctx, sess, cfg)
	default:
		return Dispatch{}, fmt.Errorf("unknown launch path %v for agent %q", path, sess.Agent)
	}
}

// CheckDispatchCapability refuses unsupported or unidentifiable tmux before a
// caller creates any review workspace or breadcrumb.
func (c *Client) CheckDispatchCapability(ctx context.Context) error {
	_, err := tmux.New(c.run).CheckGenerationCapability(ctx)
	return err
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
func (c *Client) launchCodex(ctx context.Context, sess Session, cfg config.Config) (Dispatch, error) {
	codexPath, err := launch.CodexPath(cfg.Launch.Defaults)
	if err != nil {
		return Dispatch{}, fmt.Errorf("resolve codex binary: %w", err)
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
	// Same guard, same reason as launchInline's: this is the other clean-room
	// dispatch, and profile.Model lands in the identical argv neighbourhood
	// (`codex exec --sandbox read-only --model <value>`). Validate below only
	// rejects CLAUDE model ids on a Codex profile, so an option-like value
	// passes it untouched and the check would otherwise rest on clap refusing
	// `--model --flag` rather than swallowing it — which is exactly the
	// parser-behavior assumption the guard exists not to depend on.
	if err := sandbox.RejectOptionLike("reviewer model", profile.Model); err != nil {
		return Dispatch{}, err
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
		return Dispatch{}, fmt.Errorf("codex review profile invalid: %w", err)
	}
	codexArgs := launch.CodexExecArgs(profile, []string{prompt})
	if err := c.CheckDispatchCapability(ctx); err != nil {
		return Dispatch{}, err
	}
	session, err := c.ensureSession(ctx)
	if err != nil {
		return Dispatch{}, err
	}
	target, err := newWindowTarget(session)
	if err != nil {
		return Dispatch{}, err
	}
	name, err := ReviewWindowName(sess.Ref)
	if err != nil {
		return Dispatch{}, err
	}
	args := []string{
		"new-window",
		"-P", "-F", tmux.IdentityFormat,
		"-t", target,
		"-n", name,
		"-c", sess.Workspace,
		"--", codexPath,
	}
	args = append(args, codexArgs...)
	out, err := c.run.Run(ctx, "tmux", args...)
	if err != nil {
		return Dispatch{}, fmt.Errorf("open Codex review window: %w", err)
	}
	dispatch, err := newDispatch(sess.Ref, out)
	if err != nil {
		return Dispatch{}, fmt.Errorf("read Codex review window identity: %w", err)
	}
	slog.Info("Successfully dispatched Codex clean-room review.",
		"ref", sess.Ref.String(), "session_id", session.ID, "window", name)
	return dispatch, nil
}

// A same-name/different-key collision is NOT probed for before creating a
// window, deliberately. The digest carries 160 bits over the canonical key, so
// two distinct sessions cannot land on one name by accident; a same-name window
// therefore means either a leftover from this same session — the same key, so
// not a collision — or a window planted by someone who can already write to the
// operator's tmux session, which is a far larger compromise than a mis-targeted
// review. Buying the check would cost a `list-windows` fork per launch, in a
// package whose targeting is built around not forking per ref (see WindowsLive
// in admission.go), and what it protects against is already covered downstream:
// VerifyDispatched revalidates each dispatch's exact native id, session, and
// name after the fact, and every act on an existing window resolves through
// ResolveWindowExact under the session's native id rather than by name lookup.

// launchInline composes the claude argv and opens it in a tmux window rooted
// at the workspace. It uses launch.ClaudePath/Resolve/BuilderArgs — never
// launch.Exec (which would replace this process); the review runs in its own
// tmux window via the Runner.
func (c *Client) launchInline(ctx context.Context, sess Session, cfg config.Config) (Dispatch, error) {
	claudePath, err := launch.ClaudePath(cfg.Launch.Defaults)
	if err != nil {
		return Dispatch{}, fmt.Errorf("resolve claude binary: %w", err)
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
	//
	// Note the sharper edge of the same rule: the discard covers an EXPLICIT
	// ambient effort too, not just a derived one. `[launch.defaults] effort =
	// "low"` plus `[pr] model = "opus"` and no `[pr] effort` reviews at
	// medium, dropping the operator's floor. That is the "own posture, not a
	// patch" design being consistent — a project-level model override in
	// launch.resolve re-derives the same way — and `[pr] effort` is the way to
	// pin a level that survives it.
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
		return Dispatch{}, err
	}
	if err := sandbox.RejectOptionLike("reviewer effort", profile.Effort); err != nil {
		return Dispatch{}, err
	}

	// Validate the assembled posture before dispatch. This path needs it more
	// than `forgectl launch` does, not less: a launch execs into the operator's
	// own terminal, so a value claude rejects prints its error where they are
	// looking. A review is dispatched into a DETACHED tmux window, where the
	// same rejection is an empty pane and no error anywhere — a silent failure
	// that reads as "the review is still thinking". launchCodex has validated
	// since it shipped; this is the inline half.
	if err := profile.Validate(); err != nil {
		return Dispatch{}, fmt.Errorf("clean-room review profile invalid: %w", err)
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

	if err := c.CheckDispatchCapability(ctx); err != nil {
		return Dispatch{}, err
	}
	session, err := c.ensureSession(ctx)
	if err != nil {
		return Dispatch{}, err
	}
	target, err := newWindowTarget(session)
	if err != nil {
		return Dispatch{}, err
	}
	name, err := ReviewWindowName(sess.Ref)
	if err != nil {
		return Dispatch{}, err
	}
	args := []string{
		"new-window",
		"-P", "-F", tmux.IdentityFormat,
		"-t", target,
		"-n", name,
		"-c", sess.Workspace,
		"--", claudePath,
	}
	args = append(args, claudeArgs...)

	slog.Debug("Preparing to dispatch review into tmux window.",
		"session_id", session.ID, "window", name, "workspace", sess.Workspace)
	out, err := c.run.Run(ctx, "tmux", args...)
	if err != nil {
		return Dispatch{}, fmt.Errorf("open review window: %w", err)
	}
	dispatch, err := newDispatch(sess.Ref, out)
	if err != nil {
		return Dispatch{}, fmt.Errorf("read review window identity: %w", err)
	}
	slog.Info("Successfully dispatched clean-room review.",
		"ref", sess.Ref.String(), "session_id", session.ID, "window", name)
	return dispatch, nil
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
