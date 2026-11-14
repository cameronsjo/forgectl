// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

// Package rexbin finds and vets the `rex` binary, the command-line client of
// the Rex terminal multiplexer.
//
// Rex ships `rex` inside its app bundle and does not link it onto PATH, so a
// machine where nothing linked it has no `rex` for forgectl to run. [Resolver]
// looks in this order and stops at the first candidate that holds:
//
//  1. the `[surface.rex] binary_path` config key (passed in as ConfigPath);
//  2. `rex` on PATH;
//  3. the app bundles under the install roots, matched by glob
//     (see [DefaultBundlePatterns]), never by one hard-coded path, because a
//     Rex update can move the binary inside the bundle.
//
// A candidate must be an executable regular file. An unusable PATH or bundle
// candidate is skipped; an unusable config key is a refusal, because an
// operator who named a binary wants that one. When no candidate holds, the
// error lists every place that was tried.
//
// The first candidate found is then vetted by running `<rex> whoami --json`
// through an [exec.Runner]. The reply must carry a parseable `rex_version` and
// an `api_revision` equal to [APIRevision]. A mismatch is a refusal naming the
// binary's version and revision and the revision forgectl needs, not a
// warning, and the resolver does not fall through to a later candidate: the
// binary it found is the one the operator would have run.
//
// The package has no caller yet; the Rex surface backend will build one
// Resolver and call Resolve:
//
//	r := rexbin.New(runner)
//	bin, err := r.Resolve(ctx, cfg.Surface.Rex.BinaryPath)
//	// bin.Path is absolute; bin.Source says which layer chose it.
//
// `whoami` is read-only, so resolving never changes a running Rex.
package rexbin
