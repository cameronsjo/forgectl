package merge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// merge-audit.jsonl (atelier P4, T10.4): one JSON line per merge attempt,
// merge outcome and refusal, each carrying the SHA-256 of the line before it
// ("prev"; "" on the first line). The chain detects truncation (a write
// cut short) and accidental damage to earlier lines (one edited or removed
// by mistake). It does not detect an edit to the last line, or the last
// lines removed whole, which no later line names, nor a deliberate rewrite
// by the operator's own user, who can rewrite the whole file. A merge's
// attempt line's hash goes into the squash commit's body, so the record of
// each merge also sits on the default branch. These are the pure parts;
// internal/surface/worker holds the file.

// Audit results.
const (
	// AuditMerging is written just before `gh pr merge` runs; its hash is
	// the one the squash body carries.
	AuditMerging = "merging"
	// AuditMerged: the merge commit is on the default branch.
	AuditMerged = "merged"
	// AuditUnconfirmed: the merge could not be shown to be this attempt's
	// on the default branch (see LandUnconfirmed).
	AuditUnconfirmed = "merged-unconfirmed"
	// AuditFailed: `gh pr merge` failed and GitHub says the PR is not
	// merged.
	AuditFailed = "merge-failed"
	// AuditMergedElsewhere: GitHub says the PR merged, but its merge
	// commit's message does not carry this attempt's hash: not this merge.
	AuditMergedElsewhere = "merged-elsewhere"
	// AuditUnknown: `gh pr merge` failed and GitHub could not be read after
	// it; the PR may have merged.
	AuditUnknown = "merge-unknown"
	// AuditRefused: the policy, the subject or the pre-merge re-read
	// refused.
	AuditRefused = "refused"
)

// AuditLine is one merge-audit.jsonl line.
type AuditLine struct {
	Time  string `json:"time"`
	Actor By     `json:"actor"`
	// Worker is the worker's name: a refusal with no PR is keyed on it.
	Worker string `json:"worker"`
	Repo   string `json:"repo"`
	RepoID int64  `json:"repo_id"`
	PR     int    `json:"pr"`
	Head   string `json:"head"`
	// PolicyHash is PolicyHash of the resolved policy the line was decided
	// under.
	PolicyHash string      `json:"policy_hash"`
	Checks     []CheckSeen `json:"checks"`
	Markers    []Evidence  `json:"markers"`
	Result     string      `json:"result"`
	Reasons    []string    `json:"reasons"`
	// MergeCommit is the squash commit, on an outcome line.
	MergeCommit string `json:"merge_commit,omitempty"`
	// Attempt is the hash of the AuditMerging line an outcome line closes.
	Attempt string `json:"attempt,omitempty"`
	// Prev is the SHA-256 of the line before, "" on the first.
	Prev string `json:"prev"`
}

// LineHash is the SHA-256 of one audit line, without its newline.
func LineHash(raw []byte) string {
	sum := sha256.Sum256(bytes.TrimSuffix(raw, []byte("\n")))
	return hex.EncodeToString(sum[:])
}

// AuditEntry is one line of the file as read.
type AuditEntry struct {
	// N is the 1-based line number.
	N    int
	Line AuditLine
	Hash string
	// OK is false when the line does not decode; Line is then empty.
	OK bool
}

// ChainBreak is the first place the chain does not hold.
type ChainBreak struct {
	Line   int    `json:"line"`
	Reason string `json:"reason"`
}

// ErrAuditPartial reports an audit file whose last line has no newline: a
// write was cut short. Nothing more is appended until it is repaired by
// hand, so no line chains onto a fragment.
var ErrAuditPartial = errors.New("merge: merge-audit.jsonl ends in a partial line; repair it by hand (see docs/herdr.md)")

// ErrAuditDamaged reports an audit file whose last line does not chain onto
// the line before it: it does not decode, or its prev is not that line's
// hash (or not "" on a first line). Nothing more is appended until it is
// repaired or moved aside by hand, so no new line vouches for a damaged one.
var ErrAuditDamaged = errors.New("merge: merge-audit.jsonl is damaged: its last line does not chain onto the line before it; repair it or move it aside by hand (see docs/herdr.md)")

// splitLines splits data into lines, each without its newline, and reports
// whether the last one had none.
func splitLines(data []byte) (lines [][]byte, partial bool) {
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return append(lines, data), true
		}
		lines = append(lines, data[:i])
		data = data[i+1:]
	}
	return lines, false
}

func decodeAuditLine(raw []byte) (AuditLine, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var l AuditLine
	if err := dec.Decode(&l); err != nil {
		return AuditLine{}, err
	}
	if dec.More() {
		return AuditLine{}, errors.New("more than one JSON value on the line")
	}
	return l, nil
}

// ParseAudit reads every line and checks the chain over the whole file. It
// returns every line (those that do not decode with OK false) and the first
// break, or nil when the chain holds.
func ParseAudit(data []byte) ([]AuditEntry, *ChainBreak) {
	lines, partial := splitLines(data)
	var out []AuditEntry
	var brk *ChainBreak
	note := func(n int, format string, a ...any) {
		if brk == nil {
			brk = &ChainBreak{Line: n, Reason: fmt.Sprintf(format, a...)}
		}
	}
	prev := ""
	for i, raw := range lines {
		n := i + 1
		e := AuditEntry{N: n, Hash: LineHash(raw)}
		l, err := decodeAuditLine(raw)
		switch {
		case err != nil:
			note(n, "line %d does not decode: %v", n, err)
		default:
			e.Line, e.OK = l, true
			if l.Prev != prev {
				note(n, "line %d names prev %q, expected %q, the hash of line %d", n, l.Prev, prev, n-1)
			}
		}
		if partial && n == len(lines) {
			note(n, "line %d has no newline: a write was cut short", n)
		}
		out = append(out, e)
		prev = e.Hash
	}
	return out, brk
}

// checkLastLink checks the last of lines (at least one) names the hash of
// the line before it as prev, or "" when it is the first.
func checkLastLink(lines [][]byte) error {
	n := len(lines)
	last, err := decodeAuditLine(lines[n-1])
	if err != nil {
		return fmt.Errorf("%w (line %d does not decode: %v)", ErrAuditDamaged, n, err)
	}
	want := ""
	if n > 1 {
		want = LineHash(lines[n-2])
	}
	if last.Prev != want {
		return fmt.Errorf("%w (line %d names prev %q, expected %q)", ErrAuditDamaged, n, last.Prev, want)
	}
	return nil
}

// ReasonSet is a refusal's reasons as a set: sorted, without repeats. The
// audit's refusal limit and the drain's merge-refused events both compare
// refusals by it.
func ReasonSet(reasons []string) []string {
	set := slices.Clone(reasons)
	slices.Sort(set)
	return slices.Compact(set)
}

// sameSubject reports whether e and l are about the same thing: the same
// actor and repository, and the same PR, or for lines with no PR the same
// worker.
func sameSubject(e, l AuditLine) bool {
	if e.Actor != l.Actor || !strings.EqualFold(e.Repo, l.Repo) || e.RepoID != l.RepoID || e.PR != l.PR {
		return false
	}
	return l.PR != 0 || e.Worker == l.Worker
}

// repeatsRefusal reports whether the refusal l repeats e, the most recent
// line about the same subject: e is a refusal too, at the same head, with
// the same reason set and policy hash.
func repeatsRefusal(e, l AuditLine) bool {
	return e.Result == AuditRefused && e.Head == l.Head && e.PolicyHash == l.PolicyHash &&
		slices.Equal(ReasonSet(e.Reasons), ReasonSet(l.Reasons))
}

// AppendAudit encodes l chained onto existing, the whole file as it is now.
// It returns the line to append (with its newline) and its hash. A refusal
// that repeats the most recent line about the same subject (sameSubject,
// repeatsRefusal) is not written: write is false. Any later line about that
// subject that is not the same refusal (a merge attempt, an outcome, another
// refusal) ends the repeat. An existing file ending in a partial line is
// ErrAuditPartial, and one whose last line does not chain onto the line
// before it is ErrAuditDamaged.
func AppendAudit(existing []byte, l AuditLine) (line []byte, hash string, write bool, err error) {
	lines, partial := splitLines(existing)
	if partial {
		return nil, "", false, ErrAuditPartial
	}
	l.Prev = ""
	if n := len(lines); n > 0 {
		if err := checkLastLink(lines); err != nil {
			return nil, "", false, err
		}
		l.Prev = LineHash(lines[n-1])
	}
	if l.Result == AuditRefused {
		for i := len(lines) - 1; i >= 0; i-- {
			e, err := decodeAuditLine(lines[i])
			if err != nil || !sameSubject(e, l) {
				continue
			}
			if repeatsRefusal(e, l) {
				return nil, "", false, nil
			}
			break
		}
	}
	if l.Checks == nil {
		l.Checks = []CheckSeen{}
	}
	if l.Markers == nil {
		l.Markers = []Evidence{}
	}
	if l.Reasons == nil {
		l.Reasons = []string{}
	}
	// termsafe:allow-raw-json written to the private audit file; `surface audit` prints it through termsafe
	data, err := json.Marshal(l)
	if err != nil {
		return nil, "", false, fmt.Errorf("merge: encode the audit line: %w", err)
	}
	return append(data, '\n'), LineHash(data), true, nil
}
