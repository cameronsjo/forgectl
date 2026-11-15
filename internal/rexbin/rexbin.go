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
// the code-signing requirement built from it (together with the Developer ID certificate checks). Measured with `codesign
// --verify` on Rex Beta 0.1.0-preview.19.
const TeamID = "AG92C3PT77"

// whoamiTimeout bounds one `whoami --json` run, so a hung candidate cannot
// hold resolution for as long as the caller's context allows.
const whoamiTimeout = 5 * time.Second

// signTimeout bounds one `codesign --verify` run, for the same reason.
const signTimeout = 5 * time.Second

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

// Verify re-runs the trust gates on a previously resolved binary and reports
// whether it may still be executed. Call it immediately before each exec of
// [Resolved].Path, and exec exactly that string: Resolve judged the file once,
// and /Applications can be rewritten by any admin account at any time.
//
// The gate level follows the path, not the label. A path inside an .app, or a
// symlink that resolves into one, is re-gated in full (chain, regular
// executable file, signature) whatever its Source says; a symlink that names an
// .app but resolves outside one, or cannot be resolved, is refused, so a pinned `binary_path` that names the bundle binary gets the
// same check as a discovered one. Any other path must still be an executable
// regular file. A Path that is not absolute and already clean (no `..`, no
// doubled separators) is refused, because the kernel resolves `..` after
// following symlinks and would open a different file than the one gated, and
// a Source that is not one of the three constants is refused. Verify narrows
// the window to the gap between Verify and exec; it does not close it (see the
// package doc). It does not re-run `whoami`, so a bundle swapped for another
// validly signed Rex build passes. The signature gate checks only the team and
// the Developer ID certificate kind. Off darwin there is no signature check;
// Rex is macOS-only.
func (r *Resolver) Verify(ctx context.Context, res Resolved) error {
	q := termsafe.QuoteArgMax(res.Path, 0)
	switch res.Source {
	case SourceConfig, SourcePATH, SourceBundle:
	default:
		return fmt.Errorf("%s: unknown source %s", q, termsafe.QuoteArgMax(string(res.Source), 0))
	}
	if !filepath.IsAbs(res.Path) || filepath.Clean(res.Path) != res.Path {
		return fmt.Errorf("%s: not an absolute, clean path", q)
	}
	target, inApp, aerr := r.appTarget(res.Path)
	if aerr != nil {
		return aerr
	}
	if !inApp {
		return r.usable(res.Path)
	}
	if _, err := r.bundleCandidate(ctx, target); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", q, ctx.Err())
		}
		return err
	}
	return nil
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
	// Dev and Ino identify the file on disk; meaningful only when UIDKnown.
	Dev, Ino uint64
	ModTime  time.Time
}

// FS is the slice of the filesystem the resolver reads; tests fake it. Stat
// follows symlinks; Lstat does not.
type FS interface {
	Stat(name string) (Info, error)
	Lstat(name string) (Info, error)
	// Readlink returns the text of the symlink at name.
	Readlink(name string) (string, error)
	Glob(pattern string) ([]string, error)
}

type osFS struct{}

func (osFS) Stat(name string) (Info, error)  { return infoOf(os.Stat(name)) }
func (osFS) Lstat(name string) (Info, error) { return infoOf(os.Lstat(name)) }

func infoOf(fi fs.FileInfo, err error) (Info, error) {
	if err != nil {
		return Info{}, err
	}
	uid, dev, ino, known := sysInfo(fi)
	return Info{Mode: fi.Mode(), IsDir: fi.IsDir(), UID: uid, UIDKnown: known, Dev: dev, Ino: ino, ModTime: fi.ModTime()}, nil
}

func (osFS) Readlink(name string) (string, error) { return os.Readlink(name) }

func (osFS) Glob(pattern string) ([]string, error) { return filepath.Glob(pattern) }

// Resolver finds a usable rex binary. Build one with New.
type Resolver struct {
	runner   fexec.Runner
	fs       FS
	lookPath func(string) (string, error)
	patterns []string
	uid      int
	goos     string
	// signTimeout bounds one codesign run.
	signTimeout time.Duration
	home        string
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
		runner:      runner,
		fs:          osFS{},
		lookPath:    exec.LookPath,
		uid:         os.Getuid(),
		goos:        runtime.GOOS,
		signTimeout: signTimeout,
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
		// A pin inside an .app (or a symlink into one) gets the bundle checks
		// before it is run, like a discovered bundle; any other pin is the
		// operator's own choice and is only checked to be executable.
		target, before, gated, gerr := r.gateAppPath(ctx, abs)
		if gerr != nil {
			if ctx.Err() != nil {
				return Resolved{}, fmt.Errorf("%s: %w", termsafe.QuoteArgMax(abs, 0), ctx.Err())
			}
			return Resolved{}, fmt.Errorf("rex binary from [surface.rex] binary_path is refused as an app-bundle binary: %w", termsafe.Error(gerr))
		}
		got, verr := r.vet(ctx, abs, SourceConfig)
		if verr == nil && gated {
			if cerr := r.unchanged(target, before); cerr != nil {
				return Resolved{}, cerr
			}
		}
		return got, verr
	}
	tried := []string{"[surface.rex] binary_path (not set)"}

	pathBin, pathErr := r.fromPATH()
	if pathErr == nil {
		target, before, gated, gerr := r.gateAppPath(ctx, pathBin)
		switch {
		case gerr != nil && ctx.Err() != nil:
			return Resolved{}, fmt.Errorf("%s: %w", termsafe.QuoteArgMax(pathBin, 0), ctx.Err())
		case gerr != nil:
			pathErr = fmt.Errorf("PATH hit is refused as an app-bundle binary: %w", termsafe.Error(gerr))
		default:
			got, verr := r.vet(ctx, pathBin, SourcePATH)
			if verr == nil && gated {
				if cerr := r.unchanged(target, before); cerr != nil {
					return Resolved{}, cerr
				}
			}
			return got, verr
		}
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
			m = filepath.Clean(m)
			if seen[m] {
				continue
			}
			seen[m] = true
			before, err := r.bundleCandidate(ctx, m)
			if err != nil {
				if ctx.Err() != nil {
					return Resolved{}, fmt.Errorf("%s: %w", termsafe.QuoteArgMax(m, 0), ctx.Err())
				}
				tried = append(tried, err.Error())
				continue
			}
			got, err := r.vet(ctx, m, SourceBundle)
			if err == nil {
				// The gates judged the file at one moment and whoami ran it at
				// another: refuse if it is not the same file any more.
				if cerr := r.unchanged(m, before); cerr != nil {
					tried = append(tried, cerr.Error())
					continue
				}
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
			return "", fmt.Errorf("cannot expand %s from %s: no usable home directory (%w); use an absolute path instead", q, key, termsafe.Error(r.homeErr))
		}
		path = filepath.Join(r.home, strings.TrimPrefix(strings.TrimPrefix(value, "~"), "/"))
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s must be an absolute path or start with ~/, got %s", key, q)
	}
	path = filepath.Clean(path)
	if err := r.usable(path); err != nil {
		return "", fmt.Errorf("rex binary from %s is unusable: %w", key, termsafe.Error(err))
	}
	return path, nil
}

// fromPATH returns the absolute path of an executable `rex` on PATH, or why
// there is none.
func (r *Resolver) fromPATH() (string, error) {
	p, err := r.lookPath("rex")
	if err != nil {
		return "", fmt.Errorf("rex not found: %w", termsafe.Error(err))
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

// gateAppPath runs the bundle checks on path when it (or the file it resolves
// to) is inside an .app, returning the gated target and its identity as
// judged. gated is false, and nothing is checked, for a plain binary.
func (r *Resolver) gateAppPath(ctx context.Context, path string) (target string, before Info, gated bool, err error) {
	target, in, err := r.appTarget(path)
	if err != nil {
		return "", Info{}, false, err
	}
	if !in {
		return "", Info{}, false, nil
	}
	before, err = r.bundleCandidate(ctx, target)
	if err != nil {
		return "", Info{}, false, err
	}
	return target, before, true, nil
}

// usable accepts an executable regular file, following symlinks (the config
// and PATH layers). The exec-bit test does not apply on Windows, where Stat
// reports every file as 0666; Rex is macOS-only.
func (r *Resolver) usable(path string) error {
	info, err := r.fs.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", termsafe.QuoteArgMax(path, 0), termsafe.Error(err))
	}
	return checkFile(path, info)
}

// checkFile refuses a directory, a non-regular file, or a file with no exec bit.
func checkFile(path string, info Info) error {
	q := termsafe.QuoteArgMax(path, 0)
	switch {
	case info.IsDir:
		return fmt.Errorf("%s is a directory", q)
	case !info.Mode.IsRegular():
		return fmt.Errorf("%s is not a regular file", q)
	case info.Mode.Perm()&0o111 == 0:
		return fmt.Errorf("%s is not executable", q)
	}
	return nil
}

// bundleCandidate gates a bundle-discovered binary before anything runs it: a
// chain of real (non-symlink) components from the file up to the .app
// directory, each owned by the current user or root and not group- or
// other-writable; an executable regular file; and on darwin the code-signature
// check. It returns the file's identity as judged, for [Resolver.unchanged].
// The returned error is the reason it was skipped, naming the path. path must
// be clean.
func (r *Resolver) bundleCandidate(ctx context.Context, path string) (Info, error) {
	file, err := r.trustedChain(path)
	if err != nil {
		return Info{}, err
	}
	if err := checkFile(path, file); err != nil {
		return Info{}, err
	}
	if r.goos == "darwin" {
		if err := r.verifySignature(ctx, path); err != nil {
			return Info{}, err
		}
	}
	return file, nil
}

// trustedChain judges the file and each directory from it up to and including
// the .app bundle with Lstat: none may be a symlink, each must be owned by the
// current user or root, and none may be writable by group or other, because
// anyone who could write there could swap the binary. It returns the file's
// own Info. The mode check reads POSIX permission bits only and ignores ACLs.
// The directory that holds the .app (/Applications) is not judged; see the
// package doc for the rename race that leaves.
func (r *Resolver) trustedChain(path string) (Info, error) {
	q := termsafe.QuoteArgMax(path, 0)
	bundle, ok := bundleDir(path)
	if !ok {
		return Info{}, fmt.Errorf("%s: not inside an .app bundle", q)
	}
	var file Info
	for p := path; ; p = filepath.Dir(p) {
		info, err := r.fs.Lstat(p)
		if err != nil {
			return Info{}, fmt.Errorf("%s: %w", q, termsafe.Error(err))
		}
		pq := termsafe.QuoteArgMax(p, 0)
		switch {
		case info.Mode&fs.ModeSymlink != 0:
			return Info{}, fmt.Errorf("%s: %s is a symlink", q, pq)
		case !info.UIDKnown:
			return Info{}, fmt.Errorf("%s: cannot tell who owns %s", q, pq)
		case info.UID != r.uid && info.UID != 0:
			return Info{}, fmt.Errorf("%s: %s is owned by uid %d, not you or root", q, pq, info.UID)
		case info.Mode.Perm()&0o022 != 0:
			return Info{}, fmt.Errorf("%s: %s is writable by group or other (mode %04o)", q, pq, info.Mode.Perm())
		}
		if p == path {
			file = info
		}
		if p == bundle {
			return file, nil
		}
		if filepath.Dir(p) == p {
			return Info{}, fmt.Errorf("%s: never reached the .app bundle", q)
		}
	}
}

// unchanged refuses when the file at path is no longer the one the gates
// judged (device, inode or modification time differ, or it is a symlink now).
func (r *Resolver) unchanged(path string, before Info) error {
	now, err := r.fs.Lstat(path)
	if err != nil || now.Mode&fs.ModeSymlink != 0 || now.Dev != before.Dev || now.Ino != before.Ino || !now.ModTime.Equal(before.ModTime) {
		return fmt.Errorf("%s: binary changed while being verified", termsafe.QuoteArgMax(path, 0))
	}
	return nil
}

// bundleDir returns the directory the ownership chain must reach: the
// outermost component whose name ends in ".app", compared without regard to
// case (APFS is case-insensitive, so `REX BETA.APP` names the same bundle).
// Outermost, so an .app nested inside a Rex bundle cannot cut the chain short
// and an .app above the Rex one (a pinned path under Other.app) is judged too.
// path must be clean.
func bundleDir(path string) (string, bool) {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i, c := range parts {
		if i > 0 && strings.EqualFold(filepath.Ext(c), ".app") {
			return filepath.FromSlash(strings.Join(parts[:i+1], "/")), true
		}
	}
	return "", false
}

// maxLinkHops bounds the symlinks one resolution follows (the kernel stops at
// 32 on macOS).
const maxLinkHops = 40

// hasAppComponent reports whether any component of p names an .app, without
// regard to case.
func hasAppComponent(p string) bool {
	for _, c := range strings.Split(filepath.ToSlash(p), "/") {
		if strings.EqualFold(filepath.Ext(c), ".app") {
			return true
		}
	}
	return false
}

// walkLinks resolves path the way the kernel does, one component at a time
// with Lstat and Readlink, and returns the resolved path and the text of every
// symlink it followed, in any component. Unlike filepath.EvalSymlinks it keeps
// going from what it has already seen, and it reports a failure together with
// the link texts read before it.
func (r *Resolver) walkLinks(path string) (resolved string, texts []string, err error) {
	rest := strings.Split(strings.Trim(filepath.ToSlash(path), "/"), "/")
	cur := "/"
	for hops := 0; len(rest) > 0; {
		c := rest[0]
		rest = rest[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, c)
		info, lerr := r.fs.Lstat(next)
		if lerr != nil {
			return "", texts, lerr
		}
		if info.Mode&fs.ModeSymlink == 0 {
			cur = next
			continue
		}
		if hops++; hops > maxLinkHops {
			return "", texts, fmt.Errorf("more than %d symlinks", maxLinkHops)
		}
		text, rerr := r.fs.Readlink(next)
		if rerr != nil {
			return "", texts, rerr
		}
		texts = append(texts, text)
		if filepath.IsAbs(text) {
			cur = "/"
		}
		rest = append(strings.Split(strings.Trim(filepath.ToSlash(text), "/"), "/"), rest...)
	}
	return cur, texts, nil
}

// appTarget decides whether path is a bundle binary and what to gate. A path
// named inside an .app is gated as written (the Lstat chain then refuses any
// symlink in it). Otherwise it is resolved through its symlinks: if the
// resolved file is inside an .app, that file is the target. Fail closed: if
// any symlink followed, or the resolution failing partway, names an .app, the
// path claims to be a bundle binary, and it is refused unless it resolves into
// an .app and passes the gates, because an attacker who can rewrite a
// directory the link passes through could otherwise steer it back to a plain
// executable file. inApp is false, with no error, only for a binary that no
// .app name touches.
func (r *Resolver) appTarget(path string) (target string, inApp bool, err error) {
	if _, in := bundleDir(path); in {
		return path, true, nil
	}
	q := termsafe.QuoteArgMax(path, 0)
	resolved, texts, werr := r.walkLinks(path)
	claims := false
	for _, t := range texts {
		if hasAppComponent(t) {
			claims = true
		}
	}
	if werr != nil {
		if len(texts) > 0 || claims {
			return "", false, fmt.Errorf("%s: cannot resolve its symlinks: %w", q, termsafe.Error(werr))
		}
		return "", false, nil
	}
	if _, in := bundleDir(resolved); in {
		return resolved, true, nil
	}
	if claims {
		return "", false, fmt.Errorf("%s: a symlink in it names an app bundle but it resolves to %s, outside one", q, termsafe.QuoteArgMax(resolved, 0))
	}
	return "", false, nil
}

// codesignPath is the system code-signature tool, named absolutely so a
// stand-in earlier on PATH cannot turn the check off.
const codesignPath = "/usr/bin/codesign"

// verifySignature runs the system code-signature check against TeamID, under
// its own timeout. A caller that cancelled gets that back from Resolve; a
// codesign that only hit its own timeout is a skipped candidate.
func (r *Resolver) verifySignature(ctx context.Context, path string) error {
	// Developer ID Application only: the intermediate and leaf OIDs below exist
	// on Developer ID certificates and not on Apple Development or Apple
	// Distribution ones, which the team ID alone would also accept. The bundle
	// identifier is not pinned, because Rex.app and Rex Beta.app may differ.
	req := `anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] /* exists */` +
		` and certificate leaf[field.1.2.840.113635.100.6.1.13] /* exists */` +
		` and certificate leaf[subject.OU] = "` + TeamID + `"`
	runCtx, cancel := context.WithTimeout(ctx, r.signTimeout)
	defer cancel()
	_, err := r.runner.Run(runCtx, codesignPath, "--verify", "--strict", "-R="+req, path)
	if err == nil {
		return nil
	}
	q := termsafe.QuoteArgMax(path, 0)
	if ctx.Err() == nil && runCtx.Err() != nil {
		return fmt.Errorf("%s: code signature check did not finish within %s", q, r.signTimeout)
	}
	return fmt.Errorf("%s: code signature is not Team ID %s: %w", q, TeamID, termsafe.Error(err))
}

// whoami is the part of `rex whoami --json` the resolver reads; other fields
// are ignored.
type whoami struct {
	RexVersion  *string `json:"rex_version"`
	APIRevision *int    `json:"api_revision"`
}

var versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// whoamiArgs is the one call the resolver makes of a candidate. --autostart=false
// matters: without it `rex` starts a Rex server when none is running, and a
// server started here would inherit this process's whole environment and hand
// it to every block it later runs. With it, a missing server is a refusal
// (the Rex app must be running), the same one the adapter makes.
var whoamiArgs = []string{"--autostart=false", "--color=never", "whoami", "--json"}

// vet runs `whoami --json` on path, which executes it, and checks the reply.
func (r *Resolver) vet(ctx context.Context, path string, src Source) (Resolved, error) {
	q := termsafe.QuoteArgMax(path, 0)
	runCtx, cancel := context.WithTimeout(ctx, whoamiTimeout)
	defer cancel()
	out, err := r.runner.Run(runCtx, path, whoamiArgs...)
	if err != nil {
		if ctx.Err() != nil {
			// The caller cancelled or ran out of time: say so, not "incompatible".
			return Resolved{}, fmt.Errorf("%s whoami --json: %w", q, ctx.Err())
		}
		return Resolved{}, fmt.Errorf("%w: %s whoami --json failed (is the Rex app running? it is never started for this check): %w", ErrIncompatible, q, termsafe.Error(err))
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
