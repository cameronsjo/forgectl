package projects

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tests := []struct {
		name string
		env  string
		want string
	}{
		{"unset falls back to ~/Projects", "", filepath.Join(home, "Projects")},
		{"absolute value is used as is", "/srv/code", "/srv/code"},
		{"leading ~/ expands", "~/work", filepath.Join(home, "work")},
		{"bare tilde is not expanded", "~work", "~work"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PROJECTS_DIR", tt.env)
			got, err := ResolveRoot()
			if err != nil {
				t.Fatalf("ResolveRoot() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ResolveRoot() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveRoot_HomeLookupFailure(t *testing.T) {
	lookupErr := errors.New("no home")
	failing := func() (string, error) { return "", lookupErr }
	for _, env := range []string{"", "~/work"} {
		t.Run("needs home: "+env, func(t *testing.T) {
			t.Setenv("PROJECTS_DIR", env)
			got, err := resolveRoot(failing)
			if !errors.Is(err, lookupErr) {
				t.Fatalf("resolveRoot error = %v, want wrapping %v", err, lookupErr)
			}
			if got != "" {
				t.Errorf("resolveRoot = %q, want no root alongside the error", got)
			}
		})
	}
	t.Run("absolute value never consults home", func(t *testing.T) {
		t.Setenv("PROJECTS_DIR", "/srv/code")
		got, err := resolveRoot(failing)
		if err != nil || got != "/srv/code" {
			t.Errorf("resolveRoot = %q, %v; want /srv/code, nil", got, err)
		}
	})
}

func TestPlacement_RefusesEmptyRoot(t *testing.T) {
	if _, err := Placement("", Repo{Host: "github.com", Owner: "o", Name: "n"}, ""); err == nil {
		t.Fatal("Placement with an empty root must refuse, not place relative to the cwd")
	}
}

func TestNew_FailedRootLookupKeepsTheCause(t *testing.T) {
	t.Setenv("PROJECTS_DIR", "")
	lookupErr := errors.New("no home")
	c := newWithRoot(nil, func() (string, error) {
		return resolveRoot(func() (string, error) { return "", lookupErr })
	}, withGitLookPath(func(string) (string, error) { return "", errors.New("no git") }))
	if c.Dir != "" {
		t.Fatalf("Dir = %q, want empty after a failed lookup", c.Dir)
	}

	_, err := c.Discover(context.Background())
	if !errors.Is(err, lookupErr) || strings.Contains(err.Error(), "directory not found") {
		t.Errorf("Discover error = %v, want the lookup cause and not %q", err, "projects directory not found")
	}

	_, err = c.ResolveTarget("anything")
	if !errors.Is(err, lookupErr) || !errors.Is(err, ErrTargetNotFound) {
		t.Errorf("ResolveTarget error = %v, want ErrTargetNotFound wrapping the lookup cause", err)
	}
}

func TestDiscoverDir_ExplicitDirIgnoresRootError(t *testing.T) {
	c := &Client{rootErr: errors.New("no home")}
	if _, err := c.discoverDir(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil || strings.Contains(err.Error(), "no home") {
		t.Errorf("discoverDir of an explicit missing dir = %v, want the not-found error, not the root cause", err)
	}
}
