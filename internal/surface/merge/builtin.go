package merge

import (
	"path"
	"slices"
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

// builtinRefusedGlobs are paths no drain merge may touch, beside every
// top-level file (builtinRefusal): CI, agent and release inputs (.github,
// .claude, scripts, the shipped helper), and the whole Go tree, internal/**
// and cmd/**. Every Go package of the module is compiled into the binary the
// nightly release ships, the merge path, the bless ceremony, self-update and
// the shipped agent skill included, so none is merged by the drain; the
// per-repository allowlist cannot reach one. TestBuiltinRefusalsCoverTheBinary
// checks every package in the binary's `go list -deps` closure is refused.
//
// They are matched case-insensitively. A changed path is printable ASCII
// (config.CheckChangedPath), so ASCII case folding is all a case-folding
// checkout can do to it.
var builtinRefusedGlobs = []string{
	".github/**",
	".claude/**",
	"scripts/**",
	"helper/**",
	"internal/**",
	"cmd/**",
}

// builtinRefusedBase are file names refused in any directory: the module
// files that decide what code a build pulls in.
var builtinRefusedBase = []string{"go.mod", "go.sum", "go.work", "go.work.sum"}

// builtinRefusedBuildBase are other languages' module, lock, build and
// toolchain files, refused in any directory: a nested one still decides
// what a build fetches or runs. requirements*.txt is matched by prefix and
// suffix (builtinRefusal).
var builtinRefusedBuildBase = []string{
	"Cargo.toml", "Cargo.lock", "build.rs", "rust-toolchain", "rust-toolchain.toml",
	"package.json", "package-lock.json", "pnpm-lock.yaml", "pnpm-workspace.yaml", "yarn.lock", "bun.lockb", "bun.lock",
	".npmrc", ".yarnrc", ".yarnrc.yml",
	"pyproject.toml", "uv.lock", "poetry.lock", "Pipfile", "Pipfile.lock",
	"Gemfile", "Gemfile.lock",
	".tool-versions", ".mise.toml", "mise.toml",
	"flake.nix", "flake.lock",
	"Dockerfile", "Makefile",
}

// builtinRefusedAgentBase are the agent instruction files a coding agent
// loads from any directory it works in, refused at any depth.
var builtinRefusedAgentBase = []string{"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md"}

// builtinRefusedSegment are directory names refused at any depth: agent, CI
// and Cargo configuration a nested copy of still takes effect from.
var builtinRefusedSegment = []string{".claude", ".github", ".cargo"}

// builtinRefusal returns why p is refused whatever the config says, or "".
func builtinRefusal(p string) string {
	// The repository root holds the module, build, lint, release and agent
	// configuration (main.go, go.mod, .golangci.yml, .goreleaser.yaml,
	// release-please files, AGENTS.md, CLAUDE.md, a Makefile) and whatever
	// root configuration comes next, so no top-level file is merged.
	if !strings.Contains(p, "/") {
		return "top-level files (the module, build, lint, release and agent configuration at the repository root) are refused"
	}
	base := path.Base(p)
	for _, b := range builtinRefusedBase {
		if strings.EqualFold(base, b) {
			return "module files (" + strings.Join(builtinRefusedBase, ", ") + ") are refused in any directory"
		}
	}
	lowerBase := strings.ToLower(base)
	if slices.ContainsFunc(builtinRefusedBuildBase, func(b string) bool { return strings.EqualFold(base, b) }) ||
		strings.HasPrefix(lowerBase, "requirements") && strings.HasSuffix(lowerBase, ".txt") {
		return "build and toolchain files (" + strings.Join(builtinRefusedBuildBase, ", ") + ", requirements*.txt) are refused in any directory"
	}
	for _, b := range builtinRefusedAgentBase {
		if strings.EqualFold(base, b) {
			return "agent instruction files (" + strings.Join(builtinRefusedAgentBase, ", ") + ") are refused in any directory"
		}
	}
	lower := strings.ToLower(p)
	for _, g := range builtinRefusedGlobs {
		if config.MatchMergeGlob(g, lower) {
			return "it is under the built-in refused set (" + g + ")"
		}
	}
	for _, seg := range strings.Split(path.Dir(p), "/") {
		for _, sg := range builtinRefusedSegment {
			if strings.EqualFold(seg, sg) {
				return "a " + sg + " directory is refused at any depth"
			}
		}
	}
	return ""
}
