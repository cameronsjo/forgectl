package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

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
// fallback so no server binds and no browser opens, and the terminal checks so
// both sides of them run under `go test`.
type docsReadRuntime struct {
	lookPath         func(string) (string, error)
	stdinIsTerminal  func(io.Reader) bool
	stdoutIsTerminal func(io.Writer) bool
	fallback         func(cmd *cobra.Command, deps module.Deps, idx *docspkg.Index, doc docspkg.Doc) error
}

func productionDocsReadRuntime() docsReadRuntime {
	return docsReadRuntime{
		lookPath:         osexec.LookPath,
		stdinIsTerminal:  docsReadInputIsTerminal,
		stdoutIsTerminal: docsReadOutputIsTerminal,
		fallback:         serveDocsReadFallback,
	}
}

// docsReadOutputIsTerminal inspects Cobra's actual output sink, as
// k8sOutputIsTerminal does, rather than assuming os.Stdout.
func docsReadOutputIsTerminal(out io.Writer) bool {
	fdWriter, ok := out.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(fdWriter.Fd()))
}

// docsReadInputIsTerminal is docsReadOutputIsTerminal for Cobra's input source.
func docsReadInputIsTerminal(in io.Reader) bool {
	fdReader, ok := in.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(fdReader.Fd()))
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
work. mdroll never fetches remote images for read (--no-remote-images).

mdroll is optional. Without it, and with stdin and stdout both terminals, read
serves the doc set as ` + "`forgectl docs serve --open`" + ` would and opens the
browser on that document, holding the terminal until Ctrl-C. Without it and
without both terminals (an agent, a pipe, </dev/null), read starts nothing: it
prints the document's absolute path and exits 0.

  forgectl docs read README.md
  forgectl docs read docs/plans/thing.md
  forgectl docs read -- -odd-name.md      a name starting with '-'
`,
		Args: docsArgs("docs read", cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			err := runDocsRead(cmd, deps, args[0], timeout, productionDocsReadRuntime())
			// mdroll's own exit status (a silentCodedError) passes through
			// unchanged: once the child has started, its code is not ours to
			// reclassify. Everything else failed before there was anything to
			// read, so it is "could not run" (exit 2, docs_errors.go).
			var passthrough *silentCodedError
			if err == nil || errors.As(err, &passthrough) {
				return err
			}
			return docsFail(cmd, "docs read", "", err, 2, false)
		},
	}
	cmd.SetFlagErrorFunc(docsFlagError("docs read"))
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
	noteSkippedPaths(cmd.ErrOrStderr(), idx)

	doc, path, err := resolveDocsReadTarget(idx, target)
	if err != nil {
		return err
	}

	mdroll, err := resolveMdroll(rt.lookPath)
	if err == nil {
		return runMdroll(cmd, mdroll, path)
	}
	errOut := cmd.ErrOrStderr()
	// Refused, not absent: "not installed" would contradict the line above it.
	why := "mdroll not installed"
	if errors.Is(err, osexec.ErrDot) {
		_, _ = fmt.Fprintln(errOut, "mdroll found only via a relative PATH entry; ignoring")
		why = "mdroll unavailable"
	}

	// A server blocks until Ctrl-C. Started without a terminal it would hang the
	// caller — an agent, a script — with nothing to interrupt it, which is the
	// one thing ADR-0008 rules out. Its rule 1 asks for stdin AND stdout to be
	// terminals, so either one redirected means no server: answer with where the
	// document is and leave the browsing to a command that says it serves.
	if !rt.stdinIsTerminal(cmd.InOrStdin()) || !rt.stdoutIsTerminal(cmd.OutOrStdout()) {
		_, _ = fmt.Fprintln(errOut, why+"; not starting the HTML reader without a terminal; run `forgectl docs serve --open` to browse")
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), safeColumnPath(path))
		return nil
	}
	_, _ = fmt.Fprintf(errOut, "%s; opening %s in the HTML reader\n", why, termsafe.QuotePath(doc.RelPath))
	return rt.fallback(cmd, deps, idx, doc)
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
			return docspkg.Doc{}, "", fmt.Errorf("resolve %s: %w", termsafe.QuotePath(target), termsafe.Error(err))
		}
		canonical, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return docspkg.Doc{}, "", fmt.Errorf("resolve %s: %w", termsafe.QuotePath(target), termsafe.Error(err))
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
// indexed Doc for the path it resolved to, UNDER label. Overlapping roots (cwd
// and ./docs are both defaults) index one file twice; looking it up by path
// alone could answer with the other root's entry, and the HTML fallback would
// then open a URL under a label the operator never named.
func resolveIndexedDoc(idx *docspkg.Index, label, rel, target string) (docspkg.Doc, string, error) {
	path, err := idx.Resolve(label, rel)
	if err != nil {
		return docspkg.Doc{}, "", fmt.Errorf("%w: %s: %w", errDocNotIndexed, termsafe.QuotePath(target), err)
	}
	doc, ok := findUnderRoot(idx, label, path)
	if !ok {
		return docspkg.Doc{}, "", fmt.Errorf("%w: %s", errDocNotIndexed, termsafe.QuotePath(target))
	}
	return doc, path, nil
}

// findUnderRoot returns the Doc indexed under label for the canonical path,
// which Index.Resolve has already confirmed lies inside that root.
func findUnderRoot(idx *docspkg.Index, label, path string) (docspkg.Doc, bool) {
	for _, r := range idx.Roots() {
		if r.Label != label {
			continue
		}
		rel, err := filepath.Rel(r.Path, path)
		if err != nil {
			return docspkg.Doc{}, false
		}
		return idx.Find(label, filepath.ToSlash(rel))
	}
	return docspkg.Doc{}, false
}

// resolveMdroll resolves mdroll on PATH to an absolute path, the way
// surface_backend.go resolves tmux, cmux, and herdr: the program that runs is
// decided once, here, and never re-resolved by the exec layer. A hit only
// through a relative PATH entry (exec.ErrDot) is refused, not run: it would
// execute whatever file of that name sits in the current directory.
func resolveMdroll(lookPath func(string) (string, error)) (string, error) {
	path, err := lookPath(mdrollBinary)
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

// mdrollArgs is the argument contract with mdroll, measured against mdroll
// 0.4.2 (tokuhirom/mdroll f2bce16, src/cli.rs). clap rejects an unknown flag,
// so every flag here must exist in the oldest mdroll this supports:
//   - --watch reloads when the file changes, as the HTML reader's live reload
//     would have.
//   - --no-remote-images stops mdroll fetching http(s) images, which it does by
//     default. A remote image in a doc is a tracking beacon; the HTML reader
//     blocks it with img-src 'self' data:, and read must not reopen it.
//   - "--" ends option parsing so the path can never be read as a flag. The
//     path is absolute already, but the separator does not depend on that.
func mdrollArgs(path string) []string {
	return []string{"--watch", "--no-remote-images", "--", path}
}

// runMdroll runs mdroll on path with no shell, handing it the command's stdin,
// stdout, and stderr. In production those are the process's own terminal file
// descriptors, which os/exec passes through untouched, so mdroll's raw-mode
// keys (search, TOC, link picker) work as if it had been started directly.
// mdroll's exit status becomes forgectl's, and a signal that killed it becomes
// 128+signo, the shell's convention.
func runMdroll(cmd *cobra.Command, mdroll, path string) error {
	// mdroll reads the doc by path, and re-reads it by path under --watch, so
	// a check-then-open window remains between Index.Resolve and each of
	// mdroll's opens. Only someone who can write to a directory on the doc's
	// path inside the root (including a member of a group that can write to
	// a shared root) can race it, and that writer could change the doc
	// directly. Such a writer could also redirect the path outside the root,
	// which shows only the user's own file on the user's own terminal. It is
	// accepted rather than closed: handing mdroll content instead of a path
	// would drop --watch and relative image resolution (forgectl#773).
	child := osexec.CommandContext(cmd.Context(), mdroll, mdrollArgs(path)...) //nolint:gosec // G204: absolute LookPath result, fixed flags, "--" before an index-resolved path
	child.Stdin = cmd.InOrStdin()
	child.Stdout = cmd.OutOrStdout()
	child.Stderr = cmd.ErrOrStderr()
	if err := child.Run(); err != nil {
		var exitErr *osexec.ExitError
		if errors.As(err, &exitErr) {
			if code := exitErr.ExitCode(); code > 0 {
				return newSilentCodedError(code)
			}
			if ws, ok := exitErr.Sys().(interface {
				Signaled() bool
				Signal() syscall.Signal
			}); ok && ws.Signaled() {
				return newSilentCodedError(128 + int(ws.Signal()))
			}
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
