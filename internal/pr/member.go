package pr

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/sandbox"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// breadcrumbMember is the CAPABILITY a successful membership check yields: not
// "yes, that path is a member" but "here is the authoritative file I verified,
// and here is exactly what it contained when I verified it".
//
// The distinction is the whole point. The old assertMember returned a bool and
// left the caller acting on the operand it had been handed — fine when the
// only mutation was a workspace removal gated on a live sandbox, but not when
// the operation is UNLINKING A FILE. A capability makes it impossible to
// delete something other than what was checked, because the path to delete
// comes out of the check rather than in from the caller.
type breadcrumbMember struct {
	// path is the authoritative entry: a real, non-symlink .json file joined
	// under the client's own session directory. Never the caller's operand.
	path string
	// displayPath is path quoted once at the capability boundary. Downstream
	// refusal messages can never accidentally render the hostile filename raw.
	displayPath string
	// info is that file's identity at check time, for a SameFile recheck
	// immediately before the unlink.
	info fs.FileInfo
	// dirInfo is the canonical session directory's identity at check time, so
	// a parent swapped underneath the operation is detected.
	dirInfo fs.FileInfo
	// bytes are the exact file contents authorized.
	bytes []byte
	// breadcrumb is the record decoded from those bytes.
	breadcrumb Breadcrumb
}

// resolveBreadcrumbMember resolves operand to the authoritative session-directory
// member it names, or refuses.
//
// The historical behaviors it preserves: an operand may be an exact member, a
// "./" or parent-relative alias, or a symlink from outside the directory that
// points into it. All three keep working, because breaking them would break
// the paths users actually type.
//
// What changes is what comes back. The returned path is always a real
// enumerated entry joined under c.sessionsDir — never the operand, and never
// an alias. So `teardown outside-link.json` selects and deletes the REAL
// breadcrumb, and an in-directory symlink can never turn a reported success
// into unlinking the alias while the real record survives.
//
// Ambiguity refuses. Two entries resolving to one target leave no way to know
// which the caller meant, so only an EXACT lexical name settles it; any other
// operand shape is rejected with zero mutation.
func (c *Client) resolveBreadcrumbMember(operand string) (breadcrumbMember, error) {
	member, err := c.resolveBreadcrumbEntry(operand)
	if err != nil {
		return breadcrumbMember{}, err
	}
	bc, err := decodeBreadcrumbRecord(member.bytes, member.path)
	if err != nil {
		return breadcrumbMember{}, err
	}
	member.breadcrumb = bc
	return member, nil
}

// resolveBreadcrumbEntry is resolveBreadcrumbMember's identity half: everything
// up to and including the authorized bytes, WITHOUT decoding them. The returned
// member's breadcrumb field is the zero value.
//
// It exists for exactly one caller — `pr repair --apply --forget-if-absent` on a
// record this build cannot decode. That record blocks every launch (the
// counting paths refuse on it) while no verb can read it, so the one escape has
// to be a removal that proves WHICH file it is unlinking without ever claiming
// to know what the file says. The pinned-handle protocol downstream answers
// that question from dev+ino and byte equality, neither of which needs a
// decode.
//
// Every other caller goes through resolveBreadcrumbMember: a decode failure is
// a refusal there, and must stay one.
func (c *Client) resolveBreadcrumbEntry(operand string) (breadcrumbMember, error) {
	canonicalDir, err := filepath.EvalSymlinks(c.sessionsDir)
	if err != nil {
		return breadcrumbMember{}, fmt.Errorf("resolve pr sessions dir %s: %w", c.sessionsDir, err)
	}
	dirInfo, err := os.Lstat(canonicalDir)
	if err != nil {
		return breadcrumbMember{}, fmt.Errorf("stat pr sessions dir %s: %w", canonicalDir, err)
	}
	if !dirInfo.IsDir() {
		return breadcrumbMember{}, fmt.Errorf("pr sessions dir %s is not a directory", canonicalDir)
	}

	entries, err := os.ReadDir(c.sessionsDir)
	if err != nil {
		return breadcrumbMember{}, fmt.Errorf("read pr sessions dir: %w", err)
	}

	// The enumerated candidate set: non-directory .json entries only.
	type candidate struct {
		lexical   string // c.sessionsDir joined with the entry name
		resolved  string
		isSymlink bool
	}
	var candidates []candidate
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		lexical := filepath.Join(c.sessionsDir, e.Name())
		candidates = append(candidates, candidate{
			lexical:   lexical,
			resolved:  resolvePath(lexical),
			isSymlink: e.Type()&fs.ModeSymlink != 0,
		})
	}

	want := resolvePath(operand)
	operandLexical := lexicalAbs(operand)

	// An EXACT lexical name is the only thing that disambiguates, so look for
	// it first and independently of resolution.
	var selected *candidate
	for i := range candidates {
		if candidates[i].lexical == operandLexical {
			selected = &candidates[i]
			break
		}
	}
	if selected == nil {
		var matches []*candidate
		for i := range candidates {
			if candidates[i].resolved == want {
				matches = append(matches, &candidates[i])
			}
		}
		switch len(matches) {
		case 0:
			slog.Error("Teardown target is not a known session breadcrumb; refusing.", "path", operand)
			return breadcrumbMember{}, fmt.Errorf("%q is not a known pr session breadcrumb", operand)
		case 1:
			selected = matches[0]
		default:
			slog.Error("Teardown target is ambiguous; refusing.", "path", operand, "matches", len(matches))
			return breadcrumbMember{}, fmt.Errorf(
				"%q resolves to a breadcrumb reachable under %d names; name the exact breadcrumb file to disambiguate",
				operand, len(matches))
		}
	}

	// An in-directory symlink selects its REAL target member, and only if that
	// target is itself an enumerated member. Deleting the alias would leave the
	// real breadcrumb behind while reporting success; deleting a target outside
	// the enumerated set would reach past the guard entirely.
	if selected.isSymlink {
		target := selected.resolved
		var realEntry *candidate
		for i := range candidates {
			if !candidates[i].isSymlink && candidates[i].resolved == target {
				realEntry = &candidates[i]
				break
			}
		}
		if realEntry == nil {
			slog.Error("Teardown target is a link outside the known breadcrumb set; refusing.", "path", operand)
			return breadcrumbMember{}, fmt.Errorf(
				"%q is a link whose target is not itself a known pr session breadcrumb", operand)
		}
		selected = realEntry
	}

	// Defense in depth: the authoritative entry must sit directly in the
	// canonical session directory.
	if parent := filepath.Dir(resolvePath(selected.lexical)); parent != canonicalDir {
		return breadcrumbMember{}, fmt.Errorf(
			"breadcrumb %s resolves outside the pr session directory", termsafe.QuotePath(selected.lexical))
	}

	info, err := os.Lstat(selected.lexical)
	if err != nil {
		return breadcrumbMember{}, fmt.Errorf("stat breadcrumb %s: %w",
			termsafe.QuotePath(selected.lexical), termsafe.Error(err))
	}
	if !info.Mode().IsRegular() {
		return breadcrumbMember{}, fmt.Errorf("breadcrumb %s is not a regular file", termsafe.QuotePath(selected.lexical))
	}

	// LOCATION guard first, then the bytes — the same order loadBreadcrumbRecord
	// applies, and for the same reason: membership inside the forgectl-owned dir
	// is settled before the file is read.
	if !sandbox.WithinWorkspace(c.sessionsDir, selected.lexical) {
		slog.Error("Breadcrumb path escapes session-state dir; refusing.", "path", selected.lexical, "sessionsDir", c.sessionsDir)
		return breadcrumbMember{}, fmt.Errorf("breadcrumb %s is not inside the forgectl session-state dir",
			termsafe.QuotePath(selected.lexical))
	}
	file, err := os.Open(selected.lexical) //nolint:gosec // location-validated directly above
	if err != nil {
		return breadcrumbMember{}, fmt.Errorf("read breadcrumb %s: %w",
			termsafe.QuotePath(selected.lexical), termsafe.Error(err))
	}
	data, readErr := readBreadcrumbBytes(file)
	closeErr := file.Close()
	if readErr != nil {
		return breadcrumbMember{}, fmt.Errorf("read breadcrumb %s: %w",
			termsafe.QuotePath(selected.lexical), termsafe.Error(readErr))
	}
	if closeErr != nil {
		return breadcrumbMember{}, fmt.Errorf("close breadcrumb %s: %w",
			termsafe.QuotePath(selected.lexical), termsafe.Error(closeErr))
	}
	return breadcrumbMember{
		path:        selected.lexical,
		displayPath: termsafe.QuotePath(selected.lexical),
		info:        info,
		dirInfo:     dirInfo,
		bytes:       data,
	}, nil
}

// lexicalAbs returns the absolute, cleaned form of path WITHOUT resolving
// symlinks — the form an exact-name comparison needs, since resolution is
// exactly what an exact name is meant to bypass.
func lexicalAbs(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}
