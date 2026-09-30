package launch

import (
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestIsClaudePassthrough pins which Claude argv reaches claude byte-clean.
//
// Mutation that turns it red: drop the args[0] subcommand check (the "mcp"
// and "update" rows flip to false), drop the args[1]-after-"--" check (the
// "-- mcp list" row flips), drop the agents exclusion (the agents rows flip
// to true), or drop the `--` stop in scanClaudeFlags (the "after claude's
// separator" rows flip to true).
func TestIsClaudePassthrough(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"hello"}, false},
		{[]string{"--resume", "abc"}, false},
		{[]string{"--model", "opus", "fix the bug"}, false},
		{[]string{"mcp", "list"}, true},
		{[]string{"update"}, true},
		{[]string{"plugins"}, true},
		{[]string{"doctor"}, true},
		{[]string{"--help"}, true},
		{[]string{"-p", "hi", "-v"}, true},
		{[]string{"--version"}, true},
		// Print mode is its own posture, not a byte-clean passthrough.
		{[]string{"-p", "hi"}, false},
		// claude dispatches a subcommand after its own `--` too.
		{[]string{"--", "mcp", "list"}, true},
		{[]string{"--", "hello"}, false},
		{[]string{"--"}, false},
		// Claude's own separator makes what follows prompt text.
		{[]string{"--model", "opus", "--", "--help"}, false},
		// A subcommand name is only a subcommand in first position; later it
		// is a prompt word or a flag value.
		{[]string{"--model", "opus", "mcp"}, false},
		{[]string{"explain", "doctor"}, false},
		// agents starts sessions, so it keeps its posture in either slot.
		{[]string{"agents"}, false},
		{[]string{"--", "agents"}, false},
	}
	for _, tc := range cases {
		if got := IsClaudePassthrough(tc.args); got != tc.want {
			t.Errorf("IsClaudePassthrough(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestIsClaudePrintMode pins which argv takes the print posture.
//
// Mutation that turns it red: drop "--output-format" from the flag list (both
// output-format rows flip), drop the `=value` prefix match (the
// "--output-format=stream-json" row flips), or drop the `--` stop (the
// "-- -p" row flips).
func TestIsClaudePrintMode(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"hello"}, false},
		{[]string{"-p", "hi"}, true},
		{[]string{"--model", "opus", "--print", "hi"}, true},
		{[]string{"--output-format", "json", "hi"}, true},
		{[]string{"--output-format=stream-json"}, true},
		{[]string{"--", "-p"}, false},
		{[]string{"--model", "opus", "--", "--print"}, false},
	}
	for _, tc := range cases {
		if got := IsClaudePrintMode(tc.args); got != tc.want {
			t.Errorf("IsClaudePrintMode(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestConsumeLeadingSeparator pins "exactly one, and only when leading".
//
// Mutation that turns it red: strip every leading "--" in a loop (the
// "-- -- x" row loses claude's separator), or strip a "--" anywhere (the
// "x -- y" row changes).
func TestConsumeLeadingSeparator(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		{nil, nil},
		{[]string{"--"}, []string{}},
		{[]string{"--", "which"}, []string{"which"}},
		{[]string{"--", "--", "x"}, []string{"--", "x"}},
		{[]string{"x", "--", "y"}, []string{"x", "--", "y"}},
		{[]string{"-p", "x"}, []string{"-p", "x"}},
	}
	for _, tc := range cases {
		got := ConsumeLeadingSeparator(tc.in)
		if len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got, tc.want)) {
			t.Errorf("ConsumeLeadingSeparator(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// parseClaudeHelpCommands extracts every command name, aliases included, from
// the "Commands:" section of `claude --help`. An entry line is indented by
// exactly two spaces and starts with `name` or `name|alias`; wrapped
// description lines are indented much further and are skipped.
func parseClaudeHelpCommands(help string) []string {
	var names []string
	in := false
	for _, line := range strings.Split(help, "\n") {
		if !in {
			in = strings.TrimSpace(line) == "Commands:"
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			break // the next section heading
		}
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			continue
		}
		field := strings.Fields(line)[0]
		names = append(names, strings.Split(field, "|")...)
	}
	return names
}

func TestParseClaudeHelpCommands(t *testing.T) {
	help := "Usage: claude\n\nOptions:\n  -h, --help   Display help\n\nCommands:\n" +
		"  agents [options]                      Manage background agents\n" +
		"  doctor                                Check the health of your Claude Code\n" +
		"                                        installation. Reads settings files\n" +
		"  plugin|plugins                        Manage Claude Code plugins\n" +
		"  stop|kill <id>                        Stop a background session.\n" +
		"\nExamples:\n  claude -p hi                          A later section is not commands.\n"
	got := parseClaudeHelpCommands(help)
	want := []string{"agents", "doctor", "plugin", "plugins", "stop", "kill"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseClaudeHelpCommands = %q, want %q", got, want)
	}
}

// TestClaudeSubcommands_MatchInstalledHelp is the drift alarm #676 asks for:
// when Claude Code adds, renames, or drops a subcommand, the list forgectl
// routes on no longer matches and this fails naming the difference. It needs
// a real `claude` on PATH, so it skips without one (CI has none) and under
// -short.
//
// Mutation that turns it red: delete "ultrareview" (or any entry) from
// claudeSubcommands, or add a name Claude does not list.
func TestClaudeSubcommands_MatchInstalledHelp(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the installed claude binary")
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not on PATH")
	}
	out, err := exec.CommandContext(t.Context(), path, "--help").Output() //nolint:gosec // G204: the claude binary LookPath resolved, with a fixed argument
	if err != nil {
		t.Fatalf("claude --help: %v", err)
	}
	installed := parseClaudeHelpCommands(string(out))
	if len(installed) == 0 {
		t.Fatalf("found no commands in `claude --help`; the parser no longer matches its layout:\n%s", out)
	}

	have := map[string]bool{}
	for _, s := range claudeSubcommands {
		have[s] = true
	}
	want := map[string]bool{}
	for _, s := range installed {
		want[s] = true
	}
	var missing, extra []string
	for s := range want {
		if !have[s] {
			missing = append(missing, s)
		}
	}
	for s := range have {
		if !want[s] {
			extra = append(extra, s)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Errorf("claudeSubcommands differs from the installed `claude --help`\n  listed by claude, missing here: %q\n  listed here, gone from claude: %q", missing, extra)
	}
}
