package tmux

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

// sessionFormat is the -F spec for list-sessions. Fields, in order:
// server pid, server start, native session id, name, window count,
// attached(1/0), created(unix), path — joined by FieldSep.
//
// The generation prefix mirrors windowFormat's: every row carries the server
// incarnation that produced it, so an identity built from a row needs no second
// probe that could disagree with it.
const sessionFormat = "#{pid}" + FieldSep +
	"#{start_time}" + FieldSep +
	"#{session_id}" + FieldSep +
	"#{session_name}" + FieldSep +
	"#{session_windows}" + FieldSep +
	"#{?session_attached,1,0}" + FieldSep +
	"#{session_created}" + FieldSep +
	"#{session_path}"

// sessionFieldCount is how many fields sessionFormat emits.
const sessionFieldCount = 8

// sessionIdentityFormat is the -F spec for a `new-session -P` result: the same
// generation-then-native-id triple as IdentityFormat, with the session id in
// place of the window id. Sharing the shape means both go through
// parseIdentityTriple and cannot drift apart.
const sessionIdentityFormat = "#{pid}" + FieldSep + "#{start_time}" + FieldSep + "#{session_id}"

// ListSessions returns all tmux sessions. When no tmux server is running it
// returns an empty slice (not an error) — "no sessions" is a normal state, not
// a failure.
func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	args := []string{"list-sessions", "-F", sessionFormat}
	out, err := c.run.Run(ctx, c.tmuxBin, args...)
	if err != nil {
		if c.absentDefaultServer(ctx, args, err) {
			return nil, nil
		}
		return nil, err
	}
	return parseSessions(out)
}

// parseSessions turns list-sessions output into Sessions.
//
// EXACT, not >=, for the reason parseWindows is (see the comment there): a
// session NAME may legally carry FieldSep, and under a `len(f) < N` check
// `work<sep>pad` split into a row whose Name read "work" with every later field
// shifted one right — so Path read a window count and Tree rendered a session
// that does not exist. A separator anywhere in a row can only push the count
// above sessionFieldCount, so requiring exactly that many drops the forged row
// instead of misreading it. Since #237 the shifted row is worse than a display
// bug: field 2 is the native session id, and a shifted row would offer a
// well-formed-looking id that names a different session.
func parseSessions(out string) ([]Session, error) {
	lines := splitLines(out)
	sessions := make([]Session, 0, len(lines))
	for _, line := range lines {
		f := splitFields(line)
		if len(f) != sessionFieldCount {
			continue
		}
		sessions = append(sessions, Session{
			ServerPID:   f[0],
			ServerStart: f[1],
			ID:          f[2],
			Name:        f[3],
			Windows:     atoi(f[4]),
			Attached:    f[5] == "1",
			Created:     parseUnix(f[6]),
			Path:        f[7],
		})
	}
	return parsedRows(sessions, lines, "list-sessions", sessionFieldCount)
}

// ResolveSessionExact finds the session whose name matches exactly, by Go
// string equality over a listing — never by handing the name to tmux as a `-t`
// operand, which is what let a prefix sibling answer for a missing session
// (forgectl#237).
//
// Because the comparison is Go's, every tmux-legal name works: spaces,
// punctuation, a literal leading "=", a name that is also a glob. None of them
// mean anything here.
func (c *Client) ResolveSessionExact(ctx context.Context, name string) (SessionIdentity, error) {
	sessions, err := c.ListSessions(ctx)
	if err != nil {
		return SessionIdentity{}, err
	}
	selector := c.currentSelector()
	var found *Session
	for i := range sessions {
		if sessions[i].Name != name {
			continue
		}
		if found != nil {
			// tmux forbids duplicate session names, so two exact matches means
			// the listing is not describing the server we think it is. Refuse
			// rather than pick one.
			return SessionIdentity{}, fmt.Errorf(
				"tmux reported two sessions named %q (%s and %s); refusing to guess which one you meant",
				name, found.ID, sessions[i].ID)
		}
		found = &sessions[i]
	}
	if found == nil {
		return SessionIdentity{}, fmt.Errorf("%w: %q", ErrSessionNotFound, name)
	}
	if err := ValidateSessionID(found.ID); err != nil {
		return SessionIdentity{}, err
	}
	if err := found.Identity(selector).Generation.qualified(); err != nil {
		return SessionIdentity{}, fmt.Errorf("session %q: %w", name, err)
	}
	return found.Identity(selector), nil
}

// CreateSession creates a detached session and returns its identity, captured
// from the create command's own output so the id and the generation that minted
// it come from a single call.
//
// name is a CREATION operand, not a target: `-s` names the new session and tmux
// does not run it through the target grammar. It is therefore passed through
// untouched — no "=" prefix, no trailing colon, nothing a target builder would
// add. Prepending "=" here would create a session literally called "=name".
func (c *Client) CreateSession(ctx context.Context, name, dir string) (SessionIdentity, error) {
	if name == "" {
		return SessionIdentity{}, errors.New("cannot create a tmux session with an empty name")
	}
	args := []string{"new-session", "-d", "-P", "-F", sessionIdentityFormat, "-s", name}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	out, err := c.run.Run(ctx, c.tmuxBin, args...)
	if err != nil {
		return SessionIdentity{}, c.classifyCreateFailure(args, name, err)
	}
	triple, err := parseIdentityTriple(out, "session", ValidateSessionID)
	if err != nil {
		return SessionIdentity{}, fmt.Errorf("read identity of new session %q: %w", name, err)
	}
	return SessionIdentity{
		Generation: ServerGeneration{Selector: c.currentSelector(), PID: triple.PID, StartTime: triple.StartTime},
		ID:         triple.ID,
		Name:       name,
	}, nil
}

// classifyCreateFailure is the ONLY place a create failure's stderr is read.
// Centralizing it is the point: a caller that pattern-matched stderr itself
// could turn an unrelated exit 1 into "already exists" and then attach to
// whatever it found, which is the same wrong-object bug #237 is about, arriving
// through the error path instead of the target path.
//
// Every one of these must hold before the failure counts as a duplicate: the
// argv is exactly the one CreateSession built, tmux exited 1, and stderr is
// tmux's duplicate diagnostic for exactly this name. Equality, not Contains — a
// session name can carry the diagnostic's text.
func (c *Client) classifyCreateFailure(args []string, name string, err error) error {
	var commandErr *internalexec.CommandError
	if !errors.As(err, &commandErr) {
		return err
	}
	if commandErr.Name != c.tmuxBin || commandErr.ExitCode != 1 || !reflect.DeepEqual(commandErr.Args, args) {
		return err
	}
	if strings.TrimRight(commandErr.Stderr, "\n") != "duplicate session: "+name {
		// A localized or otherwise unrecognized diagnostic stays an ordinary
		// creation error. It must never become success.
		return err
	}
	return fmt.Errorf("%w: %q: %w", ErrDuplicateSession, name, err)
}

// EnsureSession resolves a session by exact name, creating it if absent, and
// returns a generation-qualified identity either way.
//
// The sequence is fixed, and the constraint that shapes it is that another
// process can create the same name between the lookup and the create:
//
//  1. list and resolve by exact Go equality; if present, done.
//  2. if absent, create EXACTLY once.
//  3. on the typed duplicate-session failure only, re-list EXACTLY once and
//     require the exact name to be present.
//  4. anything else — an unrecognized create failure, a re-list that fails, a
//     re-list where the name still is not there, a prefix sibling standing in
//     for it — fails closed with no second create and no attach.
func (c *Client) EnsureSession(ctx context.Context, name, dir string) (SessionIdentity, error) {
	identity, err := c.ResolveSessionExact(ctx, name)
	switch {
	case err == nil:
		return identity, nil
	case !errors.Is(err, ErrSessionNotFound):
		return SessionIdentity{}, err
	}

	identity, createErr := c.CreateSession(ctx, name, dir)
	if createErr == nil {
		return identity, nil
	}
	if !errors.Is(createErr, ErrDuplicateSession) {
		return SessionIdentity{}, fmt.Errorf("create tmux session %q: %w", name, createErr)
	}

	// Lost the race. Exactly one re-list, and the winner must carry the exact
	// name — a sibling that merely prefix-matches is not the session anyone
	// asked for.
	identity, err = c.ResolveSessionExact(ctx, name)
	if err != nil {
		// Both causes are wrapped: the duplicate verdict is what says a second
		// create must never be attempted, and the re-list failure is what says
		// why the winner could not be adopted. A caller that saw only one of
		// them could reasonably retry the create.
		return SessionIdentity{}, fmt.Errorf(
			"tmux reported session %q already exists (%w) but it could not be resolved: %w", name, createErr, err)
	}
	return identity, nil
}

// RenameSession renames the session the identity names, revalidating it first.
//
// newName is a rename OPERAND: it is the session's new name, not a target, and
// never enters a target builder. The old name is not passed to tmux at all —
// the `-t` operand is the native id, so a prefix sibling cannot be renamed by
// mistake (forgectl#237 reproduced exactly that with `rename-session -t forge`
// renaming `forge-review`).
func (c *Client) RenameSession(ctx context.Context, want SessionIdentity, newName string) error {
	if newName == "" {
		return errors.New("cannot rename a tmux session to an empty name")
	}
	current, err := c.RevalidateSession(ctx, want)
	if err != nil {
		return fmt.Errorf("rename session %q: %w", want.Name, err)
	}
	_, err = c.run.Run(ctx, c.tmuxBin, "rename-session", "-t", current.ID, newName)
	return err
}

// KillSession kills the session the identity names, revalidating it first.
func (c *Client) KillSession(ctx context.Context, want SessionIdentity) error {
	current, err := c.RevalidateSession(ctx, want)
	if err != nil {
		return fmt.Errorf("kill session %q: %w", want.Name, err)
	}
	_, err = c.run.Run(ctx, c.tmuxBin, "kill-session", "-t", current.ID)
	return err
}

// KillOthers kills every session except the one the identity names.
//
// This is the highest-blast-radius command in the package: it kills everything
// it is not pointed at, so a wrong or stale target does not kill the wrong
// session, it kills all the RIGHT ones. Revalidation immediately before
// dispatch is not belt-and-braces here, it is the control.
func (c *Client) KillOthers(ctx context.Context, keep SessionIdentity) error {
	current, err := c.RevalidateSession(ctx, keep)
	if err != nil {
		return fmt.Errorf("kill other sessions (keeping %q): %w", keep.Name, err)
	}
	_, err = c.run.Run(ctx, c.tmuxBin, "kill-session", "-a", "-t", current.ID)
	return err
}
