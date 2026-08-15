package pr

// Test plan for the RECORD half of breadcrumb.go (#212)
//
// validateBreadcrumbRecord (Classification: schema only, zero filesystem)
//   [x] A current record with a DELETED workspace still validates — the whole
//       premise of #212: record validity and workspace actionability are
//       independent
//   [x] A legacy record (empty Agent, absent Local) validates
//   [x] Required-field, canonical-ref, one-way locality, zero-time, and
//       relative/empty-workspace rejections are preserved exactly
//   [x] Validation performs NO workspace filesystem access (seams would fire)
// decodeBreadcrumb (Classification: one decoder, frozen grammar)
//   [x] Unknown fields are rejected
//   [x] Trailing content after the record is REJECTED (forgectl#289)
//   [x] Trailing WHITESPACE is still accepted — writeBreadcrumb emits it
//   [x] The refusal message echoes no file bytes, so hostile trailing content
//       cannot ride it to a terminal
// FuzzDecodeBreadcrumbRecord
//   [x] Every accepted record has an absolute nonempty workspace, a complete
//       canonical ref, a valid locality relation, and a nonzero timestamp —
//       and never any workspace-existence invariant

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validRecord() Breadcrumb {
	return Breadcrumb{
		Workspace: filepath.Join(string(filepath.Separator), "tmp", "forgectl-workflow-deleted"),
		Ref:       "o/r#1",
		Agent:     "claude",
		CreatedAt: time.Unix(1, 0).UTC(),
	}
}

// TestValidateBreadcrumbRecord_AcceptsDeletedWorkspace is the core of the
// split: the recorded directory does not exist and never will, and the record
// is still valid.
func TestValidateBreadcrumbRecord_AcceptsDeletedWorkspace(t *testing.T) {
	bc := validRecord()
	bc.Workspace = filepath.Join(t.TempDir(), "forgectl-workflow-never-existed")
	if err := validateBreadcrumbRecord(bc); err != nil {
		t.Fatalf("validateBreadcrumbRecord with an absent workspace = %v, want nil", err)
	}
}

// TestValidateBreadcrumbRecord_TouchesNoFilesystem proves the independence
// structurally rather than by inspection: every filesystem seam is replaced
// with one that fails the test if called.
func TestValidateBreadcrumbRecord_TouchesNoFilesystem(t *testing.T) {
	tripped := func(op string) func(string) (fs.FileInfo, error) {
		return func(p string) (fs.FileInfo, error) {
			t.Errorf("record validation performed a filesystem call: %s(%q)", op, p)
			return nil, nil
		}
	}
	swapFSSeams(t, tripped("lstat"), tripped("stat"), func(p string) (string, error) {
		t.Errorf("record validation performed a filesystem call: evalsymlinks(%q)", p)
		return p, nil
	})
	if err := validateBreadcrumbRecord(validRecord()); err != nil {
		t.Fatalf("validateBreadcrumbRecord = %v, want nil", err)
	}
}

func TestValidateBreadcrumbRecord_AcceptsLegacyRecord(t *testing.T) {
	// Pre-upgrade shape: no Agent, no Local key at all.
	bc := validRecord()
	bc.Agent = ""
	if err := validateBreadcrumbRecord(bc); err != nil {
		t.Fatalf("legacy record (empty Agent, absent Local) = %v, want nil", err)
	}
	// And the legitimate converse of the locality check: a real forge repo
	// whose owner is literally "local", with the flag unset.
	bc = validRecord()
	bc.Ref = localOwnerSentinel + "/tools#3"
	if err := validateBreadcrumbRecord(bc); err != nil {
		t.Fatalf("owner %q with Local unset = %v, want nil", localOwnerSentinel, err)
	}
}

func TestValidateBreadcrumbRecord_Rejections(t *testing.T) {
	cases := []struct {
		name  string
		mutar func(*Breadcrumb)
	}{
		{"empty workspace", func(b *Breadcrumb) { b.Workspace = "" }},
		{"relative workspace", func(b *Breadcrumb) { b.Workspace = "relative/forgectl-workflow-x" }},
		{"empty ref", func(b *Breadcrumb) { b.Ref = "" }},
		{"malformed ref", func(b *Breadcrumb) { b.Ref = "not a ref" }},
		{"incomplete ref", func(b *Breadcrumb) { b.Ref = "42" }},
		{"zero createdAt", func(b *Breadcrumb) { b.CreatedAt = time.Time{} }},
		{"forged locality", func(b *Breadcrumb) { b.Local = true; b.Ref = "realowner/r#1" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bc := validRecord()
			tc.mutar(&bc)
			if err := validateBreadcrumbRecord(bc); err == nil {
				t.Errorf("validateBreadcrumbRecord(%+v) = nil, want a rejection", bc)
			}
		})
	}
}

func TestDecodeBreadcrumb_RejectsUnknownFields(t *testing.T) {
	if _, err := decodeBreadcrumb([]byte(`{"workspace":"/tmp/x","surprise":1}`)); err == nil {
		t.Error("decodeBreadcrumb accepted an unknown field; the strict grammar is load-bearing")
	}
}

// firstDoc is the one well-formed record every trailing-content case appends
// to, so the cases differ only in what follows it.
const firstDoc = `{"workspace":"/tmp/forgectl-workflow-a","ref":"o/r#1","createdAt":"2026-01-01T00:00:00Z"}`

// TestDecodeBreadcrumb_RejectsTrailingContent is forgectl#289, and it replaces
// the test #212 wrote to pin the OPPOSITE behaviour.
//
// A breadcrumb file is exactly one record. While bytes after the first document
// were ignored, the file could say two different things and which one a reader
// got depended on where it happened to stop — the same leniency
// DisallowUnknownFields already refuses INSIDE the record.
func TestDecodeBreadcrumb_RejectsTrailingContent(t *testing.T) {
	secondDoc := `{"workspace":"/tmp/forgectl-workflow-b","ref":"o/r#2","createdAt":"2026-01-01T00:00:00Z"}`
	cases := []struct {
		name string
		body string
	}{
		{"second document", firstDoc + secondDoc},
		{"second document after newline", firstDoc + "\n" + secondDoc},
		{"second document after whitespace run", firstDoc + " \t\r\n  " + secondDoc},
		{"truncated second document", firstDoc + "\n" + `{"workspace":"/tmp/forg`},
		{"trailing NUL", firstDoc + "\x00"},
		{"second document after NUL", firstDoc + "\x00" + secondDoc},
		{"trailing array", firstDoc + "\n[]"},
		{"trailing bare token", firstDoc + "\nnull"},
		{"trailing garbage", firstDoc + "\nnot json at all"},
		{"trailing closing brace", firstDoc + "}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeBreadcrumb([]byte(tc.body)); err == nil {
				t.Errorf("decodeBreadcrumb accepted %s; a breadcrumb file is exactly one record", tc.name)
			}
		})
	}
}

// TestDecodeBreadcrumb_TrailingContentErrorCarriesNoFileBytes guards the
// property that makes the refusal safe to log: the message is a CONSTANT, so
// nothing the file says rides it to an operator's terminal.
//
// The tail that matters is a quoted STRING. json.Decoder.Token returns a
// string token as its verbatim Go value, so a message that reported the token
// — or wrapped the syntax error, which quotes the first offending byte — would
// echo whatever the file chose, raw ANSI and bidi overrides included. Both
// spellings are covered below, and either one turns this red.
func TestDecodeBreadcrumb_TrailingContentErrorCarriesNoFileBytes(t *testing.T) {
	const marker = "MARKERBYTES1234"
	// Two shapes. A quoted STRING whose escapes keep the tail valid json, so
	// it really does decode to a token carrying a raw ESC and a raw bidi
	// override — that token is what Token hands back verbatim. And a bare
	// word, whose first byte the syntax error quotes instead.
	for _, tail := range []string{
		"\n\"\\u001b[31m" + marker + "\u202e\"",
		"\n" + marker,
		"\n\x1b[31m" + marker,
	} {
		_, err := decodeBreadcrumb([]byte(firstDoc + tail))
		if err == nil {
			t.Fatalf("trailing %q must be rejected", tail)
		}
		got := err.Error()
		if strings.Contains(got, marker) {
			t.Errorf("refusal message echoed file bytes: %q", got)
		}
		for _, control := range []string{"\x1b", "\u202e", `\x1b`, `\u202e`} {
			if strings.Contains(got, control) {
				t.Errorf("refusal message carries %q from the file: %q", control, got)
			}
		}
	}
}

// TestDecodeBreadcrumbRecord_TrailingContentNamesPathAndRemedy pins the
// operator-facing half of the refusal. A record rejected for trailing content
// is unreachable by every verb — `pr list` skips it, `pr teardown` refuses it
// at this same decode — so the only remedy is removing the file, and the
// message has to say both WHICH file and WHAT to do.
//
// The two halves come from different places on purpose: the path from
// decodeBreadcrumbRecord's wrapper, the remedy from the constant. Asserting
// them together is what proves the pairing survives.
func TestDecodeBreadcrumbRecord_TrailingContentNamesPathAndRemedy(t *testing.T) {
	path := filepath.Join(string(filepath.Separator), "tmp", "sessions", "o-r-1-2.json")
	_, err := decodeBreadcrumbRecord([]byte(firstDoc+"\n{}"), path)
	if err == nil {
		t.Fatal("trailing content must be rejected")
	}
	got := err.Error()
	if !strings.Contains(got, path) {
		t.Errorf("refusal does not name the file to remove: %q", got)
	}
	if !strings.Contains(got, "by hand") {
		t.Errorf("refusal names no remedy, and no forgectl verb can reach this record: %q", got)
	}
}

// TestDecodeBreadcrumbRecord_EscapesAControlBearingPath closes the other half
// of the same hazard. The refusal's own message is a constant, but the WRAPPER
// interpolates the breadcrumb's filename — and that name is chosen by the same
// actor who planted the trailing bytes.
//
// It matters more here than at a typical error site because the message now
// tells the operator to delete the file: a name that can repaint the line or
// reverse its direction is the classic setup for deleting the wrong one. This
// error reaches the terminal through fang's handler, which renders
// err.Error() verbatim, so nothing downstream would catch it.
func TestDecodeBreadcrumbRecord_EscapesAControlBearingPath(t *testing.T) {
	hostile := filepath.Join(string(filepath.Separator), "tmp", "sessions",
		"o-r-1-\x1b[2K\rinnocent\u202egnj.json")

	// Both refusal routes through this wrapper, since either can name the file.
	cases := map[string][]byte{
		"trailing content": []byte(firstDoc + "\n{}"),
		"invalid record":   []byte(`{"workspace":"relative","ref":"o/r#1","createdAt":"2026-01-01T00:00:00Z"}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := decodeBreadcrumbRecord(body, hostile)
			if err == nil {
				t.Fatal("must be rejected")
			}
			got := err.Error()
			for _, control := range []string{"\x1b", "\r", "\u202e"} {
				if strings.Contains(got, control) {
					t.Errorf("error carries raw %q from the breadcrumb filename: %q", control, got)
				}
			}
			// The name must still be identifiable — escaped, not dropped.
			if !strings.Contains(got, "innocent") {
				t.Errorf("escaping lost the filename entirely, leaving nothing to act on: %q", got)
			}
		})
	}
}

// TestDecodeBreadcrumb_AcceptsTrailingWhitespace is the half of the tightening
// that keeps it a hardening rather than a migration: writeBreadcrumb appends a
// newline to every file it writes, so rejecting trailing whitespace would
// reject forgectl's own output.
func TestDecodeBreadcrumb_AcceptsTrailingWhitespace(t *testing.T) {
	for _, tail := range []string{"", "\n", "  \t\r\n\n", "\r\n"} {
		bc, err := decodeBreadcrumb([]byte(firstDoc + tail))
		if err != nil {
			t.Fatalf("decodeBreadcrumb with trailing %q = %v, want acceptance", tail, err)
		}
		if bc.Ref != "o/r#1" {
			t.Errorf("ref = %q, want %q", bc.Ref, "o/r#1")
		}
	}
}

// FuzzDecodeBreadcrumbRecord asserts the record-phase invariants hold for
// every accepted record — and, critically, that none of them is a
// workspace-existence claim.
func FuzzDecodeBreadcrumbRecord(f *testing.F) {
	f.Add(`{"workspace":"/tmp/forgectl-workflow-a","ref":"o/r#1","agent":"claude","createdAt":"2026-01-01T00:00:00Z"}`)
	f.Add(`{"workspace":"/tmp/forgectl-workflow-a","ref":"local/r#1","local":true,"createdAt":"2026-01-01T00:00:00Z"}`)
	f.Add(`{"workspace":"relative","ref":"o/r#1","createdAt":"2026-01-01T00:00:00Z"}`)
	f.Add(`{}`)
	f.Add(`not json`)

	f.Fuzz(func(t *testing.T, body string) {
		bc, err := decodeBreadcrumb([]byte(body))
		if err != nil {
			return
		}
		if err := validateBreadcrumbRecord(bc); err != nil {
			return
		}
		if bc.Workspace == "" || !filepath.IsAbs(bc.Workspace) {
			t.Fatalf("accepted record has a non-absolute workspace %q", bc.Workspace)
		}
		ref, err := ParseRef(bc.Ref)
		if err != nil || !ref.Complete() {
			t.Fatalf("accepted record has an unusable ref %q (err %v)", bc.Ref, err)
		}
		if bc.Local && ref.Owner != localOwnerSentinel {
			t.Fatalf("accepted record claims locality with owner %q", ref.Owner)
		}
		if bc.CreatedAt.IsZero() {
			t.Fatal("accepted record has a zero timestamp")
		}
		if strings.TrimSpace(bc.Workspace) == "" {
			t.Fatalf("accepted record has a blank workspace %q", bc.Workspace)
		}
	})
}
