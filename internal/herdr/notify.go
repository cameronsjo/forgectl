package herdr

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr/wire"
	"github.com/cameronsjo/forgectl/internal/redact"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Sound is the sound a notification plays.
type Sound uint8

const (
	// SoundDefault leaves herdr's default.
	SoundDefault Sound = iota
	SoundNone
	SoundDone
	SoundRequest
)

// Notification is one herdr notification. Title and Body are untrusted text
// (item names, a script's WHAT line): NotificationShow renders them through
// termsafe and caps them before they reach the argv.
type Notification struct {
	Title string
	Body  string
	Sound Sound
}

// NotificationMaxRunes caps a notification's title and body.
const NotificationMaxRunes = 200

// notifyStreamCap bounds what herdr may print back; its reply is one short
// JSON line.
const notifyStreamCap = 4096

// NotificationResult is what herdr says it did with a notification.
type NotificationResult struct {
	// Shown is herdr's own "shown" answer. A reply with no result (a herdr
	// that predates it) reads as shown: the call exited 0.
	Shown bool
	// Reason is herdr's reason when it did not show it: disabled,
	// rate_limited, no_foreground_client, busy, or a value a newer herdr
	// adds. Empty when shown.
	Reason string
}

// NotificationArgs is the argv, after the binary and any --session pin, of
// `notification show TITLE [--body B] [--sound S]`.
//
// The title comes first: herdr reads the first operand after `show` as the
// title, whatever it looks like, and every later one as an option. With the
// title last, `notification show --body B ... TITLE` takes "--body" as the
// title and exits 2 with "unknown option: B" (herdr 0.9.3, forgectl#1181).
func NotificationArgs(n Notification) ([]exec.Arg, error) {
	title := notificationText(n.Title)
	if title == "" {
		return nil, errors.New("herdr: notification show needs a title")
	}
	// herdr prints its usage for these as the title instead of showing them.
	if title == "help" {
		title = " " + title
	}
	args := []exec.Arg{exec.MustFixed("notification"), exec.MustFixed("show"), exec.Opaque(title)}
	if body := notificationText(n.Body); body != "" {
		args = append(args, exec.MustFixed("--body"), exec.Opaque(body))
	}
	switch n.Sound {
	case SoundDefault:
	case SoundNone:
		args = append(args, exec.MustFixed("--sound"), exec.MustFixed("none"))
	case SoundDone:
		args = append(args, exec.MustFixed("--sound"), exec.MustFixed("done"))
	case SoundRequest:
		args = append(args, exec.MustFixed("--sound"), exec.MustFixed("request"))
	default:
		return nil, fmt.Errorf("herdr: unknown notification sound %d", n.Sound)
	}
	return args, nil
}

// NotificationOutcome reads one `notification show` run: herdr's refusal, a
// failure with herdr's own stderr in it, or the shown/reason result.
func NotificationOutcome(res exec.SensitiveResult, err error) (NotificationResult, error) {
	if err != nil {
		return NotificationResult{}, runFailure("notification show", res, err)
	}
	data, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		return NotificationResult{}, errors.New("herdr notification show: the reply was cut short")
	}
	// An exit-0 reply can still be herdr's refusal envelope (#722).
	if e := refusal(data); e != nil {
		return NotificationResult{}, e
	}
	r, derr := wire.DecodeResult[struct {
		Shown  *bool  `json:"shown"`
		Reason string `json:"reason"`
	}](data)
	if derr != nil || r.Shown == nil {
		return NotificationResult{Shown: true}, nil //nolint:nilerr // no result to read: the exit status is all herdr said
	}
	if *r.Shown {
		return NotificationResult{Shown: true}, nil
	}
	reason := printableMax(r.Reason)
	if reason == "" {
		reason = "no reason given"
	}
	return NotificationResult{Reason: reason}, nil
}

// NotificationShow runs `herdr notification show TITLE [--body B] [--sound S]`
// and returns herdr's shown/reason answer. A notification herdr declined to
// show is not an error here; the caller decides what it means.
//
// It goes through the sensitive seam ([exec.KindHerdrNotify]) rather than the
// Client's Runner, which logs its argv: the text is an item's name and WHAT
// line, and must not become a log line. herdrPath must be absolute. The
// server reached is herdr's own choice, as for every call in this package.
func NotificationShow(ctx context.Context, run exec.SensitiveRunner, herdrPath string, n Notification) (NotificationResult, error) {
	if !filepath.IsAbs(herdrPath) {
		return NotificationResult{}, errors.New("herdr: notification show needs an absolute herdr path")
	}
	args, err := NotificationArgs(n)
	if err != nil {
		return NotificationResult{}, err
	}
	res, err := run.RunSensitive(ctx, exec.SensitiveCommand{
		Kind:      exec.KindHerdrNotify,
		Path:      exec.Secret(herdrPath),
		Args:      args,
		StdoutCap: notifyStreamCap,
		StderrCap: notifyStreamCap,
	})
	return NotificationOutcome(res, err)
}

// runFailure words a failed herdr run: herdr's structured refusal when its
// stderr carries one, else the seam's error with herdr's own stderr after it
// (a usage error is plain text and exits 2), redacted and inert, so the
// operator sees why without running herdr by hand.
func runFailure(what string, res exec.SensitiveResult, err error) error {
	var se *exec.SensitiveError
	if errors.As(err, &se) && se.Outcome == exec.OutcomeExit && res.ExitCode == exitFailure {
		if data, complete := res.Stderr.CopyBytesForParse(); complete {
			if e := refusal(data); e != nil {
				e.cause = err
				return e
			}
		}
	}
	if detail := stderrDetail(res); detail != "" {
		return fmt.Errorf("herdr %s: %w; herdr said: %s", what, err, detail)
	}
	return fmt.Errorf("herdr %s: %w", what, err)
}

// stderrDetail is herdr's stderr as one inert, capped line with credential
// shapes redacted, or "" when it wrote none.
func stderrDetail(res exec.SensitiveResult) string {
	data, _ := res.Stderr.CopyBytesForParse()
	text := strings.TrimSpace(redact.Text(string(data)))
	if text == "" {
		return ""
	}
	return printableMax(text)
}

// notificationText renders untrusted text as one inert, capped line. A value
// starting with "-" would parse as a flag; the seam refuses it, so it gets a
// leading space instead (herdr's `--` handling at this verb is unmeasured).
func notificationText(s string) string {
	s = strings.TrimSpace(termsafe.SafeLineMax(s, NotificationMaxRunes))
	if strings.HasPrefix(s, "-") {
		s = " " + s
	}
	return s
}
