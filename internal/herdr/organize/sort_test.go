package organize

import "testing"

func TestSortKey(t *testing.T) {
	const root = "/home/u/Projects"
	tests := []struct {
		name     string
		cwd      string
		wantWing string
		wantRepo string
		wantCWD  string
	}{
		{"wing and repo", root + "/forge/tool/sub", "forge", "tool", root + "/forge/tool/sub"},
		{"worktree suffix stripped", root + "/forge/tool/.claude/worktrees/feat", "forge", "tool", root + "/forge/tool"},
		{"bare worktrees dir stripped", root + "/forge/tool/.claude/worktrees", "forge", "tool", root + "/forge/tool"},
		{"one part under root", root + "/forge", "forge", "", root + "/forge"},
		{"root itself sorts under tilde", root, "~", "", root},
		{"sibling prefix dir is outside", "/home/u/Projects2/x/y", "~", "", "/home/u/Projects2/x/y"},
		{"parent is outside", "/home/u", "~", "", "/home/u"},
		{"no cwd sorts under tilde", "", "~", "", ""},
		{"relative cwd is outside", "x/y", "~", "", "x/y"},
		{"host-tree repo files under host and owner", root + "/github.com/owner/name", "github.com", "owner", root + "/github.com/owner/name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := sortKeyFor(root, tt.cwd, "t1")
			if k.wing != tt.wantWing || k.repo != tt.wantRepo || k.cwd != tt.wantCWD {
				t.Errorf("sortKeyFor(%q) = {%q %q %q}, want {%q %q %q}",
					tt.cwd, k.wing, k.repo, k.cwd, tt.wantWing, tt.wantRepo, tt.wantCWD)
			}
		})
	}
}

func TestSortKey_Less(t *testing.T) {
	const root = "/r"
	tests := []struct {
		name string
		a, b sortKey
		want bool
	}{
		{"wing first", sortKeyFor(root, "/r/a/z", "t9"), sortKeyFor(root, "/r/b/a", "t1"), true},
		{"then repo", sortKeyFor(root, "/r/a/x", "t9"), sortKeyFor(root, "/r/a/y", "t1"), true},
		{"then cwd", sortKeyFor(root, "/r/a/x/1", "t9"), sortKeyFor(root, "/r/a/x/2", "t1"), true},
		{"tab id compares as a string: t10 before t9", sortKeyFor(root, "/r/a/x", "t10"), sortKeyFor(root, "/r/a/x", "t9"), true},
		{"tab id string order is not reversed", sortKeyFor(root, "/r/a/x", "t9"), sortKeyFor(root, "/r/a/x", "t10"), false},
		{"outside root sorts under tilde, after letters", sortKeyFor(root, "/r/zed/x", "t1"), sortKeyFor(root, "/elsewhere", "t1"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.less(tt.b); got != tt.want {
				t.Errorf("less = %v, want %v", got, tt.want)
			}
		})
	}
}
