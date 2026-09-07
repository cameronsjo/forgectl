// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
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
const (
	exitTasksUnreachable  = 2
	exitTasksUnauthorized = 3
)

// tasksModule declares the tasks extension (ADR-0005): a read-only Vikunja
// client, a local cache, and three verbs (ls/show/ready). It claims no
// config section — host and keychain service are flags/env, not persisted
// preferences, so there is nothing here for the config registry to own.
var tasksModule = module.Manifest{
	Name: "tasks",
	Tier: module.TierExtension,
	New:  newTasksCmd,
}

func newTasksCmd(deps module.Deps) *cobra.Command {
	var host, keychainService string

	cmd := &cobra.Command{
		Use:   "tasks",
		Short: "Browse a Vikunja task board (read-only)",
		Long: `tasks is a read-only client for a Vikunja instance: a local cache of what
it returns, and three verbs over that data.

  forgectl tasks ls             list open tasks
  forgectl tasks show <id>      one task, its detail and its relations
  forgectl tasks ready          open tasks with no active "blocked" relation,
                                 ranked by Vikunja's own position field

The bearer token is read fresh from the macOS login keychain on every run
(service name below) and never touches argv, a log line, an error string, or
the local cache. A revoked token fails loudly; it never falls back to stale
cache data. A network failure MAY fall back to cache, and states the cache's
age when it does.`,
	}
	cmd.PersistentFlags().StringVar(&host, "host", tasks.DefaultHost, "Vikunja API host")
	cmd.PersistentFlags().StringVar(&keychainService, "keychain-service", tasks.DefaultKeychainService,
		"login keychain service name holding the bearer token")

	cmd.AddCommand(
		newTasksLsCmd(deps, &host, &keychainService),
		newTasksShowCmd(deps, &host, &keychainService),
		newTasksReadyCmd(deps, &host, &keychainService),
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
			snap, fromCache, err := loadTasksSnapshot(cmd.Context(), deps.Runner, *host, *keychainService)
			if err != nil {
				return tasksExitError(err)
			}
			reportCacheFallback(cmd.ErrOrStderr(), fromCache, snap)
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
		if _, err := fmt.Fprintf(out, "%6d  %s\n", r.ID, termsafe.SafeLine(r.Title)); err != nil {
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
			snap, fromCache, err := loadTasksSnapshot(cmd.Context(), deps.Runner, *host, *keychainService)
			if err != nil {
				return tasksExitError(err)
			}
			reportCacheFallback(cmd.ErrOrStderr(), fromCache, snap)

			task, ok := findTask(snap.Tasks, id)
			if !ok {
				return WithExitCode(fmt.Errorf("tasks show: no task with id %d in the current snapshot", id), 1)
			}
			if asJSON {
				enc := termsafe.JSONEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(task)
			}
			return printTaskDetail(cmd.OutOrStdout(), task)
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
	if _, err := fmt.Fprintf(out, "#%d  %s  [%s]\n", t.ID, termsafe.SafeLine(t.Title), status); err != nil {
		return err
	}
	if t.Description != "" {
		if _, err := fmt.Fprintf(out, "\n%s\n", termsafe.SafeLine(t.Description)); err != nil {
			return err
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
			if _, err := fmt.Fprintf(out, "  %-12s #%d  %s  [%s]\n", kind, rel.ID, termsafe.SafeLine(rel.Title), relStatus); err != nil {
				return err
			}
		}
	}
	return nil
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
			snap, fromCache, err := loadTasksSnapshot(cmd.Context(), deps.Runner, *host, *keychainService)
			if err != nil {
				return tasksExitError(err)
			}
			reportCacheFallback(cmd.ErrOrStderr(), fromCache, snap)

			ready := tasks.Ready(snap.Tasks)
			if asJSON {
				rows := make([]tasksLsJSON, 0, len(ready))
				for _, t := range ready {
					rows = append(rows, tasksLsJSON{ID: t.ID, Title: t.Title, Done: t.Done, Position: t.Position})
				}
				enc := termsafe.JSONEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			return printTasksList(cmd.OutOrStdout(), tasks.Snapshot{Tasks: ready}, false)
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

// loadTasksSnapshot reads the token, builds a client (which host-pins
// before ever sending it), fetches a fresh Snapshot, and caches it. On a
// network failure ONLY it falls back to a locally cached Snapshot — never
// on an auth rejection, which must fail loudly rather than silently serve
// data that may no longer be current.
func loadTasksSnapshot(ctx context.Context, runner exec.Runner, host, keychainService string) (tasks.Snapshot, bool, error) {
	token, err := tasks.ReadToken(ctx, runner, keychainService)
	if err != nil {
		return tasks.Snapshot{}, false, err
	}
	client, err := newTasksClient(ctx, runner, host, token)
	if err != nil {
		if tasks.IsUnreachable(err) {
			return cachedTasksSnapshot(err)
		}
		return tasks.Snapshot{}, false, err
	}
	snap, err := client.FetchAll(ctx)
	if err != nil {
		if tasks.IsUnreachable(err) {
			return cachedTasksSnapshot(err)
		}
		return tasks.Snapshot{}, false, err
	}
	snap.FetchedAt = time.Now().UTC()
	if path, pathErr := config.TasksCachePath(); pathErr == nil {
		_ = tasks.SaveCache(path, snap) // best-effort; a cache write failure must not fail the command
	}
	return snap, false, nil
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

func reportCacheFallback(stderr io.Writer, fromCache bool, snap tasks.Snapshot) {
	if !fromCache {
		return
	}
	age := time.Since(snap.FetchedAt).Round(time.Second)
	fmt.Fprintf(stderr, "forgectl: tasks.sjo.lol unreachable — serving cached data, %s old\n", age) //nolint:errcheck // best-effort stderr notice
}

// tasksExitError maps a tasks package sentinel to its distinct exit code
// (ADR-0008: honest exit codes). Any other error keeps the default 1.
func tasksExitError(err error) error {
	switch {
	case tasks.IsUnauthorized(err):
		return WithExitCode(err, exitTasksUnauthorized)
	case tasks.IsUnreachable(err):
		return WithExitCode(err, exitTasksUnreachable)
	default:
		return err
	}
}
