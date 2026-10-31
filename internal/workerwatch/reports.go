package workerwatch

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"time"
)

// CompleteMarker is the last line a finished chief-of-staff report carries.
const CompleteMarker = "REPORT-COMPLETE"

const (
	// reportTailBytes is how much of a report's end is read to find its
	// last line.
	reportTailBytes = 4 << 10
	// maxReportFiles caps how many report files one brief dir lists.
	maxReportFiles = 50
)

// reportNameRe matches the report files a brief owes: report.md, or
// report-<topic>.md for a fix round (the chief-of-staff convention).
var reportNameRe = regexp.MustCompile(`^report(?:-[A-Za-z0-9._-]{1,64})?\.md$`)

// ReportFile is one report file in a brief dir.
type ReportFile struct {
	Name     string    `json:"name"`
	Complete bool      `json:"complete"`
	Modified time.Time `json:"modified"`
}

// Reports is what a worker's brief dir holds.
type Reports struct {
	// Files is newest first.
	Files []ReportFile `json:"files"`
	// Error says why the dir could not be read; Files is then empty.
	Error string `json:"error,omitempty"`
}

// Newest is the most recently modified report, if any.
func (r Reports) Newest() (ReportFile, bool) {
	if len(r.Files) == 0 {
		return ReportFile{}, false
	}
	return r.Files[0], true
}

// CompleteCount is how many reports end in CompleteMarker.
func (r Reports) CompleteCount() int {
	n := 0
	for _, f := range r.Files {
		if f.Complete {
			n++
		}
	}
	return n
}

// ScanReports lists the report files in dir. Only regular files are read: a
// symlink named report.md is skipped, so the watch reads nothing outside the
// brief dir.
func ScanReports(fsys fs.FS) Reports {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return Reports{Error: readDirReason(err)}
	}
	var files []ReportFile
	for _, e := range entries {
		if !reportNameRe.MatchString(e.Name()) || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, ReportFile{
			Name:     e.Name(),
			Complete: endsComplete(fsys, e.Name(), info.Size()),
			Modified: info.ModTime().UTC(),
		})
	}
	slices.SortStableFunc(files, func(a, b ReportFile) int {
		if c := b.Modified.Compare(a.Modified); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	if len(files) > maxReportFiles {
		files = files[:maxReportFiles]
	}
	return Reports{Files: files}
}

// ScanReportDir is ScanReports over a directory on disk.
func ScanReportDir(dir string) Reports {
	if dir == "" {
		return Reports{}
	}
	return ScanReports(os.DirFS(dir))
}

func readDirReason(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "the brief dir does not exist"
	case errors.Is(err, fs.ErrPermission):
		return "the brief dir is not readable"
	}
	return "the brief dir could not be read"
}

// endsComplete reports whether the file's last non-blank line is exactly
// CompleteMarker. It reads only the last reportTailBytes.
func endsComplete(fsys fs.FS, name string, size int64) (complete bool) {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			complete = false
		}
	}()
	if size > reportTailBytes {
		if s, ok := f.(io.Seeker); ok {
			if _, err := s.Seek(size-reportTailBytes, io.SeekStart); err != nil {
				return false
			}
		}
	}
	tail, err := io.ReadAll(io.LimitReader(f, reportTailBytes*2))
	if err != nil {
		return false
	}
	return LastLineComplete(tail)
}

// LastLineComplete reports whether text's last non-blank line is exactly
// CompleteMarker (surrounding spaces and a CR allowed).
func LastLineComplete(text []byte) bool {
	text = bytes.TrimRight(text, " \t\r\n")
	if i := bytes.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	return string(bytes.TrimSpace(text)) == CompleteMarker
}

// String is a one-line summary: "none", "2 (1 complete) · newest report.md".
func (r Reports) String() string {
	if r.Error != "" {
		return r.Error
	}
	n, ok := r.Newest()
	if !ok {
		return "none"
	}
	mark := ""
	if n.Complete {
		mark = " " + CompleteMarker
	}
	return fmt.Sprintf("%d (%d complete) · newest %s%s", len(r.Files), r.CompleteCount(), n.Name, mark)
}
