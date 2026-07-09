package pr

import (
	"context"
	"fmt"
	"log/slog"

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

// windowName is the tmux window name for a review session: "pr-<N>".
func windowName(ref Ref) string { return fmt.Sprintf("pr-%d", ref.Number) }

// windowTarget is the tmux target "<session>:pr-<N>" for select/attach.
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
	claudeArgs := launch.BuilderArgs(profile, []string{"-p", reviewPrompt})

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
// It returns whether a post actually fired.
func (c *Client) PostReview(ctx context.Context, sess Session, review string, headless bool) (posted bool, err error) {
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
