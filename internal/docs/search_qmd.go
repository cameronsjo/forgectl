package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	forgexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/redact"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// qmd backend limits.
const (
	// qmdWindowFactor and maxQMDWindow size the -n forgectl asks qmd for.
	// qmd applies -n across ALL its default collections, before forgectl's
	// containment gate drops out-of-root hits, so a top-limit window could
	// hold no in-root hit at all. Asking for limit*qmdWindowFactor rows,
	// capped at maxQMDWindow, leaves room for the gate to discard.
	qmdWindowFactor = 10
	maxQMDWindow    = 1000
	// maxQMDRows bounds the window for a --limit at or past maxQMDWindow,
	// where limit+1 is asked for instead. It is qmd's own --all row count,
	// and keeps limit+1 from overflowing.
	maxQMDRows = 100000
	// maxQMDOutputBytes caps qmd's whole stdout. qmd prints one JSON array,
	// and a snippet is at most ~300 characters, so maxQMDWindow rows fit in
	// well under this; output past it fails closed.
	maxQMDOutputBytes = 8 << 20
)

// ErrSearchBackendFailed reports that the search backend ran but produced no
// usable answer: it exited non-zero, or its output was not the JSON forgectl
// requires. Nothing from that run is returned.
var ErrSearchBackendFailed = errors.New("search backend failed")

// qmdArgs is the fixed qmd argv. Only the window and the query vary.
//
//   - search, never query: `qmd search` is BM25 over qmd's SQLite FTS
//     index. `qmd query` loads local LLM models (slow, may download) and
//     prints trust-gate text to stdout before its JSON.
//   - --json: one JSON array on stdout ("[]" for no match).
//   - --full-path: each hit's file is its on-disk path (absolute, or
//     "./"-relative when under qmd's working directory) instead of a qmd://
//     URI. A hit whose file qmd cannot find on disk still comes back as a
//     qmd:// URI, which the gate drops.
//   - "--" before the query: qmd parses its argv with node's util.parseArgs,
//     which treats every argument after "--" as positional, so a query such
//     as --version is searched as text.
//
// Neither flag is the containment boundary; every hit is gated through the
// Index (searchQMD).
func qmdArgs(query string, window int) []string {
	return []string{"search", "--json", "--full-path", "-n", strconv.Itoa(window), "--", query}
}

// qmdWindow is the -n forgectl requests for limit results: limit times
// qmdWindowFactor, capped at maxQMDWindow, and otherwise limit+1 (at most
// maxQMDRows), so one hit past the limit can still prove truncation.
func qmdWindow(limit int) int {
	switch {
	case limit < maxQMDWindow/qmdWindowFactor:
		return limit * qmdWindowFactor
	case limit < maxQMDWindow:
		return maxQMDWindow
	default:
		return min(limit, maxQMDRows-1) + 1
	}
}

// qmdHit is the part of one `qmd search --json` row forgectl reads. Every
// field is untrusted: file is only a candidate path for the gate, title is
// never used (the Index's own title is), and the snippet's text is never
// used either: only its "@@ -start,count @@" header, which names the lines
// forgectl re-reads from the doc itself (qmdSnippet).
type qmdHit struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Snippet string `json:"snippet"`
}

// searchQMD runs q through `qmd search` once, over qmd's default
// collections, and keeps only the hits the containment gate accepts.
//
// Each hit's file must name a doc the Index holds at an exact (root,
// relative path) in some root, the first such root in configuration order,
// and Index.Open must open it at that path: the same gate the rg backend
// and the reader use. A path merely under a root is not enough, because
// qmd's index can be stale. For the same reason the snippet is re-read from
// the opened doc, so it is the doc's own text even when qmd's index holds
// other content under this path (forgectl#743). A qmd:// URI, a relative path not starting with "./", and
// anything the gate refuses are counted in Skipped.
//
// qmd's stdout must be exactly one JSON array. Anything else, including a
// banner line before the JSON, fails closed with ErrSearchBackendFailed and
// no results, as does a non-zero exit. The query never reaches argv logging
// (StreamingRunner does not log argv) or forgectl's own error text; on a
// failed run qmd's own stderr is surfaced, sanitised and capped, and qmd may
// echo the query there. A missing qmd wraps ErrNoSearchBackend; an expired or
// cancelled ctx is returned as itself.
//
// Truncated is set when a gated hit existed past limit, and also when qmd
// returned its whole -n window: qmd ranked more rows than it printed, so
// in-root hits may exist that forgectl never saw.
func (s Searcher) searchQMD(ctx context.Context, idx *Index, q string, limit int, resp SearchResponse) (SearchResponse, error) {
	qmdPath, err := s.lookBinary("qmd", "install qmd, or use --backend ripgrep")
	if err != nil {
		return resp, err
	}
	// qmd prints a hit under its working directory as "./<rel>", relative
	// to the realpath of that directory. The child inherits ours.
	getwd := s.Getwd
	if getwd == nil {
		getwd = os.Getwd
	}
	cwd := ""
	if wd, wdErr := getwd(); wdErr == nil {
		if realWD, evalErr := filepath.EvalSymlinks(wd); evalErr == nil {
			cwd = realWD
		}
	}

	window := qmdWindow(limit)
	stdout := &cappedBuffer{limit: maxQMDOutputBytes}
	stderr := &cappedBuffer{limit: maxSearchStderrBytes}
	runErr := s.Runner.RunStreaming(ctx, nil, stdout, stderr, qmdPath, qmdArgs(q, window)...)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return resp, fmt.Errorf("docs search: %w", ctxErr)
	}
	if runErr != nil {
		return resp, qmdFailure(runErr, stderr.String())
	}
	if stdout.overflowed {
		return resp, fmt.Errorf("%w: qmd output exceeded %d bytes", ErrSearchBackendFailed, maxQMDOutputBytes)
	}
	hits, err := decodeQMDHits(stdout.buf.Bytes())
	if err != nil {
		return resp, err
	}

	titles := searchTitles(idx)
	roots := idx.Roots()
	seen := make(map[hitKey]bool)
	for _, h := range hits {
		hit, doc, ok := gateQMDHit(idx, roots, titles, cwd, h)
		if !ok {
			resp.Skipped++
			continue
		}
		key := hitKey{absPath: doc.abs, line: hit.Line}
		if seen[key] {
			_ = doc.f.Close()
			continue
		}
		seen[key] = true
		if len(resp.Results) >= limit {
			_ = doc.f.Close()
			resp.Truncated = true
			break
		}
		// Only a returned hit's snippet is read (qmdSnippet).
		hit.Snippet = qmdSnippet(doc.f, h)
		_ = doc.f.Close()
		resp.Results = append(resp.Results, hit)
	}
	if len(hits) >= window {
		resp.Truncated = true
	}
	return resp, nil
}

// qmdFailure describes a failed qmd run: its stderr when it wrote any, else
// why it never ran or its exit status. It never includes the command line,
// which carries the query.
func qmdFailure(runErr error, stderr string) error {
	msg := strings.TrimSpace(stderr)
	var cmdErr *forgexec.CommandError
	isCmdErr := errors.As(runErr, &cmdErr)
	switch {
	case msg != "":
	case isCmdErr && cmdErr.Err != nil:
		msg = redact.Text(cmdErr.Err.Error())
	case isCmdErr:
		msg = fmt.Sprintf("exit %d", cmdErr.ExitCode)
	default:
		msg = runErr.Error()
	}
	return fmt.Errorf("%w: qmd search: %s", ErrSearchBackendFailed, termsafe.SafeLineMax(msg, maxSearchErrorRunes))
}

// decodeQMDHits requires out to be exactly one JSON array of hit objects,
// with nothing but whitespace around it. It does not skip a leading
// non-JSON line: a qmd that prints text before its JSON (as `qmd query`
// does with its trust-gate notice) is refused rather than guessed at.
func decodeQMDHits(out []byte) ([]qmdHit, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	var hits []qmdHit
	if err := dec.Decode(&hits); err != nil {
		return nil, fmt.Errorf("%w: qmd output is not a JSON array of results: %s", ErrSearchBackendFailed, termsafe.SafeLineMax(err.Error(), maxSearchErrorRunes))
	}
	if hits == nil {
		// A bare "null" decodes without error; qmd prints "[]" for no match.
		return nil, fmt.Errorf("%w: qmd output is not a JSON array of results", ErrSearchBackendFailed)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: qmd printed more than one JSON value", ErrSearchBackendFailed)
	}
	return hits, nil
}

// gateQMDHit maps one qmd row to a result, without its Snippet, plus the
// doc gatePath opened, which the caller closes; or it reports false. file
// must be absolute, or "./"-relative to cwd (qmd's own realpath
// rendering); the joined path is then offered to gatePath under each root
// in configuration order, and the first root that holds it wins.
func gateQMDHit(idx *Index, roots []Root, titles map[searchKey]string, cwd string, h qmdHit) (SearchResult, *openHit, bool) {
	var path string
	switch {
	case filepath.IsAbs(h.File):
		path = filepath.Clean(h.File)
	case cwd != "" && (h.File == "./" || strings.HasPrefix(h.File, "./")):
		path = filepath.Join(cwd, h.File)
	default:
		// A qmd:// URI (the file is gone from disk), or a shape qmd does not
		// print under --full-path.
		return SearchResult{}, nil, false
	}
	for _, root := range roots {
		hit, ok := gatePath(idx, root, titles, path)
		if !ok {
			continue
		}
		return SearchResult{
			Root:  root.Label,
			Path:  hit.rel,
			Title: hit.title,
			Line:  max(h.Line, 0),
		}, hit, true
	}
	return SearchResult{}, nil, false
}

// maxQMDSnippetLines caps how many lines of a qmd snippet's header window
// are re-read. qmd 2.8.3 names at most four (extractSnippet).
const maxQMDSnippetLines = 8

// qmdSnippet re-reads the lines a qmd hit's snippet covers from f, the doc
// gatePath opened, and returns them joined and cut as qmd's own snippet
// text was. qmd 2.8.3's snippet opens with "@@ -start,count @@", start
// being the 1-based line of the file the snippet begins on; without that
// header the hit's own line is read alone. Only a bounded window is read.
func qmdSnippet(f io.ReaderAt, h qmdHit) string {
	first, count, ok := qmdSnippetLines(h.Snippet)
	if !ok {
		first, count = h.Line, 1
	}
	off, ok := lineOffset(f, first)
	if !ok {
		return ""
	}
	buf := make([]byte, 4*snippetWindowBytes)
	n, err := f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	text := buf[:n]
	end := 0
	for range min(count, maxQMDSnippetLines) {
		i := bytes.IndexByte(text[end:], '\n')
		if i < 0 {
			end = len(text)
			break
		}
		end += i + 1
	}
	text = text[:end]
	if end == n && n == len(buf) {
		text = trimPartialRune(text)
	}
	body := strings.TrimSpace(strings.ToValidUTF8(string(text), "\uFFFD"))
	return snippetAround(body, 0, maxSnippetRunes)
}

// qmdSnippetLines parses the "@@ -start,count @@" header on the first line
// of a qmd snippet.
func qmdSnippetLines(s string) (first, count int, ok bool) {
	header, _, _ := strings.Cut(s, "\n")
	rest, found := strings.CutPrefix(header, "@@ -")
	if !found {
		return 0, 0, false
	}
	span, _, found := strings.Cut(rest, " ")
	if !found {
		return 0, 0, false
	}
	a, b, found := strings.Cut(span, ",")
	if !found {
		return 0, 0, false
	}
	first, err1 := strconv.Atoi(a)
	count, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil || first < 1 || count < 1 {
		return 0, 0, false
	}
	return first, count, true
}
