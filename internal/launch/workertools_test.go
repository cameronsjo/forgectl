package launch

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// TestWorkerToolsArg: a list of capitalized names becomes one --tools value
// with duplicates dropped; unset gives nothing; every shape Claude Code would
// read as "no tools" or drop silently is refused.
func TestWorkerToolsArg(t *testing.T) {
	if got, err := workerToolsArg(nil); got != "" || err != nil {
		t.Errorf("nil = %q, %v; want unset", got, err)
	}
	got, err := workerToolsArg([]string{"Bash", "Read", "Bash", "WebFetch"})
	if err != nil || got != "Bash,Read,WebFetch" {
		t.Errorf("list = %q, %v; want Bash,Read,WebFetch", got, err)
	}
	for _, bad := range [][]string{
		{},
		{"default"},
		{"read"},             // lowercase: 2.1.305 started with no tools at all
		{"Read", ""},         // empty entry
		{"Read,Bash"},        // a comma would split into names forgectl never checked
		{"mcp__github__x"},   // --tools takes built-ins only
		{"Bash(git *)"},      // a permission rule, not a tool name
		{"Read Bash"},        // line separator
		{"Re ad"},            // space
		{"Read", "Edit\x00"}, // control character
	} {
		if _, err := workerToolsArg(bad); !errors.Is(err, ErrWorkerTools) {
			t.Errorf("%q = %v, want ErrWorkerTools", bad, err)
		}
	}
}

// toolsArg returns the value after --tools in args, or "" with ok false.
func toolsArg(args []string) (string, bool) {
	if i := slices.Index(args, "--tools"); i >= 0 && i+1 < len(args) {
		return args[i+1], true
	}
	return "", false
}

// TestWorkerLaunchPassesTheToolsList: a claude worker gets --tools after
// --settings and before the prompt; no list means no flag; a bad list refuses
// the launch; a codex worker never gets the flag.
func TestWorkerLaunchPassesTheToolsList(t *testing.T) {
	built, err := buildWorker(t, "claude", config.LaunchWorker{Tools: []string{"Bash", "Read"}})
	if err != nil {
		t.Fatalf("BuildInvocation: %v", err)
	}
	args := built.Invocation.Args
	got, ok := toolsArg(args)
	if !ok || got != "Bash,Read" {
		t.Errorf("--tools = %q (present %v), want Bash,Read; args %q", got, ok, args)
	}
	if slices.Index(args, "--tools") < slices.Index(args, "--settings") {
		t.Errorf("--tools comes before --settings: %q", args)
	}

	built, err = buildWorker(t, "claude", config.LaunchWorker{})
	if err != nil {
		t.Fatalf("BuildInvocation: %v", err)
	}
	if _, ok := toolsArg(built.Invocation.Args); ok {
		t.Errorf("no tools key still passed --tools: %q", built.Invocation.Args)
	}

	if _, err := buildWorker(t, "claude", config.LaunchWorker{Tools: []string{}}); !errors.Is(err, ErrWorkerTools) {
		t.Errorf("an empty list launched: %v", err)
	}
	built, err = buildWorker(t, "codex", config.LaunchWorker{Tools: []string{"read"}})
	if err != nil {
		t.Fatalf("codex worker read the claude tools list: %v", err)
	}
	if _, ok := toolsArg(built.Invocation.Args); ok {
		t.Errorf("codex worker got --tools: %q", built.Invocation.Args)
	}
}

// TestWorkerPostureReportsTheToolsList: launch which shows the list and says
// when the launch would refuse it; a codex worker shows none.
func TestWorkerPostureReportsTheToolsList(t *testing.T) {
	w := DefaultWorkerPosture(config.LaunchConfig{Worker: config.LaunchWorker{Tools: []string{"Read"}}})
	if !slices.Equal(w.Tools, []string{"Read"}) || w.Refused != "" {
		t.Errorf("good list: tools %q refused %q", w.Tools, w.Refused)
	}
	w = DefaultWorkerPosture(config.LaunchConfig{Worker: config.LaunchWorker{Tools: []string{"read"}}})
	if !strings.Contains(w.Refused, "[launch.worker] tools") {
		t.Errorf("bad list not reported as refused: %q", w.Refused)
	}
	lc := config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "codex"}, Worker: config.LaunchWorker{Tools: []string{"read"}}}
	if w := DefaultWorkerPosture(lc); w.Tools != nil || w.Refused != "" {
		t.Errorf("codex worker posture read the claude tools list: %+v", w)
	}
}
