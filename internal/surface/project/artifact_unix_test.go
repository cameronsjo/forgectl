//go:build unix

package project

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyArtifact(test *testing.T) {
	worktree := test.TempDir()
	path := filepath.Join(worktree, "result.txt")
	contents := []byte("verified result\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		test.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	wantDigest := hex.EncodeToString(digest[:])
	got, err := VerifyArtifact(context.Background(), worktree, "result.txt", wantDigest)
	if err != nil {
		test.Fatal(err)
	}
	if got.RelativePath != "result.txt" || got.Size != int64(len(contents)) || got.SHA256 != wantDigest {
		test.Fatalf("digest = %#v", got)
	}
	sourceRoot, err := os.OpenRoot(worktree)
	if err != nil {
		test.Fatal(err)
	}
	defer func() { _ = sourceRoot.Close() }()
	read, err := sourceRoot.ReadFile("result.txt")
	if err != nil {
		test.Fatal(err)
	}
	if string(read) != string(contents) {
		test.Fatalf("source changed: %q", read)
	}
	info, err := sourceRoot.Stat("result.txt")
	if err != nil {
		test.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		test.Fatalf("source mode = %o", info.Mode().Perm())
	}
}

func TestVerifyArtifactRejectsInvalidInputs(test *testing.T) {
	worktree := test.TempDir()
	path := filepath.Join(worktree, "result.txt")
	contents := []byte("result")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		test.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	wantDigest := hex.EncodeToString(digest[:])
	outside := filepath.Join(test.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, contents, 0o600); err != nil {
		test.Fatal(err)
	}
	tests := map[string]struct {
		path   string
		digest string
		want   error
		reason string
	}{
		"missing":           {path: "missing.txt", digest: wantDigest, want: ErrConflict, reason: "open artifact"},
		"outside absolute":  {path: outside, digest: wantDigest, want: ErrInvalidInput, reason: "outside worktree"},
		"outside traversal": {path: filepath.Join("..", filepath.Base(outside)), digest: wantDigest, want: ErrInvalidInput, reason: "contains traversal"},
		"invalid digest":    {path: "result.txt", digest: "bad", want: ErrInvalidInput, reason: "expected sha256 is invalid"},
	}
	for name, row := range tests {
		test.Run(name, func(test *testing.T) {
			_, err := VerifyArtifact(context.Background(), worktree, row.path, row.digest)
			if !errors.Is(err, row.want) {
				test.Fatalf("error = %v, want %v", err, row.want)
			}
			if !strings.Contains(err.Error(), row.reason) {
				test.Fatalf("error = %v, want reason %q", err, row.reason)
			}
		})
	}
}

func TestVerifyArtifactRejectsSymlinkAndHardlink(test *testing.T) {
	worktree := test.TempDir()
	contents := []byte("result")
	target := filepath.Join(test.TempDir(), "target.txt")
	if err := os.WriteFile(target, contents, 0o600); err != nil {
		test.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	wantDigest := hex.EncodeToString(digest[:])
	if err := os.Symlink(target, filepath.Join(worktree, "link.txt")); err != nil {
		test.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(target), filepath.Join(worktree, "link-dir")); err != nil {
		test.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(worktree, "hard.txt")); err != nil {
		test.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(worktree, "directory"), 0o700); err != nil {
		test.Fatal(err)
	}
	for _, path := range []string{"link.txt", "link-dir/target.txt", "hard.txt", "directory"} {
		test.Run(path, func(test *testing.T) {
			if _, err := VerifyArtifact(context.Background(), worktree, path, wantDigest); !errors.Is(err, ErrConflict) {
				test.Fatalf("error = %v, want ErrConflict", err)
			}
		})
	}
}

func TestVerifyArtifactRejectsOversizedAndWrongDigest(test *testing.T) {
	worktree := test.TempDir()
	large := filepath.Join(worktree, "large.bin")
	if err := os.WriteFile(large, make([]byte, maxArtifactBytes+1), 0o600); err != nil {
		test.Fatal(err)
	}
	if _, err := VerifyArtifact(context.Background(), worktree, "large.bin", "0000000000000000000000000000000000000000000000000000000000000000"); !errors.Is(err, ErrConflict) {
		test.Fatalf("oversized error = %v, want ErrConflict", err)
	}
	small := filepath.Join(worktree, "small.txt")
	if err := os.WriteFile(small, []byte("small"), 0o600); err != nil {
		test.Fatal(err)
	}
	if _, err := VerifyArtifact(context.Background(), worktree, "small.txt", "0000000000000000000000000000000000000000000000000000000000000000"); !errors.Is(err, ErrConflict) {
		test.Fatalf("wrong digest error = %v, want ErrConflict", err)
	}
}

func TestVerifyArtifactRejectsCanceledAndChangedSource(test *testing.T) {
	worktree := test.TempDir()
	path := filepath.Join(worktree, "result.txt")
	contents := []byte("result")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		test.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	wantDigest := hex.EncodeToString(digest[:])
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := VerifyArtifact(ctx, worktree, "result.txt", wantDigest); !errors.Is(err, context.Canceled) {
		test.Fatalf("canceled error = %v, want context.Canceled", err)
	}
	oldHook := artifactAfterOpenHook
	defer func() { artifactAfterOpenHook = oldHook }()
	artifactAfterOpenHook = func(path string) {
		if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
			test.Fatalf("mutate source: %v", err)
		}
	}
	if _, err := VerifyArtifact(context.Background(), worktree, "result.txt", wantDigest); !errors.Is(err, ErrConflict) {
		test.Fatalf("changed error = %v, want ErrConflict", err)
	}
}

func TestVerifyArtifactRejectsRenamedAndReplacedDirectory(test *testing.T) {
	worktree := test.TempDir()
	directory := filepath.Join(worktree, "nested")
	if err := os.Mkdir(directory, 0o700); err != nil {
		test.Fatal(err)
	}
	path := filepath.Join(directory, "result.txt")
	contents := []byte("result")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		test.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	wantDigest := hex.EncodeToString(digest[:])
	oldHook := artifactAfterOpenHook
	defer func() { artifactAfterOpenHook = oldHook }()
	artifactAfterOpenHook = func(openedPath string) {
		oldDirectory := directory + ".old"
		if err := os.Rename(directory, oldDirectory); err != nil {
			test.Fatalf("rename directory: %v", err)
		}
		if err := os.Mkdir(directory, 0o700); err != nil {
			test.Fatalf("replace directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(directory, "result.txt"), contents, 0o600); err != nil {
			test.Fatalf("replace file: %v", err)
		}
	}
	if _, err := VerifyArtifact(context.Background(), worktree, "nested/result.txt", wantDigest); !errors.Is(err, ErrConflict) {
		test.Fatalf("replaced directory error = %v, want ErrConflict", err)
	}
}

func TestVerifyArtifactRejectsRenamedAndReplacedWorktree(test *testing.T) {
	parent := test.TempDir()
	worktree := filepath.Join(parent, "worktree")
	if err := os.Mkdir(worktree, 0o700); err != nil {
		test.Fatal(err)
	}
	path := filepath.Join(worktree, "result.txt")
	contents := []byte("result")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		test.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	wantDigest := hex.EncodeToString(digest[:])
	oldHook := artifactAfterOpenHook
	defer func() { artifactAfterOpenHook = oldHook }()
	artifactAfterOpenHook = func(openedPath string) {
		oldWorktree := worktree + ".old"
		if err := os.Rename(worktree, oldWorktree); err != nil {
			test.Fatalf("rename worktree: %v", err)
		}
		if err := os.Mkdir(worktree, 0o700); err != nil {
			test.Fatalf("replace worktree: %v", err)
		}
		if err := os.WriteFile(filepath.Join(worktree, "result.txt"), contents, 0o600); err != nil {
			test.Fatalf("replace result: %v", err)
		}
	}
	if _, err := VerifyArtifact(context.Background(), worktree, "result.txt", wantDigest); !errors.Is(err, ErrConflict) {
		test.Fatalf("replaced worktree error = %v, want ErrConflict", err)
	}
}
