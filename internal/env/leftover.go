// leftover.go owns every scratch name forgectl creates beside a target, and
// the scan that refuses a write while a previous run's scratch is still there.
//
// # Why the names carry the target
//
// Two things are created beside a target and can outlive their run: the
// writeAtomic scratch directory (its temp file holds the whole new document,
// secrets included) and the `env set --sops` work directory (a plaintext
// value, a decrypted read-back, the ciphertext backup). A signal handler
// covers the catchable signals under --sops. SIGKILL, SIGSTOP, a fault and a
// power loss run no code, so whatever they interrupt stays behind. Both
// directories carry a `*` .gitignore from creation (scratch.go,
// cameronsjo/forgectl#698 and #737), so git neither stages nor lists them,
// and this scan, which lists the directory itself, is what notices them.
//
// Before #737 the writeAtomic temp file sat directly beside the target, where
// `git add -A` would commit it. A forgectl that old can still have left one,
// so the scan keeps refusing on that name too.
//
// The lock is per target (`<base>.lock`), and the scratch names used to carry
// no target. So a scan for them could not tell a dead run's leftover from a
// concurrent writer's live file on a sibling target, and a directory-wide
// sweep would have deleted that live file. Each name now carries a scope tag
// derived from the target, so the scan below, holding the target's lock,
// sees only names that the lock covers.
//
// # Why a short hash, not the base name
//
// A base name can be up to NAME_MAX bytes, and a prefix plus a random suffix on
// top of it would overflow. Twelve hex digits of SHA-256 are 48 bits: a
// collision between two targets in one directory is not a practical event,
// and the cost of one is a false refusal, never a missed leftover. The base is
// lowercased first. On a case-insensitive volume (APFS by default) `.ENV` and
// `.env` are one file and share one lock, so they must share a tag. On a
// case-sensitive one they are two files that now share a tag, and the cost is
// again only a false refusal.
//
// # Why refuse, and never delete
//
// Holding the lock, a scoped leftover cannot belong to a concurrent forgectl
// run. It CAN belong to a sops child that outlived a SIGKILLed parent: the
// child is not in its own process group, and nothing kills it. Deleting its
// work directory under it is a race with a writer, and the leftover is also
// the only evidence that a previous write died half done. So the scan refuses,
// names every path, and leaves the operator to decide. It removes nothing.
package env

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// sopsScratchPrefix begins every name the `--sops` path creates beside a
// target. internal/cli's __sops-edit still checks only this prefix when it
// constrains its work directory, and every scoped name keeps it.
const sopsScratchPrefix = ".forgectl-sops-"

// scopeTagLen is the scope tag's length in hex digits.
const scopeTagLen = 12

// scopeTag returns the tag that scopes scratch names to base. See the package
// comment above for the hash, the length, and the lowercasing.
func scopeTag(base string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(base)))
	return hex.EncodeToString(sum[:])[:scopeTagLen]
}

// envTempPrefix is the prefix of the temp file writeAtomic created directly
// beside t before cameronsjo/forgectl#737: `.env-<tag>.`, then a random part
// and `.tmp`. Nothing creates it now; the scan still refuses on it. It must
// not match IsEnvFileName, so a leftover cannot be reached through --file
// without --any-file.
func (t Target) envTempPrefix() string { return tempPrefix + scopeTag(t.base) + "." }

// envScratchDirPrefix is writeAtomic's scratch-directory prefix for t:
// `.forgectl-env-<tag>-`, then a random part.
func (t Target) envScratchDirPrefix() string {
	return envScratchPrefix + scopeTag(t.base) + "-"
}

// SopsWorkDirPattern is the os.MkdirTemp pattern for t's `--sops` work
// directory: `.forgectl-sops-<tag>-` plus MkdirTemp's random suffix.
func (t Target) SopsWorkDirPattern() string { return sopsScratchPrefix + scopeTag(t.base) + "-" }

// SopsBackupPath is where an interrupted `--sops` run leaves the ciphertext
// backup: `.forgectl-sops-<tag>.backup`, beside the target. It is absolute and
// in the target's own directory, so a rename out of the work directory never
// crosses a filesystem.
func (t Target) SopsBackupPath() string {
	return filepath.Join(filepath.Dir(t.path), t.sopsBackupName())
}

func (t Target) sopsBackupName() string {
	return sopsScratchPrefix + scopeTag(t.base) + ".backup"
}

// The shapes names had before they carried a scope tag. A leftover in one of
// these shapes cannot be attributed to any target, so it earns a warning and
// never a refusal. A refusal would block every target in the directory on a
// file that may belong to none of them.
var (
	// 10 random bytes, unpadded base32: 16 characters.
	legacyEnvTemp = regexp.MustCompile(`^\.env-[A-Z2-7]{16}\.tmp$`)
	// os.MkdirTemp's decimal random suffix.
	legacySopsWorkDir = regexp.MustCompile(`^\.forgectl-sops-[0-9]+$`)
)

// leftoverWarnings receives the warning for an unattributable leftover. It is a
// variable only so tests can capture it.
var leftoverWarnings io.Writer = os.Stderr

// maxNamedLeftovers bounds how many paths one refusal lists. A directory
// holding more than this is named by count past it.
const maxNamedLeftovers = 8

// scanLeftovers refuses when t's directory holds scratch scoped to t, and warns
// about scratch in a legacy, unscoped shape. It must run while t's lock is
// held, before anything creates new scratch of its own. Otherwise it would
// find that scratch, or race a concurrent run's live scratch.
//
// It lists through the pinned descriptor, like every other operation here.
// A listing that fails is a refusal. Proceeding would skip the one check that
// stands between a dead run's plaintext and another write on top of it.
func scanLeftovers(t Target) error {
	names, err := t.dir.names()
	if err != nil {
		return fmt.Errorf("refusing to write %s: its directory could not be listed to check for leftovers from an interrupted run: %w", t.Rel(), err)
	}
	sort.Strings(names)

	envPrefix := t.envTempPrefix()
	envDirPrefix := t.envScratchDirPrefix()
	workPrefix := t.SopsWorkDirPattern()
	backupName := t.sopsBackupName()
	relDir := filepath.Dir(t.Rel())
	rel := func(name string) string { return termsafe.QuotePath(filepath.Join(relDir, name)) }

	var backups, workDirs, envDirs, temps, legacy []string
	for _, name := range names {
		switch {
		case name == backupName:
			backups = append(backups, name)
		case strings.HasPrefix(name, workPrefix):
			workDirs = append(workDirs, name)
		case strings.HasPrefix(name, envDirPrefix):
			envDirs = append(envDirs, name)
		case strings.HasPrefix(name, envPrefix) && strings.HasSuffix(name, ".tmp"):
			temps = append(temps, name)
		case legacyEnvTemp.MatchString(name), legacySopsWorkDir.MatchString(name):
			legacy = append(legacy, name)
		}
	}

	for _, name := range legacy {
		_, _ = fmt.Fprintf(leftoverWarnings,
			"warning: %s looks like a leftover from an interrupted forgectl write made before leftovers were named by target; it may hold a secret in plaintext, so inspect it and delete it by hand\n",
			rel(name))
	}

	if len(backups)+len(workDirs)+len(envDirs)+len(temps) == 0 {
		return nil
	}

	var lines []string
	for _, name := range backups {
		lines = append(lines, fmt.Sprintf(
			"%s: left by an `env set --sops` interrupted while sops may have been writing %s, which may now hold an unverified value; this is its ciphertext from before that run. Restore %s from it or from git, then delete it",
			rel(name), termsafe.QuotePath(t.Rel()), termsafe.QuotePath(t.Rel())))
	}
	for _, name := range workDirs {
		line := fmt.Sprintf(
			"%s: the work directory of an interrupted `env set --sops`; it may hold a plaintext value or sops' decrypted copy of the whole file, and a sops process that outlived forgectl may still be using it. Make sure no sops process is running, then delete it",
			rel(name))
		if _, _, exists, err := t.dir.lstat(name + "/backup"); err == nil && exists {
			line += fmt.Sprintf(". Its ciphertext backup of %s from before that run is %s", termsafe.QuotePath(t.Rel()), rel(name+"/backup"))
		}
		lines = append(lines, line)
	}
	for _, name := range envDirs {
		lines = append(lines, fmt.Sprintf(
			"%s: the scratch directory of an interrupted write to %s; it may hold the whole new file, secrets included. git does not list it, because it carries a .gitignore. Inspect it, then delete it",
			rel(name), termsafe.QuotePath(t.Rel())))
	}
	for _, name := range temps {
		lines = append(lines, fmt.Sprintf(
			"%s: the temp file of an interrupted write to %s; it may hold the whole new file, secrets included. Inspect it, then delete it",
			rel(name), termsafe.QuotePath(t.Rel())))
	}

	if extra := len(lines) - maxNamedLeftovers; extra > 0 {
		lines = append(lines[:maxNamedLeftovers], fmt.Sprintf("and %d more", extra))
	}
	msg := "refusing to write " + termsafe.QuotePath(t.Rel()) +
		": a previous forgectl run on it was interrupted and left scratch behind. Nothing was removed:\n  - " +
		strings.Join(lines, "\n  - ")
	if twin := caseTwin(t, names); twin != "" {
		// The scope tag lowercases the base, so on a case-sensitive volume
		// this target and its case twin share every scratch name, and the
		// entries above may be the twin's (cameronsjo/forgectl#652).
		msg += fmt.Sprintf("\n%s shares these scratch names because its name differs only in letter case, so they may belong to a run on it instead", rel(twin))
	}
	return errors.New(msg)
}

// caseTwin returns an entry of names that equals t's base in every letter but
// case and is a DIFFERENT file, or "" when there is none. It lowercases exactly
// as scopeTag does, so a twin it finds really shares the tag. On a
// case-insensitive volume the listing can spell the target itself in another
// case (the file is stored as `.ENV`, the target was named `.env`), so a
// candidate that is the same file as the target is not a twin. A candidate
// that cannot be compared is not claimed either: the note is advice, and a
// false one would send the operator to the wrong file.
func caseTwin(t Target, names []string) string {
	lower := strings.ToLower(t.base)
	for _, name := range names {
		if name == t.base || strings.ToLower(name) != lower {
			continue
		}
		// A target that does not exist yet cannot be the candidate: on a
		// case-insensitive volume the candidate's existence would mean the
		// target's.
		if _, _, exists, err := t.dir.lstat(t.base); err == nil && !exists {
			return name
		}
		if same, err := t.dir.sameFile(name, t.base); err != nil || same {
			continue
		}
		return name
	}
	return ""
}
