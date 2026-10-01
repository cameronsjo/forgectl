// Package gitstate answers audit.GitStatusFunc with git: for a batch of
// paths in one working tree, which are tracked, which are untracked but not
// ignored, and which are ignored (forgectl#14, lane 2). It runs two hardened
// gitenv.Local `git ls-files` calls per batch, so a scanned repo's config
// cannot make the question run a program or reach the network, under a
// per-repo deadline, so a FIFO it must read cannot hang it.
package gitstate

import (
	"context"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/audit"
	fexec "github.com/cameronsjo/forgectl/internal/exec"
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

// ignoredArgs ask for the ignored set by name. A path is called ignored only
// when git lists it here: one git lists nowhere (a FIFO, which ls-files never
// reports) is unknown, never assumed ignored.
var ignoredArgs = []string{"ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--"}

// DefaultTimeout bounds one repo's status, every batch together. A FIFO
// .gitignore or .git/index makes ls-files wait forever for a writer; the
// deadline kills git's process group and the repo reads as unanswered.
const DefaultTimeout = 30 * time.Second

// Status runs git ls-files in repo over rels and returns each path git
// reports: tracked, untracked-unignored, or ignored. A path git lists
// nowhere is absent from the map. Each pathspec carries :(literal), so a
// name holding glob characters matches only itself. Any failed batch fails
// the whole call, so a caller never mistakes an unanswered path for an
// answered one. ctx bounds the whole call.
func Status(ctx context.Context, r gitenv.Runner, repo string, rels []string) (map[string]audit.GitState, error) {
	out := make(map[string]audit.GitState, len(rels))
	for _, batch := range batches(rels) {
		specs := make([]string, 0, len(batch))
		for _, rel := range batch {
			specs = append(specs, ":(literal)"+rel)
		}
		stdout, err := gitenv.Run(ctx, r, gitenv.Local, append(append([]string{"-C", repo}, lsFilesArgs...), specs...)...)
		if err != nil {
			return nil, err
		}
		parse(stdout, out)
		ignored, err := gitenv.Run(ctx, r, gitenv.Local, append(append([]string{"-C", repo}, ignoredArgs...), specs...)...)
		if err != nil {
			return nil, err
		}
		for _, p := range strings.Split(ignored, "\x00") {
			if _, seen := out[p]; p != "" && !seen {
				out[p] = audit.GitIgnored
			}
		}
	}
	return out, nil
}

// Func binds Status to a context and runner as an audit.GitStatusFunc, each
// repo under its own DefaultTimeout.
func Func(ctx context.Context, r gitenv.Runner) audit.GitStatusFunc {
	return FuncWithTimeout(ctx, r, DefaultTimeout)
}

// FuncWithTimeout is Func with the per-repo deadline named. Each repo's
// calls run in a process group of their own, killed whole at the deadline.
func FuncWithTimeout(ctx context.Context, r gitenv.Runner, timeout time.Duration) audit.GitStatusFunc {
	return func(repo string, rels []string) (map[string]audit.GitState, error) {
		rctx, cancel := context.WithTimeout(fexec.WithProcessGroup(ctx), timeout)
		defer cancel()
		return Status(rctx, r, repo, rels)
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
