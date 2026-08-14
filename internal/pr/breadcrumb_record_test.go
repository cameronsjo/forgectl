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
//   [x] Two concatenated JSON documents remain ACCEPTED (forgectl#289)
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

// TestDecodeBreadcrumb_AcceptsTrailingDocument pins the grammar #212
// deliberately did NOT change. Tightening it is forgectl#289; until then this
// test exists so the acceptance is a recorded decision rather than an
// accident, and so tightening it cannot happen silently.
func TestDecodeBreadcrumb_AcceptsTrailingDocument(t *testing.T) {
	bc, err := decodeBreadcrumb([]byte(`{"workspace":"/tmp/forgectl-workflow-a","ref":"o/r#1","createdAt":"2026-01-01T00:00:00Z"}
{"workspace":"/tmp/forgectl-workflow-b","ref":"o/r#2","createdAt":"2026-01-01T00:00:00Z"}`))
	if err != nil {
		t.Fatalf("decodeBreadcrumb with a trailing document = %v; the accepted grammar must not change in #212", err)
	}
	if bc.Ref != "o/r#1" {
		t.Errorf("ref = %q, want the FIRST document's ref %q", bc.Ref, "o/r#1")
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
