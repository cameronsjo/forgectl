package launch

import (
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestIsClaudeSubcommandCall pins which Claude argv is a byte-clean
// subcommand run.
//
// Mutation that turns it red: drop the args[1]-after-"--" check (the
// "-- mcp list" row flips), or drop the agents exclusion (the agents rows
// flip to true).
func TestIsClaudeSubcommandCall(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"hello"}, false},
		{[]string{"--resume", "abc"}, false},
		{[]string{"mcp", "list"}, true},
		{[]string{"update"}, true},
		{[]string{"plugins"}, true},
		{[]string{"doctor"}, true},
		{[]string{"-p", "hi"}, false},
		// claude dispatches a subcommand after its own `--` too.
		{[]string{"--", "mcp", "list"}, true},
		{[]string{"--", "hello"}, false},
		{[]string{"--"}, false},
		// A subcommand name is only a subcommand in first position; later it
		// is a prompt word or a flag value.
		{[]string{"--model", "opus", "mcp"}, false},
		{[]string{"explain", "doctor"}, false},
		// agents starts sessions, so it keeps its posture in either slot.
		{[]string{"agents"}, false},
		{[]string{"--", "agents"}, false},
	}
	for _, tc := range cases {
		if got := IsClaudeSubcommandCall(tc.args); got != tc.want {
			t.Errorf("IsClaudeSubcommandCall(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestIsClaudeHelpOrVersion pins "first argument only". Later, a help token
// can be a flag's value: `-p --append-system-prompt --help "<task>"` runs the
// task in claude 2.1.285.
//
// Mutation that turns it red: scan every argument instead of args[0] (the
// value-slot rows flip to true).
func TestIsClaudeHelpOrVersion(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"--help"}, true},
		{[]string{"-h"}, true},
		{[]string{"--version"}, true},
		{[]string{"-v"}, true},
		{[]string{"-p", "--append-system-prompt", "--help", "hi"}, false},
		{[]string{"--append-system-prompt", "--help", "hi"}, false},
		{[]string{"--model", "opus", "-v"}, false},
		{[]string{"--", "--help"}, false},
	}
	for _, tc := range cases {
		if got := IsClaudeHelpOrVersion(tc.args); got != tc.want {
			t.Errorf("IsClaudeHelpOrVersion(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestIsClaudePrintMode pins which argv takes the print posture.
//
// Mutation that turns it red: drop "--output-format" from the flag list (both
// output-format rows flip), drop the `=value` prefix match (the
// "--output-format=stream-json" row flips), or drop the `--` stop (the
// "-- hi -p" row flips; after `--` a bare token puts -p in flag position). Match a print flag in any slot again, ignoring the token
// before it (every value-slot row flips to true), or drop "--resume" from
// claudeNoValueFlags (the "--resume -p" row flips to false). Stop at every
// `--` again (the "--append-system-prompt -- -p" rows flip to false), read a
// `--` as a value after any value-assumed flag instead of only a known one
// (the "--some-future-flag --" row flips to true), drop the `prev == "--"`
// case from inFlagPosition (the value-`--` rows flip to false), or split
// glued short clusters into their flags (the "-cp" and "-pc" rows flip).
// Judge a `--` by the previous token alone again, ignoring its slot (the
// "--model --model -- -p x" row flips to true).
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
		{[]string{"--", "hi", "-p"}, false},
		// Flag position: after a bare token, a --flag=value, a boolean flag,
		// or an optional-value flag (which never takes a dash-prefixed token).
		{[]string{"hi", "-p"}, true},
		{[]string{"--model=opus", "-p", "hi"}, true},
		{[]string{"--verbose", "--print", "hi"}, true},
		{[]string{"--resume", "-p", "hi"}, true},
		// A value slot: the option's value, not print mode.
		{[]string{"--append-system-prompt", "-p", "task"}, false},
		{[]string{"--model", "--print", "hi"}, false},
		{[]string{"--append-system-prompt", "--output-format", "hi"}, false},
		{[]string{"--system-prompt", "--output-format=json", "hi"}, false},
		// An unknown flag is assumed to take a value.
		{[]string{"--some-future-flag", "-p", "hi"}, false},
		// A `--` after a known required-value flag is its value, not the end
		// of options: `claude --append-system-prompt -- -p hi` runs in print
		// mode (2.1.285). After an unknown flag, a boolean one, or a `--`
		// already taken as a value, it ends the options, and the run gets the
		// builder posture (forgectl#766).
		{[]string{"--append-system-prompt", "--", "-p", "hi"}, true},
		{[]string{"-n", "--", "--print", "hi"}, true},
		{[]string{"--model", "--", "--model", "--", "-p"}, true},
		{[]string{"--some-future-flag", "--", "-p", "hi"}, false},
		{[]string{"--verbose", "--", "-p", "hi"}, false},
		{[]string{"--model=opus", "--", "-p", "hi"}, false},
		{[]string{"--model", "--", "--", "-p", "hi"}, false},
		// Slot state, not just the previous token: a value flag taken as a
		// value takes nothing, so this `--` ends the options.
		{[]string{"--model", "--model", "--", "-p", "x"}, false},
		// A known value flag's dash-prefixed value leaves the next token in
		// flag position: claude takes "-x" as the model, then -p.
		{[]string{"--model", "-x", "-p", "hi"}, true},
		// A maybe-value (after an unknown flag) keeps hiding what follows.
		{[]string{"--some-future-flag", "--model", "--", "-p", "hi"}, false},
		// Glued short flags are not print mode: claude 2.1.285 opens the
		// interactive session for `-cp` and `-pc` under a terminal.
		{[]string{"-cp", "hi"}, false},
		{[]string{"-pc", "hi"}, false},
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

// TestClaudeFlagLists_MatchInstalledHelp pins claudeNoValueFlags and
// claudeValueFlags to the installed `claude --help` (forgectl#766), as
// TestClaudeSubcommands_MatchInstalledHelp pins the subcommands. An option
// with a `<value>` is a value flag; every other option (a boolean or an
// `[optional]` value, which claude never takes from a dash-prefixed token)
// is a no-value flag. If a listed no-value flag starts taking a value,
// interactive runs would be routed to print mode with nothing failing; this
// is what fails instead. It needs a real `claude` on PATH, so it skips
// without one (CI has none) and under -short.
//
// Mutation that turns it red: delete "--verbose" from claudeNoValueFlags or
// "--settings" from claudeValueFlags, or move an entry from one list to the
// other.
func TestClaudeFlagLists_MatchInstalledHelp(t *testing.T) {
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
	noValue, value := parseClaudeHelpOptions(string(out))
	if len(noValue) == 0 || len(value) == 0 {
		t.Fatalf("found no options in `claude --help`; the parser no longer matches its layout:\n%s", out)
	}
	diffFlagSet(t, "claudeNoValueFlags", claudeNoValueFlags, noValue)
	diffFlagSet(t, "claudeValueFlags", claudeValueFlags, value)
}

// diffFlagSet reports every flag listed in have but not in want, and the
// reverse.
func diffFlagSet(t *testing.T, name string, have map[string]bool, want []string) {
	t.Helper()
	wantSet := map[string]bool{}
	for _, f := range want {
		wantSet[f] = true
	}
	var missing, extra []string
	for f := range wantSet {
		if !have[f] {
			missing = append(missing, f)
		}
	}
	for f := range have {
		if !wantSet[f] {
			extra = append(extra, f)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Errorf("%s differs from the installed `claude --help`\n  listed by claude, missing here: %q\n  listed here, not so listed by claude: %q", name, missing, extra)
	}
}

// parseClaudeHelpOptions splits the "Options:" section of `claude --help`
// into the flags that take no value (booleans and `[optional]` values) and
// the ones that take a required `<value>`. An entry line is indented by
// exactly two spaces and starts with "-"; its names are comma-separated and
// end where the value placeholder or the two-space gap before the
// description begins.
func parseClaudeHelpOptions(help string) (noValue, value []string) {
	in := false
	for _, line := range strings.Split(help, "\n") {
		if !in {
			in = strings.TrimSpace(line) == "Options:"
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			break // the next section heading
		}
		if !strings.HasPrefix(line, "  -") {
			continue // a wrapped description line
		}
		spec := strings.TrimPrefix(line, "  ")
		if i := strings.Index(spec, "  "); i >= 0 {
			spec = spec[:i]
		}
		takesValue := false
		if i := strings.IndexAny(spec, "<["); i >= 0 {
			takesValue = spec[i] == '<'
			spec = spec[:i]
		}
		for _, name := range strings.Split(spec, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if takesValue {
				value = append(value, name)
			} else {
				noValue = append(noValue, name)
			}
		}
	}
	return noValue, value
}

func TestParseClaudeHelpOptions(t *testing.T) {
	help := "Usage: claude [options]\n\nArguments:\n  prompt   Your prompt\n\nOptions:\n" +
		"  --add-dir <directories...>            Additional directories to allow tool\n" +
		"                                        access to\n" +
		"  --allowedTools, --allowed-tools <tools...>\n" +
		"      Comma or space-separated list of tool names to allow\n" +
		"  -c, --continue                        Continue the most recent conversation\n" +
		"  -d, --debug [filter]                  Enable debug mode\n" +
		"  --exclude-dynamic-system-prompt-sections\n" +
		"      Move per-machine sections\n" +
		"  -n, --name <name>                     Set a display name\n" +
		"\nCommands:\n  mcp                                   Configure MCP servers\n"
	noValue, value := parseClaudeHelpOptions(help)
	wantNoValue := []string{"-c", "--continue", "-d", "--debug", "--exclude-dynamic-system-prompt-sections"}
	wantValue := []string{"--add-dir", "--allowedTools", "--allowed-tools", "-n", "--name"}
	if !reflect.DeepEqual(noValue, wantNoValue) {
		t.Errorf("noValue = %q, want %q", noValue, wantNoValue)
	}
	if !reflect.DeepEqual(value, wantValue) {
		t.Errorf("value = %q, want %q", value, wantValue)
	}
}
