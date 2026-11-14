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
	"runtime"
	"strings"
	"time"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// APIRevision is the Rex API revision forgectl is built against (Rex Beta
// 0.1.0-preview.19 reports 14). A binary reporting any other revision is
// refused.
const APIRevision = 14

// TeamID is the Apple Developer Team ID that signs the Rex app (Developer ID
// Application: Superlogical, Inc.). A bundle-discovered candidate must satisfy
// the code-signing requirement built from it. Measured with `codesign
// --verify` on Rex Beta 0.1.0-preview.19.
const TeamID = "AG92C3PT77"

// whoamiTimeout bounds one `whoami --json` run, so a hung candidate cannot
// hold resolution for as long as the caller's context allows.
const whoamiTimeout = 5 * time.Second

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

// Resolved is the rex binary Resolve chose and ran `whoami --json` against.
//
// It records a compatibility result, not an authenticity result: any
// executable can print the two JSON fields that were checked. The bytes at
// Path are not pinned, so a caller that execs Path later must re-verify at use
// time (the file can change between check and use).
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

// exactBundles are the app names tried before any wildcard match, in order.
var exactBundles = []string{"Rex.app", "Rex Beta.app"}

// escapeGlob escapes the pattern metacharacters in a literal path prefix.
func escapeGlob(s string) string {
	if filepath.Separator == '\\' { // no escape syntax on Windows
		return s
	}
	return strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`).Replace(s)
}

// DefaultBundlePatterns returns the glob patterns for the rex binary inside an
// installed Rex app bundle, in the order they are tried: for /Applications and
// then the per-user Applications directory, the exact names (Rex.app, then Rex
// Beta.app) and then any other Rex*.app. home must be absolute, otherwise the
// per-user root is dropped; glob metacharacters in it are escaped.
func DefaultBundlePatterns(home string) []string {
	roots := []string{"/Applications"}
	if home != "" && filepath.IsAbs(home) {
		roots = append(roots, filepath.Join(escapeGlob(home), "Applications"))
	}
	var out []string
	for _, root := range roots {
		for _, app := range exactBundles {
			out = append(out, root+"/"+app+"/"+bundleRel)
		}
		out = append(out, root+"/Rex*.app/"+bundleRel)
	}
	return out
}

// Info is the part of a file's metadata the resolver reads.
type Info struct {
	Mode  fs.FileMode
	IsDir bool
	// UID owns the file; meaningful only when UIDKnown.
	UID      int
	UIDKnown bool
}

// FS is the slice of the filesystem the resolver reads; tests fake it. Stat
// follows symlinks.
type FS interface {
	Stat(name string) (Info, error)
	Glob(pattern string) ([]string, error)
}

type osFS struct{}

func (osFS) Stat(name string) (Info, error) {
	fi, err := os.Stat(name)
	if err != nil {
		return Info{}, err
	}
	uid, known := ownerOf(fi)
	return Info{Mode: fi.Mode(), IsDir: fi.IsDir(), UID: uid, UIDKnown: known}, nil
}

func (osFS) Glob(pattern string) ([]string, error) { return filepath.Glob(pattern) }

// Resolver finds a usable rex binary. Build one with New.
type Resolver struct {
	runner   fexec.Runner
	fs       FS
	lookPath func(string) (string, error)
	patterns []string
	uid      int
	goos     string
	home     string
	// homeErr is why there is no usable home directory, or nil.
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

// WithBundlePatterns replaces the app-bundle glob patterns, tried in order.
// Each match must lie inside an `.app` directory.
func WithBundlePatterns(patterns ...string) Option {
	return func(r *Resolver) { r.patterns = patterns }
}

// WithUID replaces the uid the ownership gate accepts besides root.
func WithUID(uid int) Option { return func(r *Resolver) { r.uid = uid } }

// WithGOOS replaces the OS name that decides whether the code-signature check
// runs (it runs on "darwin").
func WithGOOS(goos string) Option { return func(r *Resolver) { r.goos = goos } }

// WithHome replaces the home directory used to expand a `~/` config value and
// to build the per-user bundle pattern. A non-nil err, or a non-absolute home,
// means there is none.
func WithHome(home string, err error) Option {
	return func(r *Resolver) {
		r.setHome(home, err)
		r.patterns = DefaultBundlePatterns(r.home)
	}
}

func (r *Resolver) setHome(home string, err error) {
	switch {
	case err != nil:
		r.home, r.homeErr = "", err
	case !filepath.IsAbs(home):
		r.home, r.homeErr = "", fmt.Errorf("$HOME %s is not an absolute path", termsafe.QuoteArgMax(home, 0))
	default:
		r.home, r.homeErr = home, nil
	}
}

// New returns a Resolver that runs `whoami --json` and `codesign` through
// runner. By default it reads the real filesystem and PATH and searches
// [DefaultBundlePatterns].
func New(runner fexec.Runner, opts ...Option) *Resolver {
	r := &Resolver{
		runner:   runner,
		fs:       osFS{},
		lookPath: exec.LookPath,
		uid:      os.Getuid(),
		goos:     runtime.GOOS,
	}
	home, err := os.UserHomeDir()
	r.setHome(home, err)
	r.patterns = DefaultBundlePatterns(r.home)
	for _, o := range opts {
		o(r)
	}
	return r
}

// ErrNotFound is wrapped by the error Resolve returns when no candidate holds.
var ErrNotFound = errors.New("rex binary not found")

// ErrIncompatible is wrapped by the error Resolve returns when a binary was
// found but is not a compatible Rex: `whoami --json` failed or timed out, did
// not parse, or reported another API revision. A cancelled or expired caller
// context is returned as that context error, not as ErrIncompatible. An
// unusable `binary_path` is returned as a plain error (neither class).
var ErrIncompatible = errors.New("rex binary is not compatible")

// Resolve returns the first usable rex binary, in the order config key
// (configPath, empty to skip), PATH, app bundle, and checks it with `whoami
// --json`. See the package doc for the refusal rules and the trust model.
func (r *Resolver) Resolve(ctx context.Context, configPath string) (Resolved, error) {
	if configPath != "" {
		abs, err := r.configBinary(configPath)
		if err != nil {
			return Resolved{}, err
		}
		return r.vet(ctx, abs, SourceConfig)
	}
	tried := []string{"[surface.rex] binary_path (not set)"}

	pathBin, pathErr := r.fromPATH()
	if pathErr == nil {
		return r.vet(ctx, pathBin, SourcePATH)
	}
	tried = append(tried, "PATH: "+pathErr.Error())

	if r.homeErr != nil {
		tried = append(tried, "~/Applications: no usable home directory: "+r.homeErr.Error())
	}

	incompatible := false
	seen := map[string]bool{}
	for _, pat := range r.patterns {
		matches, err := r.fs.Glob(pat)
		if err != nil {
			tried = append(tried, fmt.Sprintf("%s: %v", termsafe.QuoteArgMax(pat, 0), err))
			continue
		}
		if len(matches) == 0 {
			tried = append(tried, termsafe.QuoteArgMax(pat, 0)+": no match")
			continue
		}
		for _, m := range matches {
			if seen[m] {
				continue
			}
			seen[m] = true
			if err := r.bundleCandidate(ctx, m); err != nil {
				tried = append(tried, err.Error())
				continue
			}
			got, err := r.vet(ctx, m, SourceBundle)
			if err == nil {
				return got, nil
			}
			if ctx.Err() != nil {
				return Resolved{}, err
			}
			incompatible = true
			tried = append(tried, err.Error())
		}
	}

	class := ErrNotFound
	if incompatible {
		class = ErrIncompatible
	}
	return Resolved{}, fmt.Errorf("%w; tried:\n  - %s", class, strings.Join(tried, "\n  - "))
}

// configBinary turns the `binary_path` value into the absolute path to check.
// A leading `~/` expands against the home directory; any other relative value
// is refused, because resolving it against the working directory would pick a
// different file for each caller.
func (r *Resolver) configBinary(value string) (string, error) {
	const key = "[surface.rex] binary_path"
	q := termsafe.QuoteArgMax(value, 0)
	path := value
	if value == "~" || strings.HasPrefix(value, "~/") {
		if r.homeErr != nil {
			return "", fmt.Errorf("cannot expand %s from %s: no usable home directory (%w); use an absolute path instead", q, key, r.homeErr)
		}
		path = filepath.Join(r.home, strings.TrimPrefix(strings.TrimPrefix(value, "~"), "/"))
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s must be an absolute path or start with ~/, got %s", key, q)
	}
	path = filepath.Clean(path)
	if err := r.usable(path); err != nil {
		return "", fmt.Errorf("rex binary from %s is unusable: %w", key, err)
	}
	return path, nil
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

// usable accepts an executable regular file. The exec-bit test does not apply
// on Windows, where Stat reports every file as 0666; Rex is macOS-only.
func (r *Resolver) usable(path string) error {
	q := termsafe.QuoteArgMax(path, 0)
	info, err := r.fs.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", q, err)
	}
	if info.IsDir {
		return fmt.Errorf("%s is a directory", q)
	}
	if info.Mode.Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", q)
	}
	return nil
}

// bundleCandidate gates a bundle-discovered binary before anything runs it:
// an executable file, an ownership and mode check on the file and every
// directory up to the .app bundle, and on darwin the code-signature check. The
// returned error is the reason it was skipped, naming the path.
func (r *Resolver) bundleCandidate(ctx context.Context, path string) error {
	if err := r.usable(path); err != nil {
		return err
	}
	if err := r.trustedChain(path); err != nil {
		return err
	}
	if r.goos == "darwin" {
		if err := r.verifySignature(ctx, path); err != nil {
			return err
		}
	}
	return nil
}

// trustedChain requires the file and each directory from it up to and
// including the .app bundle to be owned by the current user or root and not
// writable by group or other: anyone who could write there could swap the
// binary. Stat follows symlinks, so a symlinked component is judged by its
// target's own metadata, not by the directories above the target.
func (r *Resolver) trustedChain(path string) error {
	q := termsafe.QuoteArgMax(path, 0)
	bundle, ok := bundleDir(path)
	if !ok {
		return fmt.Errorf("%s: not inside an .app bundle", q)
	}
	for p := path; ; p = filepath.Dir(p) {
		info, err := r.fs.Stat(p)
		if err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
		pq := termsafe.QuoteArgMax(p, 0)
		if !info.UIDKnown {
			return fmt.Errorf("%s: cannot tell who owns %s", q, pq)
		}
		if info.UID != r.uid && info.UID != 0 {
			return fmt.Errorf("%s: %s is owned by uid %d, not you or root", q, pq, info.UID)
		}
		if info.Mode.Perm()&0o022 != 0 {
			return fmt.Errorf("%s: %s is writable by group or other (mode %04o)", q, pq, info.Mode.Perm())
		}
		if p == bundle {
			return nil
		}
	}
}

// bundleDir returns the first path component that ends in ".app", with the
// directories above it.
func bundleDir(path string) (string, bool) {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i, c := range parts {
		if i > 0 && strings.HasSuffix(c, ".app") {
			return filepath.FromSlash(strings.Join(parts[:i+1], "/")), true
		}
	}
	return "", false
}

// verifySignature runs the system code-signature check against TeamID.
func (r *Resolver) verifySignature(ctx context.Context, path string) error {
	req := `anchor apple generic and certificate leaf[subject.OU] = "` + TeamID + `"`
	if _, err := r.runner.Run(ctx, "codesign", "--verify", "--strict", "-R="+req, path); err != nil {
		return fmt.Errorf("%s: code signature is not Team ID %s: %w", termsafe.QuoteArgMax(path, 0), TeamID, err)
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

// vet runs `whoami --json` on path, which executes it, and checks the reply.
func (r *Resolver) vet(ctx context.Context, path string, src Source) (Resolved, error) {
	q := termsafe.QuoteArgMax(path, 0)
	runCtx, cancel := context.WithTimeout(ctx, whoamiTimeout)
	defer cancel()
	out, err := r.runner.Run(runCtx, path, "whoami", "--json")
	if err != nil {
		if ctx.Err() != nil {
			// The caller cancelled or ran out of time: say so, not "incompatible".
			return Resolved{}, fmt.Errorf("%s whoami --json: %w", q, ctx.Err())
		}
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
