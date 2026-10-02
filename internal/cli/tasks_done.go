// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/tasks"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// jsonCodeCredentialMissing is the --json failure code for `tasks done` run
// with no write credential in the keychain. It has its own code because the
// fix is not in the caller's hands: an agent that gets it reports the task as
// still open and does not retry.
const jsonCodeCredentialMissing = "credential_missing" //nolint:gosec // G101: a failure code a caller branches on, not a credential

// tasksDoneJSON is the `tasks done --json` success shape. Title rides through
// verbatim, as it does for ls and show: JSON is a machine sink.
type tasksDoneJSON struct {
	ID               int    `json:"id"`
	ProjectID        int    `json:"project_id"`
	Title            string `json:"title"`
	Done             bool   `json:"done"`
	AlreadyDone      bool   `json:"already_done"`
	EvidenceRecorded bool   `json:"evidence_recorded"`
}

// tasksDoneInput is one `tasks done` call as the flags and arguments gave it.
type tasksDoneInput struct {
	id                             string
	evidence, closer, writeService string
	host                           string
	asJSON                         bool
}

func newTasksDoneCmd(deps module.Deps, host *string) *cobra.Command {
	in := tasksDoneInput{}
	cmd := &cobra.Command{
		Use:   "done <id>",
		Short: "Mark one task done and record who closed it and why",
		Long: `done marks one task done and appends a closed-by line to its description:
who closed it, through what, when, and the evidence given.

  forgectl tasks done 42 --evidence "merged owner/repo#12"

<id> is the numeric id that ls and show print, without the #.

--evidence is required: one line, at most 300 characters, saying what you
observed that shows the work is finished. The merged PR or commit, as
owner/repo#N or a URL; for work with no PR, the command run and its result.
It is written to the task, which everyone the project is shared with can
read, so it must not hold a credential.

--closer names who is closing the task. It is self-declared: this command
does not verify it and reads it from nowhere but the flag. Who really made
the change is the account the credential belongs to.

The task is read before it is changed, and read again afterwards. An already
done task is reported as such and nothing is written. A repeating task is
refused. One task per call.

CREDENTIAL

done reads the write entry in the macOS login keychain
(--write-keychain-service, default ` + tasks.DefaultWriteKeychainService + `). It never falls back
to the read entry, and it refuses --keychain-service. Storing that entry is
operator setup:

  security add-generic-password -s ` + tasks.DefaultWriteKeychainService + ` -a "$USER" -w

The command prompts for the token. The token must belong to a bot user that
may update tasks only in the projects it should close in.

CLOSE RECORD

Every call that sends an update writes one JSON line to stderr and appends
the same line to tasks-closes.jsonl in the forgectl config directory: time,
task and project id, closer, evidence, the keychain entry's name, the host,
and the outcome (closed, not_confirmed, write_refused, or unauthorized).
Never the token. The record does not depend on log_level. A call that sends
no update writes no record.

EXIT CODES

  0  the task is done: closed by this call, or already done
  1  anything else, including a task that was not found, a repeating task,
     an update the server refused, an update whose outcome is unknown
     (not_confirmed: read the task before retrying), a missing write
     credential, and a bad argument
  2  the instance could not be reached
  3  the server rejected the credential
  4  the host is not one a keychain credential may be sent to`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			in.id, in.host = args[0], *host
			return runTasksDone(cmd, deps, in)
		},
	}
	cmd.Flags().StringVar(&in.evidence, "evidence", "",
		"what you observed that shows the work is finished: one line, at most 300 characters (required)")
	cmd.Flags().StringVar(&in.closer, "closer", tasks.DefaultDoneCloser,
		"who is closing the task; self-declared, not verified, and written to the task")
	cmd.Flags().StringVar(&in.writeService, "write-keychain-service", tasks.DefaultWriteKeychainService,
		"login keychain service name holding the write token")
	cmd.Flags().BoolVar(&in.asJSON, "json", false, "emit the result as machine-readable JSON")
	return cmd
}

// runTasksDone closes one task.
//
// The order is the control. Everything the caller typed is checked first; the
// host rule and the keychain read come after; the request comes last. A call
// that will be refused for its own arguments never reads the write credential.
func runTasksDone(cmd *cobra.Command, deps module.Deps, in tasksDoneInput) error {
	ctx := cmd.Context()

	id, err := checkTasksDoneArgs(cmd, in)
	if err != nil {
		return jsonFailure(cmd, WithExitCode(err, 1), in.asJSON, jsonCodeUsage)
	}

	token, err := readTasksKeychainToken(ctx, deps.Runner, tasksWriteKeychainFlag, in.writeService, in.host, deps.Cfg.Tasks.AllowedHosts)
	if err != nil {
		if errors.Is(err, tasks.ErrTokenNotFound) {
			// No fallback to the read entry, on purpose: that token was
			// stored for reading, and sending it on an update would be this
			// command deciding its scope for the operator.
			return jsonFailure(cmd, WithExitCode(missingWriteCredentialError(in.writeService), 1), in.asJSON, jsonCodeCredentialMissing)
		}
		return jsonFailure(cmd, tasksExitError(err), in.asJSON, jsonCodeFailed)
	}

	// Never the cache, on any path below: a close acts on what the board says
	// now, and a cached task is neither that nor something a close may edit.
	client, err := newTasksClient(ctx, deps.Runner, in.host, token)
	if err != nil {
		return jsonFailure(cmd, tasksExitError(err), in.asJSON, jsonCodeFailed)
	}

	now := time.Now()
	result, closeErr := client.CompleteTask(ctx, tasks.CloseRequest{
		TaskID:   id,
		Closer:   in.closer,
		Surface:  tasks.SurfaceDone,
		Evidence: in.evidence,
		Now:      now,
	})
	if outcome, sent := tasks.CloseOutcome(result, closeErr); sent {
		writeTasksDoneRecord(cmd.ErrOrStderr(), tasks.CloseRecord{
			Time:       now,
			TaskID:     id,
			ProjectID:  result.ProjectID,
			Surface:    tasks.SurfaceDone,
			Closer:     in.closer,
			Evidence:   in.evidence,
			Credential: in.writeService,
			Host:       in.host,
			Outcome:    outcome,
		})
	}
	if closeErr != nil {
		return jsonFailure(cmd, tasksExitError(closeErr), in.asJSON, tasks.CloseErrorCode(closeErr))
	}

	if in.asJSON {
		enc := termsafe.JSONEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(tasksDoneJSON{
			ID:               result.ID,
			ProjectID:        result.ProjectID,
			Title:            result.Title,
			Done:             result.Confirmed || result.AlreadyDone,
			AlreadyDone:      result.AlreadyDone,
			EvidenceRecorded: result.EvidenceRecorded,
		})
	}
	return printTasksDone(deps.Theme.Writer(cmd.OutOrStdout(), nil), result)
}

// checkTasksDoneArgs checks everything the caller typed, and returns the task
// id. It reads nothing and sends nothing.
func checkTasksDoneArgs(cmd *cobra.Command, in tasksDoneInput) (int, error) {
	if cmd.Flags().Changed("keychain-service") {
		return 0, fmt.Errorf("tasks done: %s names the read credential and is not used here; "+
			"done reads its own entry, named by %s (default %s)",
			tasksKeychainFlag, tasksWriteKeychainFlag, tasks.DefaultWriteKeychainService)
	}
	id, err := strconv.Atoi(in.id)
	// The round trip refuses what Atoi tolerates: a sign and leading zeros.
	if err != nil || id <= 0 || strconv.Itoa(id) != in.id {
		return 0, fmt.Errorf("tasks done: %s is not a task id: give the numeric id, without #, as `forgectl tasks ls` prints it",
			termsafe.QuoteArgMax(in.id, termsafe.ArgEchoMaxRunes))
	}
	if strings.TrimSpace(in.evidence) == "" {
		return 0, errors.New("tasks done: --evidence is required: one line saying what you observed that shows the work is finished, " +
			"such as the merged PR as owner/repo#N")
	}
	// The same check the close makes on its own. Made here as well so that
	// evidence it would refuse is refused before any credential is read.
	if err := tasks.ValidateEvidence(in.evidence); err != nil {
		return 0, fmt.Errorf("tasks done: --evidence: %s", strings.TrimPrefix(err.Error(), "tasks: "))
	}
	if err := tasks.CheckKeychainService(in.writeService); err != nil {
		return 0, fmt.Errorf("tasks done: %s: %s", tasksWriteKeychainFlag, strings.TrimPrefix(err.Error(), "tasks: "))
	}
	return id, nil
}

// writeCredentialSetupCommand is the command an operator runs to store the
// write token. It ends at -w, with no value after it: `security` then prompts,
// and a value there would put the token on a command line.
//
// The entry name is printed only when it is a plain service name. The line is
// meant to be copied into a shell, and any other name is replaced by a
// placeholder so that nothing in it can be read as shell syntax.
func writeCredentialSetupCommand(service string) string {
	name := "<name>"
	if tasks.ValidKeychainService(service) {
		name = service
	}
	return "security add-generic-password -s " + name + ` -a "$USER" -w`
}

// missingWriteCredentialError words the absence of the write entry. The
// command is the last thing in the message so that it can be copied whole.
func missingWriteCredentialError(service string) error {
	return fmt.Errorf("tasks done: the login keychain has no usable entry for the service %s names (%s), so nothing was read or changed. "+
		"done never falls back to the read entry. Storing a write token is operator setup, done once and outside any agent session; "+
		"this command prompts for the token: %s",
		tasksWriteKeychainFlag, termsafe.QuoteArgMax(service, termsafe.ArgEchoMaxRunes), writeCredentialSetupCommand(service))
}

// printTasksDone renders a close that did not fail: one line saying what
// happened to which task, and a second only when there is something the
// caller should not miss.
//
// The title is board text and goes through safeTitle. A changed key is named
// only when the client vetted it into ChangedKeys; any other difference is a
// count, because a JSON key is server text as much as a title is.
func printTasksDone(out io.Writer, result tasks.CloseResult) error {
	if result.AlreadyDone {
		_, err := fmt.Fprintf(out, "task %d in project %d was already done: %s\n"+
			"evidence not recorded: nothing is written to a task that is already done\n",
			result.ID, result.ProjectID, safeTitle(result.Title))
		return err
	}
	if _, err := fmt.Fprintf(out, "closed task %d in project %d: %s\n", result.ID, result.ProjectID, safeTitle(result.Title)); err != nil {
		return err
	}
	var notes []string
	if !result.EvidenceRecorded {
		notes = append(notes, "evidence not recorded: the task reads back done, and its description does not end with this call's closed-by line")
	}
	if len(result.ChangedKeys) > 0 {
		notes = append(notes, "also changed, outside the keys a close is expected to touch: "+safeText(strings.Join(result.ChangedKeys, ", ")))
	}
	if result.UnnamedChanges > 0 {
		notes = append(notes, fmt.Sprintf("%d other key(s) differ and are not named here", result.UnnamedChanges))
	}
	if len(notes) == 0 {
		return nil
	}
	_, err := fmt.Fprintln(out, strings.Join(notes, "; "))
	return err
}

// writeTasksDoneRecord writes the close record for a call that sent an update.
//
// It cannot fail the command. By the time it runs the update has been sent,
// and reporting the call as failed over a record would tell the caller to
// retry a close that may already have happened. A sink that could not be
// written is named on stderr in one plain line instead.
func writeTasksDoneRecord(stderr io.Writer, rec tasks.CloseRecord) {
	if err := tasks.WriteCloseRecord(tasksCloseRecordWriter(stderr), rec); err != nil {
		fmt.Fprintf(stderr, "forgectl: tasks: the close record for task %d was not written everywhere it belongs: %s\n", //nolint:errcheck // stderr is the only place left to say so
			rec.TaskID, safeText(err.Error()))
	}
}

// tasksCloseRecordWriter is where a close record goes when this machine's
// keychain supplied the credential: stderr, and the close log in the config
// directory. Stderr is what the caller sees; the file is what is still there
// after the terminal, or the MCP client that owned the process, is gone.
//
// It is not slog. forgectl's logger discards everything unless log_level is
// set, and a record that exists only when logging is on is not a record.
func tasksCloseRecordWriter(stderr io.Writer) io.Writer {
	return newRecordFanOut(stderr, closeLogWriter{})
}

// recordFanOut writes each record to every sink.
//
// Every sink is attempted whatever the others did, and any failure is
// reported: a record that reached one sink and not the other was not fully
// written, and the caller says so. A sink that takes fewer bytes than it was
// given has failed.
type recordFanOut struct {
	sinks []io.Writer
}

func newRecordFanOut(sinks ...io.Writer) io.Writer {
	return recordFanOut{sinks: sinks}
}

func (f recordFanOut) Write(p []byte) (int, error) {
	var failures []error
	for _, sink := range f.sinks {
		n, err := sink.Write(p)
		if err == nil && n != len(p) {
			err = io.ErrShortWrite
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	if err := errors.Join(failures...); err != nil {
		return 0, err
	}
	return len(p), nil
}

// closeLogWriter appends each write to the close log
// (config.TasksCloseLogPath). The file is opened for one write and closed
// again, so a long-lived MCP server holds no descriptor between closes and the
// path is resolved when a record is written, not when the server started.
//
// One record is one Write of one short line on a descriptor opened O_APPEND,
// so two processes closing tasks at once do not interleave their lines.
type closeLogWriter struct{}

func (closeLogWriter) Write(p []byte) (n int, err error) {
	path, err := config.TasksCloseLogPath()
	if err != nil {
		return 0, fmt.Errorf("the close log path could not be resolved: %w", termsafe.Error(err))
	}
	f, err := config.OpenAppendFile(path)
	if err != nil {
		return 0, fmt.Errorf("the close log could not be opened: %w", err)
	}
	n, err = f.Write(p)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return n, fmt.Errorf("append to the close log %s: %w", safePath(path), termsafe.Error(err))
	}
	return n, nil
}
