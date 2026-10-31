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

	"github.com/cameronsjo/forgectl/internal/surface/hcadapter"
)

const (
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

// ErrNoHC means the hc command is not on PATH.
var ErrNoHC = errors.New("hc was not found on PATH")

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

// Source is the process side of hc, shared with the worker surface: the hc
// adapter (hcadapter.Adapter) runs every call, so the watch gets the same
// absolute hc path, environment scrub, sealed argv and output caps as a
// worker launch. It reaches the core HC_SESSION selects.
type Source interface {
	// GetJSON returns `hc get --json <target>`; a missing terminal wraps
	// hcadapter.ErrNoTerminal.
	GetJSON(ctx context.Context, target string) ([]byte, error)
	// ListJSON returns `hc ls --json`.
	ListJSON(ctx context.Context) ([]byte, error)
	// ReadText returns `hc read <target>` and whether it is whole.
	ReadText(ctx context.Context, target string) ([]byte, bool, error)
	// Events runs `hc events --json`, writing its stdout to w until ctx
	// ends or the stream stops.
	Events(ctx context.Context, w io.Writer) error
}

// CLI is HC over the hc command line. A nil Source means hc could not be
// found: every read reports ErrNoHC and Events closes at once.
type CLI struct {
	Source Source
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
	if c.Source == nil {
		return Terminal{}, fmt.Errorf("hc get: %w", ErrNoHC)
	}
	out, err := c.Source.GetJSON(ctx, target)
	if err != nil {
		return Terminal{}, hcFailure(ctx, "hc get", err)
	}
	var g hcGet
	if json.Unmarshal(out, &g) != nil {
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
	if c.Source == nil {
		return nil, fmt.Errorf("hc ls: %w", ErrNoHC)
	}
	out, err := c.Source.ListJSON(ctx)
	if err != nil {
		return nil, hcFailure(ctx, "hc ls", err)
	}
	var resp struct {
		Terminals []hcGet `json:"terminals"`
	}
	if json.Unmarshal(out, &resp) != nil {
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

// screenCutNote ends a screen that was longer than the runner's output cap.
const screenCutNote = "\n[the screen was cut: only its start fit]"

// Read implements HC. It returns hc's text as is: the caller strips and
// escapes it before it reaches a terminal.
func (c CLI) Read(ctx context.Context, target string) (string, error) {
	if err := checkTarget(target); err != nil {
		return "", err
	}
	if c.Source == nil {
		return "", fmt.Errorf("hc read: %w", ErrNoHC)
	}
	out, complete, err := c.Source.ReadText(ctx, target)
	if err != nil {
		return "", hcFailure(ctx, "hc read", err)
	}
	if !complete {
		return string(out) + screenCutNote, nil
	}
	return string(out), nil
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
	if errors.Is(err, hcadapter.ErrNoTerminal) {
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
// anyone else's `hc wait`. With no Source, Wake and Done are closed at once.
func (c CLI) Events(ctx context.Context) EventStream {
	wake := make(chan struct{}, 1)
	done := make(chan struct{})
	if c.Source == nil {
		close(wake)
		close(done)
		return EventStream{Wake: wake, Done: done}
	}
	pr, pw := io.Pipe()
	go func() {
		defer close(done)
		err := c.Source.Events(ctx, pw)
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
