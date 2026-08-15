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
