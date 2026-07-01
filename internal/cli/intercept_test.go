package cli

import (
	"reflect"
	"testing"
)

// TestLaunchIntercept guards the rule that only a `launch`/`cl` command token —
// optionally preceded by inert global flags (--no-icons) — routes into the
// launcher. A root --help/--version must NOT be skipped into it (that was a bug:
// `forgectl --version launch` used to launch instead of printing the version).
func TestLaunchIntercept(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantRest []string
		wantOK   bool
	}{
		{"bare launch", []string{"launch"}, []string{}, true},
		{"launch builder", []string{"launch", "-p", "hi"}, []string{"-p", "hi"}, true},
		{"cl alias with own-verb", []string{"cl", "which"}, []string{"which"}, true},
		{"inert flag before launch", []string{"--no-icons", "launch", "which"}, []string{"which"}, true},
		{"valued inert flag before cl", []string{"--no-icons=true", "cl"}, []string{}, true},
		{"root --version disables shortcut", []string{"--version", "launch"}, nil, false},
		{"root --help disables shortcut", []string{"--help"}, nil, false},
		{"unrelated verb", []string{"tmux", "ls"}, nil, false},
		{"empty", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rest, ok := launchIntercept(tc.args)
			if ok != tc.wantOK {
				t.Fatalf("launchIntercept(%v) ok = %v, want %v", tc.args, ok, tc.wantOK)
			}
			if ok && !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("launchIntercept(%v) rest = %v, want %v", tc.args, rest, tc.wantRest)
			}
		})
	}
}
