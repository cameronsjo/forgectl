// Package gitstate answers audit.GitStatusFunc with git: for a batch of
// paths in one working tree, which are tracked and which are untracked but
// not ignored (forgectl#14, lane 2). It runs one hardened gitenv.Local
// `git ls-files` per batch, so a scanned repo's config cannot make the
// question run a program or reach the network.
package gitstate

import (
	"context"
	"strings"

	"github.com/cameronsjo/forgectl/internal/audit"
	"github.com/cameronsjo/forgectl/internal/gitenv"
)

// Batch bounds. One call carries every path of a repo when they fit;
// past either bound the paths split across calls, keeping argv well under
// the smallest ARG_MAX forgectl runs on (macOS, 1 MiB including the
// environment).
const (
	maxBatchBytes = 64 << 10
	maxBatchPaths = 2000
)

// lsFilesArgs precede the pathspecs: -z for NUL-separated records whatever
// a name holds, -t for the tag that tells a tracked path ("H", and the
// other index tags) from an untracked one ("?"), --cached and --others with
// --exclude-standard so an ignored path appears in neither list.
var lsFilesArgs = []string{"ls-files", "-z", "-t", "--cached", "--others", "--exclude-standard", "--"}

// Status runs git ls-files in repo over rels and returns the tracked and
// untracked-unignored ones; a path it omits is ignored. Each pathspec carries
// :(literal), so a name holding glob characters matches only itself. Any
// failed batch fails the whole call, so a caller never mistakes an
// unanswered path for an ignored one.
func Status(ctx context.Context, r gitenv.Runner, repo string, rels []string) (map[string]audit.GitState, error) {
	out := make(map[string]audit.GitState, len(rels))
	for _, batch := range batches(rels) {
		args := append([]string{"-C", repo}, lsFilesArgs...)
		for _, rel := range batch {
			args = append(args, ":(literal)"+rel)
		}
		stdout, err := gitenv.Run(ctx, r, gitenv.Local, args...)
		if err != nil {
			return nil, err
		}
		parse(stdout, out)
	}
	return out, nil
}

// Func binds Status to a context and runner as an audit.GitStatusFunc.
func Func(ctx context.Context, r gitenv.Runner) audit.GitStatusFunc {
	return func(repo string, rels []string) (map[string]audit.GitState, error) {
		return Status(ctx, r, repo, rels)
	}
}

// batches splits rels so no batch passes maxBatchBytes of pathspec or
// maxBatchPaths paths. A single path past the byte bound rides alone.
func batches(rels []string) [][]string {
	var out [][]string
	var cur []string
	size := 0
	for _, rel := range rels {
		n := len(rel) + len(":(literal)") + 1
		if len(cur) > 0 && (size+n > maxBatchBytes || len(cur) >= maxBatchPaths) {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, rel)
		size += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// parse reads -z -t records ("<tag> <path>\x00") into states. A tracked
// record wins over an untracked one for the same path.
func parse(stdout string, states map[string]audit.GitState) {
	for _, rec := range strings.Split(stdout, "\x00") {
		if len(rec) < 3 || rec[1] != ' ' {
			continue
		}
		p := rec[2:]
		if rec[0] == '?' {
			if _, seen := states[p]; !seen {
				states[p] = audit.GitUntracked
			}
			continue
		}
		states[p] = audit.GitTracked
	}
}
