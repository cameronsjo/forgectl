// Package sops writes one scalar into a SOPS-encrypted YAML document without
// the value ever reaching an argv, a terminal, or a transcript.
//
// The package splits into a pure half and an effectful half. path.go, value.go,
// edit.go, and file.go make every decision — what a key path may look like,
// what a value may contain, which line to overwrite, whether the target is a
// SOPS document at all — and touch nothing. driver.go is the thin wrapper that
// runs the `sops` binary and the filesystem around those decisions.
//
// The value's route deserves stating once, because it is the reason the package
// exists: it arrives on stdin, a no-echo prompt, or the clipboard; it is written
// to a 0600 file; the FILE'S PATH travels in the child's environment and the
// value never does; the child reads the file and edits the decrypted document
// sops handed it. At no point is the value an argument to any process.
package sops

import (
	"errors"
	"regexp"
	"strings"
)

// maxPathBytes bounds a dotted path before it is split. A real key path is a
// handful of identifiers; anything near this is a caller mistake, and bounding
// it early keeps a pathological input away from the splitter and the walk.
const maxPathBytes = 1024

// maxPathSegments bounds the walk depth for the same reason.
const maxPathSegments = 16

// segmentPattern is the grammar one path segment must match. It is
// deliberately WIDER than internal/env's ValidKey (which forbids the hyphen),
// because real YAML keys in estate secrets files carry hyphens — and that
// width is exactly why a refusal here must never echo the argument. Plenty of
// provider token formats parse as a single valid segment, so a secret pasted
// into the key slot reaches this grammar and passes it.
var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*$`)

// PathGrammar is the rule ParsePath enforces, as prose for an error message.
// The message names the rule and never the input; see errBadPath.
const PathGrammar = "[A-Za-z0-9_][A-Za-z0-9_-]* separated by '.'"

// errBadPath is the single refusal for every malformed path. One message for
// every cause is deliberate: a per-cause message ("segment 2 is empty", "a
// segment contains a dot") is a side channel that describes the rejected
// input, and the rejected input may be a secret. The rule is actionable on its
// own — a caller who reads it can see what shape was wanted.
func errBadPath() error {
	return errors.New("path segments must match " + PathGrammar)
}

// ParsePath splits a dotted key path into its segments.
//
// A key whose own name contains a dot is unreachable by design, and that is a
// deliberate refusal rather than a gap: a dotted string cannot distinguish the
// document {a: {b.c: v}} from {a: {b: {c: v}}}, so any escaping scheme would be
// guessing which one the caller meant. Refusing is honest; the alternative is
// an escaping surface on every path for a case no estate secrets file has.
func ParsePath(path string) ([]string, error) {
	if path == "" || len(path) > maxPathBytes {
		return nil, errBadPath()
	}
	segments := strings.Split(path, ".")
	if len(segments) > maxPathSegments {
		return nil, errBadPath()
	}
	for _, segment := range segments {
		if !segmentPattern.MatchString(segment) {
			return nil, errBadPath()
		}
	}
	return segments, nil
}

// JoinExtract renders segments as the bracketed address `sops --extract`
// takes: ["a"]["b"]. Every segment has already passed segmentPattern, which
// admits no quote, backslash, or bracket, so there is nothing here that could
// break out of the quoting — the grammar is the escaping.
func JoinExtract(segments []string) string {
	var b strings.Builder
	for _, segment := range segments {
		b.WriteString(`["`)
		b.WriteString(segment)
		b.WriteString(`"]`)
	}
	return b.String()
}
