package docs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	forgexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Search limits. Every one of them bounds something a query or a doc tree
// controls: the query itself, one rg JSON record, one snippet, and the rg
// diagnostics kept for an error message.
const (
	// maxQueryBytes caps the query before anything runs.
	maxQueryBytes = 1024
	// maxRgRecordBytes caps one rg JSON record. rg ignores --max-columns under
	// --json (measured on rg 14.1.0: a 10 KB line came through whole), so a
	// long line reaches us intact; a record past this cap is dropped up to
	// its newline and counted in SearchResponse.Skipped. --max-filesize=1M
	// already bounds a line to 1 MiB before JSON escaping.
	maxRgRecordBytes = 2 << 20
	// maxSnippetRunes caps a result's snippet, cut around the first match.
	maxSnippetRunes = 240
	// maxSearchStderrBytes caps the rg diagnostics kept for an error message.
	maxSearchStderrBytes = 4 << 10
	// maxSearchErrorRunes caps one SearchError.Message after escaping.
	maxSearchErrorRunes = 512
)

// The search backends, as named in SearchResponse.Backend, Searcher.Backend,
// `docs search --backend`, and [docs] search_backend. ripgrep is the default;
// qmd is opt-in only and never picked because it happens to be installed
// (ADR-0008 rule 4: no hidden mode switch).
const (
	SearchBackendRipgrep = "ripgrep"
	SearchBackendQMD     = "qmd"
)

// ValidSearchBackend reports whether name is a backend Search accepts. The
// empty string is valid and means the default, ripgrep.
func ValidSearchBackend(name string) bool {
	switch name {
	case "", SearchBackendRipgrep, SearchBackendQMD:
		return true
	}
	return false
}

// ErrNoSearchBackend reports that no search backend binary is installed.
var ErrNoSearchBackend = errors.New("no search backend available")

// ErrInvalidQuery reports a query ValidateQuery refused.
var ErrInvalidQuery = errors.New("invalid search query")

// SearchResult is one full-text hit. Its JSON shape is shared by every
// surface that returns search results; later changes may only add fields.
type SearchResult struct {
	Root    string `json:"root"`
	Path    string `json:"path"`
	Title   string `json:"title"`
	Line    int    `json:"line"`
	Snippet string `json:"snippet"`
}

// SearchError reports one root rg could not fully search: rg itself failed
// on it (an unreadable file, say), or some of its output could not be
// parsed. Hits from that root that did arrive are still in Results. Message
// is terminal-safe and capped.
type SearchError struct {
	Root    string `json:"root"`
	Message string `json:"message"`
}

// SearchResponse is the full answer to one query. Results and Errors are
// never nil, so they encode as [] rather than null. Results are in root
// order (the configured order), then in rg's path order within a root, so
// a truncated answer is a stable prefix (the qmd backend instead keeps qmd's
// ranking order). Truncated is set when at least one more indexed hit existed
// past the limit, and under qmd also when qmd's result window came back full. Skipped counts hits that were
// dropped rather than returned: hits outside the index, paths rg could only
// report as raw bytes, and oversized records. A root that failed is never
// counted in Skipped; it is listed in Errors. SkippedPaths lists the paths
// the index walk could not read ({root, path, reason}, as in `docs check`'s
// skipped array; both include skips under vault roots),
// so docs under them were never searched; it is never nil.
type SearchResponse struct {
	Backend   string         `json:"backend"`
	Query     string         `json:"query"`
	Results   []SearchResult `json:"results"`
	Truncated bool           `json:"truncated"`
	Skipped   int            `json:"skipped"`
	Errors    []SearchError  `json:"errors"`
	// SkippedPaths is additive (ADR-0008): a separate key because Skipped
	// already counts dropped hits.
	SkippedPaths []SkippedPath `json:"skipped_paths"`
}

// ValidateQuery rejects a query before any subprocess sees it: an empty or
// whitespace-only query (rg matches every line against an empty pattern),
// one over maxQueryBytes, one carrying a NUL, and invalid UTF-8.
func ValidateQuery(q string) error {
	switch {
	case strings.TrimSpace(q) == "":
		return fmt.Errorf("%w: query is empty", ErrInvalidQuery)
	case len(q) > maxQueryBytes:
		return fmt.Errorf("%w: query is %d bytes, over the %d-byte limit", ErrInvalidQuery, len(q), maxQueryBytes)
	case strings.IndexByte(q, 0) >= 0:
		return fmt.Errorf("%w: query contains a NUL byte", ErrInvalidQuery)
	case !utf8.ValidString(q):
		return fmt.Errorf("%w: query is not valid UTF-8", ErrInvalidQuery)
	}
	return nil
}

// Searcher runs full-text queries over an Index's roots with ripgrep, or
// with qmd when Backend is SearchBackendQMD (search_qmd.go).
//
// Runner must be a StreamingRunner: the backend's output is consumed as it
// arrives and capped (rgStream, cappedBuffer), never buffered whole, and
// StreamingRunner does not log argv, so the query stays out of the logs.
// LookPath defaults to os/exec.LookPath and Getwd to os.Getwd; Getwd is read
// only by the qmd backend.
type Searcher struct {
	Runner   forgexec.StreamingRunner
	LookPath func(string) (string, error)
	Backend  string
	Getwd    func() (string, error)
}

// lookBinary resolves name to an absolute path, refusing one found only in
// a relative PATH entry. Every failure wraps ErrNoSearchBackend; hint says
// how to install the binary.
func (s Searcher) lookBinary(name, hint string) (string, error) {
	lookPath := s.LookPath
	if lookPath == nil {
		lookPath = osexec.LookPath
	}
	p, err := lookPath(name)
	if errors.Is(err, osexec.ErrDot) {
		return "", fmt.Errorf("%w: %s found only in a relative PATH entry; refusing", ErrNoSearchBackend, name)
	}
	if err != nil {
		return "", fmt.Errorf("%w: %s not found on PATH (%s)", ErrNoSearchBackend, name, hint)
	}
	if p, err = filepath.Abs(p); err != nil {
		return "", fmt.Errorf("%w: resolve %s path: %w", ErrNoSearchBackend, name, err)
	}
	return p, nil
}

// searchTitles maps every indexed doc's (root label, relative path) to its
// title: the exact-membership table both backends gate hits through.
func searchTitles(idx *Index) map[searchKey]string {
	titles := make(map[searchKey]string, len(idx.docs))
	for _, d := range idx.docs {
		titles[searchKey{root: d.RootLabel, rel: d.RelPath}] = d.Title
	}
	return titles
}

// rgArgs is the fixed rg argv for one root; only the query and the path
// vary. Two flags carry the security posture:
//
//   - --no-config: rg otherwise reads RIPGREP_CONFIG_PATH, and a config file
//     holding --follow made rg return symlinks pointing outside the root
//     (measured on rg 14.1.0).
//   - "--" before the query: without it a query such as --version is parsed
//     as an rg flag (measured: rg printed its version and exited 0).
//
// --sort=path makes rg walk single-threaded in path order, so the same
// query over the same tree returns the same hits in the same order and a
// truncated answer is a stable prefix. It also bounds rg to one worker.
//
// Neither security flag is the containment boundary. Every hit is gated
// through the Index (Search), so a wrong flag here can make search miss a
// file but never return one from outside a root.
func rgArgs(query, path string) []string {
	return []string{
		"--no-config",
		"--json",
		"--fixed-strings",
		"--ignore-case",
		"--no-follow",
		"--no-ignore",
		"--iglob=*.md",
		"--iglob=*.markdown",
		"--glob=!node_modules/",
		"--glob=!vendor/",
		"--max-count=5",
		"--max-filesize=1M",
		"--sort=path",
		"--",
		query,
		path,
	}
}

// Search runs q over every root of idx, one rg process per root, and returns
// at most limit results.
//
// Each hit is kept only when its path is inside its root and names a doc the
// Index holds at that exact (root, relative path), and Index.Resolve accepts
// it. That is the same membership gate the reader serves through, so search
// returns nothing the reader would refuse to serve, whatever rg reports.
//
// A doc reachable through two overlapping roots (cwd and cwd/docs, say) is
// returned once, under the first root in configuration order.
//
// rg exit 1 (no match) is an empty success. Any other rg failure on a root
// is recorded in Errors, that root's hits are kept, and the remaining roots
// are still searched; only an expired or cancelled ctx aborts the search. A
// missing rg wraps ErrNoSearchBackend.
//
// With Backend set to SearchBackendQMD the query goes to qmd instead
// (searchQMD); an unknown Backend is an error before anything runs.
func (s Searcher) Search(ctx context.Context, idx *Index, q string, limit int) (SearchResponse, error) {
	backend := s.Backend
	if backend == "" {
		backend = SearchBackendRipgrep
	}
	resp := SearchResponse{Backend: backend, Query: q, Results: []SearchResult{}, Errors: []SearchError{}, SkippedPaths: []SkippedPath{}}
	if idx != nil {
		resp.SkippedPaths = append(resp.SkippedPaths, idx.skipped...)
	}
	if !ValidSearchBackend(backend) {
		return resp, fmt.Errorf("docs search: unknown search backend %q (want %q or %q)", backend, SearchBackendRipgrep, SearchBackendQMD)
	}
	if err := ValidateQuery(q); err != nil {
		return resp, err
	}
	if limit < 1 {
		return resp, fmt.Errorf("docs search: limit must be at least 1, not %d", limit)
	}
	if s.Runner == nil {
		return resp, errors.New("docs search: streaming runner is unavailable")
	}
	if backend == SearchBackendQMD {
		return s.searchQMD(ctx, idx, q, limit, resp)
	}
	rgPath, err := s.lookBinary("rg", "install ripgrep")
	if err != nil {
		return resp, err
	}

	titles := searchTitles(idx)

	seen := make(map[hitKey]bool)
	for _, root := range idx.Roots() {
		done, err := s.searchRoot(ctx, idx, root, rgPath, q, limit, titles, seen, &resp)
		if err != nil {
			return resp, err
		}
		if done {
			break
		}
	}
	return resp, nil
}

// searchRoot runs rg over one root, appending gated hits to resp and any
// failure to resp.Errors. It reports done once resp is truncated, so the
// caller starts no further rg process, and returns an error only when ctx
// itself expired or was cancelled.
//
// It has exactly two returns: the ctx abort, and one tail that every other
// outcome (success, no match, rg failure, truncation) passes through, where
// rootFailures records what went wrong. A root that hit the limit is still
// checked, so a failure is never hidden by truncation.
func (s Searcher) searchRoot(ctx context.Context, idx *Index, root Root, rgPath, q string, limit int, titles map[searchKey]string, seen map[hitKey]bool, resp *SearchResponse) (bool, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	path := root.Path
	if root.OnlyFile != "" {
		path = root.OnlyFile
	}

	unparsed := 0
	stream := &rgStream{}
	stream.handle = func(rec []byte) bool {
		var m rgRecord
		if err := json.Unmarshal(rec, &m); err != nil {
			unparsed++
			return true
		}
		if m.Type != "match" {
			return true
		}
		hit, abs, ok := gateHit(idx, root, titles, m)
		if !ok {
			resp.Skipped++
			return true
		}
		key := hitKey{absPath: abs, line: hit.Line}
		if seen[key] {
			return true // already returned under an earlier, overlapping root
		}
		seen[key] = true
		if len(resp.Results) >= limit {
			// One indexed hit past the limit is proof there is more; stop
			// rg rather than read the rest of its output.
			resp.Truncated = true
			cancel()
			return false
		}
		resp.Results = append(resp.Results, hit)
		return true
	}
	stderr := &cappedBuffer{limit: maxSearchStderrBytes}

	runErr := s.Runner.RunStreaming(runCtx, nil, stream, stderr, rgPath, rgArgs(q, path)...)
	stream.Close()
	resp.Skipped += stream.oversized

	if ctxErr := ctx.Err(); ctxErr != nil && !resp.Truncated {
		return false, fmt.Errorf("docs search: %w", ctxErr)
	}
	resp.Errors = append(resp.Errors, rootFailures(root, unparsed, runErr, stderr.String(), resp.Truncated)...)
	return resp.Truncated, nil
}

// rootFailures reports what went wrong on one root's rg run: records that
// could not be parsed, and rg's own failure. rg exit 1 (no match) is not a
// failure. On a truncated run runErr is the cancellation Search itself
// triggered at the limit, so it is ignored there, but rg's stderr (fully
// drained by the time the run returns) still is not: rg that wrote a
// diagnostic before the limit stopped it had failed on something.
func rootFailures(root Root, unparsed int, runErr error, stderr string, truncated bool) []SearchError {
	var out []SearchError
	if unparsed > 0 {
		out = append(out, searchError(root, fmt.Sprintf("%d rg output records could not be parsed", unparsed)))
	}
	msg := strings.TrimSpace(stderr)
	switch {
	case truncated:
		// runErr is our own cancel; only a diagnostic counts.
	case runErr == nil:
		return out
	default:
		var cmdErr *forgexec.CommandError
		isCmdErr := errors.As(runErr, &cmdErr)
		if isCmdErr && cmdErr.ExitCode == 1 {
			return out
		}
		switch {
		case msg != "":
		case isCmdErr && cmdErr.Err != nil:
			// The reason rg never ran or was killed (exec format error,
			// EACCES, a signal we did not send), all of which read exit -1,
			// or a real non-1 exit with nothing on stderr, where Err is the
			// *exec.ExitError ("exit status 2") and ExitCode is 2.
			// cmdErr.Err, not cmdErr.Error(): the latter prefixes the
			// command line, which a runner that keeps Args would fill with
			// the query.
			msg = cmdErr.Err.Error()
		case isCmdErr:
			msg = fmt.Sprintf("exit %d", cmdErr.ExitCode)
		default:
			msg = runErr.Error()
		}
	}
	if msg != "" {
		out = append(out, searchError(root, "rg failed: "+msg))
	}
	return out
}

// searchError builds a terminal-safe, capped SearchError for root.
func searchError(root Root, msg string) SearchError {
	return SearchError{Root: root.Label, Message: termsafe.SafeLineMax(msg, maxSearchErrorRunes)}
}

// hitKey identifies one returned hit by the doc's canonical path and line,
// so overlapping roots do not return the same line twice.
type hitKey struct {
	absPath string
	line    int
}

// searchKey names one indexed doc by root label and slash-separated relative
// path, the pair walkRoot indexed it under.
type searchKey struct {
	root, rel string
}

// rgRecord is the part of one rg --json record Search reads. A path or line
// rg cannot express as UTF-8 arrives as {"bytes": base64} instead of text.
type rgRecord struct {
	Type string `json:"type"`
	Data struct {
		Path       rgData `json:"path"`
		Lines      rgData `json:"lines"`
		LineNumber int    `json:"line_number"`
		Submatches []struct {
			Start int `json:"start"`
		} `json:"submatches"`
	} `json:"data"`
}

type rgData struct {
	Text  *string `json:"text"`
	Bytes string  `json:"bytes"`
}

// gateHit turns one rg match into a result plus the doc's canonical path,
// or reports false when the hit must not be returned: a path only
// expressible as bytes, a path outside the root, or a path the Index does
// not hold at that exact (root, relPath).
func gateHit(idx *Index, root Root, titles map[searchKey]string, m rgRecord) (SearchResult, string, bool) {
	if m.Data.Path.Text == nil {
		return SearchResult{}, "", false
	}
	relSlash, title, abs, ok := gatePath(idx, root, titles, *m.Data.Path.Text)
	if !ok {
		return SearchResult{}, "", false
	}
	line := ""
	if m.Data.Lines.Text != nil {
		line = *m.Data.Lines.Text
	} else if raw, err := base64.StdEncoding.DecodeString(m.Data.Lines.Bytes); err == nil {
		line = strings.ToValidUTF8(string(raw), "\uFFFD")
	}
	start := 0
	if len(m.Data.Submatches) > 0 {
		start = m.Data.Submatches[0].Start
	}
	return SearchResult{
		Root:    root.Label,
		Path:    relSlash,
		Title:   title,
		Line:    m.Data.LineNumber,
		Snippet: snippetAround(line, start, maxSnippetRunes),
	}, abs, true
}

// gatePath is the containment gate every backend's hit passes through. It
// reports the slash-separated relative path, title, and canonical absolute
// path of the doc at path under root, or false unless path is inside root,
// names a doc the Index holds at exactly that (root, relative path), and
// Index.Resolve accepts it. That is the reader's own membership rule, so no
// backend can return a doc the reader would refuse to serve.
func gatePath(idx *Index, root Root, titles map[searchKey]string, path string) (string, string, string, bool) {
	rel, err := filepath.Rel(root.Path, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", "", false
	}
	relSlash := filepath.ToSlash(rel)
	// Exact membership first: walkRoot never indexes a symlinked file, so a
	// symlink a backend reported (rg under a config that re-enabled --follow,
	// say) has no entry here even where Resolve would follow it to an
	// indexed target.
	title, ok := titles[searchKey{root: root.Label, rel: relSlash}]
	if !ok {
		return "", "", "", false
	}
	abs, err := idx.Resolve(root.Label, relSlash)
	if err != nil {
		return "", "", "", false
	}
	return relSlash, title, abs, true
}

// snippetAround returns at most maxRunes runes of line, centred on byte
// offset start and cut only at rune boundaries, with line breaks and tabs
// folded to spaces. Terminal safety is the output side's job.
func snippetAround(line string, start, maxRunes int) string {
	line = strings.TrimRight(line, "\r\n")
	line = strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '\t':
			return ' '
		}
		return r
	}, line)
	if start < 0 {
		start = 0
	}
	if start > len(line) {
		start = len(line)
	}
	for start > 0 && start < len(line) && !utf8.RuneStart(line[start]) {
		start--
	}
	runes := []rune(line)
	if len(runes) <= maxRunes {
		return line
	}
	center := utf8.RuneCountInString(line[:start])
	from := max(center-maxRunes/2, 0)
	to := min(from+maxRunes, len(runes))
	from = max(to-maxRunes, 0)
	return string(runes[from:to])
}

// rgStream splits rg's stdout into newline-terminated records and hands
// each to handle, holding at most one record of maxRgRecordBytes at a time. A
// longer record is discarded up to its newline and counted in oversized.
// Once handle returns false the stream discards everything after.
//
// Write never returns an error: os/exec copies the child's stdout through a
// goroutine, and a writer that fails leaves the child blocked on a full pipe
// instead of exiting. Stopping is done by cancelling the child's context.
type rgStream struct {
	handle    func([]byte) bool
	buf       []byte
	dropping  bool
	done      bool
	oversized int
}

func (w *rgStream) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && !w.done {
		chunk := p
		complete := false
		if i := bytes.IndexByte(p, '\n'); i >= 0 {
			chunk, p, complete = p[:i], p[i+1:], true
		} else {
			p = nil
		}
		if w.dropping {
			w.dropping = !complete
			continue
		}
		if len(w.buf)+len(chunk) > maxRgRecordBytes {
			w.buf = w.buf[:0]
			w.oversized++
			w.dropping = !complete
			continue
		}
		w.buf = append(w.buf, chunk...)
		if complete {
			w.emit()
		}
	}
	return n, nil
}

// Close hands over a final record rg did not newline-terminate.
func (w *rgStream) Close() {
	if !w.done && !w.dropping && len(w.buf) > 0 {
		w.emit()
	}
}

func (w *rgStream) emit() {
	rec := w.buf
	w.buf = w.buf[:0]
	if !w.handle(rec) {
		w.done = true
	}
}

// cappedBuffer keeps the first limit bytes written to it and discards the
// rest, without ever failing a write (see rgStream). overflowed records that
// something was discarded.
type cappedBuffer struct {
	limit      int
	buf        bytes.Buffer
	overflowed bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	room := max(b.limit-b.buf.Len(), 0)
	if room > 0 {
		_, _ = b.buf.Write(p[:min(room, len(p))])
	}
	if len(p) > room {
		b.overflowed = true
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }
