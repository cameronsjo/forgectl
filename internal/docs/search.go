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
)

// SearchBackendRipgrep names the rg backend in SearchResponse.Backend.
const SearchBackendRipgrep = "ripgrep"

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

// SearchResponse is the full answer to one query. Results is never nil, so
// it encodes as [] rather than null when nothing matched. Truncated is set
// when at least one more indexed hit existed past the limit. Skipped counts
// records that were dropped rather than returned: hits outside the index,
// paths rg could only report as raw bytes, oversized records, and roots rg
// could only partly search.
type SearchResponse struct {
	Backend   string         `json:"backend"`
	Query     string         `json:"query"`
	Results   []SearchResult `json:"results"`
	Truncated bool           `json:"truncated"`
	Skipped   int            `json:"skipped"`
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

// Searcher runs full-text queries over an Index's roots with ripgrep.
//
// Runner must be a StreamingRunner: rg's output is consumed as it arrives
// and capped (rgStream), never buffered whole, and StreamingRunner does not
// log argv, so the query stays out of the logs. LookPath defaults to
// os/exec.LookPath.
type Searcher struct {
	Runner   forgexec.StreamingRunner
	LookPath func(string) (string, error)
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
// Neither is the containment boundary. Every hit is gated through the Index
// (Search), so a wrong flag here can make search miss a file but never
// return one from outside a root.
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
// rg exit 1 (no match) is an empty success. rg exit 2 on a root that yielded
// hits keeps them and counts the root in Skipped; exit 2 with no hits is an
// error. A missing rg wraps ErrNoSearchBackend.
func (s Searcher) Search(ctx context.Context, idx *Index, q string, limit int) (SearchResponse, error) {
	resp := SearchResponse{Backend: SearchBackendRipgrep, Query: q, Results: []SearchResult{}}
	if err := ValidateQuery(q); err != nil {
		return resp, err
	}
	if limit < 1 {
		return resp, fmt.Errorf("docs search: limit must be at least 1, not %d", limit)
	}
	if s.Runner == nil {
		return resp, errors.New("docs search: streaming runner is unavailable")
	}
	lookPath := s.LookPath
	if lookPath == nil {
		lookPath = osexec.LookPath
	}
	rgPath, err := lookPath("rg")
	if err != nil {
		return resp, fmt.Errorf("%w: rg not found on PATH (install ripgrep)", ErrNoSearchBackend)
	}
	if rgPath, err = filepath.Abs(rgPath); err != nil {
		return resp, fmt.Errorf("%w: resolve rg path: %w", ErrNoSearchBackend, err)
	}

	titles := make(map[searchKey]string, len(idx.docs))
	for _, d := range idx.docs {
		titles[searchKey{root: d.RootLabel, rel: d.RelPath}] = d.Title
	}

	for _, root := range idx.Roots() {
		done, err := s.searchRoot(ctx, idx, root, rgPath, q, limit, titles, &resp)
		if err != nil {
			return resp, err
		}
		if done {
			break
		}
	}
	return resp, nil
}

// searchRoot runs rg over one root, appending gated hits to resp. It reports
// done once resp is truncated, so the caller starts no further rg process.
func (s Searcher) searchRoot(ctx context.Context, idx *Index, root Root, rgPath, q string, limit int, titles map[searchKey]string, resp *SearchResponse) (bool, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	path := root.Path
	if root.OnlyFile != "" {
		path = root.OnlyFile
	}

	matched := 0
	stream := &rgStream{}
	stream.handle = func(rec []byte) bool {
		var m rgRecord
		if err := json.Unmarshal(rec, &m); err != nil || m.Type != "match" {
			return true
		}
		matched++
		hit, ok := gateHit(idx, root, titles, m)
		if !ok {
			resp.Skipped++
			return true
		}
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

	if resp.Truncated {
		return true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("docs search: %w", ctxErr)
	}
	if runErr == nil {
		return false, nil
	}
	var cmdErr *forgexec.CommandError
	if !errors.As(runErr, &cmdErr) {
		return false, fmt.Errorf("docs search: run rg: %w", runErr)
	}
	switch {
	case cmdErr.ExitCode == 1:
		return false, nil
	case cmdErr.ExitCode == 2 && matched > 0:
		resp.Skipped++
		return false, nil
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = fmt.Sprintf("exit %d", cmdErr.ExitCode)
	}
	return false, fmt.Errorf("docs search: rg failed on root %s: %s", termsafe.SafeLine(root.Label), termsafe.SafeLine(msg))
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

// gateHit turns one rg match into a result, or reports false when the hit
// must not be returned: a path only expressible as bytes, a path outside the
// root, or a path the Index does not hold at that exact (root, relPath).
func gateHit(idx *Index, root Root, titles map[searchKey]string, m rgRecord) (SearchResult, bool) {
	if m.Data.Path.Text == nil {
		return SearchResult{}, false
	}
	rel, err := filepath.Rel(root.Path, *m.Data.Path.Text)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return SearchResult{}, false
	}
	relSlash := filepath.ToSlash(rel)
	// Exact membership first: walkRoot never indexes a symlinked file, so a
	// symlink rg reported (under a config that re-enabled --follow, say) has
	// no entry here even where Resolve would follow it to an indexed target.
	title, ok := titles[searchKey{root: root.Label, rel: relSlash}]
	if !ok {
		return SearchResult{}, false
	}
	if _, err := idx.Resolve(root.Label, relSlash); err != nil {
		return SearchResult{}, false
	}
	line := ""
	if m.Data.Lines.Text != nil {
		line = *m.Data.Lines.Text
	} else if raw, err := base64.StdEncoding.DecodeString(m.Data.Lines.Bytes); err == nil {
		line = strings.ToValidUTF8(string(raw), "�")
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
	}, true
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
// rest, without ever failing a write (see rgStream).
type cappedBuffer struct {
	limit int
	buf   bytes.Buffer
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		_, _ = b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }
