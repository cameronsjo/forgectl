package herdr

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
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

// NotificationShow runs `herdr notification show [--body B] [--sound S] TITLE`.
//
// It goes through the sensitive seam ([exec.KindHerdrNotify]) rather than the
// Client's Runner, which logs its argv: the text is an item's name and WHAT
// line, and must not become a log line. herdrPath must be absolute. The
// server reached is herdr's own choice, as for every call in this package.
func NotificationShow(ctx context.Context, run exec.SensitiveRunner, herdrPath string, n Notification) error {
	if !filepath.IsAbs(herdrPath) {
		return errors.New("herdr: notification show needs an absolute herdr path")
	}
	title := notificationText(n.Title)
	if title == "" {
		return errors.New("herdr: notification show needs a title")
	}
	args := []exec.Arg{exec.MustFixed("notification"), exec.MustFixed("show")}
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
		return fmt.Errorf("herdr: unknown notification sound %d", n.Sound)
	}
	args = append(args, exec.Opaque(title))
	res, err := run.RunSensitive(ctx, exec.SensitiveCommand{
		Kind:      exec.KindHerdrNotify,
		Path:      exec.Secret(herdrPath),
		Args:      args,
		StdoutCap: notifyStreamCap,
		StderrCap: notifyStreamCap,
	})
	if err != nil {
		var se *exec.SensitiveError
		if errors.As(err, &se) && se.Outcome == exec.OutcomeExit && res.ExitCode == exitFailure {
			if data, complete := res.Stderr.CopyBytesForParse(); complete {
				if e := refusal(data); e != nil {
					e.cause = err
					return e
				}
			}
		}
		return fmt.Errorf("herdr notification show: %w", err)
	}
	// An exit-0 reply can still be herdr's refusal envelope (#722).
	if data, complete := res.Stdout.CopyBytesForParse(); complete {
		if e := refusal(data); e != nil {
			return e
		}
	}
	return nil
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
