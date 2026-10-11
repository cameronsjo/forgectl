//go:build unix

package worker

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMergeAuditAppendAndRead(t *testing.T) {
	state := t.TempDir()
	a := MergeAudit{stateBase: state}
	if data, err := a.Read(); err != nil || data != nil {
		t.Fatalf("no file yet: %q, %v", data, err)
	}
	var seen [][]byte
	for _, line := range []string{"one\n", "two\n"} {
		if err := a.Append(func(existing []byte) ([]byte, error) {
			seen = append(seen, existing)
			return []byte(line), nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 || seen[0] != nil || string(seen[1]) != "one\n" {
		t.Fatalf("fn saw %q", seen)
	}
	data, err := a.Read()
	if err != nil || string(data) != "one\ntwo\n" {
		t.Fatalf("read %q, %v", data, err)
	}
	path := filepath.Join(state, "forgectl", "surface", mergeAuditName)
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", st, err)
	}
	// Nothing to write writes nothing; an error from fn is returned as is.
	if err := a.Append(func([]byte) ([]byte, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := a.Append(func([]byte) ([]byte, error) { return []byte("three\n"), boom }); !errors.Is(err, boom) {
		t.Fatalf("fn error: %v", err)
	}
	if err := a.Append(func([]byte) ([]byte, error) { return []byte("no newline"), nil }); err == nil {
		t.Fatal("a line with no newline was appended")
	}
	if data, _ := a.Read(); string(data) != "one\ntwo\n" {
		t.Fatalf("after refused appends: %q", data)
	}
}

func TestMergeAuditRefusesAFullFileAndASymlink(t *testing.T) {
	state := t.TempDir()
	a := MergeAudit{stateBase: state}
	if err := a.Append(func([]byte) ([]byte, error) { return []byte("x\n"), nil }); err != nil {
		t.Fatal(err)
	}
	big := append(bytes.Repeat([]byte("y"), MaxMergeAuditBytes-2), '\n')
	if err := a.Append(func([]byte) ([]byte, error) { return big, nil }); !errors.Is(err, ErrMergeAuditFull) {
		t.Fatalf("an append past the cap: %v, want ErrMergeAuditFull", err)
	}
	path := filepath.Join(state, "forgectl", "surface", mergeAuditName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(); !errors.Is(err, ErrLedgerUnreadable) {
		t.Fatalf("read through a symlink: %v", err)
	}
	if err := a.Append(func([]byte) ([]byte, error) { return []byte("z\n"), nil }); !errors.Is(err, ErrLedgerUnreadable) {
		t.Fatalf("append through a symlink: %v", err)
	}
}
