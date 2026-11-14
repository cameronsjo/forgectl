// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package rexbin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// APIRevision is the Rex API revision forgectl is built against (Rex Beta
// 0.1.0-preview.19 reports 14). A binary reporting any other revision is
// refused.
const APIRevision = 14

// Source names the layer that supplied a resolved binary.
type Source string

const (
	// SourceConfig is the `[surface.rex] binary_path` key.
	SourceConfig Source = "config"
	// SourcePATH is a `rex` found on PATH.
	SourcePATH Source = "PATH"
	// SourceBundle is a binary inside an installed Rex app bundle.
	SourceBundle Source = "app bundle"
)

// Resolved is a vetted Rex binary.
type Resolved struct {
	// Path is absolute.
	Path string
	// Source is the layer that chose Path.
	Source Source
	// Version is the `rex_version` the binary reported.
	Version string
	// APIRevision is the `api_revision` the binary reported.
	APIRevision int
}

// bundleRel is where the rex binary sits inside a Rex app bundle.
const bundleRel = "Contents/Helpers/*.rex/Contents/MacOS/rex"

// DefaultBundlePatterns returns the glob patterns for the rex binary inside an
// installed Rex app bundle, system-wide then per-user. home may be empty, which
// drops the per-user pattern. The app name is a wildcard (`Rex.app`,
// `Rex Beta.app`) and so is the helper bundle's name.
func DefaultBundlePatterns(home string) []string {
	patterns := []string{"/Applications/Rex*.app/" + bundleRel}
	if home != "" {
		patterns = append(patterns, filepath.Join(home, "Applications", "Rex*.app", bundleRel))
	}
	return patterns
}

// FS is the slice of the filesystem the resolver reads; tests fake it.
type FS interface {
	Stat(name string) (fs.FileInfo, error)
	Glob(pattern string) ([]string, error)
}

type osFS struct{}

func (osFS) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }
func (osFS) Glob(pattern string) ([]string, error) { return filepath.Glob(pattern) }

// Resolver finds and vets the rex binary. Build one with New.
type Resolver struct {
	runner   fexec.Runner
	fs       FS
	lookPath func(string) (string, error)
	patterns []string
	// homeErr is why the per-user Applications root is not searched, or nil.
	homeErr error
}

// Option configures a Resolver.
type Option func(*Resolver)

// WithFS replaces the filesystem the resolver reads.
func WithFS(f FS) Option { return func(r *Resolver) { r.fs = f } }

// WithLookPath replaces the PATH lookup (default os/exec.LookPath).
func WithLookPath(fn func(string) (string, error)) Option {
	return func(r *Resolver) { r.lookPath = fn }
}

// WithBundlePatterns replaces the app-bundle glob patterns.
func WithBundlePatterns(patterns ...string) Option {
	return func(r *Resolver) { r.patterns, r.homeErr = patterns, nil }
}

// New returns a Resolver that vets candidates through runner. By default it
// reads the real filesystem and PATH and searches [DefaultBundlePatterns].
func New(runner fexec.Runner, opts ...Option) *Resolver {
	home, homeErr := os.UserHomeDir()
	r := &Resolver{
		homeErr:  homeErr,
		runner:   runner,
		fs:       osFS{},
		lookPath: exec.LookPath,
		patterns: DefaultBundlePatterns(home),
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// ErrNotFound is wrapped by the error Resolve returns when no candidate holds.
var ErrNotFound = errors.New("rex binary not found")

// ErrIncompatible is wrapped by the error Resolve returns when the binary is
// found but is not a usable Rex: `whoami --json` failed, did not parse, or
// reported another API revision.
var ErrIncompatible = errors.New("rex binary is not compatible")

// Resolve returns the first usable rex binary, in the order config key
// (configPath, empty to skip), PATH, app bundle, and vets it with `whoami
// --json`. See the package doc for the refusal rules.
func (r *Resolver) Resolve(ctx context.Context, configPath string) (Resolved, error) {
	var tried []string

	if configPath != "" {
		abs, err := filepath.Abs(configPath)
		if err != nil {
			return Resolved{}, fmt.Errorf("rex binary from [surface.rex] binary_path %s is unusable: %w", termsafe.QuoteArgMax(configPath, 0), err)
		}
		if err := r.usable(abs); err != nil {
			return Resolved{}, fmt.Errorf("rex binary from [surface.rex] binary_path is unusable: %w", err)
		}
		return r.vet(ctx, abs, SourceConfig)
	}
	tried = append(tried, "[surface.rex] binary_path (not set)")

	pathBin, pathErr := r.fromPATH()
	if pathErr == nil {
		return r.vet(ctx, pathBin, SourcePATH)
	}
	tried = append(tried, "PATH: "+pathErr.Error())

	for _, pat := range r.patterns {
		matches, err := r.fs.Glob(pat)
		if err != nil {
			tried = append(tried, fmt.Sprintf("%s: %v", pat, err))
			continue
		}
		if len(matches) == 0 {
			tried = append(tried, pat+": no match")
			continue
		}
		for _, m := range matches {
			if uerr := r.usable(m); uerr != nil {
				tried = append(tried, fmt.Sprintf("%s: %v", pat, uerr))
				continue
			}
			return r.vet(ctx, m, SourceBundle)
		}
	}

	if r.homeErr != nil {
		tried = append(tried, "~/Applications: no home directory: "+r.homeErr.Error())
	}
	return Resolved{}, fmt.Errorf("%w; tried:\n  - %s", ErrNotFound, strings.Join(tried, "\n  - "))
}

// fromPATH returns the absolute path of an executable `rex` on PATH, or why
// there is none.
func (r *Resolver) fromPATH() (string, error) {
	p, err := r.lookPath("rex")
	if err != nil {
		return "", fmt.Errorf("rex not found: %w", err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("%s: %w", termsafe.QuoteArgMax(p, 0), err)
	}
	if err := r.usable(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// usable accepts an executable regular file.
func (r *Resolver) usable(path string) error {
	q := termsafe.QuoteArgMax(path, 0)
	info, err := r.fs.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", q, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", q)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", q)
	}
	return nil
}

// whoami is the part of `rex whoami --json` the resolver reads; other fields
// are ignored.
type whoami struct {
	RexVersion  *string `json:"rex_version"`
	APIRevision *int    `json:"api_revision"`
}

var versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

func (r *Resolver) vet(ctx context.Context, path string, src Source) (Resolved, error) {
	q := termsafe.QuoteArgMax(path, 0)
	out, err := r.runner.Run(ctx, path, "whoami", "--json")
	if err != nil {
		return Resolved{}, fmt.Errorf("%w: %s whoami --json failed: %w", ErrIncompatible, q, err)
	}
	var w whoami
	if err := json.Unmarshal([]byte(out), &w); err != nil {
		return Resolved{}, fmt.Errorf("%w: %s whoami --json did not return JSON: %v", ErrIncompatible, q, err)
	}
	if w.RexVersion == nil || !versionRe.MatchString(*w.RexVersion) {
		return Resolved{}, fmt.Errorf("%w: %s whoami --json has no parseable rex_version", ErrIncompatible, q)
	}
	if w.APIRevision == nil {
		return Resolved{}, fmt.Errorf("%w: %s whoami --json has no api_revision (rex_version %q)", ErrIncompatible, q, *w.RexVersion)
	}
	if *w.APIRevision != APIRevision {
		return Resolved{}, fmt.Errorf("%w: %s is rex %q with api_revision %d; forgectl needs api_revision %d",
			ErrIncompatible, q, *w.RexVersion, *w.APIRevision, APIRevision)
	}
	return Resolved{Path: path, Source: src, Version: *w.RexVersion, APIRevision: *w.APIRevision}, nil
}
