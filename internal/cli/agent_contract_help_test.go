package cli

import (
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

// TestAgentContractHelpText pins the help items of forgectl#1150 and #1095:
// env says it needs a git repository, env set carries the safe value
// sources, surface brief says which exit-1 outcome is safe to retry, and
// the --json flags that named no keys now name them.
func TestAgentContractHelpText(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	find := func(path ...string) string {
		t.Helper()
		c, _, err := root.Find(path)
		if err != nil || c.Name() != path[len(path)-1] {
			t.Fatalf("no command %v: %v", path, err)
		}
		help := c.Long
		if f := c.Flags().Lookup("json"); f != nil {
			help += "\n" + f.Usage
		}
		return help
	}
	for _, tc := range []struct {
		path []string
		want []string
	}{
		{[]string{"env"}, []string{"inside a git repository"}},
		{[]string{"env", "set"}, []string{"op read", "--clipboard", "Never inline the secret", "git repository"}},
		{[]string{"surface", "brief"}, []string{"only refused is safe to retry", "unconfirmed already sent Enter"}},
		{[]string{"config"}, []string{`"entries"`, `"env_resolved"`}},
		{[]string{"doctor"}, []string{`"checks"`, `"healthy"`}},
		{[]string{"sessions", "why"}, []string{`"session_id"`, `"key_decisions"`}},
		{[]string{"sessions", "last"}, []string{`"session_id"`, `"artifacts"`}},
		{[]string{"desk", "status"}, []string{`"found"`, `"pending"`, `"events"`}},
		{[]string{"desk", "plan"}, []string{`"waves"`, `"warnings"`}},
	} {
		help := find(tc.path...)
		for _, w := range tc.want {
			if !strings.Contains(help, w) {
				t.Errorf("%s help lacks %q", strings.Join(tc.path, " "), w)
			}
		}
	}
	if !strings.Contains(root.Long, "--json") || !strings.Contains(root.Long, "menu --json") {
		t.Errorf("root help names neither --json nor menu --json: %q", root.Long)
	}
}
