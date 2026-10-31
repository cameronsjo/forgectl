// Package workerwatch collects what `forgectl surface watch` shows about
// chief-of-staff workers: each worker's hc terminal, its report files and its
// pull request. It only reads. Nothing here sends keys to, kills, or briefs a
// terminal.
//
// Everything a worker produces (its screen, its file names, its branch, its
// PR's review bodies) is untrusted. This package caps and validates what it
// keeps; the renderers escape it again.
package workerwatch

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

const (
	// maxNameRunes caps a worker name. hc names are short slugs.
	maxNameRunes = 64
	// maxWorkersLines caps how many lines of a workers file are read.
	maxWorkersLines = 500
	// maxWorkersLineBytes caps one workers-file line.
	maxWorkersLineBytes = 8 << 10
)

// Entry is one worker named in a workers file, or found in hc by --cos.
type Entry struct {
	Name     string
	Worktree string // absolute; "" when unknown
	BriefDir string // absolute; "" when unknown
}

// ParseWorkers reads chief-of-staff workers lines: name|worktree|brief dir,
// one per worker. Extra fields are ignored, as the chief-of-staff scripts
// ignore them. Blank lines and lines starting with # are skipped. A line that
// cannot be used is reported in problems and skipped, so one bad line does
// not hide the others. A name seen twice keeps its first line.
func ParseWorkers(r io.Reader) (entries []Entry, problems []string, err error) {
	br := bufio.NewReader(r)
	seen := map[string]bool{}
	for n := 1; ; n++ {
		raw, tooLong, rerr := readLine(br)
		if rerr != nil && rerr != io.EOF {
			return entries, problems, fmt.Errorf("read the workers file: %w", rerr)
		}
		if rerr == io.EOF && raw == "" && !tooLong {
			return entries, problems, nil
		}
		if n > maxWorkersLines {
			problems = append(problems, fmt.Sprintf("only the first %d lines were read", maxWorkersLines))
			return entries, problems, nil
		}
		line := strings.TrimSpace(raw)
		switch {
		case tooLong:
			problems = append(problems, fmt.Sprintf("line %d: longer than %d bytes", n, maxWorkersLineBytes))
		case line == "" || strings.HasPrefix(line, "#"):
		default:
			e, why := parseWorkersLine(line)
			switch {
			case why != "":
				problems = append(problems, fmt.Sprintf("line %d: %s", n, why))
			case seen[e.Name]:
				problems = append(problems, fmt.Sprintf("line %d: %s is listed twice; the first line is used", n, e.Name))
			default:
				seen[e.Name] = true
				entries = append(entries, e)
			}
		}
		if rerr == io.EOF {
			return entries, problems, nil
		}
	}
}

// readLine reads one line without its newline. A line longer than
// maxWorkersLineBytes is consumed to its end and reported as tooLong, so one
// bad line costs only itself.
func readLine(br *bufio.Reader) (line string, tooLong bool, err error) {
	var b strings.Builder
	for {
		chunk, isPrefix, rerr := br.ReadLine()
		if rerr != nil {
			return b.String(), tooLong, rerr
		}
		if !tooLong {
			if b.Len()+len(chunk) > maxWorkersLineBytes {
				tooLong = true
				b.Reset()
			} else {
				b.Write(chunk)
			}
		}
		if !isPrefix {
			if tooLong {
				return "", true, nil
			}
			return b.String(), false, nil
		}
	}
}

func parseWorkersLine(line string) (Entry, string) {
	fields := strings.Split(line, "|")
	if len(fields) < 3 {
		return Entry{}, "want name|worktree|brief dir"
	}
	name := strings.TrimSpace(fields[0])
	if why := checkName(name); why != "" {
		return Entry{}, why
	}
	wt, why := cleanAbs(strings.TrimSpace(fields[1]), "worktree")
	if why != "" {
		return Entry{}, why
	}
	bd, why := cleanAbs(strings.TrimSpace(fields[2]), "brief dir")
	if why != "" {
		return Entry{}, why
	}
	return Entry{Name: name, Worktree: wt, BriefDir: bd}, ""
}

// checkName refuses a name that could not be an hc terminal name, or would
// render as something other than itself.
func checkName(name string) string {
	switch {
	case name == "":
		return "the name is empty"
	case !utf8.ValidString(name):
		return "the name is not UTF-8"
	case utf8.RuneCountInString(name) > maxNameRunes:
		return fmt.Sprintf("the name is longer than %d characters", maxNameRunes)
	case strings.HasPrefix(name, "-"):
		return "the name starts with -"
	}
	for _, r := range name {
		if termsafe.IsUnsafeTerminalRune(r) || termsafe.IsInvisibleRune(r) || r == ' ' {
			return "the name holds a space, a control or an invisible character"
		}
	}
	return ""
}

func cleanAbs(p, what string) (string, string) {
	if p == "" {
		return "", ""
	}
	if !utf8.ValidString(p) || strings.ContainsFunc(p, termsafe.IsUnsafeTerminalRune) {
		return "", "the " + what + " holds a control character"
	}
	if !filepath.IsAbs(p) {
		return "", "the " + what + " is not an absolute path"
	}
	return filepath.Clean(p), ""
}
