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
//     vouches for this binary, so no ownership or signature gate applies
//     unless it lies inside an .app, directly or through a symlink, in which
//     case it gets the bundle gates below before it is run;
//  2. `rex` on PATH, on the same terms (a PATH hit that fails the bundle
//     gates is skipped and listed);
//  3. the installed Rex app bundles, matched by glob (see
//     [DefaultBundlePatterns]), never by one hard-coded path, because a Rex
//     update can move the binary inside the bundle.
//
// # Bundle order and gates
//
// Bundles are tried per root, /Applications first and then ~/Applications
// (only when $HOME is absolute): the exact names Rex.app and Rex Beta.app, in
// that order, then any other Rex*.app in sorted order. The exact names come
// first so a look-alike that merely sorts earlier does not get tried before a
// genuine install; it is the gates below, not the order, that keep a planted
// bundle from running.
//
// /Applications is writable by every admin account, so a discovered bundle is
// not trusted by name. Before a bundle candidate is run it must pass:
//
//   - a chain of real components from the rex file up to the .app directory,
//     judged with lstat: no symlink anywhere in it (the .app, a directory or
//     the file), each component owned by the current user or root, and none
//     writable by group or other. The mode check reads POSIX mode bits and
//     ignores ACLs. The chain ends at the outermost component whose name ends
//     in .app, compared without regard to case (APFS is case-insensitive), so
//     an untrusted .app above the Rex bundle is judged too;
//   - an executable regular file;
//   - on darwin, `/usr/bin/codesign --verify --strict` against a requirement
//     that the certificate chain is a Developer ID one and the leaf
//     certificate's team is [TeamID], run through the
//     [exec.Runner] under its own 5 second timeout.
//
// A candidate that fails a gate is skipped without being run, and its reason
// is listed if resolution fails. Every candidate that passes is tried in
// order and the first whose `whoami --json` is compatible wins. If none is,
// the error lists each candidate and why. The file's device, inode and
// modification time are recorded by the gate and checked again after
// `whoami` returns; a change refuses the candidate as "binary changed while
// being verified". For a bundle, [Resolved].Path is the cleaned path that was
// gated, never a symlink; a config or PATH hit is returned as found.
//
// # Residual race
//
// The gates narrow the window between check and use; they do not close it.
// The directory that holds the .app (/Applications) is root:admin with mode
// 775 and no sticky bit, so any admin-group account can rename a bundle in or
// out of it at any moment, and the gates judge a path name, not the bytes that
// later run. Two consequences:
//
//   - The device, inode and modification time check after `whoami` catches a
//     swap that is left in place. It cannot stop the code from having run, and
//     a swap that is reverted before the check (genuine bundle out after
//     codesign, attacker in for the exec, genuine back) passes it. An
//     admin-group account that wins that race runs its code as the operator.
//   - Every later exec of [Resolved].Path has the same exposure. Call
//     [Resolver.Verify] right before each one; it repeats the gates and shrinks
//     the window to the gap between Verify and exec.
//
// The directory holding a ~/Applications bundle, and $HOME above it, are not
// judged either; the default (a directory only you can write) is safe, a
// group-writable or symlinked one is not. Setups that need more should set
// `[surface.rex] binary_path` to a binary in a directory only they can write.
// A pinned or PATH binary outside any .app is not gated: that choice is the
// operator's to vouch for. One inside an .app, or a symlink whose target is,
// is gated when it is resolved and again by [Resolver.Verify] before each
// later use; the exec follows a symlink afresh, so repointing it after the
// check is part of the race above.
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
// The Rex surface backend (`--surface rex`) builds one Resolver, calls Resolve,
// and calls Verify before every command:
//
//	r := rexbin.New(runner)
//	bin, err := r.Resolve(ctx, cfg.Surface.Rex.BinaryPath)
//	// bin.Path is absolute; bin.Source says which layer chose it.
package rexbin
