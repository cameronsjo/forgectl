package organize

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/herdr"
)

func pane(term, cwd, title string) herdr.Pane {
	return herdr.Pane{TerminalID: term, CWD: cwd, TerminalTitleStripped: title}
}

func TestMatchKey(t *testing.T) {
	if got, want := matchKey(pane("t", "/a/b", "vim")), "/a/b :: vim"; got != want {
		t.Errorf("matchKey = %q, want %q", got, want)
	}
	if got, want := matchKey(pane("t", "", "vim")), " :: vim"; got != want {
		t.Errorf("null cwd: matchKey = %q, want %q", got, want)
	}
}

func TestClassify(t *testing.T) {
	cfg := Config{
		Default: "misc",
		Rules: []Rule{
			{Glob: "*/forge/* :: *", Workspace: "forge"},
			{Glob: "*/home/* :: *", Workspace: "home"},
		},
	}
	tests := []struct {
		name          string
		panes         []herdr.Pane
		wantRule      int
		wantWorkspace string
		wantSortTerm  string
	}{
		{
			name:          "first pane matching decides, in pane order",
			panes:         []herdr.Pane{pane("t1", "/p/home/x", "a"), pane("t2", "/p/forge/y", "b")},
			wantRule:      1,
			wantWorkspace: "home",
			wantSortTerm:  "t1",
		},
		{
			name:          "later pane decides when earlier pane matches nothing",
			panes:         []herdr.Pane{pane("t1", "/p/other", "a"), pane("t2", "/p/forge/y", "b")},
			wantRule:      0,
			wantWorkspace: "forge",
			wantSortTerm:  "t2",
		},
		{
			name:          "no match goes to default and sorts by the first pane",
			panes:         []herdr.Pane{pane("t1", "/p/other", "a"), pane("t2", "/p/else", "b")},
			wantRule:      -1,
			wantWorkspace: "misc",
			wantSortTerm:  "t1",
		},
		{
			name:          "null cwd matches on the empty-cwd key",
			panes:         []herdr.Pane{pane("t1", "", "a")},
			wantRule:      -1,
			wantWorkspace: "misc",
			wantSortTerm:  "t1",
		},
		{
			name:          "rules apply in order within one pane",
			panes:         []herdr.Pane{pane("t1", "/p/forge/home/x", "a")},
			wantRule:      0,
			wantWorkspace: "forge",
			wantSortTerm:  "t1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule, ws, sp := Classify(cfg, tt.panes)
			if rule != tt.wantRule || ws != tt.wantWorkspace || sp.TerminalID != tt.wantSortTerm {
				t.Errorf("Classify = (%d, %q, %q), want (%d, %q, %q)",
					rule, ws, sp.TerminalID, tt.wantRule, tt.wantWorkspace, tt.wantSortTerm)
			}
		})
	}
}

func TestClassify_NullCWDMatchesTitleOnlyRule(t *testing.T) {
	cfg := Config{Default: "misc", Rules: []Rule{{Glob: " :: htop", Workspace: "sys"}}}
	rule, ws, _ := Classify(cfg, []herdr.Pane{pane("t1", "", "htop")})
	if rule != 0 || ws != "sys" {
		t.Errorf("Classify = (%d, %q), want (0, sys): a null cwd must match on \" :: title\"", rule, ws)
	}
}

func TestClassify_GlobSeesTheRawWorktreeCWD(t *testing.T) {
	cfg := Config{Default: "misc", Rules: []Rule{{Glob: "*/.claude/worktrees/* :: *", Workspace: "wt"}}}
	rule, ws, _ := Classify(cfg, []herdr.Pane{pane("t1", "/p/forge/x/.claude/worktrees/feat", "a")})
	if rule != 0 || ws != "wt" {
		t.Errorf("Classify = (%d, %q), want (0, wt): the match key must carry the raw worktree cwd", rule, ws)
	}
}
