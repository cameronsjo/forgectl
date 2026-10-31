package hcadapter

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// The read-only calls below are the hc access `surface watch` uses. They run
// through the adapter's own runner, so the watch gets what every adapter call
// gets: the absolute hc path chosen at construction, the launching session's
// handles dropped from the environment (exec.UnsetLauncherSessionHandles),
// sealed argv, and capped output. They take a name or an address, not a
// ledger reference, so they make no ownership check: the watch only shows
// what hc reports, and never acts on a terminal.

// ErrNoTerminal reports that hc has no terminal by that name or address.
var ErrNoTerminal = errors.New("hc: no such terminal")

// ErrQueryFailed reports an hc call that failed for any other reason. Its
// text names the verb and the runner's failure class, never hc's stderr.
var ErrQueryFailed = errors.New("hc: the call failed")

// GetJSON returns `hc get --json <target>` as hc printed it.
func (a *Adapter) GetJSON(ctx context.Context, target string) ([]byte, error) {
	return a.query(ctx, "get", exec.KindHcProbe,
		exec.MustFixed("get"), exec.MustFixed("--json"), exec.EndOfOptions(), exec.Opaque(target))
}

// ListJSON returns `hc ls --json` as hc printed it.
func (a *Adapter) ListJSON(ctx context.Context) ([]byte, error) {
	return a.query(ctx, "ls", exec.KindHcProbe, exec.MustFixed("ls"), exec.MustFixed("--json"))
}

// ReadText returns `hc read <target>`, the terminal's screen as text. It is
// untrusted: the caller strips and escapes it before showing it. complete is
// false when the screen was longer than the runner's output cap and only its
// start came back.
func (a *Adapter) ReadText(ctx context.Context, target string) (text []byte, complete bool, err error) {
	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHcRead,
		exec.MustFixed("read"), exec.EndOfOptions(), exec.Opaque(target)))
	if runErr != nil {
		return nil, false, a.queryError("read", res, runErr)
	}
	text, complete = res.Stdout.CopyBytesForParse()
	return text, complete, nil
}

func (a *Adapter) query(ctx context.Context, verb string, kind exec.CommandKind, args ...exec.Arg) ([]byte, error) {
	res, runErr := a.run.RunSensitive(ctx, a.command(kind, args...))
	if runErr != nil {
		return nil, a.queryError(verb, res, runErr)
	}
	raw, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		return nil, fmt.Errorf("%w: hc %s output was over %d bytes", ErrQueryFailed, verb, stdoutCap)
	}
	return raw, nil
}

func (a *Adapter) queryError(verb string, res exec.SensitiveResult, runErr error) error {
	if errorKind(res.Stderr) == kindNotFound {
		return fmt.Errorf("hc %s: %w", verb, ErrNoTerminal)
	}
	return fmt.Errorf("%w: hc %s: %v", ErrQueryFailed, verb, classifyRunError(runErr))
}

// EventStreamer runs a long-lived command with the launching session's
// handles removed from its environment and copies its stdout to w until ctx
// ends. exec.OSRunner satisfies it.
type EventStreamer interface {
	RunStreamingScrubbed(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error
}

// StreamEvents runs `hc events --json` with the adapter's hc path and writes
// its stdout to w until ctx ends or the stream stops. The stream outlives the
// sensitive runner's per-call bound, so it goes through s, which applies the
// same environment scrub.
func (a *Adapter) StreamEvents(ctx context.Context, s EventStreamer, w io.Writer) error {
	if s == nil {
		return fmt.Errorf("%w: no event streamer", ErrQueryFailed)
	}
	return s.RunStreamingScrubbed(ctx, w, io.Discard, a.hcPath, "events", "--json")
}
