// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/tasks"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Exit codes `forgectl tasks` opts into via WithExitCode. Distinct from each
// other and from the default 1 so a caller can tell "the box is down" (2,
// matching the reference probe script's UNREACHABLE) from "the credential is
// bad" (3, matching its UNAUTHENTICATED/FORBIDDEN) without parsing text.
//
// exitTasksHostRefused (4) is the one code with no counterpart in that probe
// script, which folds a pinning refusal into its generic precondition exit.
// It is broken out here because a refusal to send the credential is a
// security verdict, and collapsing it into the default 1 makes it
// indistinguishable from a malformed task id — so a script cannot alert on
// the case that most warrants alerting.
const (
	exitTasksUnreachable  = 2
	exitTasksUnauthorized = 3
	exitTasksHostRefused  = 4
)

// tasksModule declares the tasks extension (ADR-0005): a credentialed Vikunja
// client, a local cache, three read verbs (ls/show/ready), one write verb
// (done), and an MCP server over the same client (mcp). It owns the [tasks]
// config section, which holds one thing: the hosts, besides the default, that
// a keychain credential may be sent to. That is in the config file and not a
// flag because a flag is supplied by whoever runs the command, and the list
// is the operator's. Host, keychain service names, and the mcp transport
// stay flags.
var tasksModule = module.Manifest{
	Name:      "tasks",
	Tier:      module.TierExtension,
	ConfigKey: "tasks",
	New:       newTasksCmd,
}

func newTasksCmd(deps module.Deps) *cobra.Command {
	var host, keychainService string

	cmd := &cobra.Command{
		Use:   "tasks",
		Short: "Browse a Vikunja task board, close a task, or serve the board as an MCP server",
		Long: `tasks is a client for a Vikunja instance: a local cache of what it returns,
three read verbs over that data, one verb that closes a task, and an MCP
server over the same client.

  forgectl tasks ls             list open tasks
  forgectl tasks show <id>      one task, its detail and its relations
  forgectl tasks ready          open tasks with no active "blocked" relation,
                                 ranked by Vikunja's own position field
  forgectl tasks done <id>      mark one task done and record who closed it
                                 and why (--evidence is required)
  forgectl tasks mcp            serve the board to an MCP client (stdio, or
                                 streamable HTTP with --http)

ls, show and ready issue GETs only. ` + "`done`" + ` reads one task and sends one
update to it. ` + "`mcp`" + ` exposes four read tools and three write tools:
create_task, add_comment and complete_task. What any of them can actually do
is decided by the credential's own grant, not by this binary.

TWO CREDENTIALS

  --keychain-service        the entry ls, show, ready and ` + "`mcp`" + ` over stdio
                            read (default ` + tasks.DefaultKeychainService + `)
  --write-keychain-service  the entry ` + "`done`" + ` reads (default ` + tasks.DefaultWriteKeychainService + `)

` + "`done`" + ` reads only its own entry. It never falls back to the read entry, and
it refuses --keychain-service, so a token stored for reading is not sent on a
write. A service name is 1 to 64 letters, digits, '.', '_' or '-'.

The bearer token is read fresh on every run — from the macOS login keychain
for every verb here, EXCEPT ` + "`mcp --http`" + `, which has no keychain to read and
takes ` + "`--token-file`" + ` instead. There is no environment-variable source on
either path. The token never touches argv, a log line, an error string, the
local cache, or a close record.

WHERE A KEYCHAIN CREDENTIAL MAY GO

A keychain credential is sent only to ` + tasks.DefaultHost + `, or to a host listed
in the forgectl config file:

  [tasks]
  allowed_hosts = ["<hostname>"]

Any other --host is refused with exit 4, before the keychain is read. An entry
is a plain hostname: no port, user, path, or IP address.

A revoked token fails loudly; it never falls back to stale cache data. On the
read verbs a network failure MAY fall back to cache, and states the cache's
age when it does. ` + "`done`" + ` never reads or writes the cache.`,
		// The validator and the help runner are the root's own. Without them a
		// parent with subcommands answers an unknown verb with its help and
		// exit 0, which a script reads as success.
		Args:          safeRootArgs,
		RunE:          showRootHelp,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// The root's own setting; it is per command and not inherited. At cobra's
	// default of zero a mistyped verb is matched by prefix only, so `don`
	// names `done` and `dne` names nothing.
	cmd.SuggestionsMinimumDistance = 2
	cmd.PersistentFlags().StringVar(&host, "host", tasks.DefaultHost,
		"Vikunja API host; a keychain credential goes only to the default or a host under [tasks] allowed_hosts")
	cmd.PersistentFlags().StringVar(&keychainService, "keychain-service", tasks.DefaultKeychainService,
		"login keychain service name holding the read token (ls, show, ready, mcp over stdio)")

	cmd.AddCommand(
		newTasksLsCmd(deps, &host, &keychainService),
		newTasksShowCmd(deps, &host, &keychainService),
		newTasksReadyCmd(deps, &host, &keychainService),
		newTasksDoneCmd(deps, &host),
		newTasksMCPCmd(deps, &host, &keychainService),
	)
	return cmd
}

// tasksLsJSON and tasksShowJSON are the `--json` wire shapes. Title and
// Description ride through verbatim in JSON mode — JSON is a machine sink,
// not a terminal, so termsafe sanitization (needed only to protect a TTY
// from a planted control sequence) does not apply here; a consumer that
// prints these fields to its own terminal owns that sanitization itself.
type tasksLsJSON struct {
	ID       int     `json:"id"`
	Title    string  `json:"title"`
	Done     bool    `json:"done"`
	Position float64 `json:"position"`
}

func snapshotToLsJSON(snap tasks.Snapshot, includeDone bool) []tasksLsJSON {
	rows := make([]tasksLsJSON, 0, len(snap.Tasks))
	for _, t := range snap.Tasks {
		if !includeDone && t.Done {
			continue
		}
		rows = append(rows, tasksLsJSON{ID: t.ID, Title: t.Title, Done: t.Done, Position: t.Position})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Position < rows[j].Position })
	return rows
}

func newTasksLsCmd(deps module.Deps, host, keychainService *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:           "ls",
		Short:         "List open tasks",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			snap, fromCache, err := loadTasksSnapshot(cmd, deps.Runner, *host, *keychainService, deps.Cfg.Tasks.AllowedHosts)
			if err != nil {
				return tasksExitError(err)
			}
			reportCacheFallback(cmd.ErrOrStderr(), *host, fromCache, snap)
			if asJSON {
				rows := snapshotToLsJSON(snap, false)
				if rows == nil {
					rows = []tasksLsJSON{}
				}
				enc := termsafe.JSONEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			return printTasksList(deps.Theme.Writer(cmd.OutOrStdout(), nil), snap, false)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable list to stdout")
	return cmd
}

func printTasksList(out io.Writer, snap tasks.Snapshot, includeDone bool) error {
	rows := snapshotToLsJSON(snap, includeDone)
	if len(rows) == 0 {
		_, err := fmt.Fprintln(out, "no open tasks")
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(out, "%6d  %s\n", r.ID, safeTitle(r.Title)); err != nil {
			return err
		}
	}
	return nil
}

func newTasksShowCmd(deps module.Deps, host, keychainService *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:           "show <id>",
		Short:         "Show one task, its detail and its relations",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.Atoi(args[0])
			if err != nil {
				return WithExitCode(fmt.Errorf("tasks show: %q is not a task id", args[0]), 1)
			}
			snap, fromCache, err := loadTasksSnapshot(cmd, deps.Runner, *host, *keychainService, deps.Cfg.Tasks.AllowedHosts)
			if err != nil {
				return tasksExitError(err)
			}
			reportCacheFallback(cmd.ErrOrStderr(), *host, fromCache, snap)

			task, ok := findTask(snap.Tasks, id)
			if !ok {
				return WithExitCode(fmt.Errorf("tasks show: no task with id %d in the current snapshot", id), 1)
			}
			if asJSON {
				enc := termsafe.JSONEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(task)
			}
			return printTaskDetail(deps.Theme.Writer(cmd.OutOrStdout(), nil), task)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the task as machine-readable JSON")
	return cmd
}

func findTask(all []tasks.Task, id int) (tasks.Task, bool) {
	for _, t := range all {
		if t.ID == id {
			return t, true
		}
	}
	return tasks.Task{}, false
}

func printTaskDetail(out io.Writer, t tasks.Task) error {
	status := "open"
	if t.Done {
		status = "done"
	}
	if _, err := fmt.Fprintf(out, "#%d  %s  [%s]\n", t.ID, safeTitle(t.Title), status); err != nil {
		return err
	}
	if t.Description != "" {
		shown := safeText(t.Description)
		if _, err := fmt.Fprintf(out, "\n%s\n", shown); err != nil {
			return err
		}
		// safeText cuts from the end, and the end is where a closed-by line
		// sits: a reader of a long description would see everything except
		// who closed the task and why.
		if descriptionWasCut(t.Description, shown) {
			if _, err := fmt.Fprintf(out, "description last line (the description above is truncated): %s\n",
				termsafe.SafeLineMax(lastDescriptionLine(t.Description), closingLineShowMaxRunes)); err != nil {
				return err
			}
		}
	}
	if len(t.RelatedTasks) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(out, "\nrelations:"); err != nil {
		return err
	}
	kinds := make([]string, 0, len(t.RelatedTasks))
	for k := range t.RelatedTasks {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		for _, rel := range t.RelatedTasks[kind] {
			relStatus := "open"
			if rel.Done {
				relStatus = "done"
			}
			// The kind is a key of the server's related_tasks object:
			// board text, like the title beside it.
			if _, err := fmt.Fprintf(out, "  %-12s #%d  %s  [%s]\n", safeTitle(kind), rel.ID, safeTitle(rel.Title), relStatus); err != nil {
				return err
			}
		}
	}
	return nil
}

// descriptionProbeMaxRunes is the cap descriptionWasCut renders under. It only
// has to be larger than safeText's own.
const descriptionProbeMaxRunes = 4096

// descriptionWasCut reports whether shown, the safeText rendering of
// description, left any of it out.
//
// The description's length does not answer that: the cap counts runes of
// escaped output, so a short description full of control characters is cut
// and a long plain one at the cap is not. Rendering it again under a larger
// cap does. A description that fits under safeText's cap renders the same
// under any larger one; one that was cut renders longer, or cut further on.
func descriptionWasCut(description, shown string) bool {
	return shown != termsafe.SafeLineMax(description, descriptionProbeMaxRunes)
}

// closingLineShowMaxRunes caps the last line `show` prints for a truncated
// description. A full closed-by trailer is a closer of up to 100 characters,
// evidence of up to 300, and about 60 of fixed text; the title cap would cut
// the evidence off the one line this exists to show.
const closingLineShowMaxRunes = 600

// lastDescriptionLine is the description's last line, not counting line breaks
// after it: the line a closed-by trailer occupies.
func lastDescriptionLine(description string) string {
	description = strings.TrimRight(description, "\r\n")
	return description[strings.LastIndexByte(description, '\n')+1:]
}

func newTasksReadyCmd(deps module.Deps, host, keychainService *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ready",
		Short: `Open tasks with no active "blocked" relation, ranked by position`,
		Long: `ready lists open tasks that carry no ACTIVE "blocked" relation — a related
task under the "blocked" kind that is not itself done. This is the ` + "`bd ready`" + `
semantic, reimplemented on Vikunja's own relation graph rather than a bespoke
dependency store.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			snap, fromCache, err := loadTasksSnapshot(cmd, deps.Runner, *host, *keychainService, deps.Cfg.Tasks.AllowedHosts)
			if err != nil {
				return tasksExitError(err)
			}
			reportCacheFallback(cmd.ErrOrStderr(), *host, fromCache, snap)

			readySnap := tasks.Snapshot{Tasks: tasks.Ready(snap.Tasks)}
			if asJSON {
				// Same row-builder as ls, deliberately: a second hand-rolled
				// copy is how the two verbs' JSON shapes drift apart when a
				// field is added to one of them.
				rows := snapshotToLsJSON(readySnap, false)
				if rows == nil {
					rows = []tasksLsJSON{}
				}
				enc := termsafe.JSONEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			return printTasksList(deps.Theme.Writer(cmd.OutOrStdout(), nil), readySnap, false)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable list to stdout")
	return cmd
}

// newTasksClient is a seam over tasks.NewClient: production always takes
// this path (host pinning included). Tests substitute a factory pointed at
// an httptest server, so the full ls/show/ready command flow can be
// exercised — and the token's journey through it asserted — without a live
// instance or any real network access.
var newTasksClient = tasks.NewClient

// tasksKeychainFlag and tasksWriteKeychainFlag are the two flags that name a
// keychain entry, as a refusal about one spells it.
const (
	tasksKeychainFlag      = "--keychain-service"
	tasksWriteKeychainFlag = "--write-keychain-service"
)

// checkTasksKeychainUse is what must hold before a `tasks` verb reads a
// keychain credential: the host is one the credential may go to, and the
// service name is one this client will hand to the keychain tool.
//
// The host is checked here, before the read, so that a credential the command
// will not send is never read into the process. tasks.NewClient checks it
// again when the client is built; that copy is the backstop for a caller that
// skips this one, and it runs after the read.
func checkTasksKeychainUse(flag, service, host string, allowedHosts []string) error {
	if err := tasks.CheckAllowedHost(host, allowedHosts); err != nil {
		return WithExitCode(err, exitTasksHostRefused)
	}
	if err := tasks.CheckKeychainService(service); err != nil {
		return WithExitCode(fmt.Errorf("tasks: %s: %s", flag, strings.TrimPrefix(err.Error(), "tasks: ")), 1)
	}
	return nil
}

// readTasksKeychainToken is the one way a `tasks` verb reads the keychain:
// checkTasksKeychainUse, then the read. Every verb that takes a keychain
// credential goes through it (ls, show, ready, done, and mcp over stdio), so
// the check cannot be left out of one of them; a verb walk in the tests pins
// that for verbs added later.
//
// cmd is the verb being run. It supplies the context, and the name and stderr
// a refused host is recorded under.
func readTasksKeychainToken(
	cmd *cobra.Command,
	runner exec.Runner,
	flag, service, host string,
	allowedHosts []string,
) (tasks.Token, error) {
	if err := checkTasksKeychainUse(flag, service, host, allowedHosts); err != nil {
		if tasks.IsHostRefused(err) {
			recordTasksHostRefusal(cmd, service, host)
		}
		return tasks.Token{}, err
	}
	return tasks.ReadToken(cmd.Context(), runner, service, allowedHosts)
}

// recordTasksHostRefusal appends one line to the close log for a verb the host
// rule refused.
//
// The refusal itself is the returned error, which reaches stderr with exit 4.
// That is gone when the terminal or the agent session that ran the command is,
// and a host that came from text on the board is exactly the case an operator
// needs to hear about afterwards. So the line goes to the file, and only to
// the file: stderr already says what happened.
//
// It cannot change the outcome. A line that could not be appended is reported
// in one plain line on stderr, and the refusal is returned as it was.
func recordTasksHostRefusal(cmd *cobra.Command, service, host string) {
	err := tasks.WriteHostRefusalRecord(closeLogWriter{}, tasks.HostRefusalRecord{
		Time:       time.Now(),
		Verb:       cmd.Name(),
		Host:       host,
		Credential: service,
	})
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "forgectl: tasks: the refusal of this host could not be recorded in the close log: %s\n", //nolint:errcheck // stderr is the only place left to say so
			safeText(err.Error()))
	}
}

// loadTasksSnapshot reads the token, builds a client (which host-pins
// before ever sending it), fetches a fresh Snapshot, and caches it. On a
// network failure ONLY it falls back to a locally cached Snapshot — never
// on an auth rejection, which must fail loudly rather than silently serve
// data that may no longer be current.
func loadTasksSnapshot(
	cmd *cobra.Command,
	runner exec.Runner,
	host, keychainService string,
	allowedHosts []string,
) (tasks.Snapshot, bool, error) {
	ctx := cmd.Context()
	token, err := readTasksKeychainToken(cmd, runner, tasksKeychainFlag, keychainService, host, allowedHosts)
	if err != nil {
		return tasks.Snapshot{}, false, err
	}
	client, err := newTasksClient(ctx, runner, host, token)
	if err != nil {
		if mayServeCache(err) {
			return cachedTasksSnapshot(err)
		}
		return tasks.Snapshot{}, false, err
	}
	snap, err := client.FetchAll(ctx)
	if err != nil {
		if mayServeCache(err) {
			return cachedTasksSnapshot(err)
		}
		return tasks.Snapshot{}, false, err
	}
	snap.FetchedAt = time.Now().UTC()
	cacheSnapshot(ctx, cmd.ErrOrStderr(), snap)
	return snap, false, nil
}

// cacheSnapshot writes the fetched snapshot to the local cache. A cache
// write failure must not fail the command — the data is already in hand —
// but it must not be silent either: an unwritable cache dir means the NEXT
// network outage has no fallback, and the operator would otherwise learn
// that only during the outage.
//
// So the failure is one plain line on the command's stderr. The slog call
// alone is not enough: with log_level unset the global logger discards
// everything, so under the default config a warning sent only there is never
// seen. The slog call stays for an operator who has turned logging on and
// reads the log file rather than the terminal.
func cacheSnapshot(ctx context.Context, stderr io.Writer, snap tasks.Snapshot) {
	path, err := config.TasksCachePath()
	if err != nil {
		slog.WarnContext(ctx, "Could not resolve the tasks cache path, skipping the cache write. A later network outage will have no fallback data.",
			"error", err)
		reportCacheWriteFailure(stderr, err)
		return
	}
	if err := tasks.SaveCache(path, snap); err != nil {
		slog.WarnContext(ctx, "Failed to write the tasks cache. The command succeeded, but this snapshot was not cached; a later network outage falls back to an older cache, if there is one.",
			"path", path, "error", err)
		reportCacheWriteFailure(stderr, err)
	}
}

// reportCacheWriteFailure is cacheSnapshot's line on stderr. The error names
// at most the cache path and an OS reason; it goes through safeText like every
// other error text this package prints.
func reportCacheWriteFailure(stderr io.Writer, err error) {
	fmt.Fprintf(stderr, "forgectl: tasks: this snapshot was not cached, so a later network outage falls back to an older cache, if there is one: %s\n", //nolint:errcheck // stderr is the only place left to say so
		safeText(err.Error()))
}

// mayServeCache decides whether err is the kind of failure a stale cache may
// paper over. Only a genuine network failure is.
//
// The IsHostRefused arm is not redundant with the sentinels being distinct —
// it is the second lock on the same door. A refusal to send the credential
// must never be served from cache, because cache-serving returns a nil error
// and the command then exits 0: the security verdict would present as a
// successful run. That is strictly worse than losing the exit code, and it
// is one careless `%w` in the client away, so the guard is stated here too
// rather than resting on the wrap being right forever.
func mayServeCache(err error) bool {
	return tasks.IsUnreachable(err) && !tasks.IsHostRefused(err)
}

// cachedTasksSnapshot loads the local cache after a network failure. If no
// cache exists either, the original network error is returned so the
// caller's exit code still reflects "unreachable", not a generic failure.
func cachedTasksSnapshot(cause error) (tasks.Snapshot, bool, error) {
	path, err := config.TasksCachePath()
	if err != nil {
		return tasks.Snapshot{}, false, cause
	}
	snap, err := tasks.LoadCache(path)
	if err != nil {
		return tasks.Snapshot{}, false, cause
	}
	return snap, true, nil
}

// reportCacheFallback names the host that actually failed, not the default
// one: under --host the operator is otherwise told a different instance is
// down than the one they asked for, during exactly the incident this notice
// exists to explain.
func reportCacheFallback(stderr io.Writer, host string, fromCache bool, snap tasks.Snapshot) {
	if !fromCache {
		return
	}
	age := time.Since(snap.FetchedAt).Round(time.Second)
	fmt.Fprintf(stderr, "forgectl: %s unreachable — serving cached data, %s old\n", //nolint:errcheck // best-effort stderr notice
		safeTitle(host), age)
}

// tasksExitError maps a tasks package sentinel to its distinct exit code
// (ADR-0008: honest exit codes). Any other error keeps the default 1.
func tasksExitError(err error) error {
	switch {
	case tasks.IsUnauthorized(err):
		return WithExitCode(err, exitTasksUnauthorized)
	case tasks.IsHostRefused(err):
		return WithExitCode(err, exitTasksHostRefused)
	case tasks.IsUnreachable(err):
		return WithExitCode(err, exitTasksUnreachable)
	default:
		return err
	}
}
