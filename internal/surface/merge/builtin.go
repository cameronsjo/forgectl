package merge

import (
	"path"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
)

// The refusals below hold whatever [surface.merge] says (ADR-0011,
// 2026-10-09 amendment, decision 4). The config can narrow a merge, never
// widen it past these.

// refusedRepo is a repository that installs live from its default branch,
// or holds the machine's own configuration: a merge there runs in every
// session at the next start with no release in between.
type refusedRepo struct {
	name string
	id   int64
}

// builtinRefusedRepos are the repositories a workbench marketplace entry
// installs from, plus cameronsjo/cadence, cadence-lab, workbench and the
// dotfiles repositories. Names compare case-insensitively against both the
// row's recorded name and GitHub's current one; ids (read from GitHub on
// 2026-10-09) catch a rename.
//
// debt: a static list, read from workbench's marketplace.json on 2026-10-09;
// upgrade to reading the installed marketplace manifests when a plugin
// source is added to workbench.
var builtinRefusedRepos = []refusedRepo{
	{"cameronsjo/artificer-design-system", 1230351328},
	{"cameronsjo/artificer-voice", 1325544140},
	{"cameronsjo/bosun", 1120800636},
	{"cameronsjo/cadence", 1175723838},
	{"cameronsjo/cadence-lab", 1175730122},
	{"cameronsjo/dotfiles", 1106285560},
	{"cameronsjo/dotfiles-core", 1341870400},
	{"cameronsjo/homelab", 1122046219},
	{"cameronsjo/llm-council", 1118644239},
	{"cameronsjo/media-mcp", 1111887667},
	{"cameronsjo/mouse-mcp", 1111381191},
	{"cameronsjo/obaass", 1155866155},
	{"cameronsjo/obsidi-backup", 1141968545},
	{"cameronsjo/obsidi-claude", 1123020888},
	{"cameronsjo/obsidi-mcp", 1156328859},
	{"cameronsjo/workbench", 1106213945},
}

// refusedRepoFor returns the built-in entry name or id hits, if any.
func refusedRepoFor(name string, id int64) (refusedRepo, bool) {
	for _, r := range builtinRefusedRepos {
		if strings.EqualFold(r.name, name) || (id != 0 && r.id == id) {
			return r, true
		}
	}
	return refusedRepo{}, false
}

// builtinRefusedGlobs are paths no drain merge may touch: CI and agent
// configuration, CodeRabbit's config, and the gate's own code (the merge
// policy, the status reads, config resolution, launch, git and process
// plumbing, the host-pinned gh runner, and the bless and signing helpers).
// They are matched case-insensitively, so a case-folding checkout cannot
// slip a differently cased spelling past them.
var builtinRefusedGlobs = []string{
	".github/**",
	".claude/**",
	".coderabbit.yaml",
	".coderabbit.yml",
	"internal/surface/**",
	"internal/cli/surface_*",
	"internal/config/**",
	"internal/launch/**",
	"internal/gitenv/**",
	"internal/exec/**",
	"internal/githubauth/**",
	"internal/bless/**",
	"internal/cli/workflow_bless*",
	"internal/selfupdate/**",
}

// builtinRefusedBase are file names refused in any directory: the module
// files that decide what code a build pulls in.
var builtinRefusedBase = []string{"go.mod", "go.sum", "go.work", "go.work.sum"}

// builtinRefusal returns why p is refused whatever the config says, or "".
func builtinRefusal(p string) string {
	base := path.Base(p)
	for _, b := range builtinRefusedBase {
		if strings.EqualFold(base, b) {
			return "module files (" + strings.Join(builtinRefusedBase, ", ") + ") are refused in any directory"
		}
	}
	lower := strings.ToLower(p)
	for _, g := range builtinRefusedGlobs {
		if config.MatchMergeGlob(g, lower) {
			return "it is under the built-in refused set (" + g + ")"
		}
	}
	return ""
}
