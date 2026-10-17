package project

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

const maxArtifactBytes int64 = 32 << 20

type ArtifactDigest struct {
	RelativePath string `json:"relative_path"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}

func VerifyArtifact(ctx context.Context, worktree, path, expectedSHA256 string) (ArtifactDigest, error) {
	return verifyArtifact(ctx, worktree, path, expectedSHA256)
}

func artifactInputs(worktree, path, expectedSHA256 string) (string, error) {
	if worktree == "" || !filepath.IsAbs(worktree) {
		return "", fmt.Errorf("%w: worktree must be absolute", ErrInvalidInput)
	}
	if path == "" {
		return "", fmt.Errorf("%w: artifact path is empty", ErrInvalidInput)
	}
	if hasParentComponent(path) {
		return "", fmt.Errorf("%w: artifact path contains traversal", ErrInvalidInput)
	}
	if !validDigest(expectedSHA256) {
		return "", fmt.Errorf("%w: expected sha256 is invalid", ErrInvalidInput)
	}

	worktree = filepath.Clean(worktree)
	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(worktree, candidate)
	}
	candidate = filepath.Clean(candidate)
	relative, err := filepath.Rel(worktree, candidate)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: artifact path is outside worktree", ErrInvalidInput)
	}
	return relative, nil
}

func hasParentComponent(path string) bool {
	for _, component := range strings.Split(path, string(filepath.Separator)) {
		if component == ".." {
			return true
		}
	}
	return false
}
