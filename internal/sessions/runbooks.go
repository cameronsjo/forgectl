package sessions

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// RunbookRow is one derived full-text index row bound for the mart's
// `runbooks` table. Markdown is the source of truth (plan D4); these rows are
// droppable and regenerable. Path is relative to the corpus root — the
// upsert key, identical across machines syncing the same corpus.
type RunbookRow struct {
	SessionID string
	Project   string
	Slug      string
	Title     string
	Type      string
	Path      string
	FullText  string
	Machine   string
}

// ScanRunbooks walks the corpus root for *.md files and parses each into an
// index row. A missing root is not an error — the corpus may not exist on
// this machine yet (its relocation is a downstream item); the sync stage then
// leaves the mart's index untouched.
func ScanRunbooks(root, machine string) ([]RunbookRow, error) {
	info, err := os.Stat(root)
	if os.IsNotExist(err) {
		slog.Debug("Runbook corpus root absent, skipping runbook indexing.", "root", root)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat runbooks root %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("runbooks root %s is not a directory", root)
	}

	var rows []RunbookRow
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Never follow symlinks: os.ReadFile would read the TARGET, letting a
		// planted .md symlink exfiltrate an arbitrary local file (~/.pgpass,
		// a key) into the shared cross-machine mart. Real files only.
		if d.Type()&fs.ModeSymlink != 0 {
			slog.Warn("Skipping symlink in runbook corpus — only regular files are indexed.", "path", path)
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read runbook %s: %w", path, readErr)
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rows = append(rows, ParseRunbook(filepath.ToSlash(rel), string(raw), machine))
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk runbooks root %s: %w", root, walkErr)
	}
	slog.Debug("Successfully scanned runbook corpus.", "root", root, "files", len(rows))
	return rows, nil
}

// ParseRunbook builds an index row from one markdown document. Pure.
//
// Metadata is best-effort by design — the corpus is human-authored markdown:
//   - YAML frontmatter `key: value` scalars supply title/project/type/
//     session_id when present (a deliberately minimal parse; no YAML
//     dependency for four optional scalar keys).
//   - Title falls back to the first `# ` heading, then the slug.
//   - Slug is the filename stem; a `{project}/slug.md` layout supplies the
//     project fallback.
func ParseRunbook(relPath, content, machine string) RunbookRow {
	row := RunbookRow{
		Path:     relPath,
		FullText: content,
		Machine:  machine,
		Slug:     strings.TrimSuffix(filepath.Base(relPath), filepath.Ext(relPath)),
	}
	if dir := filepath.Dir(relPath); dir != "." {
		// First path segment: the vault-style {project}/<slug>.md convention.
		row.Project = strings.SplitN(filepath.ToSlash(dir), "/", 2)[0]
	}

	body := content
	if fm, rest, ok := splitFrontmatter(content); ok {
		body = rest
		for key, val := range fm {
			switch key {
			case "title":
				row.Title = val
			case "project":
				row.Project = val
			case "type":
				row.Type = val
			case "session_id", "sessionId":
				row.SessionID = val
			}
		}
	}
	if row.Title == "" {
		row.Title = firstHeading(body)
	}
	if row.Title == "" {
		row.Title = row.Slug
	}
	return row
}

// splitFrontmatter extracts scalar `key: value` pairs from a leading YAML
// frontmatter block (`---` ... `---`). Nested/multiline values are skipped,
// not parsed — the four keys the index consumes are all scalars. CRLF fences
// are tolerated (the corpus syncs across macOS/Windows/WSL).
func splitFrontmatter(content string) (map[string]string, string, bool) {
	rest, found := strings.CutPrefix(content, "---\n")
	if !found {
		rest, found = strings.CutPrefix(content, "---\r\n")
	}
	if !found {
		return nil, content, false
	}
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, content, false
	}
	block := rest[:end]
	body := rest[end+len("\n---"):]
	body = strings.TrimPrefix(body, "\n")

	fm := make(map[string]string)
	for line := range strings.Lines(block) {
		key, val, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		val = strings.TrimSpace(val) // also drops a CRLF line's trailing \r
		val = strings.Trim(val, `"'`)
		if val != "" {
			fm[strings.TrimSpace(key)] = val
		}
	}
	return fm, body, true
}

// firstHeading returns the text of the first `# ` ATX heading, or "".
func firstHeading(body string) string {
	for line := range strings.Lines(body) {
		if h, ok := strings.CutPrefix(line, "# "); ok {
			return strings.TrimSpace(h)
		}
	}
	return ""
}
