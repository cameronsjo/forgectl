package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var (
	fullActionSHA  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	versionComment = regexp.MustCompile(`^# v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
)

// TestReleaseWorkflowActionsAreImmutable is the regression guard for #208.
// The release job handles signing material and write-scoped tokens, so a
// movable tag on any third-party action is a credential-execution boundary,
// not merely dependency-update convenience.
func TestReleaseWorkflowActionsAreImmutable(t *testing.T) {
	lines := releaseWorkflowLines(t)
	pinned := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "uses:") {
			continue
		}

		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "uses:"))
		fields := strings.Fields(value)
		if len(fields) == 0 {
			t.Fatalf("release.yml:%d: empty action reference", i+1)
		}
		ref := fields[0]
		if strings.HasPrefix(ref, "./") {
			continue // Repository-local actions are reviewed with this repository.
		}

		at := strings.LastIndexByte(ref, '@')
		if at < 1 || !fullActionSHA.MatchString(ref[at+1:]) {
			t.Errorf("release.yml:%d: third-party action %q must use a full commit SHA", i+1, ref)
			continue
		}
		comment := strings.TrimSpace(value[len(ref):])
		if !versionComment.MatchString(comment) {
			t.Errorf("release.yml:%d: pinned action %q must retain an exact version comment such as # v1.2.3", i+1, ref)
		}
		pinned++
	}
	if pinned == 0 {
		t.Fatal("release.yml contains no pinned third-party actions; the guard matched nothing")
	}
}

// TestReleaseWorkflowCheckoutDoesNotPersistCredentials pins the other half of
// #208: no checkout in the credential-bearing job may leave its token in the
// repository's git configuration for later steps to inherit.
func TestReleaseWorkflowCheckoutDoesNotPersistCredentials(t *testing.T) {
	lines := releaseWorkflowLines(t)
	checkouts := 0
	for i, line := range lines {
		if !strings.Contains(strings.TrimSpace(line), "uses: actions/checkout@") {
			continue
		}
		checkouts++
		usesIndent := leadingWhitespace(line)
		found := false
		for j := i + 1; j < len(lines); j++ {
			trimmed := strings.TrimSpace(lines[j])
			if strings.HasPrefix(trimmed, "- ") && leadingWhitespace(lines[j]) < usesIndent {
				break
			}
			if trimmed == "persist-credentials: false" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("release.yml:%d: checkout must set persist-credentials: false in the same step", i+1)
		}
	}
	if checkouts == 0 {
		t.Fatal("release.yml contains no checkout step; the credential guard matched nothing")
	}
}

func releaseWorkflowLines(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	return strings.Split(string(data), "\n")
}

func leadingWhitespace(s string) int {
	return len(s) - len(strings.TrimLeft(s, " \t"))
}
