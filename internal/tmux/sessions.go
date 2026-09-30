package tmux

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

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
	sessions, _, err := c.listSessions(ctx)
	return sessions, err
}

// listSessions is ListSessions plus how many rows it could not read
// (parseSessionRows), for a listing that tells the operator about them.
func (c *Client) listSessions(ctx context.Context) ([]Session, int, error) {
	args := c.tmuxArgs("list-sessions", "-F", sessionFormat)
	out, err := c.run.Run(ctx, c.tmuxBin, args...)
	if err != nil {
		if c.absentServer(ctx, args, err) {
			return nil, 0, nil
		}
		return nil, 0, c.serverStateError(ctx, args, err)
	}
	return parseSessionRows(out)
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
	sessions, _, err := parseSessionRows(out)
	return sessions, err
}

// parseSessionRows is parseSessions plus the number of non-empty rows it
// dropped (forgectl#806). A dropped row is a real session nobody can see —
// its name carries FieldSep, most likely — so a listing shown to an operator
// reports the count rather than silently showing fewer sessions. The drop
// itself stays: see parsedRows for why partial loss must not fail the list.
func parseSessionRows(out string) ([]Session, int, error) {
	lines := splitLines(out)
	// Drop a row whose native id is not well formed, alongside the field-count
	// check and for the same reason: a row that reaches a caller carries an id
	// that will be handed to `-t`, and a shifted or forged row can offer a
	// plausible-looking value naming something else. Validating here means no
	// unvalidated id ever leaves a parser.
	rows, unreadable := readableRows(lines, sessionFieldCount, func(f []string) bool {
		return ValidateSessionID(f[2]) == nil
	})
	sessions := make([]Session, 0, len(rows))
	for _, f := range rows {
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
	sessions, err := parsedRows(sessions, lines, "list-sessions", sessionFieldCount)
	if err != nil {
		return nil, 0, err
	}
	return sessions, unreadable, nil
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

// sessionNameReplacer is tmux's own session_check_name rewrite: ':' and '.'
// are target-grammar separators, so tmux stores each as '_'.
var sessionNameReplacer = strings.NewReplacer(":", "_", ".", "_")

// normalizeSessionName returns the name tmux will list for a session created
// as name, or refuses a name tmux would rewrite in a way forgectl does not
// predict (forgectl#815).
//
// Measured on tmux 3.4 against an isolated socket, `new-session -s`:
//
//   - ':' and '.' land as '_' ("my.proj" lists as "my_proj"). This is a fixed
//     rule in tmux's session_check_name, not version- or locale-dependent, so
//     it is PREDICTED: EnsureSession looks up and creates the '_' spelling,
//     and `forgectl open` on a directory such as my.proj finds its session
//     again rather than creating a duplicate every time.
//   - everything refuseRewrittenName lists is REFUSED. Those rewrites are
//     escaping artefacts rather than a naming rule, and predicting one would
//     silently bring the duplicate-create bug back the day tmux changed it.
func normalizeSessionName(name string) (string, error) {
	if err := refuseRewrittenName(name, true, true); err != nil {
		return "", err
	}
	return sessionNameReplacer.Replace(name), nil
}

// refuseRewrittenName refuses a session or window name that tmux 3.4 would
// store as something else, so an exact-name lookup after it could never find
// the object again (forgectl#815). Measured against an isolated socket:
//
//   - '$' followed by an ASCII letter, '_' or '{' gains a backslash ("a$b"
//     lists as `a\$b`), in session and window names alike, on the argv path
//     and inside a guarded command's single quotes. "a$1", "a$}", "a$@" and a
//     trailing '$' land unchanged.
//   - on the argv path (argv), a trailing ';' is read as a command separator
//     and dropped ("x;" lands as "x"). A ';' elsewhere lands as given, and a
//     guarded rename's quotes keep a trailing one.
//   - in a SESSION name (session), tmux vis-encodes what it stores: '\' lists
//     as `\\`, a control byte as `\t` or octal, and invalid UTF-8 as octal.
//
// FieldSep is refused first, with CreateSession's existing wording, because a
// name carrying it can never be listed at all (forgectl#806).
func refuseRewrittenName(name string, session, argv bool) error {
	if strings.Contains(name, FieldSep) {
		return fmt.Errorf("%w: name %q carries the tmux field separator (0x1F)", ErrUnsafeOperand, name)
	}
	for i := 0; i < len(name); i++ {
		b := name[i]
		switch {
		case b == '$' && i+1 < len(name) && startsTmuxVariable(name[i+1]):
			return fmt.Errorf(`%w: name %q: tmux stores "$" followed by a letter, "_" or "{" with a backslash added, so the name could not be found again`,
				ErrUnsafeOperand, name)
		case session && b == '\\':
			return fmt.Errorf(`%w: name %q: tmux stores a backslash in a session name as "\\", so the name could not be found again`,
				ErrUnsafeOperand, name)
		case session && (b < 0x20 || b == 0x7f):
			return fmt.Errorf("%w: name %q: tmux stores control byte 0x%02X at offset %d escaped, so the name could not be found again",
				ErrUnsafeOperand, name, b, i)
		}
	}
	if session && !utf8.ValidString(name) {
		return fmt.Errorf("%w: name %q is not valid UTF-8, which tmux stores escaped, so the name could not be found again",
			ErrUnsafeOperand, name)
	}
	if argv && strings.HasSuffix(name, ";") {
		return fmt.Errorf(`%w: name %q: tmux reads a trailing ";" as a command separator and drops it`, ErrUnsafeOperand, name)
	}
	return nil
}

// startsTmuxVariable reports whether b, following a '$', makes tmux 3.4 escape
// that '$' (see refuseRewrittenName).
func startsTmuxVariable(b byte) bool {
	return b == '_' || b == '{' || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
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
	// A name carrying FieldSep could never be listed again: its row splits
	// into too many fields (forgectl#806), so forgectl could not resolve,
	// rename or kill the session it had just made. The other names tmux would
	// rewrite are refused for the same reason, and ':' and '.' are mapped to
	// the '_' tmux stores (forgectl#815), so the identity returned names the
	// session the way every later listing will.
	name, err := normalizeSessionName(name)
	if err != nil {
		return SessionIdentity{}, fmt.Errorf("create tmux session: %w", err)
	}
	// escapeFormat: tmux format-expands -s, so `#(cmd)` in a name would run
	// a shell job and `#{...}` would be substituted (forgectl#806). The
	// escaped operand expands back to exactly name, which is also what tmux's
	// "duplicate session: <name>" line names, so classifyCreateFailure's exact
	// comparison against name still holds.
	args := c.tmuxArgs("new-session", "-d", "-P", "-F", sessionIdentityFormat, "-s", escapeFormat(name))
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
// argv is exactly the one CreateSession built, tmux exited 1, and stderr equals
// "duplicate session: " + name.
//
// The strength of that last check is worth stating precisely, because the
// tempting justification — "no other tmux message begins with that prefix" — is
// a claim over tmux's entire message catalog and is not provable. The provable
// property is narrower and does more work: the comparison is exact equality
// against a string THIS function constructs from the name it was given, so a
// forgery requires tmux to emit that line byte for byte. Equality, not Contains
// — under Contains, a session named after the diagnostic forges the verdict.
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
	// The lookup must use the name tmux STORES, not the one asked for: tmux
	// lists "my.proj" as "my_proj", so an exact lookup of "my.proj" never
	// matched and every `forgectl open` created another session
	// (forgectl#815). A name tmux would rewrite unpredictably is refused
	// here, before the lookup, for the same reason.
	if name != "" {
		normalized, err := normalizeSessionName(name)
		if err != nil {
			return SessionIdentity{}, fmt.Errorf("create tmux session: %w", err)
		}
		name = normalized
	}
	identity, err := c.ResolveSessionExact(ctx, name)
	switch {
	case err == nil:
		return identity, nil
	case errors.Is(err, ErrServerExited):
		// A server that exited and left its socket behind holds no session,
		// and creating is safe there: tmux's own client finds the refused
		// socket, unlinks it and starts a fresh server (forgectl#786). A
		// server that comes up in between is connected to instead, and the
		// duplicate-session handling below covers the name being taken.
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
//
// The rename runs inside generationGuarded (forgectl#785), so newName reaches
// tmux as a single-quoted token of a command string tmux parses again
// (quoteCommandOperand); a name that cannot be carried through that parser
// unchanged — one with a control character (0x00-0x1F, 0x7F) or a 0xFF byte —
// is refused before any command runs. The name is also format-escaped
// (escapeFormat), because rename-session expands `#{...}` and `#(...)` in it.
//
// The `--` is what keeps newName an operand. It is the only operator-controlled
// POSITIONAL this package hands tmux, and the TUI's rename field (internal/tui)
// is free text — so a name like `-t$9` would otherwise reach tmux's own flag
// parser. Measured on tmux 3.7b it cannot hijack the target (it consumes the
// sole positional and rename-session then fails "too few arguments"), but a
// tmux-legal name failing with a parser diagnostic is still wrong.
func (c *Client) RenameSession(ctx context.Context, want SessionIdentity, newName string) error {
	if newName == "" {
		return errors.New("cannot rename a tmux session to an empty name")
	}
	// Validated as typed, so a refusal's byte offset is the operator's; then
	// escaped and quoted. rename-session format-expands its new name even
	// inside the guard's quotes (forgectl#806), and the quoting carries the
	// escaped bytes through the command parser unchanged. Escaping only adds
	// '#', which quoteCommandOperand never refuses.
	if _, err := quoteCommandOperand(newName); err != nil {
		return fmt.Errorf("rename session %q: %w", want.Name, err)
	}
	// A typed name tmux would store as something else is refused rather than
	// rewritten (forgectl#815): the rename would report "renamed to my.proj"
	// while tmux lists "my_proj", and nothing could find it by the name the
	// operator typed. The quotes keep a trailing ';', so only the argv rule is
	// off here.
	if err := refuseRewrittenName(newName, true, false); err != nil {
		return fmt.Errorf("rename session %q: %w", want.Name, err)
	}
	if stored := sessionNameReplacer.Replace(newName); stored != newName {
		return fmt.Errorf(`rename session %q: %w: tmux stores ":" and "." in a session name as "_", so %q would land as %q; use that name instead`,
			want.Name, ErrUnsafeOperand, newName, stored)
	}
	return c.renameSessionGuarded(ctx, want, newName)
}

// renameSessionGuarded is RenameSession after its name checks: it escapes and
// quotes newName, revalidates, and runs the guarded rename. It is split out so
// the real-tmux test of quoteCommandOperand can still drive names the checks
// refuse (a backslash, a '$'), which are exactly the ones that exercise the
// quoting.
func (c *Client) renameSessionGuarded(ctx context.Context, want SessionIdentity, newName string) error {
	quotedName, err := quoteCommandOperand(escapeFormat(newName))
	if err != nil {
		return fmt.Errorf("rename session %q: %w", want.Name, err)
	}
	current, err := c.RevalidateSession(ctx, want)
	if err != nil {
		return fmt.Errorf("rename session %q: %w", want.Name, err)
	}
	return c.sessionGuarded(ctx, fmt.Sprintf("rename session %q", want.Name), current,
		"rename-session -t "+quoteSessionID(current.ID)+" -- "+quotedName)
}

// KillSession kills the session the identity names, revalidating it first.
// The kill re-proves the server generation inside the one tmux command that
// performs it (forgectl#785), so a server replaced after the revalidation
// refuses rather than killing its own $N.
func (c *Client) KillSession(ctx context.Context, want SessionIdentity) error {
	current, err := c.RevalidateSession(ctx, want)
	if err != nil {
		return fmt.Errorf("kill session %q: %w", want.Name, err)
	}
	return c.sessionGuarded(ctx, fmt.Sprintf("kill session %q", want.Name), current,
		"kill-session -t "+quoteSessionID(current.ID))
}

// sessionGuarded runs command, which acts on the revalidated session current,
// through runGuarded. The id is validated here as well as by the
// revalidation, because it is interpolated into a command string tmux parses
// again (forgectl#785).
func (c *Client) sessionGuarded(ctx context.Context, what string, current SessionIdentity, command string) error {
	if err := ValidateSessionID(current.ID); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return c.runGuarded(ctx, what, current.Generation, current.ID, command)
}

// quoteSessionID single-quotes a validated "$N" for a guarded command string.
// Unquoted, "$1" survives tmux 3.4's parser (a variable name cannot start with
// a digit), but that is a fact about one parser's variable grammar; inside
// single quotes tmux expands nothing, so the id reaches the command as
// written whatever the environment holds.
func quoteSessionID(id string) string {
	return "'" + id + "'"
}

// KillOthers kills every session except the one the identity names.
//
// This is the highest-blast-radius command in the package: it kills everything
// it is not pointed at, so a wrong or stale target does not kill the wrong
// session, it kills all the RIGHT ones. Revalidation immediately before
// dispatch is not belt-and-braces here, it is the control.
//
// ValidateSessionID is load-bearing on this line specifically, and not as
// hygiene. Measured on tmux 3.7b: `kill-session -a -t ”` exits 0 and kills
// every session except the current one. An empty operand is not a no-op and not
// an error — it silently succeeds at maximum blast radius, which is the one
// failure mode this function must not have.
//
// Three independent layers keep current.ID non-empty: preflight validates
// before any command runs, parseSessions drops rows failing
// ValidateSessionID, so a listing cannot produce an empty id to revalidate
// against, and sessionGuarded validates the id it interpolates. Relaxing any
// one "because ids are always well-formed" re-arms the sentence above.
//
// The kill runs inside generationGuarded (forgectl#785): a server replaced
// between the revalidation and the kill would otherwise read "$N" as one of
// ITS sessions and kill every other session it holds.
func (c *Client) KillOthers(ctx context.Context, keep SessionIdentity) error {
	current, err := c.RevalidateSession(ctx, keep)
	if err != nil {
		return fmt.Errorf("kill other sessions (keeping %q): %w", keep.Name, err)
	}
	return c.sessionGuarded(ctx, fmt.Sprintf("kill other sessions (keeping %q)", keep.Name), current,
		"kill-session -a -t "+quoteSessionID(current.ID))
}
