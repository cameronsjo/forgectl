package sweep

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/attention"
)

const (
	// maxBriefRead caps how much of brief.md is read for its repos.
	maxBriefRead = 256 << 10
	// tailRead is how much of a file's end is read to find its last line.
	tailRead = 4 << 10
	// maxBriefDirs caps the worker directories read.
	maxBriefDirs = 500
	// maxBriefRepos caps the repos taken from one brief.
	maxBriefRepos = 50
	// maxFiledRead caps filed.txt.
	maxFiledRead = 1 << 20
	picksDone    = "PICKS-COMPLETE"
	reportDone   = "REPORT-COMPLETE"
)

var (
	workerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	repoBulletRe = regexp.MustCompile("^- `([A-Za-z0-9][A-Za-z0-9_.-]{0,99})/([A-Za-z0-9][A-Za-z0-9_.-]{0,99})`")
)

// ReadBriefs reads the worker directories under dir whose names start with
// prefix and whose newest file is no older than since. A worker's phase is
// the furthest of: launched (brief.md), plan (picks.md ending in
// PICKS-COMPLETE), go (go.md), report (report.md ending in REPORT-COMPLETE and
// written at or after go.md).
//
// The directory is workers' territory, so it is read as untrusted: a symlinked
// directory or file is refused (a directory is counted in skipped; a file
// simply does not count), reads are capped, and a repo name must match the
// GitHub name shape.
func ReadBriefs(dir, prefix string, since time.Time) (briefs []Brief, skipped int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("read briefs directory: %w", errors.Unwrap(err))
	}
	for i, e := range entries {
		if i >= maxBriefDirs {
			skipped += len(entries) - i
			break
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !workerNameRe.MatchString(name) {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 || !e.IsDir() {
			skipped++
			continue
		}
		b := readBrief(filepath.Join(dir, name), name)
		if b.Latest.IsZero() || b.Latest.Before(since) {
			continue
		}
		briefs = append(briefs, b)
	}
	return briefs, skipped, nil
}

func readBrief(dir, name string) Brief {
	b := Brief{Name: name, Marks: map[string]time.Time{}, Phase: PhaseLaunched}
	files := map[string]struct {
		mtime time.Time
		last  string
		ok    bool
	}{}
	for _, f := range []string{"brief.md", "picks.md", "go.md", "report.md"} {
		mt, last, ok := statTail(filepath.Join(dir, f))
		files[f] = struct {
			mtime time.Time
			last  string
			ok    bool
		}{mt, last, ok}
		if ok && mt.After(b.Latest) {
			b.Latest = mt
		}
	}
	if f := files["brief.md"]; f.ok {
		b.Marks[PhaseLaunched] = f.mtime
		b.Repos = briefRepos(filepath.Join(dir, "brief.md"))
	}
	if f := files["picks.md"]; f.ok && f.last == picksDone {
		b.Marks[PhasePlan] = f.mtime
		b.Phase = PhasePlan
	}
	if f := files["go.md"]; f.ok {
		b.Marks[PhaseGo] = f.mtime
		b.Phase = PhaseGo
	}
	// A report is the finish line only when it was written after the GO.
	if f, g := files["report.md"], files["go.md"]; f.ok && f.last == reportDone && g.ok && !f.mtime.Before(g.mtime) {
		b.Marks[PhaseReport] = f.mtime
		b.Phase = PhaseReport
	}
	return b
}

// openRegular opens path for reading when it is a regular file that is not a
// symlink: Lstat refuses the link, and the opened descriptor must be the file
// Lstat saw.
func openRegular(path string) (*os.File, os.FileInfo, bool) {
	li, err := os.Lstat(path)
	if err != nil || !li.Mode().IsRegular() {
		return nil, nil, false
	}
	f, err := os.Open(path) //nolint:gosec // G304: path is under a validated worker directory
	if err != nil {
		return nil, nil, false
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || !os.SameFile(li, fi) {
		f.Close() //nolint:errcheck,gosec // refusing; nothing was read
		return nil, nil, false
	}
	return f, fi, true
}

// statTail is a file's mtime and its last non-empty line (trimmed).
func statTail(path string) (time.Time, string, bool) {
	f, fi, ok := openRegular(path)
	if !ok {
		return time.Time{}, "", false
	}
	defer f.Close() //nolint:errcheck // read-only
	size := fi.Size()
	off := max(0, size-tailRead)
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return time.Time{}, "", false
	}
	lines := bytes.Split(bytes.TrimRight(buf, " \t\r\n"), []byte("\n"))
	return fi.ModTime().UTC(), strings.TrimSpace(string(lines[len(lines)-1])), true
}

// briefRepos returns the owner/repo pairs listed as bullets under the
// "Where to work" heading, sorted and de-duplicated.
func briefRepos(path string) []string {
	f, _, ok := openRegular(path)
	if !ok {
		return nil
	}
	defer f.Close() //nolint:errcheck // read-only
	sc := bufio.NewScanner(io.LimitReader(f, maxBriefRead))
	sc.Buffer(make([]byte, 0, 64<<10), 64<<10)
	var repos []string
	in := false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			in = strings.HasPrefix(line, "## Where to work")
			continue
		}
		if !in {
			continue
		}
		m := repoBulletRe.FindStringSubmatch(line)
		if m == nil || m[1] == "." || m[1] == ".." || m[2] == "." || m[2] == ".." {
			continue
		}
		repos = append(repos, m[1]+"/"+m[2])
	}
	sort.Strings(repos)
	repos = slices.Compact(repos)
	if len(repos) > maxBriefRepos {
		repos = repos[:maxBriefRepos]
	}
	return repos
}

// ReadFiled returns the titles in dir/filed.txt, one issue per line as
// "<url>\t<title>". An absent or unreadable file gives none.
func ReadFiled(dir string) []string {
	f, _, ok := openRegular(filepath.Join(dir, "filed.txt"))
	if !ok {
		return nil
	}
	defer f.Close() //nolint:errcheck // read-only
	sc := bufio.NewScanner(io.LimitReader(f, maxFiledRead))
	sc.Buffer(make([]byte, 0, 64<<10), 64<<10)
	var titles []string
	for sc.Scan() {
		if _, title, ok := strings.Cut(sc.Text(), "\t"); ok {
			titles = append(titles, attention.Clean(title, attention.MaxText))
		}
	}
	return titles
}
