// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/theme"
)

func runUnknownHelpArgs(t *testing.T, args ...string) (stderr string, err error) {
	t.Helper()
	deps := module.Deps{Runner: &exec.FakeRunner{}, Theme: theme.Default()}
	root := newRoot(deps)
	root.SetOut(new(bytes.Buffer))
	var errBuf bytes.Buffer
	root.SetErr(&errBuf)
	err = execCommand(context.Background(), root, args, deps.Theme)
	return errBuf.String(), err
}

// TestExecCommand_UnknownCommandWithHelpFails pins forgectl#1080: an unknown
// command or subcommand must fail with --help (or -h) exactly as it does
// without, so `forgectl <x> --help` cannot pass as a capability probe.
func TestExecCommand_UnknownCommandWithHelpFails(t *testing.T) {
	t.Setenv(skipLegacyMigrateEnv, "1")
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"top-level long flag", []string{"nosuchcmd", "--help"}, `"nosuchcmd" for "forgectl"`},
		{"top-level short flag", []string{"nosuchcmd", "-h"}, `"nosuchcmd" for "forgectl"`},
		{"flag before the verb", []string{"--help", "nosuchcmd"}, `"nosuchcmd" for "forgectl"`},
		{"subcommand", []string{"desk", "nosuchsub", "--help"}, `"nosuchsub" for "forgectl desk"`},
		{"subcommand of another group", []string{"tmux", "frobnicate", "--help"}, `"frobnicate" for "forgectl tmux"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stderr, err := runUnknownHelpArgs(t, tt.args...)
			if err == nil {
				t.Fatalf("execCommand(%q) = nil, want an unknown-command error; stderr = %q", tt.args, stderr)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "unknown command") {
				t.Errorf("error = %q, want it to say unknown command", err.Error())
			}
			if ExitCode(err) == 0 {
				t.Errorf("ExitCode = 0, want non-zero")
			}
			if !strings.Contains(stderr, tt.want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.want)
			}
		})
	}
}

// TestExecCommand_RealCommandHelpStillSucceeds is the other half of the pin:
// every registered command, nested ones included, still prints help and
// succeeds, and so do the lazy cobra builtins and the bare root.
func TestExecCommand_RealCommandHelpStillSucceeds(t *testing.T) {
	t.Setenv(skipLegacyMigrateEnv, "1")
	deps := module.Deps{Runner: &exec.FakeRunner{}, Theme: theme.Default()}
	var paths [][]string
	var walk func(c *cobra.Command, prefix []string)
	walk = func(c *cobra.Command, prefix []string) {
		for _, child := range c.Commands() {
			p := append(append([]string(nil), prefix...), child.Name())
			paths = append(paths, p)
			walk(child, p)
		}
	}
	walk(newRoot(deps), nil)
	if len(paths) < 20 {
		t.Fatalf("walked only %d commands; the tree walk is broken", len(paths))
	}
	// A leaf's stray positional is not an unknown command: it keeps printing help.
	paths = append(paths, []string{"doctor", "stray"})
	paths = append(paths, nil, []string{"help"}, []string{"completion"}, []string{"completion", "zsh"})
	for _, p := range paths {
		args := append(append([]string(nil), p...), "--help")
		if stderr, err := runUnknownHelpArgs(t, args...); err != nil {
			t.Errorf("execCommand(%q) error = %v (stderr %q), want help and success", args, err, stderr)
		}
	}
}

// TestUnknownCommandWithHelp_LeavesFlagsAloneWithoutHelp pins that the check
// parses nothing on an ordinary run: cobra parses the flags itself afterwards,
// and a slice flag parsed twice would hold every value twice.
func TestUnknownCommandWithHelp_LeavesFlagsAloneWithoutHelp(t *testing.T) {
	var tags []string
	root := &cobra.Command{Use: "app", Args: safeRootArgs, SilenceErrors: true, SilenceUsage: true}
	run := &cobra.Command{Use: "run", RunE: func(*cobra.Command, []string) error { return nil }}
	run.Flags().StringSliceVar(&tags, "tag", nil, "")
	root.AddCommand(run)
	args := []string{"run", "--tag", "a"}

	if err := unknownSubcommand(root, args); err != nil {
		t.Fatalf("unknownSubcommand() = %v, want nil", err)
	}
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0] != "a" {
		t.Errorf("--tag = %q, want [a]: the check parsed the flags before cobra did", tags)
	}
}

// groupPaths walks root and returns the command path of every group: a command
// with subcommands and no positional of its own. The list comes from the tree,
// so a group added later is covered without touching this test.
func groupPaths(root *cobra.Command) [][]string {
	var paths [][]string
	var walk func(c *cobra.Command, prefix []string)
	walk = func(c *cobra.Command, prefix []string) {
		for _, child := range c.Commands() {
			p := append(append([]string(nil), prefix...), child.Name())
			if child.HasSubCommands() && !parentTakesArg(child) {
				paths = append(paths, p)
			}
			walk(child, p)
		}
	}
	walk(root, nil)
	return paths
}

// TestEveryGroupRejectsUnknownSubcommand pins forgectl#1090 for every group in
// the tree: an unknown subcommand fails with the unknown-command error, with
// and without --help, and a near miss of a real subcommand gets a suggestion.
func TestEveryGroupRejectsUnknownSubcommand(t *testing.T) {
	t.Setenv(skipLegacyMigrateEnv, "1")
	deps := module.Deps{Runner: &exec.FakeRunner{}, Theme: theme.Default()}
	root := newRoot(deps)
	groups := groupPaths(root)
	if len(groups) < 18 {
		t.Fatalf("found %d groups, want at least 18 (forgectl#1090 measured 18 that exited 0 alone)", len(groups))
	}
	for _, path := range groups {
		name := strings.Join(path, " ")
		group, _, err := root.Find(path)
		if err != nil || group == nil {
			t.Fatalf("Find(%q) = %v, %v", path, group, err)
		}
		for _, extra := range [][]string{nil, {"--help"}} {
			args := append(append(append([]string(nil), path...), "bogusverbxyz"), extra...)
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				stderr, err := runUnknownHelpArgs(t, args...)
				if err == nil {
					t.Fatalf("execCommand(%q) = nil, want an unknown-command error", args)
				}
				if ExitCode(err) == 0 || !strings.Contains(strings.ToLower(err.Error()), "unknown command") {
					t.Errorf("error = %q (exit %d), want non-zero and unknown command", err.Error(), ExitCode(err))
				}
				if !strings.Contains(stderr, `"bogusverbxyz" for "forgectl `+name+`"`) {
					t.Errorf("stderr = %q, want it to name the bad token and the group %q", stderr, name)
				}
			})
		}

		// A close miss: a real subcommand with its second letter dropped. Dropping
		// the last would still be a prefix, which cobra suggests at any distance.
		for _, sub := range group.Commands() {
			if !sub.IsAvailableCommand() || len(sub.Name()) < 4 {
				continue
			}
			args := append(append([]string(nil), path...), sub.Name()[:1]+sub.Name()[2:])
			t.Run("suggest "+strings.Join(args, " "), func(t *testing.T) {
				stderr, err := runUnknownHelpArgs(t, args...)
				if err == nil {
					t.Fatalf("execCommand(%q) = nil, want an unknown-command error", args)
				}
				if !strings.Contains(stderr, "Did you mean") || !strings.Contains(stderr, sub.Name()) {
					t.Errorf("stderr = %q, want a suggestion of %q", stderr, sub.Name())
				}
			})
			break
		}
	}
}

// TestGroupHelpStillSucceeds keeps the other half: a group's own help still
// prints and succeeds, and so does a group with no Run run bare. A runnable
// group run bare does real work (a TUI, an inventory), so only --help is run
// for those.
func TestGroupHelpStillSucceeds(t *testing.T) {
	t.Setenv(skipLegacyMigrateEnv, "1")
	deps := module.Deps{Runner: &exec.FakeRunner{}, Theme: theme.Default()}
	root := newRoot(deps)
	for _, path := range groupPaths(root) {
		group, _, _ := root.Find(path)
		argvs := [][]string{append(append([]string(nil), path...), "--help")}
		if !group.Runnable() {
			argvs = append(argvs, path)
		}
		for _, args := range argvs {
			if stderr, err := runUnknownHelpArgs(t, args...); err != nil {
				t.Errorf("execCommand(%q) error = %v (stderr %q), want help and success", args, err, stderr)
			}
		}
	}
}
