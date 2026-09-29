package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// mdrollBinary is the optional terminal reader `docs read` hands a document
// to (https://github.com/tokuhirom/mdroll). It is never required: without it,
// `docs read` opens the same document in the HTML reader.
const mdrollBinary = "mdroll"

// errDocNotIndexed reports a `docs read` target outside the indexed doc set.
var errDocNotIndexed = errors.New("not an indexed doc")

// docsReadRuntime is the seam `docs read` is tested through. Production wires
// the PATH lookup, a real child process, and `docs serve`; tests substitute the
// fallback so no server binds and no browser opens.
type docsReadRuntime struct {
	lookPath func(string) (string, error)
	fallback func(cmd *cobra.Command, deps module.Deps, idx *docspkg.Index, doc docspkg.Doc) error
}

func productionDocsReadRuntime() docsReadRuntime {
	return docsReadRuntime{
		lookPath: osexec.LookPath,
		fallback: serveDocsReadFallback,
	}
}

// newDocsReadCmd builds `forgectl docs read <file>`.
func newDocsReadCmd(deps module.Deps) *cobra.Command {
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "read <file>",
		Short: "Read one indexed doc in the terminal with mdroll, or in the HTML reader",
		Long: `read opens one document from the default doc set.

<file> is a path on disk (./README.md, docs/plans/thing.md) or a root-relative
name as ` + "`forgectl docs list`" + ` prints it (<root>/<path>). Either way it must
be a doc the index contains; anything outside the indexed roots is refused.

When mdroll (https://github.com/tokuhirom/mdroll) is on PATH, read runs it on
the document with --watch, handing it the terminal unchanged so its own keys
work. mdroll is optional: without it, read serves the doc set exactly as
` + "`forgectl docs serve --open`" + ` would and opens the browser on that document.

  forgectl docs read README.md
  forgectl docs read docs/plans/thing.md
  forgectl docs read -- -odd-name.md      a name starting with '-'
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDocsRead(cmd, deps, args[0], timeout, productionDocsReadRuntime())
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Second, "index walk deadline, e.g. 15s or 2m")
	return cmd
}

func runDocsRead(cmd *cobra.Command, deps module.Deps, target string, timeout time.Duration, rt docsReadRuntime) error {
	roots, err := resolveDocsRoots(nil, deps.Cfg.Docs)
	if err != nil {
		return err
	}
	opts, err := docsIndexOptions(deps.Cfg.Docs)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	idx, err := docspkg.NewIndexContext(ctx, roots, opts)
	cancel()
	if err != nil {
		return err
	}

	doc, path, err := resolveDocsReadTarget(idx, target)
	if err != nil {
		return err
	}

	mdroll, err := resolveMdroll(rt.lookPath)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "mdroll not found on PATH; opening %s in the HTML reader\n", termsafe.QuotePath(doc.RelPath))
		return rt.fallback(cmd, deps, idx, doc)
	}
	return runMdroll(cmd, mdroll, path)
}

// resolveDocsReadTarget maps what the operator typed onto one indexed doc and
// the on-disk path to hand a reader.
//
// Two spellings are accepted, tried in this order: a path on disk, matched by
// its canonical absolute path against the index (the same membership test the
// server's locate endpoint applies), then a root-relative "<root>/<path>" name
// as `docs list` prints it. Both end in Index.Resolve, so the path returned has
// passed the index's full containment chain — traversal, single-file roots,
// extension, and membership — and a file the reader would refuse to serve is
// refused here too.
func resolveDocsReadTarget(idx *docspkg.Index, target string) (docspkg.Doc, string, error) {
	if target == "" {
		return docspkg.Doc{}, "", fmt.Errorf("%w: empty name", errDocNotIndexed)
	}
	if _, statErr := os.Stat(target); statErr == nil {
		abs, err := filepath.Abs(target)
		if err != nil {
			return docspkg.Doc{}, "", fmt.Errorf("resolve %s: %w", termsafe.QuotePath(target), err)
		}
		canonical, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return docspkg.Doc{}, "", fmt.Errorf("resolve %s: %w", termsafe.QuotePath(target), err)
		}
		doc, ok := idx.FindByAbsPath(filepath.Clean(canonical))
		if !ok {
			return docspkg.Doc{}, "", fmt.Errorf("%w: %s is outside the indexed docs roots (see `forgectl docs list`)", errDocNotIndexed, termsafe.QuotePath(target))
		}
		return resolveIndexedDoc(idx, doc.RootLabel, doc.RelPath, target)
	}

	label, rel, ok := strings.Cut(filepath.ToSlash(target), "/")
	if ok && label != "" && rel != "" {
		for _, r := range idx.Roots() {
			if r.Label == label {
				return resolveIndexedDoc(idx, label, rel, target)
			}
		}
	}
	return docspkg.Doc{}, "", fmt.Errorf("%w: %s is neither a file in the indexed docs roots nor a <root>/<path> name (see `forgectl docs list`)", errDocNotIndexed, termsafe.QuotePath(target))
}

// resolveIndexedDoc runs (label, rel) through Index.Resolve and returns the
// indexed Doc for the path it resolved to.
func resolveIndexedDoc(idx *docspkg.Index, label, rel, target string) (docspkg.Doc, string, error) {
	path, err := idx.Resolve(label, rel)
	if err != nil {
		return docspkg.Doc{}, "", fmt.Errorf("%w: %s: %w", errDocNotIndexed, termsafe.QuotePath(target), err)
	}
	doc, ok := idx.FindByAbsPath(path)
	if !ok {
		return docspkg.Doc{}, "", fmt.Errorf("%w: %s", errDocNotIndexed, termsafe.QuotePath(target))
	}
	return doc, path, nil
}

// resolveMdroll resolves mdroll on PATH to an absolute path, the way
// surface_backend.go resolves tmux, cmux, and herdr: the program that runs is
// decided once, here, and never re-resolved by the exec layer.
func resolveMdroll(lookPath func(string) (string, error)) (string, error) {
	path, err := lookPath(mdrollBinary)
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

// mdrollArgs is the argument contract with mdroll, measured against mdroll
// 0.4.2's --help (clap): --watch reloads when the file changes, which is what
// the HTML reader's live reload would have done, and "--" ends option parsing
// so the path can never be read as a flag. The path is absolute already, but
// the separator does not depend on that.
func mdrollArgs(path string) []string {
	return []string{"--watch", "--", path}
}

// runMdroll runs mdroll on path with no shell, handing it the command's stdin,
// stdout, and stderr. In production those are the process's own terminal file
// descriptors, which os/exec passes through untouched, so mdroll's raw-mode
// keys (search, TOC, link picker) work as if it had been started directly.
// mdroll's exit status becomes forgectl's.
func runMdroll(cmd *cobra.Command, mdroll, path string) error {
	child := osexec.CommandContext(cmd.Context(), mdroll, mdrollArgs(path)...) //nolint:gosec // G204: absolute LookPath result, fixed flags, "--" before an index-resolved path
	child.Stdin = cmd.InOrStdin()
	child.Stdout = cmd.OutOrStdout()
	child.Stderr = cmd.ErrOrStderr()
	if err := child.Run(); err != nil {
		var exitErr *osexec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
			return newSilentCodedError(exitErr.ExitCode())
		}
		return fmt.Errorf("run mdroll: %w", err)
	}
	return nil
}

// serveDocsReadFallback is the no-mdroll path: `docs serve --open` over the
// same index, with the browser pointed at the resolved document instead of the
// reader's index. It blocks until Ctrl-C, like `docs serve` itself.
func serveDocsReadFallback(cmd *cobra.Command, deps module.Deps, idx *docspkg.Index, doc docspkg.Doc) error {
	openURL := func(addr string) string {
		return docspkg.ServerInfo{Addr: addr}.DocURL(doc.RootLabel, doc.RelPath)
	}
	return runDocsServeOpening(cmd, deps, idx, "", openURL, "", productionDocsServeRuntime())
}
