// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

// Package rexbin finds the `rex` binary, the command-line client of the Rex
// terminal multiplexer, and checks that it speaks the API revision forgectl
// needs.
//
// Rex ships `rex` inside its app bundle and does not link it onto PATH, so a
// machine where nothing linked it has no `rex` for forgectl to run.
// [Resolver.Resolve] looks in this order:
//
//  1. the `[surface.rex] binary_path` config key (passed in as configPath).
//     It must be an absolute path or start with `~/`; any other relative value
//     is refused, never resolved against the working directory. The operator
//     vouches for this binary, so no ownership or signature gate applies;
//  2. `rex` on PATH, on the same terms;
//  3. the installed Rex app bundles, matched by glob (see
//     [DefaultBundlePatterns]), never by one hard-coded path, because a Rex
//     update can move the binary inside the bundle.
//
// # Bundle order and gates
//
// Bundles are tried per root, /Applications first and then ~/Applications
// (only when $HOME is absolute): the exact names Rex.app and Rex Beta.app, in
// that order, then any other Rex*.app in sorted order. A planted look-alike
// therefore cannot outrank a real install by sorting first.
//
// /Applications is writable by every admin account, so a discovered bundle is
// not trusted by name. Before a bundle candidate is run it must pass:
//
//   - an executable regular file inside an .app directory;
//   - an ownership and mode check on the file and every directory up to and
//     including the .app bundle: owned by the current user or root, and not
//     writable by group or other;
//   - on darwin, `codesign --verify --strict` against a requirement that the
//     leaf certificate's team is [TeamID], run through the [exec.Runner].
//
// A candidate that fails a gate is skipped without being run, and its reason
// is listed if resolution fails. Every candidate that passes is tried in
// order and the first whose `whoami --json` is compatible wins. If none is,
// the error lists each candidate and why.
//
// # The compatibility check
//
// Resolving runs `<rex> whoami --json` (bounded by its own 5 second timeout)
// on the first config or PATH hit, which is refused at once if it is
// incompatible, and on each bundle candidate that passed the gates. The reply
// must carry a parseable `rex_version` and an `api_revision` equal to
// [APIRevision]; a mismatch is a refusal naming both versions, not a warning.
//
// This is a compatibility check, not an authenticity check: any executable can
// print those two fields. Resolving executes the candidate it checks, which is
// why bundle candidates are gated before they run and a PATH or config hit is
// the operator's own choice. The bytes that answered are not pinned, so a
// caller that later execs [Resolved].Path must re-verify at use time.
//
// # Errors
//
// [ErrNotFound] means no candidate held; [ErrIncompatible] means a binary was
// run and failed the check. A cancelled or expired caller context comes back
// as that context error. An unusable `binary_path` is a plain error naming the
// key.
//
// The package has no caller yet; the Rex surface backend will build one
// Resolver and call Resolve:
//
//	r := rexbin.New(runner)
//	bin, err := r.Resolve(ctx, cfg.Surface.Rex.BinaryPath)
//	// bin.Path is absolute; bin.Source says which layer chose it.
package rexbin
