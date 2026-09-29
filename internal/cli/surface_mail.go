package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Exit codes for the mail verbs. 0 is sent (or, for the other verbs, done), 1
// is usage, and cobra's argument errors already exit 1.
const (
	// mailExitRefused covers a refusal (policy, an unknown name, no ledger
	// here) and a delivery that failed for good.
	mailExitRefused = 2
	// mailExitQueued means the message is in the mailbox but has not reached
	// the recipient's harness yet; a later flush retries it.
	mailExitQueued = 3
)

// maxBodyInput bounds how much of a file or stdin send reads. The policy cap
// (32 KiB) is what refuses an oversized body; this only stops a mistaken
// `@/dev/zero` from reading forever before the policy can say so.
const maxBodyInput = 1 << 20

// mailSession is one ledger's mail service and who the caller is in it.
type mailSession struct {
	svc    *mail.Service
	ledger string
	getenv func(string) string
}

// self is the caller's roster name: FORGECTL_WORKER, else the coordinator.
func (s *mailSession) self() (string, error) {
	return mail.SelfName(s.svc.Roster, s.getenv)
}

// openMailSession finds the caller's ledger and wires the adapters. It is a
// variable so tests can hand the verbs a service over a temp ledger and fake
// adapters.
var openMailSession = defaultMailSession

func defaultMailSession(deps module.Deps, getenv func(string) string) (*mailSession, error) {
	base, err := config.LaunchUsageBase()
	if err != nil {
		return nil, err
	}
	ledger, err := mail.LedgerDir(filepath.Join(base, "forgectl"), getenv)
	if err != nil {
		return nil, err
	}
	claudeDir, err := mail.ClaudeConfigDir(getenv)
	if err != nil {
		return nil, err
	}
	// No pane adapter yet: the pane driver is the herdr worker verbs from
	// #536, so a pane worker's message fails with "no adapter" until then.
	adapters := map[mail.Harness]mail.Adapter{
		mail.HarnessClaude: mail.ClaudeAdapter{ConfigDir: claudeDir},
		mail.HarnessCodex:  mail.CodexAdapter{Runner: deps.Runner},
		mail.HarnessPi:     mail.PiAdapter{},
	}
	return &mailSession{
		svc: &mail.Service{
			Box:      mail.Mailbox{Dir: ledger},
			Roster:   mail.FileRoster{Dir: ledger},
			Adapters: adapters,
			Policy:   mail.DefaultPolicy(),
		},
		ledger: ledger,
		getenv: getenv,
	}, nil
}

// refused wraps err for exit 2, terminal-safe: mail errors quote names and
// details that came from other agents.
func refused(err error) error {
	return WithExitCode(termsafe.Error(err), mailExitRefused)
}

func newSurfaceSendCmd(deps module.Deps) *cobra.Command {
	var (
		priority string
		watch    bool
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "send <to> <text|@file|->",
		Short: "Send a message to the coordinator or a worker, whatever harness it runs",
		Long: `send hands a message to another agent in this coordinator's ledger:
a Claude Code worker through its inbox socket, a Codex worker through
codex queue, a pi worker through the forgectl extension.

The text is the second argument, @path to read a file, or - for stdin.
The sender is $FORGECTL_WORKER in a worker forgectl launched, else the
coordinator. Workers message the coordinator and the coordinator messages
any worker.

The recipient sees a header saying the text came from another agent, not
the operator, so it can never approve anything.

send first retries anything already queued. A message the recipient's
harness cannot take yet stays queued and later verbs retry it.

Exit codes: 0 sent, 3 queued (not delivered yet), 2 refused or failed,
1 usage.`,
		Example: `  forgectl surface send coordinator "tests pass on the branch"
  forgectl surface send pi-2 @notes.md --priority later
  git diff | forgectl surface send codex-1 - --watch`,
		Args: cobra.ExactArgs(2),
		// A refusal or a queued exit is an outcome, not a usage mistake, and
		// --json must stay the only thing on stdout.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			pri, err := mail.ParsePriority(priority)
			if err != nil {
				return err
			}
			return runSurfaceSend(cmd, deps, os.Getenv, args[0], args[1], pri, watch, asJSON)
		},
	}
	cmd.Flags().StringVar(&priority, "priority", "next", "where it lands in the recipient's queue: now, next or later")
	cmd.Flags().BoolVar(&watch, "watch", false, "also get one notice from forgectl when the recipient next goes idle")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	return cmd
}

// sendResult is send's --json shape.
type sendResult struct {
	ID       string `json:"id"`
	To       string `json:"to"`
	Status   string `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Watching bool   `json:"watching,omitempty"`
}

func runSurfaceSend(cmd *cobra.Command, deps module.Deps, getenv func(string) string, to, source string, pri mail.Priority, watch, asJSON bool) error {
	body, err := readMailBody(source, cmd.InOrStdin())
	if err != nil {
		return refused(err)
	}
	sess, err := openMailSession(deps, getenv)
	if err != nil {
		return refused(err)
	}
	self, err := sess.self()
	if err != nil {
		return refused(err)
	}
	ctx := cmd.Context()
	if _, err := sess.svc.Flush(ctx); err != nil {
		return refused(fmt.Errorf("flush the queue first: %w", err))
	}
	res, err := sess.svc.Send(ctx, self, to, body, pri)
	if err != nil {
		return refused(err)
	}
	out := sendResult{ID: res.ID, To: to, Status: string(res.Status), Detail: res.Detail}
	var watchErr error
	if watch && res.Status != mail.StatusFailed {
		if watchErr = sess.svc.Watch(to, self); watchErr == nil {
			out.Watching = true
		}
	}

	if asJSON {
		if err := writeJSON(cmd.OutOrStdout(), out); err != nil {
			return err
		}
	} else {
		verb := map[mail.Status]string{mail.StatusSent: "sent", mail.StatusQueued: "queued", mail.StatusFailed: "failed"}[res.Status]
		line := fmt.Sprintf("%s %s to %s", verb, res.ID, to)
		if res.Detail != "" {
			line += ": " + res.Detail
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), termsafe.SafeLine(line)); err != nil {
			return err
		}
	}

	switch {
	case res.Status == mail.StatusFailed:
		return WithExitCode(fmt.Errorf("message %s to %s failed", res.ID, termsafe.SafeLine(to)), mailExitRefused)
	case watchErr != nil:
		return refused(fmt.Errorf("message %s is %s, but --watch did not register: %w", res.ID, res.Status, watchErr))
	case res.Status == mail.StatusQueued:
		return newSilentCodedError(mailExitQueued)
	}
	return nil
}

// readMailBody resolves send's second argument: literal text, @path, or - for
// stdin. Reads stop at maxBodyInput; the policy cap does the refusing.
func readMailBody(source string, stdin io.Reader) (string, error) {
	var r io.Reader
	switch {
	case source == "-":
		r = stdin
	case strings.HasPrefix(source, "@") && len(source) > 1:
		f, err := os.Open(source[1:])
		if err != nil {
			return "", err
		}
		defer func() { _ = f.Close() }()
		r = f
	default:
		return source, nil
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBodyInput+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxBodyInput {
		return "", fmt.Errorf("message body is over %d bytes", maxBodyInput)
	}
	return string(data), nil
}

func newSurfaceInboxCmd(deps module.Deps) *cobra.Command {
	var (
		all    bool
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "inbox [name]",
		Short: "Show messages to and from an agent, and where each one is",
		Long: `inbox lists the messages in this coordinator's ledger that a name sent
or received, oldest first, with each one's status: queued, sent, failed or
expired. The name defaults to the caller; --all lists every message.

It only reads. Run flush, or any send, to retry what is queued.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			if all && name != "" {
				return errors.New("--all lists every message; drop the name or the flag")
			}
			return runSurfaceInbox(cmd, deps, os.Getenv, name, all, asJSON)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "list every message in the ledger")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the messages as JSON")
	return cmd
}

// inboxMessage is one message in inbox's --json shape.
type inboxMessage struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Priority  string    `json:"priority"`
	Status    string    `json:"status"`
	Detail    string    `json:"detail,omitempty"`
	Attempts  int       `json:"attempts"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Body      string    `json:"body"`
}

// inboxReport is inbox's --json shape. Ledger and Name make the inferred
// context printable (ADR-0008 rule 4): which ledger the environment selected
// and whose messages these are.
type inboxReport struct {
	Ledger   string         `json:"ledger"`
	Name     string         `json:"name,omitempty"`
	Messages []inboxMessage `json:"messages"`
}

func runSurfaceInbox(cmd *cobra.Command, deps module.Deps, getenv func(string) string, name string, all, asJSON bool) error {
	sess, err := openMailSession(deps, getenv)
	if err != nil {
		return refused(err)
	}
	if !all && name == "" {
		if name, err = sess.self(); err != nil {
			return refused(err)
		}
	}
	if name != "" {
		if err := mail.ValidateName(name); err != nil {
			return refused(err)
		}
	}
	entries, err := sess.svc.Messages(func(e mail.Entry) bool {
		return all || e.Msg.From == name || e.Msg.To == name
	})
	if err != nil {
		return refused(err)
	}

	report := inboxReport{Ledger: sess.ledger, Name: name, Messages: make([]inboxMessage, 0, len(entries))}
	for _, e := range entries {
		report.Messages = append(report.Messages, inboxMessage{
			ID: e.Msg.ID, From: e.Msg.From, To: e.Msg.To, Priority: string(e.Msg.Priority),
			Status: string(e.Status), Detail: e.Detail, Attempts: e.Attempts,
			CreatedAt: e.Msg.CreatedAt, UpdatedAt: e.UpdatedAt, Body: e.Msg.Body,
		})
	}
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), report)
	}
	out := cmd.OutOrStdout()
	if len(report.Messages) == 0 {
		_, err := fmt.Fprintln(out, "no messages")
		return err
	}
	for _, m := range report.Messages {
		first, _, _ := strings.Cut(strings.TrimSpace(m.Body), "\n")
		line := fmt.Sprintf("%s  %-7s  %s -> %s  %s", m.CreatedAt.Local().Format("15:04:05"), m.Status, m.From, m.To, first)
		if m.Detail != "" && m.Status != string(mail.StatusSent) {
			line += "  (" + m.Detail + ")"
		}
		if _, err := fmt.Fprintln(out, termsafe.SafeLineMax(line, 200)); err != nil {
			return err
		}
	}
	return nil
}

func newSurfaceFlushCmd(deps module.Deps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "flush",
		Short: "Retry queued messages that are due and expire the stale ones",
		Long: `flush retries every queued message whose backoff has passed, fails
one that reached its attempt cap, and expires one older than the TTL,
sending its sender a notice. There is no daemon: send, event and flush
are what move the queue.

Exit 0 means the flush ran. A message still queued afterwards is counted,
not an error.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSurfaceFlush(cmd, deps, os.Getenv, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the counts as JSON")
	return cmd
}

// flushReport is flush's --json shape.
type flushReport struct {
	Sent     int `json:"sent"`
	Requeued int `json:"requeued"`
	Failed   int `json:"failed"`
	Expired  int `json:"expired"`
	// Skipped counts mailbox lines that could not be read.
	Skipped int `json:"skipped"`
}

func runSurfaceFlush(cmd *cobra.Command, deps module.Deps, getenv func(string) string, asJSON bool) error {
	sess, err := openMailSession(deps, getenv)
	if err != nil {
		return refused(err)
	}
	rep, err := sess.svc.Flush(cmd.Context())
	if err != nil {
		return refused(err)
	}
	out := flushReport{Sent: rep.Sent, Requeued: rep.Requeued, Failed: rep.Failed, Expired: rep.Expired, Skipped: rep.Skipped}
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), out)
	}
	line := fmt.Sprintf("sent %d, still queued %d, failed %d, expired %d", out.Sent, out.Requeued, out.Failed, out.Expired)
	if out.Skipped > 0 {
		line += fmt.Sprintf(", %d unreadable mailbox lines skipped", out.Skipped)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), line)
	return err
}

func newSurfaceEventCmd(deps module.Deps) *cobra.Command {
	var (
		harness string
		state   string
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "event --harness claude|codex|pi [payload]",
		Short: "Record a worker's turn boundary (called by harness hooks)",
		Long: `event is what each harness's own turn hook runs: Claude Code's Stop and
UserPromptSubmit hooks, Codex's notify program, and the forgectl pi
extension. It records the worker's state, a Codex thread id, sends the
one-shot --watch notices, and flushes the queue when the worker goes idle.

  claude  reads the hook's JSON from stdin (or the payload argument)
  codex   reads the notify JSON codex passes as the last argument
  pi      takes --state idle|busy|waiting

A payload that is not a turn boundary is ignored. event prints nothing
without --json, because Claude Code adds a UserPromptSubmit hook's stdout
to the prompt. It exits 0 or 1, never 2: exit 2 from a Claude Code hook
blocks the stop or erases the prompt.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			payload := ""
			if len(args) == 1 {
				payload = args[0]
			}
			return runSurfaceEvent(cmd, deps, os.Getenv, harness, state, payload, asJSON)
		},
	}
	cmd.Flags().StringVar(&harness, "harness", "", "the harness reporting: claude, codex or pi (required)")
	cmd.Flags().StringVar(&state, "state", "", "the new state, for pi: idle, busy or waiting")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print what was recorded as JSON")
	return cmd
}

// eventResult is event's --json shape. Ignored is set, with the rest empty,
// for a payload that is not a turn boundary.
type eventResult struct {
	Worker   string   `json:"worker,omitempty"`
	State    string   `json:"state,omitempty"`
	ThreadID string   `json:"thread_id,omitempty"`
	Notified []string `json:"notified,omitempty"`
	Ignored  string   `json:"ignored,omitempty"`
}

func runSurfaceEvent(cmd *cobra.Command, deps module.Deps, getenv func(string) string, harness, stateFlag, payload string, asJSON bool) error {
	var (
		st       mail.WorkerState
		threadID string
		parseErr error
	)
	switch mail.Harness(harness) {
	case mail.HarnessClaude, mail.HarnessCodex:
		if stateFlag != "" {
			return fmt.Errorf("--state is for pi; %s reports its state in the payload", harness)
		}
		data := []byte(payload)
		if payload == "" {
			if mail.Harness(harness) == mail.HarnessCodex {
				return errors.New("codex passes its notify JSON as the last argument; none was given")
			}
			var err error
			if data, err = io.ReadAll(io.LimitReader(cmd.InOrStdin(), maxBodyInput)); err != nil {
				return fmt.Errorf("read the hook payload: %w", err)
			}
		}
		if mail.Harness(harness) == mail.HarnessClaude {
			st, parseErr = mail.ParseClaudeHook(data)
		} else {
			st, threadID, parseErr = mail.ParseCodexNotify(data)
		}
	case mail.HarnessPi:
		if payload != "" {
			return errors.New("pi reports with --state, not a payload")
		}
		if stateFlag == "" {
			return errors.New("pi needs --state idle, busy or waiting")
		}
		st, parseErr = mail.ParseState(stateFlag)
	default:
		return fmt.Errorf("--harness %s: want claude, codex or pi", termsafe.QuoteText(harness))
	}
	if parseErr != nil {
		// A hook fires for more than turn boundaries; those are not errors.
		// A payload that is not JSON at all still is.
		if errors.Is(parseErr, mail.ErrNotTurnEvent) {
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), eventResult{Ignored: parseErr.Error()})
			}
			return nil
		}
		return termsafe.Error(parseErr)
	}

	sess, err := openMailSession(deps, getenv)
	if err != nil {
		return termsafe.Error(err)
	}
	self, err := sess.self()
	if err != nil {
		return termsafe.Error(err)
	}
	notified, err := sess.svc.ApplyEvent(cmd.Context(), mail.Event{Worker: self, State: st, ThreadID: threadID})
	if err != nil {
		return termsafe.Error(err)
	}
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), eventResult{Worker: self, State: string(st), ThreadID: threadID, Notified: notified})
	}
	return nil
}
