package tmux

import (
	"context"
	"errors"
	"fmt"
	"regexp"
)

// tmux's target grammar is the whole reason this file exists.
//
// A `-t` operand is not a name. tmux resolves it through an ordered chain —
// native id, exact name, fnmatch glob, then PREFIX — and the chain differs per
// command, so the same string means different objects to `new-window` and to
// `kill-session`. A session literally named `forge` and a session named
// `forge-review` are one keystroke apart in that grammar: `kill-session -t
// forge` will happily kill `forge-review` when `forge` does not exist
// (forgectl#237, reproduced live on tmux 3.7b). Quoting cannot help — argv is
// already separated; tmux itself is the interpreter.
//
// The fix is to stop passing names as targets. Every action targets a NATIVE
// ID ($N, @N, %N), which tmux resolves by identity and never by prefix. Native
// IDs alone are not sufficient though: they are per-server counters, so a
// restarted server hands `$1` to a completely different session, and
// `move-window` reparents an `@N` out from under the session the operator
// selected. So an ID only ever travels bound to the server generation that
// minted it (socket selection + server pid + server start time) and, where it
// has one, its parent's ID. Revalidating that tuple immediately before dispatch
// is what turns "the ID I saw a moment ago" into "the object the operator
// chose".

var (
	sessionIDPattern = regexp.MustCompile(`^\$[0-9]+$`)
	paneIDPattern    = regexp.MustCompile(`^%[0-9]+$`)
)

// ValidateSessionID checks a native tmux session id ("$N"). The id stays a
// string: tmux owns the numbering, and parsing it into a Go integer would
// invite arithmetic on a value that is only ever compared for equality.
func ValidateSessionID(id string) error {
	if !sessionIDPattern.MatchString(id) {
		return fmt.Errorf("%w: %q is not a native tmux session id (want $N)", ErrMalformedID, id)
	}
	return nil
}

// ValidateWindowID checks a native tmux window id ("@N").
func ValidateWindowID(id string) error {
	if !windowIDPattern.MatchString(id) {
		return fmt.Errorf("%w: %q is not a native tmux window id (want @N)", ErrMalformedID, id)
	}
	return nil
}

// ValidatePaneID checks a native tmux pane id ("%N").
func ValidatePaneID(id string) error {
	if !paneIDPattern.MatchString(id) {
		return fmt.Errorf("%w: %q is not a native tmux pane id (want %%N)", ErrMalformedID, id)
	}
	return nil
}

// ServerSelector captures WHICH tmux server a command reaches.
//
// Selection is one of two modes, and they are mutually exclusive by
// construction — exactly one group of fields is ever populated:
//
//   - ENVIRONMENTAL (Socket empty). No `-L`/`-S` is passed, so selection is
//     entirely environmental: inside tmux, $TMUX names the socket the client is
//     attached to; outside it, $TMUX_TMPDIR moves the default socket directory.
//   - PINNED (Socket set). Every command carries `-S <Socket>`, which overrides
//     the environment outright, so $TMUX and $TMUX_TMPDIR say nothing about
//     which server is reached and are deliberately NOT captured. Recording them
//     anyway would manufacture an ErrSelectorChanged the moment the operator
//     attached to any tmux session, refusing an action against a server that
//     had not moved at all.
//
// Capturing the mode is what makes the three server identities impossible to
// cross — an id minted on a pinned socket cannot be silently acted on against
// the default server, or against a different pinned socket, because the
// selector no longer matches and every action refuses.
//
// classifyServerFailure grants the "no server, you may create the first one"
// verdict only for the mode this selector is in: an environmental client
// derives and inspects the default socket, a pinned client inspects its own,
// and an argv naming any OTHER socket is refused outright (hasExplicitSocketArg).
type ServerSelector struct {
	TmuxEnv string
	TmpDir  string
	Socket  string
}

// ServerGeneration identifies one running tmux server: which socket it is on,
// and which incarnation of the server that is. PID alone is not enough — pids
// are recycled — so the start time rides along.
type ServerGeneration struct {
	Selector  ServerSelector
	PID       string
	StartTime string
}

// qualified reports whether the generation carries enough to be revalidated.
// The zero generation is the shape a bare id acquires when someone stuffs it
// into an action struct without going through a resolver, which is exactly the
// boundary crossing this package refuses.
func (g ServerGeneration) qualified() error {
	if g.PID == "" || g.StartTime == "" {
		return fmt.Errorf("%w: server pid/start time missing", ErrUnqualifiedIdentity)
	}
	return nil
}

// SessionIdentity is a session bound to the server generation that minted its
// id. Name is display and exact-lookup only — it never reaches a `-t` operand.
type SessionIdentity struct {
	Generation ServerGeneration
	ID         string
	Name       string
}

// WindowIdentity is a window bound to its generation and its parent session's
// id. SessionID is load-bearing, not decorative: `move-window` reparents a
// window without changing its @id.
type WindowIdentity struct {
	Generation ServerGeneration
	ID         string
	SessionID  string
	Name       string
}

// PaneIdentity is a pane bound to its generation and its parent window's id.
type PaneIdentity struct {
	Generation ServerGeneration
	ID         string
	WindowID   string
}

var (
	// ErrMalformedID reports a value that is not a native tmux id.
	ErrMalformedID = errors.New("malformed native tmux id")
	// ErrUnqualifiedIdentity reports an id that arrived without the server
	// generation needed to prove it still means what it meant at capture.
	ErrUnqualifiedIdentity = errors.New("tmux identity is not qualified by a server generation")
	// ErrSelectorChanged reports that the tmux server SELECTION moved between
	// capture and use — a different socket, so a different server entirely.
	ErrSelectorChanged = errors.New("tmux server selection changed since capture")
	// ErrGenerationChanged reports that the server restarted between capture and
	// use, so every native id from the capture now belongs to a stranger.
	ErrGenerationChanged = errors.New("tmux server restarted since capture")
	// ErrObjectGone reports that the captured id no longer exists.
	ErrObjectGone = errors.New("tmux object no longer exists")
	// ErrWrongParent reports that the captured object moved under a different
	// parent since capture.
	ErrWrongParent = errors.New("tmux object was reparented since capture")
	// ErrSessionNotFound reports that no session carries the requested name
	// exactly. It is deliberately NOT satisfied by a prefix or glob sibling:
	// that fallback is forgectl#237.
	ErrSessionNotFound = errors.New("no tmux session with that exact name")
	// ErrNoServer reports that no tmux server is running on the socket THIS
	// client selects — the default one derived from the environment, or the
	// pinned one when the client carries a socket. It is the one classification
	// that permits a caller to create the first server. A command aimed at any
	// other socket never reaches this verdict.
	ErrNoServer = errors.New("no tmux server is running")
	// ErrDuplicateSession reports tmux refusing to create a session because the
	// name is taken. It is the ONLY create failure that permits a second lookup.
	ErrDuplicateSession = errors.New("a tmux session with that name already exists")
	// ErrServerUnreadable reports a server state that is neither usable nor
	// safely absent: a custom socket, a stale socket, a permission failure, or
	// an unrecognized tmux error. Callers must refuse rather than fall back to
	// the default server.
	ErrServerUnreadable = errors.New("tmux server state could not be read")
)

// currentSelector reads the live server selection. Compared against a captured
// selector, an inequality here is the "you are pointed at a different server
// now" refusal.
//
// A pinned client reports its socket and nothing else, per ServerSelector's
// two-mode contract: the pin is immutable for the client's lifetime, so this is
// constant rather than "live", which is exactly the property a pin is for.
func (c *Client) currentSelector() ServerSelector {
	if c.socket != "" {
		return ServerSelector{Socket: c.socket}
	}
	return ServerSelector{TmuxEnv: c.getenv("TMUX"), TmpDir: c.getenv("TMUX_TMPDIR")}
}

// SessionIdentity binds a listed session row to the server selection this
// client is pointed at. It exists so callers outside the package can carry a
// full identity out of a listing without the selector being exported, which
// would invite one being assembled from somewhere other than the client that
// took the listing.
func (c *Client) SessionIdentity(s Session) SessionIdentity {
	return s.Identity(c.currentSelector())
}

// WindowIdentity binds a listed window row to the current server selection.
func (c *Client) WindowIdentity(w Window) WindowIdentity {
	return w.Identity(c.currentSelector())
}

// serverStateError maps a failed tmux command onto a typed server-state error,
// routing #242's classifier rather than adding a second one. Only
// serverAbsent becomes ErrNoServer; every other verdict — including a
// custom socket we cannot inspect — becomes ErrServerUnreadable, because
// proceeding would mean acting on the default server after asking about
// another one.
func (c *Client) serverStateError(ctx context.Context, args []string, err error) error {
	failure := c.classifyServerFailure(ctx, args, err)
	switch failure.Kind {
	case serverAbsent:
		return fmt.Errorf("%w (default socket %s)", ErrNoServer, failure.SocketPath)
	case serverCanceled:
		return failure.Cause
	default:
		cause := failure.Cause
		if cause == nil {
			cause = err
		}
		return fmt.Errorf("%w: %w", ErrServerUnreadable, cause)
	}
}

// preflight runs every check that needs no tmux call: a malformed or
// unqualified identity, or a server selection that has moved. Keeping these
// ahead of the listing is what makes "zero commands run on a refusal"
// structural rather than incidental.
func (c *Client) preflight(gen ServerGeneration, id string, validate func(string) error) error {
	if err := validate(id); err != nil {
		return err
	}
	if err := gen.qualified(); err != nil {
		return err
	}
	if current := c.currentSelector(); current != gen.Selector {
		return fmt.Errorf("%w: captured %+v, now %+v", ErrSelectorChanged, gen.Selector, current)
	}
	return nil
}

// matchesGeneration reports whether a listed row came from the captured server
// incarnation.
func (g ServerGeneration) matches(pid, startTime string) bool {
	return g.PID == pid && g.StartTime == startTime
}

// RevalidateSession proves a captured session still exists, on the same server
// incarnation, immediately before it is acted on. It returns a refreshed
// identity so callers report the session's CURRENT name.
//
// Every refusal path runs zero selecting, renaming, or killing commands: the
// only tmux call this makes is the read-only listing.
func (c *Client) RevalidateSession(ctx context.Context, want SessionIdentity) (SessionIdentity, error) {
	if err := c.preflight(want.Generation, want.ID, ValidateSessionID); err != nil {
		return SessionIdentity{}, err
	}
	sessions, err := c.ListSessions(ctx)
	if err != nil {
		return SessionIdentity{}, err
	}
	for _, s := range sessions {
		if !want.Generation.matches(s.ServerPID, s.ServerStart) {
			return SessionIdentity{}, generationDrift(want.Generation, s.ServerPID, s.ServerStart)
		}
		if s.ID == want.ID {
			return SessionIdentity{Generation: want.Generation, ID: s.ID, Name: s.Name}, nil
		}
	}
	return SessionIdentity{}, fmt.Errorf("%w: session %s (%q)", ErrObjectGone, want.ID, want.Name)
}

// RevalidateWindow proves a captured window still exists on the same server
// incarnation AND still belongs to the session it was captured under.
func (c *Client) RevalidateWindow(ctx context.Context, want WindowIdentity) (WindowIdentity, error) {
	if err := c.preflight(want.Generation, want.ID, ValidateWindowID); err != nil {
		return WindowIdentity{}, err
	}
	if err := ValidateSessionID(want.SessionID); err != nil {
		return WindowIdentity{}, fmt.Errorf("window %s parent: %w", want.ID, err)
	}
	windows, err := c.ListWindows(ctx)
	if err != nil {
		return WindowIdentity{}, err
	}
	for _, w := range windows {
		if !want.Generation.matches(w.ServerPID, w.ServerStart) {
			return WindowIdentity{}, generationDrift(want.Generation, w.ServerPID, w.ServerStart)
		}
		if w.ID != want.ID {
			continue
		}
		if w.SessionID != want.SessionID {
			return WindowIdentity{}, fmt.Errorf(
				"%w: window %s was under session %s at capture and is now under %s",
				ErrWrongParent, w.ID, want.SessionID, w.SessionID)
		}
		return WindowIdentity{Generation: want.Generation, ID: w.ID, SessionID: w.SessionID, Name: w.Name}, nil
	}
	return WindowIdentity{}, fmt.Errorf("%w: window %s (%q)", ErrObjectGone, want.ID, want.Name)
}

func generationDrift(want ServerGeneration, pid, startTime string) error {
	return fmt.Errorf("%w: captured server pid %s started %s, now pid %s started %s",
		ErrGenerationChanged, want.PID, want.StartTime, pid, startTime)
}
