//go:build unix

package sops

// newWorkDir's nonce failure (#768): the one error path between the work
// directory existing and newWorkDir returning it.
//
//   [x] A failed nonce read returns the fixed error and removes the work
//       directory it had just made, .gitignore included

import (
	"errors"
	"os"
	"testing"

	"github.com/cameronsjo/forgectl/internal/env"
)

func TestNewWorkDirRemovesTheDirectoryWhenTheNonceFails(t *testing.T) {
	dir, _ := gitRepo(t)
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	target, err := env.ResolveTarget("secrets.sops.yaml", dir)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	defer target.Close()

	prev := readNonce
	readNonce = func([]byte) (int, error) { return 0, errors.New("entropy unavailable") }
	t.Cleanup(func() { readNonce = prev })

	w, err := newWorkDir(target)
	if err == nil || w != nil {
		t.Fatalf("newWorkDir = %v, %v; want the nonce failure", w, err)
	}
	if err.Error() != "could not generate a nonce" {
		t.Errorf("err = %q, want the fixed nonce error", err)
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		names := make([]string, 0, len(after))
		for _, e := range after {
			names = append(names, e.Name())
		}
		t.Errorf("newWorkDir left an entry behind after the nonce failed: %v", names)
	}
}
