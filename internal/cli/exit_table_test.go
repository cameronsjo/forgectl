// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

// The exit-code table (ADR-0014, forgectl#1085): Phase 1 moves every usage
// error to exit 2, except the four verbs whose 2 already means something else.

// TestExitTable_UsageErrors pins each row of the Phase 1 change list.
//
// Mutation that turns it red: make classifyUsageErrors return without wrapping
// (every 2 row), or make usageKeepsOne return false (the exception rows).
func TestExitTable_UsageErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want int
	}{
		{"cobra flag error", []string{"status", "--zz-no-such-flag"}, 2},
		{"argument count error", []string{"desk", "add"}, 2},
		{"argument count error, env get", []string{"env", "get"}, 2},
		{"unknown verb", []string{"zzbogus"}, 2},
		{"unknown subverb", []string{"desk", "zzbogus"}, 2},
		{"docs group flag error", []string{"docs", "--zzbogus"}, 2},
		{"docs leaf flag error keeps 2", []string{"docs", "list", "--zzbogus"}, 2},
		{"config with a stray argument", []string{"config", "zzbogus"}, 2},
		{"resume flag error", []string{"resume", "--zz-no-such-flag"}, 2},

		{"tasks unknown subverb stays 1", []string{"tasks", "zzbogus"}, 1},
		{"tasks missing argument stays 1", []string{"tasks", "show"}, 1},
		{"tasks flag error stays 1", []string{"tasks", "ready", "--zz-no-such-flag"}, 1},
		{"env check flag error stays 1", []string{"env", "check", "--zz-no-such-flag"}, 1},
		{"resume snapshot flag error stays 1", []string{"resume", "snapshot", "--zz-no-such-flag"}, 1},
		{"resume snapshot stray argument stays 1", []string{"resume", "snapshot", "x"}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := execRoot(t, tt.args...)
			if err == nil {
				t.Fatalf("%v: no error", tt.args)
			}
			if got := ExitCode(err); got != tt.want {
				t.Errorf("%v: exit = %d, want %d (err %v)", tt.args, got, tt.want, err)
			}
		})
	}
}

// TestExitTable_CompletionGroupRejectsUnknownArgument: `completion` is a lazy
// builtin, so only Execute sees it. `completion nonesuch` printed help and
// exited 0.
func TestExitTable_CompletionGroupRejectsUnknownArgument(t *testing.T) {
	isolateJSONContractEnv(t)
	_, err := executeCapturingStderr(t, "completion", "nonesuch")
	if got := ExitCode(err); err == nil || got != exitUsage {
		t.Errorf("completion nonesuch: exit = %d (err %v), want %d", got, err, exitUsage)
	}
}

// TestExitTable_LeafWalk: a bad flag on every leaf exits 2, or 1 for an
// exception. Pass-through leaves (they take the flags themselves) and the
// docs leaves (their own 2) are not in the walk's claim.
func TestExitTable_LeafWalk(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	var leaves [][]string
	var walk func(c *cobra.Command, path []string)
	walk = func(c *cobra.Command, path []string) {
		for _, sub := range c.Commands() {
			p := append(append([]string(nil), path...), sub.Name())
			if sub.HasSubCommands() {
				walk(sub, p)
				continue
			}
			if sub.DisableFlagParsing || sub.Hidden || p[0] == "launch" || p[0] == "k8s" {
				continue
			}
			leaves = append(leaves, p)
		}
	}
	walk(root, nil)
	if len(leaves) < 40 {
		t.Fatalf("walk found %d leaves, want the whole tree", len(leaves))
	}
	for _, p := range leaves {
		args := append(append([]string(nil), p...), "--zz-no-such-flag")
		want := exitUsage
		if p[0] == "tasks" || strings.Join(p, " ") == "env check" || strings.Join(p, " ") == "resume snapshot" {
			want = exitFailed
		}
		err := execRoot(t, args...)
		if err == nil {
			t.Errorf("%v: no error", args)
			continue
		}
		if got := ExitCode(err); got != want {
			t.Errorf("%v: exit = %d, want %d", args, got, want)
		}
	}
}

// TestResumeSnapshot_NeverExitsTwo: `resume snapshot` is wired as a Claude Code
// Stop hook, and a Stop hook that exits 2 is a blocking error that keeps a
// session from stopping. It must exit 0 or 1 on a bad flag, a stray argument,
// an environment failure, and a config that does not parse.
//
// Mutation that turns it red: make usageKeepsOne return false for
// `resume snapshot` (the flag and argument rows), or drop the invokesHookVerb
// exemption in Execute (the environment row), or the hook verb from hookVerbs
// (the config row).
func TestResumeSnapshot_NeverExitsTwo(t *testing.T) {
	notTwo := func(t *testing.T, err error) {
		t.Helper()
		if got := ExitCode(err); got == exitUsage {
			t.Errorf("exit = %d (err %v): a Stop hook that exits 2 blocks the session", got, err)
		}
	}
	t.Run("bad flag", func(t *testing.T) {
		notTwo(t, execRoot(t, "resume", "snapshot", "--zz-no-such-flag"))
	})
	t.Run("stray argument", func(t *testing.T) {
		notTwo(t, execRoot(t, "resume", "snapshot", "x"))
	})
	t.Run("bad flag through Execute", func(t *testing.T) {
		isolateJSONContractEnv(t)
		_, err := executeCapturingStderr(t, "resume", "snapshot", "--zz-no-such-flag")
		if err == nil {
			t.Fatal("bad flag was accepted")
		}
		notTwo(t, err)
	})
	t.Run("environment failure", func(t *testing.T) {
		stubEnvFailure(t)
		withArgs(t, "resume", "snapshot", "--quiet")
		notTwo(t, Execute(context.Background()))
	})
	t.Run("config that does not parse", func(t *testing.T) {
		writeMalformedUserConfig(t)
		_, err := executeCapturingStderr(t, "resume", "snapshot", "--quiet")
		notTwo(t, err)
	})
}

// TestExitTable_PreFangFailuresAreUsage: an environment the operator controls
// ($HOME unset, a relative $XDG_CONFIG_HOME) and a config that does not parse
// both stop a normal verb before fang starts, and both are setup the caller
// can fix.
func TestExitTable_PreFangFailuresAreUsage(t *testing.T) {
	t.Run("environment failure", func(t *testing.T) {
		stubEnvFailure(t)
		withArgs(t, "projects", "list")
		if got := ExitCode(Execute(context.Background())); got != exitUsage {
			t.Errorf("exit = %d, want %d", got, exitUsage)
		}
	})
	t.Run("config that does not parse", func(t *testing.T) {
		writeMalformedUserConfig(t)
		_, err := executeCapturingStderr(t, "projects", "list")
		if got := ExitCode(err); got != exitUsage {
			t.Errorf("exit = %d (err %v), want %d", got, err, exitUsage)
		}
	})
}

// TestClassExit pins the table's numbers, so a class cannot drift from its
// documented code. 5 and 6 are reserved: no class maps to them.
func TestClassExit(t *testing.T) {
	for c, want := range map[exitClass]int{
		classOK: 0, classFailed: 1, classUsage: 2, classUnauthorized: 3, classRefused: 4,
	} {
		if got := classExit(c); got != want {
			t.Errorf("classExit(%d) = %d, want %d", c, got, want)
		}
	}
	for c := classOK; c <= classRefused+3; c++ {
		if got := classExit(c); got == 5 || got == 6 {
			t.Errorf("classExit(%d) = %d: 5 and 6 are reserved", c, got)
		}
	}
}

// TestJSONUsageCode: a usage error raised inside RunE reports usage_error
// without moving its exit status.
func TestJSONUsageCode(t *testing.T) {
	for _, args := range [][]string{{"desk", "show", "a/b", "--json"}, {"tasks", "show", "abc", "--json"}} {
		isolateJSONContractEnv(t)
		_, stderr, err := runJSONThroughFang(t, productionJSONRoot(&exec.FakeRunner{}), args...)
		if err == nil {
			t.Fatalf("%v: no error", args)
		}
		obj := decodeOneStderrObject(t, stderr)
		if obj["code"] != jsonCodeUsage {
			t.Errorf("%v: code = %v, want %q", args, obj["code"], jsonCodeUsage)
		}
	}
}

// TestRootHelp_NamesEveryExitClass: the root help carries the table, so an
// agent that reads only --help can branch on a status.
func TestRootHelp_NamesEveryExitClass(t *testing.T) {
	long := newRoot(module.Deps{Runner: &exec.FakeRunner{}}).Long
	for _, want := range []string{"0 ok", "1 failed", "2 usage", "3 unauthorized", "4 refused", "docs/exit-codes.md"} {
		if !strings.Contains(long, want) {
			t.Errorf("root help does not mention %q", want)
		}
	}
	if strings.Contains(long, "FORGECTL_EXIT_CODES") {
		t.Error("root help names FORGECTL_EXIT_CODES; Phase 2 is deferred")
	}
}

// TestExitCodesDoc_AgreesWithClasses: the reference page lists every class at
// the number classExit returns, and marks 5 and 6 reserved.
func TestExitCodesDoc_AgreesWithClasses(t *testing.T) {
	raw, err := os.ReadFile("../../docs/exit-codes.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for c, name := range map[exitClass]string{
		classOK: "ok", classFailed: "failed", classUsage: "usage", classUnauthorized: "unauthorized", classRefused: "refused",
	} {
		row := fmt.Sprintf("| %d | `%s` |", classExit(c), name)
		if !strings.Contains(doc, row) {
			t.Errorf("docs/exit-codes.md has no row %q", row)
		}
	}
	for _, row := range []string{"| 5 | `unreachable` | Reserved.", "| 6 | `not_found` | Reserved."} {
		if !strings.Contains(doc, row) {
			t.Errorf("docs/exit-codes.md has no reserved row %q", row)
		}
	}
	if strings.Contains(doc, "FORGECTL_EXIT_CODES") {
		t.Error("docs/exit-codes.md names FORGECTL_EXIT_CODES; Phase 2 is deferred")
	}
}

// TestExitTable_CompletionLeafArgCount: `completion <shell>` leaves are created
// lazily, so they take the usage class at Execute time.
func TestExitTable_CompletionLeafArgCount(t *testing.T) {
	for _, args := range [][]string{{"completion", "bash", "x", "y"}, {"completion", "zsh", "--zz-no-such-flag"}} {
		isolateJSONContractEnv(t)
		_, err := executeCapturingStderr(t, args...)
		if got := ExitCode(err); err == nil || got != exitUsage {
			t.Errorf("%v: exit = %d (err %v), want %d", args, got, err, exitUsage)
		}
	}
}

// TestExitTable_ArgCountWalk: two stray positionals on every leaf. A leaf that
// rejects them with a count error (the "; usage:" line nameUsageArgs adds)
// exits 2, or 1 for an exception. Leaves that accept positionals, and
// pass-through verbs, are outside the claim.
func TestExitTable_ArgCountWalk(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	rejected := 0
	var walk func(c *cobra.Command, path []string)
	walk = func(c *cobra.Command, path []string) {
		for _, sub := range c.Commands() {
			p := append(append([]string(nil), path...), sub.Name())
			if sub.HasSubCommands() {
				walk(sub, p)
				continue
			}
			if sub.DisableFlagParsing || sub.Hidden || p[0] == "launch" || p[0] == "k8s" {
				continue
			}
			err := execRoot(t, append(append([]string(nil), p...), "zz1", "zz2")...)
			if err == nil || !strings.Contains(err.Error(), "; usage: ") {
				continue
			}
			rejected++
			want := exitUsage
			if p[0] == "tasks" || strings.Join(p, " ") == "env check" || strings.Join(p, " ") == "resume snapshot" {
				want = exitFailed
			}
			if got := ExitCode(err); got != want {
				t.Errorf("%v: exit = %d, want %d", p, got, want)
			}
		}
	}
	walk(root, nil)
	if rejected < 20 {
		t.Errorf("only %d leaves rejected stray arguments; the walk is not reaching the tree", rejected)
	}
}

// TestExitTable_EnvFailureKeepsExceptionVerbs: the environment failure is
// raised before the command tree exists, so the exceptions are read from argv.
// tasks and env check keep 1 (their 2 means "unreachable" and "file absent").
func TestExitTable_EnvFailureKeepsExceptionVerbs(t *testing.T) {
	for _, tt := range []struct {
		argv []string
		want int
	}{
		{[]string{"tasks", "ls"}, exitFailed},
		{[]string{"--no-icons", "tasks", "ls", "--json"}, exitFailed},
		{[]string{"env", "check", ".env"}, exitFailed},
		{[]string{"env", "get", "X"}, exitUsage},
		{[]string{"projects", "list"}, exitUsage},
	} {
		stubEnvFailure(t)
		withArgs(t, tt.argv...)
		if got := ExitCode(Execute(context.Background())); got != tt.want {
			t.Errorf("%v: exit = %d, want %d", tt.argv, got, tt.want)
		}
	}
}
