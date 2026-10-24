// Package pip is the ops layer for `forgectl pip`: a comment- and
// whitespace-preserving editor for pip.conf's INI-lite format. This file
// (inifile.go) is the pure model: parse bytes into an ordered line list,
// mutate it, serialize back out. No filesystem or Cobra dependency lives
// here — that's pip.go and internal/cli/pip.go respectively.
//
// Reversibility choice: File.Remove never deletes a line. It prefixes the
// matched entry (and any indented continuation lines belonging to it) with a
// removedMarker comment, preserving the exact original bytes after the
// marker. File.Restore strips the marker back off. This is the same
// rename-aside-and-undo shape as internal/quarantine's Hide/Restore, just
// encoded as an in-file comment toggle instead of a filesystem rename — no
// backup sidecar, the reversible state lives inline in pip.conf itself.
package pip

import "strings"

// removedMarker prefixes a line File.Remove has commented out. It is
// deliberately unusual so it never collides with a real user comment; strip
// it and the original line reappears byte-for-byte.
const removedMarker = "# forgectl-pip:removed "

// LineKind classifies one parsed line of pip.conf. It is derived metadata for
// querying/mutating the model — Serialize only ever consults Raw and
// FinalNewline, so a stale or approximate Kind can never corrupt round-trip
// output.
type LineKind int

const (
	// KindBlank is a line that is empty or all whitespace.
	KindBlank LineKind = iota
	// KindComment is a line whose trimmed form starts with '#' or ';', or an
	// unrecognized/malformed line that classify falls back to treating as
	// inert.
	KindComment
	// KindSection is a "[name]" section header.
	KindSection
	// KindEntry is a "key = value" (or "key: value") line.
	KindEntry
	// KindContinuation is a line indented deeper than the nearest preceding
	// KindEntry line, extending that entry's value.
	KindContinuation
)

// Line is one physical line of a pip.conf file. Raw is the exact original
// text (no trailing newline) — the only field Serialize reads. The rest is
// classification metadata used to locate entries for Remove/Restore.
type Line struct {
	Kind    LineKind
	Raw     string
	Section string // active section for Comment/Section/Entry/Continuation kinds
	Key     string // normalized (lowercased) key, KindEntry/KindContinuation only
}

// File is a parsed pip.conf: an ordered line list plus whether the source
// ended with a trailing newline. It preserves comments, blank lines, and
// ordering exactly, so Serialize(Parse(x)) == x for any pip.conf shape.
type File struct {
	Lines        []Line
	FinalNewline bool
}

// NewFile returns an empty File — the starting point when no pip.conf exists
// yet. Named NewFile (not New) because New is reserved package-wide for the
// house Client constructor (New(run exec.Runner, opts ...Option) *Client, see
// pip.go).
func NewFile() *File {
	return &File{}
}

// Parse reads data into a File, preserving every byte's position so
// Serialize can reproduce it exactly.
func Parse(data []byte) *File {
	text := string(data)
	if text == "" {
		return &File{}
	}

	finalNewline := strings.HasSuffix(text, "\n")
	body := strings.TrimSuffix(text, "\n")
	rawLines := strings.Split(body, "\n")

	lines := make([]Line, len(rawLines))
	for i, raw := range rawLines {
		lines[i] = Line{Raw: raw}
	}
	classify(lines)
	return &File{Lines: lines, FinalNewline: finalNewline}
}

// Serialize renders f back to bytes. It reads only Raw and FinalNewline, so
// it round-trips regardless of whether Kind/Section/Key are current.
func (f *File) Serialize() []byte {
	if len(f.Lines) == 0 {
		return nil
	}
	raws := make([]string, len(f.Lines))
	for i, l := range f.Lines {
		raws[i] = l.Raw
	}
	out := strings.Join(raws, "\n")
	if f.FinalNewline {
		out += "\n"
	}
	return []byte(out)
}

// Remove comments out every KindEntry line in section whose key matches
// (both compared case-insensitively), along with any KindContinuation lines
// that extend it. Matched lines are prefixed with removedMarker rather than
// deleted, so Restore can bring them back verbatim. Returns the number of
// entries (not lines) removed; 0 means no match — f is left unmodified.
func (f *File) Remove(section, key string) int {
	wantKey := strings.ToLower(strings.TrimSpace(key))
	removed := 0

	i := 0
	for i < len(f.Lines) {
		line := f.Lines[i]
		if line.Kind == KindEntry && strings.EqualFold(line.Section, section) && line.Key == wantKey {
			f.Lines[i].Raw = removedMarker + line.Raw
			// A value runs on through blank and comment lines (see classify),
			// so step over them and mark every continuation line it holds.
			j := i + 1
			for j < len(f.Lines) {
				k := f.Lines[j].Kind
				if k == KindContinuation {
					f.Lines[j].Raw = removedMarker + f.Lines[j].Raw
				} else if k != KindBlank && k != KindComment {
					break
				}
				j++
			}
			removed++
			i = j
			continue
		}
		i++
	}

	if removed > 0 {
		classify(f.Lines)
	}
	return removed
}

// Restore strips removedMarker off every line Remove has ever tagged,
// restoring each to its exact original text. It restores everything marked
// regardless of which Remove call (section/key) tagged it — there is one
// reversible layer, not a per-key undo stack, mirroring quarantine's
// Hide/Restore (a single Move list, not a versioned history). It is
// idempotent: calling it again once nothing is marked is a no-op. Returns the
// number of lines restored.
func (f *File) Restore() int {
	restored := 0
	for i := range f.Lines {
		if strings.HasPrefix(f.Lines[i].Raw, removedMarker) {
			f.Lines[i].Raw = strings.TrimPrefix(f.Lines[i].Raw, removedMarker)
			restored++
		}
	}
	if restored > 0 {
		classify(f.Lines)
	}
	return restored
}

// classify walks lines top-to-bottom, assigning Kind/Section/Key from each
// line's Raw text and the running section/continuation state. It fully
// overwrites every field but Raw, so it is safe to call repeatedly (e.g.
// after Remove/Restore mutate some Raw values) to refresh the model.
//
// Continuations follow configparser's defaults: a value runs on through blank
// and comment lines, and any later line indented deeper than its entry
// extends it, even one shaped like "[name]" or "key = value". Only a line at
// or left of the entry's indent ends the value.
func classify(lines []Line) {
	currentSection := ""
	valueKey, valueIndent := "", 0 // the entry whose value is still open, if any
	for i := range lines {
		raw := lines[i].Raw
		trimmed := strings.TrimSpace(raw)

		switch {
		case trimmed == "":
			lines[i] = Line{Kind: KindBlank, Raw: raw}

		case strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";"):
			lines[i] = Line{Kind: KindComment, Raw: raw, Section: currentSection}

		case valueKey != "" && indentOf(raw) > valueIndent:
			lines[i] = Line{Kind: KindContinuation, Raw: raw, Section: currentSection, Key: valueKey}

		case isSectionHeader(trimmed):
			currentSection = sectionName(trimmed)
			valueKey = ""
			lines[i] = Line{Kind: KindSection, Raw: raw, Section: currentSection}

		default:
			if key, ok := parseEntryKey(trimmed); ok {
				valueKey, valueIndent = key, indentOf(raw)
				lines[i] = Line{Kind: KindEntry, Raw: raw, Section: currentSection, Key: key}
			} else {
				// Malformed/unrecognized line: treat as inert rather than
				// guessing wrong — Serialize doesn't care either way.
				valueKey = ""
				lines[i] = Line{Kind: KindComment, Raw: raw, Section: currentSection}
			}
		}
	}
}

// isSectionHeader reports whether trimmed is a section header. configparser
// matches `\[(?P<header>.+)\]` from the start of the line without anchoring
// its end, so "[global] # mirror" is section "global": the header is
// everything up to the LAST ']', and anything after it is ignored.
func isSectionHeader(trimmed string) bool {
	return len(trimmed) >= 3 && trimmed[0] == '[' && strings.LastIndexByte(trimmed, ']') > 1
}

// sectionName returns the name of the header line trimmed (see
// isSectionHeader), with surrounding whitespace removed.
func sectionName(trimmed string) string {
	return strings.TrimSpace(trimmed[1:strings.LastIndexByte(trimmed, ']')])
}

// indentOf returns the number of leading spaces and tabs in raw — the
// configparser measure that decides whether a line continues a value.
func indentOf(raw string) int {
	return len(raw) - len(strings.TrimLeft(raw, " \t"))
}

// parseEntryKey extracts the key from a "key = value" or "key: value" line,
// picking whichever delimiter ('=' or ':') appears first — pip.conf, via
// Python's configparser, accepts either. Returns ("", false) when neither
// delimiter is present or the key half is empty (not a valid entry line).
func parseEntryKey(trimmed string) (string, bool) {
	eq := strings.IndexByte(trimmed, '=')
	co := strings.IndexByte(trimmed, ':')

	idx := -1
	switch {
	case eq == -1 && co == -1:
		return "", false
	case eq == -1:
		idx = co
	case co == -1:
		idx = eq
	case eq < co:
		idx = eq
	default:
		idx = co
	}

	key := strings.TrimSpace(trimmed[:idx])
	if key == "" {
		return "", false
	}
	return strings.ToLower(key), true
}
