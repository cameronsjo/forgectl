package organize

import "github.com/cameronsjo/forgectl/internal/herdr"

// matchKey is the string a rule's glob sees for one pane: the raw cwd and the
// stripped title. A pane with no cwd or title contributes an empty side.
func matchKey(p herdr.Pane) string {
	return p.CWD + " :: " + p.TerminalTitleStripped
}

// Classify decides which workspace a tab belongs in. It walks the tab's panes
// in order and the first pane that matches any rule decides (the rule index is
// returned, and that pane is the sort pane). No match returns rule -1, the
// default workspace, and the first pane as the sort pane. panes must be
// non-empty.
func Classify(cfg Config, panes []herdr.Pane) (rule int, workspace string, sortPane herdr.Pane) {
	for _, p := range panes {
		key := matchKey(p)
		for i, r := range cfg.Rules {
			if Match(r.Glob, key) {
				return i, r.Workspace, p
			}
		}
	}
	return -1, cfg.Default, panes[0]
}
