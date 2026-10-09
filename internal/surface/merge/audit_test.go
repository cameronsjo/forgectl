package merge

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func auditLine(result string, reasons ...string) AuditLine {
	return AuditLine{Time: "2026-10-09T20:00:00Z", Actor: ByCLI, Repo: "cameronsjo/forgectl", RepoID: forgectlID, PR: 1204, Head: head1204,
		PolicyHash: strings.Repeat("b", 64), Result: result, Reasons: reasons}
}

// appendAll chains lines onto an empty file as the store would.
func appendAll(t *testing.T, lines ...AuditLine) ([]byte, []string) {
	t.Helper()
	var file []byte
	var hashes []string
	for _, l := range lines {
		line, hash, write, err := AppendAudit(file, l)
		if err != nil {
			t.Fatal(err)
		}
		if write {
			file = append(file, line...)
			hashes = append(hashes, hash)
		}
	}
	return file, hashes
}

func TestAuditChain(t *testing.T) {
	file, hashes := appendAll(t, auditLine(AuditRefused, "a"), auditLine(AuditMerging), auditLine(AuditMerged))
	entries, brk := ParseAudit(file)
	if brk != nil || len(entries) != 3 {
		t.Fatalf("an intact file: %d entries, break %+v", len(entries), brk)
	}
	if entries[0].Line.Prev != "" || entries[1].Line.Prev != hashes[0] || entries[2].Line.Prev != hashes[1] {
		t.Fatalf("prev links %q %q %q; hashes %q", entries[0].Line.Prev, entries[1].Line.Prev, entries[2].Line.Prev, hashes)
	}
	if entries[1].Hash != hashes[1] || LineHash([]byte(strings.Split(string(file), "\n")[1])) != hashes[1] {
		t.Fatal("the returned hash is not the line's")
	}
	lines := strings.SplitAfter(string(file), "\n")
	cases := map[string]struct {
		data string
		line int
		want string
	}{
		"a line edited":      {strings.Replace(string(file), `"reasons":["a"]`, `"reasons":["b"]`, 1), 2, "line 2 names prev"},
		"a line removed":     {lines[0] + lines[2], 2, "line 2 names prev"},
		"the first removed":  {lines[1] + lines[2], 1, `line 1 names prev "` + hashes[0]},
		"a partial last":     {string(file) + `{"time":"x"`, 4, "line 4 does not decode"},
		"cut mid-line":       {strings.TrimSuffix(string(file), "\n"), 3, "line 3 has no newline"},
		"an unknown field":   {lines[0] + strings.Replace(lines[1], `"time"`, `"extra":1,"time"`, 1) + lines[2], 2, "does not decode"},
		"garbage in between": {lines[0] + "not json\n" + lines[1], 2, "line 2 does not decode"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, brk := ParseAudit([]byte(c.data))
			if brk == nil || brk.Line != c.line || !strings.Contains(brk.Reason, c.want) {
				t.Fatalf("break %+v; want line %d naming %q", brk, c.line, c.want)
			}
		})
	}
}

func TestAuditRefusalsAreRateLimited(t *testing.T) {
	file, _ := appendAll(t, auditLine(AuditRefused, "b", "a"))
	with := func(f func(*AuditLine), reasons ...string) AuditLine {
		l := auditLine(AuditRefused, reasons...)
		f(&l)
		return l
	}
	for name, c := range map[string]struct {
		l     AuditLine
		write bool
	}{
		"the same reasons in another order":  {auditLine(AuditRefused, "a", "b", "a"), false},
		"another reason":                     {auditLine(AuditRefused, "a"), true},
		"another head":                       {with(func(l *AuditLine) { l.Head = base1204 }, "a", "b"), true},
		"the other actor":                    {with(func(l *AuditLine) { l.Actor = ByDrain }, "a", "b"), true},
		"another PR":                         {with(func(l *AuditLine) { l.PR = 1 }, "a", "b"), true},
		"another policy":                     {with(func(l *AuditLine) { l.PolicyHash = strings.Repeat("c", 64) }, "a", "b"), true},
		"a repository name in another case":  {with(func(l *AuditLine) { l.Repo = "CameronSjo/Forgectl" }, "a", "b"), false},
		"a merge is never limited":           {auditLine(AuditMerging), true},
		"another worker on the same PR":      {with(func(l *AuditLine) { l.Worker = "other" }, "a", "b"), false},
		"the same reasons, another repo id":  {with(func(l *AuditLine) { l.RepoID = 7 }, "a", "b"), true},
		"the same reasons, PR 0 on that row": {with(func(l *AuditLine) { l.PR = 0 }, "a", "b"), true},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, write, err := AppendAudit(file, c.l)
			if err != nil || write != c.write {
				t.Fatalf("write %v, %v; want %v", write, err, c.write)
			}
		})
	}
}

// TestAuditRefusalRepeatIsTheLatestLine pins that a refusal repeats only the
// most recent line about the same subject: any later line about it that is
// not the same refusal ends the repeat, and lines about other subjects do
// not (T10.4 security review).
func TestAuditRefusalRepeatIsTheLatestLine(t *testing.T) {
	other := auditLine(AuditRefused, "x")
	other.PR = 1
	noPR := func(worker string) AuditLine {
		l := auditLine(AuditRefused, "no PR")
		l.PR, l.Head, l.Worker = 0, "", worker
		return l
	}
	for name, c := range map[string]struct {
		before []AuditLine
		l      AuditLine
		write  bool
	}{
		"a merge attempt in between":    {[]AuditLine{auditLine(AuditRefused, "a"), auditLine(AuditMerging)}, auditLine(AuditRefused, "a"), true},
		"an outcome in between":         {[]AuditLine{auditLine(AuditRefused, "a"), auditLine(AuditFailed)}, auditLine(AuditRefused, "a"), true},
		"another refusal in between":    {[]AuditLine{auditLine(AuditRefused, "a"), auditLine(AuditRefused, "b")}, auditLine(AuditRefused, "a"), true},
		"another PR's line in between":  {[]AuditLine{auditLine(AuditRefused, "a"), other}, auditLine(AuditRefused, "a"), false},
		"no PR: the same worker":        {[]AuditLine{noPR("w1")}, noPR("w1"), false},
		"no PR: another worker":         {[]AuditLine{noPR("w1")}, noPR("w2"), true},
		"no PR: another worker between": {[]AuditLine{noPR("w1"), noPR("w2")}, noPR("w1"), false},
	} {
		t.Run(name, func(t *testing.T) {
			file, _ := appendAll(t, c.before...)
			_, _, write, err := AppendAudit(file, c.l)
			if err != nil || write != c.write {
				t.Fatalf("write %v, %v; want %v", write, err, c.write)
			}
		})
	}
}

func TestAuditRefusesAPartialFile(t *testing.T) {
	file, _ := appendAll(t, auditLine(AuditRefused, "a"))
	if _, _, _, err := AppendAudit(bytes.TrimSuffix(file, []byte("\n")), auditLine(AuditMerging)); !errors.Is(err, ErrAuditPartial) {
		t.Fatalf("a file cut mid-line: %v, want ErrAuditPartial", err)
	}
	line, _, write, err := AppendAudit(nil, auditLine(AuditMerging))
	if err != nil || !write || !strings.Contains(string(line), `"checks":[],"markers":[]`) || !strings.HasSuffix(string(line), "\n") {
		t.Fatalf("a first line: %q, %v, %v", line, write, err)
	}
}
