package pr

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
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
func localReviewPrompt(findingsDir string) string {
	return "Review the committed changes in this working tree as a clean-room " +
		"reviewer. Inspect the diff and the checked-out tree, then write your " +
		"findings (by severity: Critical / Important / Nit, with file:line and a " +
		"concrete fix) to a file under " + findingsDir + " — the only directory you " +
		"may write to. Do NOT post, comment, merge, or push anything, and do not " +
		"attempt any network access — output the review only, to that file."
}

// windowName is the tmux window name for a review session:
// "pr-<owner>-<sanitized repo>-<N>". Owner is included, not just Number: a
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
	return fmt.Sprintf("pr-%s-%s-%d", ref.Owner, repo, ref.Number)
}

// windowTarget is the tmux target "<session>:pr-<owner>-<sanitized repo>-<N>"
// for select/attach.
func (c *Client) windowTarget(ref Ref) string {
	return c.tmuxSession + ":" + windowName(ref)
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
	switch path := LaunchPathFor(sess.Agent); path {
	case InlineSeeded:
		return c.launchInline(ctx, sess, cfg)
	case BareTUIEscalation:
		return fmt.Errorf("agent %q (bare-TUI escalation) is not yet wired", sess.Agent)
	default:
		return fmt.Errorf("unknown launch path %v for agent %q", path, sess.Agent)
	}
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

	prompt := reviewPrompt
	if sess.Ref.IsLocal() {
		// Grant --add-dir for the escape-hatch findings dir. Without this, the
		// permission-scoped Write(<dir>/**) allowlist rule is moot — Claude Code
		// won't expose a path outside the launch cwd at all.
		profile.AddDir = append(profile.AddDir, sess.FindingsDir)
		prompt = localReviewPrompt(sess.FindingsDir)
	}
	claudeArgs := launch.BuilderArgs(profile, []string{"-p", prompt})

	args := []string{
		"new-window",
		"-t", c.tmuxSession,
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
// exists to provide. Ref.IsLocal() is the reload-safe predicate (persisted
// via Ref.Owner) — it still catches a reload-reconstituted Session, e.g.
// from a future verb built on the loadSession pattern.
//
// It returns whether a post actually fired.
func (c *Client) PostReview(ctx context.Context, sess Session, review string, headless bool) (posted bool, err error) {
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
