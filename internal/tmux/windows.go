package tmux

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// windowFormat is the -F spec for list-windows -a. Fields:
// server pid, server start, native window id, native PARENT session id,
// session name, window index, window name, active(1/0), pane count.
//
// The parent session id is what makes a window action provable: a window keeps
// its @id across `move-window`, so "still exists" is not the same question as
// "still belongs to the session the operator selected".
const windowFormat = IdentityFormat + FieldSep +
	"#{session_id}" + FieldSep +
	"#{session_name}" + FieldSep +
	"#{window_index}" + FieldSep +
	"#{window_name}" + FieldSep +
	"#{?window_active,1,0}" + FieldSep +
	"#{window_panes}"

// paneFormat is the -F spec for list-panes -a. Fields:
// server pid, server start, native pane id, native PARENT window id, pane
// index, title, current command, active(1/0).
const paneFormat = "#{pid}" + FieldSep +
	"#{start_time}" + FieldSep +
	"#{pane_id}" + FieldSep +
	"#{window_id}" + FieldSep +
	"#{pane_index}" + FieldSep +
	"#{pane_title}" + FieldSep +
	"#{pane_current_command}" + FieldSep +
	"#{?pane_active,1,0}"

// windowFieldCount and paneFieldCount are how many fields the two formats
// above emit. Named so the exact-count check and the fail-closed error can
// never report different numbers.
const (
	windowFieldCount = 9
	paneFieldCount   = 8
)

// ListWindows returns every window across all sessions (list-windows -a).
func (c *Client) ListWindows(ctx context.Context) ([]Window, error) {
	windows, _, err := c.listWindows(ctx)
	return windows, err
}

// listWindows is ListWindows plus how many rows it could not read
// (parseWindowRows), for a listing that tells the operator about them.
func (c *Client) listWindows(ctx context.Context) ([]Window, int, error) {
	args := c.tmuxArgs("list-windows", "-a", "-F", windowFormat)
	out, err := c.run.Run(ctx, c.tmuxBin, args...)
	if err != nil {
		if c.absentServer(ctx, args, err) {
			return nil, 0, nil
		}
		return nil, 0, c.serverStateError(ctx, args, err)
	}
	return parseWindowRows(out)
}

func parseWindows(out string) ([]Window, error) {
	windows, _, err := parseWindowRows(out)
	return windows, err
}

// parseWindowRows is parseWindows plus the number of non-empty rows it dropped
// (forgectl#815), for the reason parseSessionRows counts them: a window whose
// name carries FieldSep is real, and a listing that silently omits it reads as
// a smaller server.
func parseWindowRows(out string) ([]Window, int, error) {
	lines := splitLines(out)
	// EXACT, not >=: windowFormat emits exactly windowFieldCount fields, and a
	// window name may legally contain FieldSep
	// (`tmux rename-window $'pr-o-r-1\x1fpad'`). Under a >= check that name
	// splits into a row whose Name reads "pr-o-r-1", so LiveReviews
	// (internal/pr/admission.go) would report a torn-down review as still live
	// in `pr list`. A separator in a name can only ever push the count ABOVE
	// the expected number, so requiring it exactly drops the forged row instead
	// of misreading it. The count is spelled once, in the constant —
	// forgectl#237 raised it from 8 to 9 by adding the parent session id.
	//
	// The window id AND its parent session id, both — a row is only usable as
	// an identity if both halves are well formed (see parseSessions).
	rows, unreadable := readableRows(lines, windowFieldCount, func(f []string) bool {
		return ValidateWindowID(f[2]) == nil && ValidateSessionID(f[3]) == nil
	})
	windows := make([]Window, 0, len(rows))
	for _, f := range rows {
		windows = append(windows, Window{
			ServerPID:   f[0],
			ServerStart: f[1],
			ID:          f[2],
			SessionID:   f[3],
			Session:     f[4],
			Index:       atoi(f[5]),
			Name:        f[6],
			Active:      f[7] == "1",
			Panes:       atoi(f[8]),
		})
	}
	// Every row failing at once with no line keeping the separator is the
	// separator being gone, not eight forged names — see parsedRows for why
	// that must be loud.
	windows, err := parsedRows(windows, lines, "list-windows", windowFieldCount)
	if err != nil {
		return nil, 0, err
	}
	return windows, unreadable, nil
}

// ListPanes returns every pane across all sessions (list-panes -a).
func (c *Client) ListPanes(ctx context.Context) ([]Pane, error) {
	panes, _, err := c.listPanes(ctx)
	return panes, err
}

// listPanes is ListPanes plus the number of pane rows it could not read, for
// DisplayPaneListing.
func (c *Client) listPanes(ctx context.Context) ([]Pane, int, error) {
	args := c.tmuxArgs("list-panes", "-a", "-F", paneFormat)
	out, err := c.run.Run(ctx, c.tmuxBin, args...)
	if err != nil {
		if c.absentServer(ctx, args, err) {
			return nil, 0, nil
		}
		return nil, 0, c.serverStateError(ctx, args, err)
	}
	return parsePaneRows(out)
}

func parsePanes(out string) ([]Pane, error) {
	panes, _, err := parsePaneRows(out)
	return panes, err
}

// parsePaneRows is parsePanes plus the number of non-empty rows it dropped
// (forgectl#823), for the reason parseWindowRows counts them. A pane row is
// easy to hide: #{pane_current_command} is whatever the pane's program calls
// itself, which on Linux is its argv[0], so a program started under
// `exec -a` with FieldSep in that name drops its pane from `tmux tree`. (A
// pane title cannot carry the byte: tmux 3.4 and 3.7c both refuse a title
// that is not printable ASCII or valid UTF-8, from select-pane -T and from
// the title escape sequence alike.)
func parsePaneRows(out string) ([]Pane, int, error) {
	lines := splitLines(out)
	// Exact for the same reason parseWindows is: pane_title and
	// pane_current_command are no more separator-free than a window name.
	rows, unreadable := readableRows(lines, paneFieldCount, func(f []string) bool {
		return ValidatePaneID(f[2]) == nil && ValidateWindowID(f[3]) == nil
	})
	panes := make([]Pane, 0, len(rows))
	for _, f := range rows {
		panes = append(panes, Pane{
			ServerPID:   f[0],
			ServerStart: f[1],
			ID:          f[2],
			WindowID:    f[3],
			Index:       atoi(f[4]),
			Title:       f[5],
			Command:     f[6],
			Active:      f[7] == "1",
		})
	}
	panes, err := parsedRows(panes, lines, "list-panes", paneFieldCount)
	if err != nil {
		return nil, 0, err
	}
	return panes, unreadable, nil
}

// ResolveWindowExact finds the window whose name matches exactly AND whose
// parent is the given session, by Go string equality over a listing.
//
// Both halves are required. Window names are not unique across a server — two
// sessions can each hold a `pr-o-r-1` — so matching on name alone would find a
// window in somebody else's session, and killing it during teardown would take
// down an unrelated review.
//
// Names are not unique INSIDE a session either — tmux accepts a second
// `new-window -n` with a name already in use — so two exact matches refuse with
// ErrAmbiguousWindow, the way ResolveSessionExact refuses duplicate sessions.
// Returning the first would let listing order decide which window a teardown
// kills or an attach selects.
func (c *Client) ResolveWindowExact(ctx context.Context, session SessionIdentity, name string) (WindowIdentity, error) {
	if err := ValidateSessionID(session.ID); err != nil {
		return WindowIdentity{}, err
	}
	windows, err := c.ListWindows(ctx)
	if err != nil {
		return WindowIdentity{}, err
	}
	var found *Window
	for i := range windows {
		if windows[i].SessionID != session.ID || windows[i].Name != name {
			continue
		}
		if found != nil {
			return WindowIdentity{}, fmt.Errorf("%w: %q is held by both %s and %s in session %s; refusing to guess which one",
				ErrAmbiguousWindow, name, found.ID, windows[i].ID, session.ID)
		}
		found = &windows[i]
	}
	if found == nil {
		return WindowIdentity{}, fmt.Errorf("%w: no window named %q in session %s", ErrObjectGone, name, session.ID)
	}
	if err := ValidateWindowID(found.ID); err != nil {
		return WindowIdentity{}, err
	}
	return found.Identity(c.currentSelector()), nil
}

// NewWindow creates a window under the generation-qualified session and
// returns the identity minted by that same command. The session is revalidated
// immediately before creation, and both calls route through tmuxArgs so a
// socket-pinned client cannot accidentally read one server and write another.
func (c *Client) NewWindow(ctx context.Context, session SessionIdentity, name, dir string, command ...string) (WindowIdentity, error) {
	return c.NewWindowWithEnv(ctx, session, name, dir, nil, command...)
}

// ErrBadEnvAssignment reports an environment entry NewWindowWithEnv refuses.
// The key must be a POSIX-shaped variable name, which is what keeps the entry
// from being parsed as a tmux flag: a name cannot begin with `-`, so `-e` can
// never be handed something that reads as an option instead of an operand.
var ErrBadEnvAssignment = errors.New("tmux: environment entry is not a KEY=VALUE assignment with a valid name")

// validateEnvAssignment accepts `KEY=VALUE` where KEY is `[A-Za-z_][A-Za-z0-9_]*`
// and VALUE carries no NUL and no newline. The value bans are about the sink,
// not the shell: a NUL cannot cross exec at all, and a newline in a tmux
// command-line argument is what a crafted value would use to forge a second
// command in output a human or a parser reads back. A trailing ";" is refused
// because tmux ends a command at any argument ending in one, which would split
// the new-window argv. A ";" inside the value is harmless.
func validateEnvAssignment(entry string) error {
	key, value, found := strings.Cut(entry, "=")
	if !found || key == "" {
		return fmt.Errorf("%w", ErrBadEnvAssignment)
	}
	for i, r := range key {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return fmt.Errorf("%w", ErrBadEnvAssignment)
		}
	}
	if strings.ContainsAny(value, "\x00\n\r") || strings.HasSuffix(value, ";") {
		return fmt.Errorf("%w", ErrBadEnvAssignment)
	}
	return nil
}

// NewWindowWithEnv is NewWindow plus per-window environment entries, each a
// `KEY=VALUE` string rendered as one `-e` flag. Without it a new window gets
// the tmux SERVER's environment, which was fixed when the server started and
// has no relationship to the environment forgectl resolved.
//
// DISCLOSURE, stated because it is not obvious: `-e` puts the value on the
// tmux command line, and process command lines are readable by other accounts
// on the machine — not just the same uid. That is acceptable for a proxy URL
// with no credentials in it and NOT acceptable for one that carries a
// password. Callers own that judgment; this function does not inspect values.
// It does keep them out of what forgectl writes down: the log and the error
// show each entry as KEY=[redacted].
//
// There is no removal form: tmux new-window can set a variable and cannot
// unset one, so a caller wanting a variable gone passes it as empty rather
// than omitting it.
func (c *Client) NewWindowWithEnv(
	ctx context.Context, session SessionIdentity, name, dir string, env []string, command ...string,
) (WindowIdentity, error) {
	if name == "" {
		return WindowIdentity{}, fmt.Errorf("cannot create a tmux window with an empty name")
	}
	// A name tmux would store as something else, or reject, could never be
	// resolved by ResolveWindowExact again (forgectl#815): see
	// refuseRewrittenName. A '.' or ':' lands as given on 3.4 and 3.7a+; only
	// tmux 3.7 itself refuses one, loudly, so it is left to tmux.
	if err := refuseRewrittenName(name, true); err != nil {
		return WindowIdentity{}, fmt.Errorf("create window %q: %w", name, err)
	}
	for _, e := range env {
		if err := validateEnvAssignment(e); err != nil {
			return WindowIdentity{}, fmt.Errorf("create window %q: %w", name, err)
		}
	}
	current, err := c.RevalidateSession(ctx, session)
	if err != nil {
		return WindowIdentity{}, fmt.Errorf("create window %q: %w", name, err)
	}
	target, err := NewWindowSessionTarget(current.ID)
	if err != nil {
		return WindowIdentity{}, err
	}
	// escapeFormat: tmux format-expands -n, so the name must be escaped to
	// land as given (forgectl#806).
	args := c.tmuxArgs("new-window", "-P", "-F", IdentityFormat, "-t", target, "-n", escapeFormat(name))
	// escapeArgvSeparator on -c and on every command argument: tmux ends a
	// command at any argv element ending in ';', which would move the window's
	// directory or cut the command short (forgectl#823). The -e values are
	// refused instead, by validateEnvAssignment.
	if dir != "" {
		args = append(args, "-c", escapeArgvSeparator(dir))
	}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	if len(command) != 0 {
		args = append(args, "--")
		for _, arg := range command {
			args = append(args, escapeArgvSeparator(arg))
		}
	}
	// The -e values that could carry a secret reach tmux's argv but never the
	// debug log or the error text: a profile value the config renderer keeps
	// redacted would otherwise land in both whenever tmux fails (#529).
	out, err := c.run.Run(exec.WithMaskedAssignments(ctx, secretBearing(env)), c.tmuxBin, args...)
	if err != nil {
		return WindowIdentity{}, fmt.Errorf("create window %q: %w", name, err)
	}
	triple, err := parseIdentityTriple(out, "window", ValidateWindowID)
	if err != nil {
		return WindowIdentity{}, fmt.Errorf("read identity of new window %q: %w", name, err)
	}
	if !current.Generation.matches(triple.PID, triple.StartTime) {
		return WindowIdentity{}, generationDrift(current.Generation, triple.PID, triple.StartTime)
	}
	return WindowIdentity{
		Generation: current.Generation,
		ID:         triple.ID,
		SessionID:  current.ID,
		Name:       name,
	}, nil
}

// secretBearing keeps the entries whose value could hold a secret: anything
// shaped like a URL or host:port (a ':', '/', '?' or '@'), which is where a
// key or a password rides. Plain constants such as the telemetry switches
// ("1", "true", "grpc") are left out on purpose: masking them also scrubs
// tmux's own error text, turning "can't find window: @1" into "@[redacted]",
// and the window target is what `pr repair` needs.
func secretBearing(env []string) []string {
	var out []string
	for _, e := range env {
		if _, value, ok := strings.Cut(e, "="); ok && strings.ContainsAny(value, ":/?@") {
			out = append(out, e)
		}
	}
	return out
}

// KillWindow kills the window the identity names, revalidating generation and
// parentage first. A window that moved to another session since capture is
// refused rather than killed — its @id would still resolve.
//
// The kill itself re-proves the generation inside the one tmux command that
// performs it (killWindowGuarded), so a server replaced between the
// revalidation and the kill refuses instead of killing its own @N
// (forgectl#756).
//
// A window that dies between the revalidation and the kill is ErrObjectGone,
// but only on tmux's exact answer for that id (windowGoneAtKillStderr) AND a
// re-read showing the same server generation answered it (confirmGoneAtKill);
// any other failure is returned as it came.
func (c *Client) KillWindow(ctx context.Context, want WindowIdentity) error {
	current, err := c.RevalidateWindow(ctx, want)
	if err != nil {
		return fmt.Errorf("kill window %q: %w", want.Name, err)
	}
	return c.killWindowGuarded(ctx, want, current)
}

// generationMismatchMarker opens what a generation-guarded command prints
// when the server that received it is not the captured incarnation: the
// marker, a space, and that server's own "#{pid}/#{start_time}". It is a fixed
// literal the guarded commands never emit themselves (kill-window prints
// nothing), so the output cannot be confused with the command having run.
const generationMismatchMarker = "forgectl-generation-mismatch"

// generationGuarded wraps ONE tmux command so the server receiving it runs the
// command only if that server is the captured incarnation:
//
//	if-shell -F -t <target> '#{==:#{pid}/#{start_time},<PID>/<START>}' \
//	    '<command>' 'display-message -p "<marker> #{pid}/#{start_time}"'
//
// Revalidating and then acting in a second tmux invocation leaves a gap in
// which the socket can come to reach a replaced server, and native ids are
// per-server counters, so the new server's @N is a stranger's window. One
// invocation is one client connection to one server, so the comparison and the
// command cannot straddle a replacement. Measured on tmux 3.4 against an
// isolated socket: a wrong start time printed the marker with the answering
// server's pid and start time, exited 0 and left the window alive; the
// captured generation killed the window and printed nothing; a target id that
// no longer exists printed "can't find window: @N" and exited 1,
// byte-identical to a bare kill-window, which keeps the #746 classification
// (windowGoneAtKillStderr) exact. The else-branch format must stay inside
// double quotes: unquoted, tmux's parser reads the '#' as a comment and the
// whole command fails with "syntax error".
//
// The else-branch reports WHICH server answered rather than a bare marker
// because a tmux that cannot evaluate the comparison would otherwise be
// indistinguishable from a replaced server: an unsupported operator can
// expand to an empty (false) string, and reading that as "a different server
// answered" would turn every kill into a phantom ErrGenerationChanged, which
// teardown reads as gone. killWindowGuarded compares the reported generation
// itself instead.
//
// The pid and start time are interpolated into a format and a command string
// tmux parses again, so both must be plain decimal (what tmux itself renders
// for #{pid} and #{start_time}). Anything else is refused before any command
// runs: WindowIdentity is exported, and a hand-built generation must not be
// able to smuggle format or command syntax into the server.
func generationGuarded(gen ServerGeneration, target, command string) ([]string, error) {
	if !isDecimal(gen.PID) || !isDecimal(gen.StartTime) {
		return nil, fmt.Errorf("%w: server pid %q / start time %q are not decimal",
			ErrUnqualifiedIdentity, gen.PID, gen.StartTime)
	}
	condition := "#{==:#{pid}/#{start_time}," + gen.PID + "/" + gen.StartTime + "}"
	return []string{"if-shell", "-F", "-t", target, condition, command,
		`display-message -p "` + generationMismatchMarker + ` #{pid}/#{start_time}"`}, nil
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// killWindowGuarded runs the generation-guarded kill of an already
// revalidated window and reads its answer through guardedAnswer. The one
// answer it reads first is tmux's exact "can't find window" line, which
// confirmGoneAtKill settles (#746).
//
// The id is validated HERE, not only by the caller's revalidation: it is
// interpolated into a command string tmux parses again, so "nothing
// unvalidated reaches tmux's parser" must be a property of this function, not
// of whoever called it (forgectl#785).
func (c *Client) killWindowGuarded(ctx context.Context, want, current WindowIdentity) error {
	what := fmt.Sprintf("kill window %q", want.Name)
	if err := ValidateWindowID(current.ID); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	guarded, err := generationGuarded(current.Generation, current.ID, "kill-window -t "+current.ID)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	out, err := c.run.Run(ctx, c.tmuxBin, c.tmuxArgs(guarded...)...)
	if windowGoneAtKillStderr(err, current.ID) {
		return c.confirmGoneAtKill(ctx, want, err)
	}
	if err != nil {
		return err
	}
	return guardedAnswer(what, current.Generation, out)
}

// runGuarded runs command through generationGuarded against target and reads
// the answer with guardedAnswer. A tmux failure is wrapped, never
// classified: none of the callers reads one as "gone" or "done", so every
// failure fails closed.
func (c *Client) runGuarded(ctx context.Context, what string, gen ServerGeneration, target, command string) error {
	guarded, err := generationGuarded(gen, target, command)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	out, err := c.run.Run(ctx, c.tmuxBin, c.tmuxArgs(guarded...)...)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return guardedAnswer(what, gen, out)
}

// guardedAnswer reads what a generation-guarded command printed. The guarded
// commands (kill-window, kill-session, rename-session, select-window) print
// nothing when they run, so:
//
//   - nothing: the captured server ran the command;
//   - the marker naming ANOTHER generation: a different server received the
//     command and ran nothing. That is the verdict revalidation gives for the
//     same state (ErrGenerationChanged);
//   - the marker naming the CAPTURED generation: the server is the right one
//     and still took the else-branch, so tmux did not evaluate the guard. The
//     command did not run and nothing about the target is known, so the error
//     is left unclassified and the caller fails closed;
//   - anything else: output this package did not expect, refused rather than
//     read as the command having run.
func guardedAnswer(what string, gen ServerGeneration, out string) error {
	if out == "" {
		return nil
	}
	answered, isMarker := strings.CutPrefix(out, generationMismatchMarker+" ")
	pid, start, isPair := strings.Cut(answered, "/")
	if !isMarker || !isPair || !isDecimal(pid) || !isDecimal(start) {
		return fmt.Errorf("%s: unexpected tmux output %q; refusing to treat the command as run", what, out)
	}
	if gen.matches(pid, start) {
		return fmt.Errorf("%s: tmux did not evaluate the generation guard (the captured server pid %s started %s answered but skipped the command); nothing was done",
			what, pid, start)
	}
	return fmt.Errorf("%s: %w; nothing was done", what, generationDrift(gen, pid, start))
}

// quoteCommandOperand renders s as ONE single-quoted token for tmux's command
// parser, for an operand that must reach a command inside generationGuarded's
// command string byte for byte. Inside single quotes tmux expands nothing — no
// $VAR, no ~, no escapes — and a quote inside s is closed, escaped, and
// reopened, which tmux's lexer concatenates into the same token the way sh
// does:
//
//	it's  ->  'it'\''s'
//
// Measured on tmux 3.4 against an isolated socket (forgectl#785): for every
// single byte 0x20-0xFE, for random multi-byte names over quotes, backslash,
// $, #, {, }, ~, ;, 0x7F and high bytes, and for names such as
// `'; kill-server; '`, `$HOME` and `#{pid}`, a guarded rename-session left
// exactly the name a bare `rename-session -- <name>` left. (One difference
// favours the guard: tmux's argv parser drops a trailing ';' from a bare
// argv element, which the quoted form keeps.)
//
// Two classes of byte are refused rather than quoted, before any command
// runs, with ErrUnsafeOperand:
//
//   - C0 controls (0x00-0x1F) and DEL (0x7F). tmux's lexer is not opaque
//     inside single quotes: a newline followed by blanks collapses them
//     ("a<LF> b" lands as "a<LF>b"), and backslash-newline is a line
//     continuation even inside the quotes ("a\<LF>b" lands as "ab"). A NUL
//     cannot reach an argv at all, and a 0x1F name is already invisible to
//     ListSessions (it is FieldSep), so refusing it costs nothing a listing
//     could show. DEL is refused with them as a control character.
//   - 0xFF. The lexer reads it as end of input inside the quotes, so
//     everything after it is parsed as unquoted command text.
func quoteCommandOperand(s string) (string, error) {
	for i := 0; i < len(s); i++ {
		if b := s[i]; b < 0x20 || b == 0x7f || b == 0xff {
			return "", fmt.Errorf("%w: byte 0x%02X at offset %d cannot be passed through tmux's command parser unchanged",
				ErrUnsafeOperand, b, i)
		}
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'", nil
}

// escapeFormat escapes s so that tmux's format expansion hands back exactly s
// (forgectl#806). tmux 3.4 format-expands a new-session -s name, a new-window
// -n name, and a rename-session name, bare or guarded alike, so an
// operator-typed `#(cmd)` starts a shell job and `#{pid}` lands as a number.
// That is not a privilege boundary for a typed name, but `forgectl open`
// names its session after a directory, and a name that silently lands as
// something else breaks every later exact-name resolve.
//
// The rule is NOT a plain '#' -> '##': tmux keeps a run of '#' that is
// directly followed by '[' verbatim (a style escape), so "#[x" lands as "#[x"
// and "##[x" as "##[x" — doubling those would add bytes. Every other '#' is
// doubled, and "##" expands back to one. Measured on tmux 3.4 against an
// isolated socket: 2,100 random names over "#[]{}(),?=aHS '", each through a
// guarded rename and an argv create, landed byte for byte.
func escapeFormat(s string) string {
	if !strings.Contains(s, "#") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) * 2)
	for i := 0; i < len(s); {
		if s[i] != '#' {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '#' {
			j++
		}
		b.WriteString(s[i:j])
		if j == len(s) || s[j] != '[' {
			b.WriteString(s[i:j])
		}
		i = j
	}
	return b.String()
}

// confirmGoneAtKill settles what kill-window's exact "can't find window"
// answer means. The answer came from whatever server the socket reached at
// that instant, and an id is only unique within one server generation, so the
// server is re-read (#{pid}/#{start_time} on every session row):
//
//   - the captured generation still answers: the window is gone
//     (ErrObjectGone);
//   - another generation answers: the server was replaced between the
//     revalidation and the kill, so the answer is about a different server
//     (ErrGenerationChanged — the old one's windows are gone or unreachable,
//     the same verdict the revalidation itself would give);
//   - no server answers, or the re-read fails: nothing confirms which server
//     spoke, so the kill error is returned unclassified and the caller fails
//     closed. That includes a review window that was the server's last, whose
//     server exited with it — a park, not a guess.
//
// Why the empty arm is not ErrObjectGone like the other two: both of those
// rest on a server that answered the re-read, which is positive evidence
// about who gave the "can't find window" answer. An empty or failed re-read
// is the absence of evidence — the answer may have come from a replacement
// that has since exited too, while the captured server lives on behind an
// unlinked socket. The costs are also unequal: a spurious park is settled by
// `pr repair`, while a wrong "gone" lets teardown remove the workspace of a
// window that may still be running.
func (c *Client) confirmGoneAtKill(ctx context.Context, want WindowIdentity, killErr error) error {
	sessions, err := c.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("kill window %q: %w (re-reading the server to confirm it: %w)", want.Name, killErr, err)
	}
	if len(sessions) == 0 {
		return fmt.Errorf("kill window %q: %w (no server answered the re-read that would confirm it)", want.Name, killErr)
	}
	for _, s := range sessions {
		if !want.Generation.matches(s.ServerPID, s.ServerStart) {
			return fmt.Errorf("kill window %q: %w", want.Name, generationDrift(want.Generation, s.ServerPID, s.ServerStart))
		}
	}
	return fmt.Errorf("%w: window %s (%q) was gone by kill-window: %w", ErrObjectGone, want.ID, want.Name, killErr)
}

// windowGoneAtKillStderr reports whether a kill-window failure is tmux saying
// the target id does not exist: exit status 1 and, as the whole of stderr,
// "can't find window: <id>" for the very id passed (forgectl#746). Measured on
// tmux 3.4 against an isolated socket: a kill of an already-killed @1 printed
// exactly that line and exited 1, while a missing server printed
// "no server running on <socket>". The id is a validated "@N", so the line
// cannot be forged by a window name, and a stderr tail that dropped bytes,
// carries any other text, or names another id is not this answer.
//
// "server exited unexpectedly" is deliberately NOT this answer either
// (forgectl#765). Measured on tmux 3.4: tmux prints it, exit 1, when the
// server dies under a client mid-command (a kill -9), and the socket file
// stays behind; a kill-window that takes the server's last window, which
// makes the server exit on purpose, exits 0 with no message. So the line
// means a crash, not a finished kill, and a crashed server's pane processes
// can outlive it. A re-read after it nearly always meets the stale socket
// (ErrServerUnreadable), so gating it on one would only add a path to a
// "gone" verdict built on a crash. It stays unclassified, and the caller
// parks.
func windowGoneAtKillStderr(err error, id string) bool {
	var cmdErr *exec.CommandError
	if !errors.As(err, &cmdErr) {
		return false
	}
	return cmdErr.ExitCode == 1 && cmdErr.StderrDropped == 0 && cmdErr.Stderr == "can't find window: "+id
}

// treeMarkers are the structural glyphs buildTree uses. Kept here (not in the
// tui glyph set) because the tree is assembled in the ops layer, which must
// not import tui. The icons flag on Tree selects between the two sets.
type treeMarkers struct {
	attached, detached, active string
}

var (
	iconTreeMarkers  = treeMarkers{attached: "●", detached: "○", active: "*"}
	asciiTreeMarkers = treeMarkers{attached: "*", detached: "-", active: "+"}
)

// Tree renders the session → window → pane hierarchy as indented text, ready
// to drop into a viewport. Sessions are sorted by name, windows by index,
// panes by index; the attached session and active window/pane are marked. Pass
// icons=false for ASCII markers (NO_COLOR / --no-icons / misconfigured term).
func (c *Client) Tree(ctx context.Context, icons bool) (string, error) {
	tree, _, err := c.TreeListing(ctx, icons)
	return tree, err
}

// TreeListing is Tree plus how many session, window and pane rows tmux
// returned that could not be read (forgectl#815, forgectl#823), so `tmux tree` and the TUI can say
// the tree is missing them rather than draw a smaller server.
func (c *Client) TreeListing(ctx context.Context, icons bool) (string, UnreadableRows, error) {
	sessions, unreadableSessions, err := c.DisplaySessionListing(ctx)
	if err != nil {
		return "", UnreadableRows{}, err
	}
	windows, unreadableWindows, err := c.DisplayWindowListing(ctx)
	if err != nil {
		return "", UnreadableRows{}, err
	}
	panes, unreadablePanes, err := c.DisplayPaneListing(ctx)
	if err != nil {
		return "", UnreadableRows{}, err
	}
	m := iconTreeMarkers
	if !icons {
		m = asciiTreeMarkers
	}
	return buildTree(sessions, windows, panes, m),
		UnreadableRows{Sessions: unreadableSessions, Windows: unreadableWindows, Panes: unreadablePanes}, nil
}

// buildTree is the pure assembly step — no exec, no I/O — so it's directly
// testable from a fixture.
//
// Every tmux-derived string it writes goes through termsafe.SafeLine, because
// this is a terminal-output boundary over text forgectl did not compose: a
// session, window, or pane name is chosen by whoever created the object, which
// is any same-uid process, and an ANSI escape or bidi override in one would
// repaint or reorder the rendered tree. SafeLine is a no-op on ordinary names,
// so the everyday tree is byte-identical. TestBuildTreeEmitsNoUnsafeRunes
// asserts over the ASSEMBLED string rather than on any single call, so a field
// added here later is covered without extending the test.
// Grouping is by native id, not by name-and-index. The old composite key
// ("session name" + "window index") re-derived parentage from two mutable
// values, so a session renamed or a window moved between the three listings
// silently filed a window under the wrong session — the same class of mistake
// as targeting by name, showing up in the display layer.
func buildTree(sessions []Session, windows []Window, panes []Pane, m treeMarkers) string {
	winBySession := map[string][]Window{}
	for _, w := range windows {
		winBySession[w.SessionID] = append(winBySession[w.SessionID], w)
	}
	panesByWindow := map[string][]Pane{}
	for _, p := range panes {
		panesByWindow[p.WindowID] = append(panesByWindow[p.WindowID], p)
	}

	sorted := make([]Session, len(sessions))
	copy(sorted, sessions)
	// Native id breaks the tie: tmux forbids duplicate session names, but a
	// listing can still show two rows with one name mid-rename, and an
	// unspecified order there would render differently run to run.
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Name != sorted[j].Name {
			return sorted[i].Name < sorted[j].Name
		}
		return sorted[i].ID < sorted[j].ID
	})

	var b strings.Builder
	for _, s := range sorted {
		marker := m.detached
		if s.Attached {
			marker = m.attached
		}
		fmt.Fprintf(&b, "%s %s\n", marker, termsafe.SafeLine(s.Name))

		ws := winBySession[s.ID]
		sort.Slice(ws, func(i, j int) bool { return ws[i].Index < ws[j].Index })
		for _, w := range ws {
			active := ""
			if w.Active {
				active = " " + m.active
			}
			unit := "panes"
			if w.Panes == 1 {
				unit = "pane"
			}
			fmt.Fprintf(&b, "  %d: %s%s (%d %s)\n", w.Index, termsafe.SafeLine(w.Name), active, w.Panes, unit)

			ps := panesByWindow[w.ID]
			sort.Slice(ps, func(i, j int) bool { return ps[i].Index < ps[j].Index })
			for _, p := range ps {
				active := ""
				if p.Active {
					active = " " + m.active
				}
				cmd := p.Command
				if cmd == "" {
					cmd = p.Title
				}
				fmt.Fprintf(&b, "    %d: %s%s\n", p.Index, termsafe.SafeLine(cmd), active)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
