// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

// execRoot runs the whole command tree with args and returns the error.
func execRoot(t *testing.T, args ...string) error {
	t.Helper()
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	root.SetArgs(args)
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})
	return root.ExecuteContext(t.Context())
}

// TestUsageArgs_NamesTheArgumentAndUsage pins forgectl#1087: a wrong argument
// count says which argument is missing or unexpected and shows the usage line,
// and moves no exit code.
func TestUsageArgs_NamesTheArgumentAndUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"desk add missing file", []string{"desk", "add"}, []string{"desk add: missing <file|->", "usage: forgectl desk add <file|->"}},
		{"desk skip names the positional, not the flag value", []string{"desk", "skip", "--reason", "x"}, []string{"missing <name>", "usage: forgectl desk skip <name> --reason <text>"}},
		{"surface close", []string{"surface", "close"}, []string{"missing <name>", "usage: forgectl surface close <name>"}},
		{"surface brief second argument", []string{"surface", "brief", "w"}, []string{"missing <text|@file>"}},
		{"tasks done", []string{"tasks", "done"}, []string{"missing <id>"}},
		{"too many", []string{"desk", "add", "a", "b"}, []string{`unexpected argument "b" after <file|->`, "usage: forgectl desk add <file|->"}},
		{"optional positional, too many", []string{"desk", "status", "a", "b"}, []string{"takes at most 1 argument, got 2", "usage: forgectl desk status [name]"}},
		{"no-argument leaf", []string{"launch", "which", "extra"}, []string{`takes no arguments, got "extra"`, "usage: forgectl launch which"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := execRoot(t, tt.args...)
			if err == nil {
				t.Fatalf("%v: no error", tt.args)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "arg(s)") {
				t.Errorf("error = %q still carries cobra's count text", err)
			}
			want := exitUsage
			if tt.args[0] == "tasks" { // a documented ADR-0014 exception: tasks keeps 2 for "unreachable"
				want = exitFailed
			}
			if got := ExitCode(err); got != want {
				t.Errorf("exit = %d, want %d: a wrong argument count is a usage error (ADR-0014)", got, want)
			}
		})
	}
}

// TestUsageArgs_EveryLeafNamesItsUsage walks the finished tree: no leaf's
// validator may hand back cobra's bare count text, whatever it takes.
func TestUsageArgs_EveryLeafNamesItsUsage(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if c.Parent() == nil || c.HasSubCommands() || c.Args == nil {
			return
		}
		for _, args := range [][]string{nil, {"a"}, {"a", "b"}, {"a", "b", "c", "d"}} {
			err := c.Args(c, args)
			if err == nil {
				continue
			}
			if strings.Contains(err.Error(), "arg(s)") {
				t.Errorf("%s with %d args: %q carries cobra's count text", c.CommandPath(), len(args), err)
			}
			if strings.Contains(err.Error(), "takes no arguments") {
				if req, opt := usageShape(c.Use); len(req)+len(opt) > 0 {
					t.Errorf("%s with %d args: %q says no arguments, but its usage line takes %v %v", c.CommandPath(), len(args), err, req, opt)
				}
			}
			if strings.Contains(err.Error(), "wrong number of arguments") {
				t.Errorf("%s with %d args: %q names no argument; give its Use a <placeholder>", c.CommandPath(), len(args), err)
			}
			if strings.HasPrefix(err.Error(), "unknown command ") {
				t.Errorf("%s with %d args: %q is cobra's NoArgs text", c.CommandPath(), len(args), err)
			}
		}
	}
	walk(root)
}

func TestUsageShape(t *testing.T) {
	tests := []struct {
		use      string
		req, opt int
		variadic bool
	}{
		{"status [name]", 0, 1, false},
		{"add <file|->", 1, 0, false},
		{"clone <repo> [dir]", 1, 1, false},
		{"pick [query...]", 0, 1, true},
		{"releases [--registry <path>] [--json]", 0, 0, false},
		{"ls", 0, 0, false},
	}
	for _, tt := range tests {
		req, opt := usageShape(tt.use)
		if len(req) != tt.req || len(opt) != tt.opt || usageVariadic(tt.use) != tt.variadic {
			t.Errorf("usageShape(%q) = %v %v variadic=%v, want %d %d %v", tt.use, req, opt, usageVariadic(tt.use), tt.req, tt.opt, tt.variadic)
		}
	}
}

func TestUsagePlaceholders(t *testing.T) {
	tests := []struct {
		use  string
		want []string
	}{
		{"add <file|->", []string{"<file|->"}},
		{"skip <name> --reason <text>", []string{"<name>"}},
		{"show <name> | show --log FILE", []string{"<name>"}},
		{"brief <name> <text|@file>", []string{"<name>", "<text|@file>"}},
		{"releases [--registry <path>] [--json]", nil},
		{"__sops-edit FILE", []string{"FILE"}},
		{"ls", nil},
		{"", nil},
	}
	for _, tt := range tests {
		got, _ := usageShape(tt.use)
		if strings.Join(got, ",") != strings.Join(tt.want, ",") {
			t.Errorf("usagePlaceholders(%q) = %v, want %v", tt.use, got, tt.want)
		}
	}
}

// jsonShapeVerbs are the verbs whose --json flag text named no shape
// (forgectl#1087). Each must list its top-level keys in its own --help.
var jsonShapeVerbs = [][]string{
	{"bench", "status"},
	{"docs", "check"}, {"docs", "list"}, {"docs", "search"},
	{"net"},
	{"pr", "prs"},
	{"projects", "list"},
	{"review"},
	{"surface", "brief"}, {"surface", "close"}, {"surface", "list"},
	{"surface", "read"}, {"surface", "ready"}, {"surface", "wait"},
}

func TestJSONFlagText_NamesTheShape(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	for _, path := range jsonShapeVerbs {
		cmd, _, err := root.Find(path)
		if err != nil || cmd == nil || cmd == root {
			t.Fatalf("no verb %v: %v", path, err)
		}
		f := cmd.Flags().Lookup("json")
		if f == nil {
			t.Errorf("%v has no --json flag", path)
			continue
		}
		if !strings.ContainsAny(f.Usage, "{[") || strings.Count(f.Usage, `"`) < 4 {
			t.Errorf("%v: --json help %q names no keys", path, f.Usage)
		}
	}
}

func TestBenchStatusJSONHelp_NamesStatesAndExit(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	cmd, _, _ := root.Find([]string{"bench", "status"})
	usage := cmd.Flags().Lookup("json").Usage
	for _, want := range []string{"ok", "degraded", "unavailable", "not-configured", "exit code is 0"} {
		if !strings.Contains(usage, want) {
			t.Errorf("bench status --json help lacks %q: %s", want, usage)
		}
	}
}

// desk add's usage line names the one file it takes and the two flags it
// requires (#1109): `desk add FILE ...` read as several files, and the
// synopsis left out --what and --why.
func TestUsageArgs_DeskAddNamesItsRequiredFlags(t *testing.T) {
	for _, args := range [][]string{{"desk", "add"}, {"desk", "add", "a", "b"}} {
		err := execRoot(t, args...)
		if err == nil || !strings.Contains(err.Error(), "usage: forgectl desk add <file|-> --what <text> --why <text>") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	var out strings.Builder
	root.SetOut(&out)
	root.SetArgs([]string{"desk", "--help"})
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "forgectl desk add FILE|- --what TEXT --why TEXT") || !strings.Contains(out.String(), "per call") {
		t.Errorf("desk --help synopsis:\n%s", out.String())
	}
}
