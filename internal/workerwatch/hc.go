package workerwatch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// hcBin is the hc CLI. It reaches whichever core HC_SESSION (or the calling
// terminal's HC_TERMINAL_SOCKET) selects, so a test or a smoke run against a
// scratch core needs nothing here.
const hcBin = "hc"

const (
	// maxHCOutput caps one hc JSON answer before it is decoded.
	maxHCOutput = 8 << 20
	// maxEventLine caps one line of the hc events stream. The first line is
	// a snapshot of every terminal, so it is far larger than an event.
	maxEventLine = 16 << 20
	// CosPrefix is the name prefix chief-of-staff workers carry.
	CosPrefix = "cos-"
)

// Statuses hc reports from a program's own OSC 7501 reports. They are the
// agent's claim about itself.
const (
	StatusWorking = "working"
	StatusIdle    = "idle"
	StatusDone    = "done"
	StatusBlocked = "blocked"
	StatusError   = "error"
)

// ErrGone means hc has no terminal by that name or address (hc exit 3,
// not_found).
var ErrGone = errors.New("no such hc terminal")

// hcNotFoundExit is hc's exit code for not_found.
const hcNotFoundExit = 3

// Terminal is what the watch reads about one hc terminal.
type Terminal struct {
	Address   string
	Name      string
	State     string // live or finished
	Status    string // one of the Status* values, or "" when it has reported none
	CreatedMS int64
	Model     string // from --model in its argv, or ""
	Cwd       string
}

// HC is the read-only part of hc the watch uses.
type HC interface {
	// Get reads one terminal by name or address.
	Get(ctx context.Context, target string) (Terminal, error)
	// List reads every live terminal.
	List(ctx context.Context) ([]Terminal, error)
	// Read returns a terminal's current screen as text.
	Read(ctx context.Context, target string) (string, error)
}

// Runner is the process seam: exec.Runner satisfies it.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// CLI is HC over the hc command line.
type CLI struct {
	Run Runner
	// Stream runs `hc events` and writes its stdout to w until ctx ends.
	// Nil means no event stream: the watch polls.
	Stream exec.StreamingRunner
}

var (
	addressRe = regexp.MustCompile(`^(?:[A-Za-z0-9._-]{1,64}/)?t[0-9a-z]{1,12}$`)
	modelRe   = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]{1,64}$`)
)

type hcGet struct {
	Address    string          `json:"address"`
	Name       *string         `json:"name"`
	State      string          `json:"state"`
	Status     json.RawMessage `json:"status"`
	CreatedMS  int64           `json:"created_ms"`
	Argv       []string        `json:"argv"`
	Cwd        string          `json:"cwd"`
	Foreground *struct {
		Argv []string `json:"argv"`
	} `json:"foreground"`
}

// terminal converts one hc record. Status is an object ({app, state}) in
// `hc get` and a bare word in `hc ls`; both are accepted.
func (g hcGet) terminal() (Terminal, bool) {
	if !addressRe.MatchString(g.Address) {
		return Terminal{}, false
	}
	t := Terminal{Address: g.Address, State: g.State, CreatedMS: g.CreatedMS, Cwd: g.Cwd}
	if g.Name != nil && checkName(*g.Name) == "" {
		t.Name = *g.Name
	}
	switch g.State {
	case "live", "finished":
	default:
		t.State = ""
	}
	t.Status = statusWord(g.Status)
	if g.Foreground != nil {
		t.Model = ModelFromArgv(g.Foreground.Argv)
	}
	if t.Model == "" {
		t.Model = ModelFromArgv(g.Argv)
	}
	return t, true
}

func statusWord(raw json.RawMessage) string {
	var word string
	if json.Unmarshal(raw, &word) != nil {
		var obj struct {
			State string `json:"state"`
		}
		if json.Unmarshal(raw, &obj) != nil {
			return ""
		}
		word = obj.State
	}
	switch word {
	case StatusWorking, StatusIdle, StatusDone, StatusBlocked, StatusError:
		return word
	}
	return ""
}

// ModelFromArgv returns the value of the last --model flag in argv, or "".
// It is shown as is, so anything that is not a plain model token is dropped.
func ModelFromArgv(argv []string) string {
	model := ""
	for i, a := range argv {
		switch {
		case a == "--model" && i+1 < len(argv):
			model = argv[i+1]
		case strings.HasPrefix(a, "--model="):
			model = strings.TrimPrefix(a, "--model=")
		}
	}
	if !modelRe.MatchString(model) {
		return ""
	}
	return model
}

// Get implements HC.
func (c CLI) Get(ctx context.Context, target string) (Terminal, error) {
	if err := checkTarget(target); err != nil {
		return Terminal{}, err
	}
	out, err := c.Run.Run(ctx, hcBin, "get", "--json", "--", target)
	if err != nil {
		return Terminal{}, hcFailure(ctx, "hc get", err)
	}
	var g hcGet
	if len(out) > maxHCOutput || json.Unmarshal([]byte(out), &g) != nil {
		return Terminal{}, errors.New("hc get returned an answer that is not a terminal record")
	}
	t, ok := g.terminal()
	if !ok {
		return Terminal{}, errors.New("hc get returned a record with no usable address")
	}
	return t, nil
}

// List implements HC.
func (c CLI) List(ctx context.Context) ([]Terminal, error) {
	out, err := c.Run.Run(ctx, hcBin, "ls", "--json")
	if err != nil {
		return nil, hcFailure(ctx, "hc ls", err)
	}
	var resp struct {
		Terminals []hcGet `json:"terminals"`
	}
	if len(out) > maxHCOutput || json.Unmarshal([]byte(out), &resp) != nil {
		return nil, errors.New("hc ls returned an answer that is not a terminal list")
	}
	ts := make([]Terminal, 0, len(resp.Terminals))
	for _, g := range resp.Terminals {
		if t, ok := g.terminal(); ok {
			ts = append(ts, t)
		}
	}
	return ts, nil
}

// Read implements HC. It returns hc's text as is: the caller strips and
// escapes it before it reaches a terminal.
func (c CLI) Read(ctx context.Context, target string) (string, error) {
	if err := checkTarget(target); err != nil {
		return "", err
	}
	out, err := c.Run.Run(ctx, hcBin, "read", "--", target)
	if err != nil {
		return "", hcFailure(ctx, "hc read", err)
	}
	if len(out) > maxHCOutput {
		out = out[len(out)-maxHCOutput:]
	}
	return out, nil
}

// checkTarget refuses a target that is neither a worker name nor an hc
// address, before it reaches argv.
func checkTarget(target string) error {
	if addressRe.MatchString(target) || checkName(target) == "" {
		return nil
	}
	return fmt.Errorf("%q is not an hc terminal name or address", target)
}

func hcFailure(ctx context.Context, what string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", what, ctxErr)
	}
	var ce *exec.CommandError
	if errors.As(err, &ce) && ce.ExitCode == hcNotFoundExit {
		return fmt.Errorf("%s: %w", what, ErrGone)
	}
	return fmt.Errorf("%s failed (run it by hand to see why)", what)
}

// wakeEvents are the hc event types that can change a row.
var wakeEvents = []string{"created", "lost", "restarted", "finished", "removed", "status", "turn"}

// EventStream is a running `hc events` subscription.
type EventStream struct {
	// Wake receives each time an event that can change a row arrives,
	// coalescing bursts: it holds at most one pending wake. It closes when
	// the stream ends, for any reason, so the caller can fall back to
	// polling.
	Wake <-chan struct{}
	// Done closes once the hc child has exited and been waited for. Cancel
	// the context, then wait on Done, so no `hc events` outlives the caller.
	Done <-chan struct{}
}

// Events subscribes to `hc events`. A subscription takes nothing from
// anyone else's `hc wait`. With no Stream runner, Wake and Done are closed
// at once.
func (c CLI) Events(ctx context.Context) EventStream {
	wake := make(chan struct{}, 1)
	done := make(chan struct{})
	if c.Stream == nil {
		close(wake)
		close(done)
		return EventStream{Wake: wake, Done: done}
	}
	pr, pw := io.Pipe()
	go func() {
		defer close(done)
		err := c.Stream.RunStreaming(ctx, nil, pw, io.Discard, hcBin, "events", "--json")
		_ = pw.CloseWithError(errors.Join(err, io.EOF)) // the reader only needs to know the stream ended
	}()
	go func() {
		defer close(wake)
		ScanEvents(pr, wake)
		_ = pr.Close() // unblocks the writer if the scan stopped early
	}()
	return EventStream{Wake: wake, Done: done}
}

// ScanEvents reads an hc events stream from r and signals wake (never
// blocking) for each event whose type is in wakeEvents. The first line, a
// snapshot, is skipped. It returns when r ends or a line is unusable.
func ScanEvents(r io.Reader, wake chan<- struct{}) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxEventLine)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		var ev struct {
			Params struct {
				Type string `json:"type"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if !slices.Contains(wakeEvents, ev.Params.Type) {
			continue
		}
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// Age is how long ago a terminal started, or 0 when hc gave no time.
func (t Terminal) Age(now time.Time) time.Duration {
	if t.CreatedMS <= 0 {
		return 0
	}
	return now.Sub(time.UnixMilli(t.CreatedMS))
}
