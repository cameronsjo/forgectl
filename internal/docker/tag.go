package docker

import "strings"

// maxTagLen is docker's own limit on a tag component (the docker/distribution
// reference grammar caps a tag at 128 characters).
const maxTagLen = 128

// devTagSuffix is the fixed alias `build` tags alongside the derived
// {repo}:{branch-slug}-{shortsha} tag, so `run`/`shell` (and humans) have a
// stable, always-current handle for "whatever I last built".
const devTagSuffix = ":dev"

// deriveTag builds the {repo}:{branch-slug}-{shortsha} tag that `docker
// build` applies alongside the caller-appended :dev alias (see devTag).
// repo, branch, and shortsha are assumed already resolved (git plumbing,
// not user input) — deriveTag itself only sanitizes branch via
// slugifyBranch.
func deriveTag(repo, branch, shortsha string) string {
	return repo + ":" + slugifyBranch(branch) + "-" + shortsha
}

// devTag builds the fixed :dev alias for repo.
func devTag(repo string) string {
	return repo + devTagSuffix
}

// slugifyBranch converts a git branch name into a valid docker tag
// component. An empty or fully-invalid input falls back to "branch" so
// deriveTag never produces a malformed tag — this is also the sanitizer
// that keeps a hostile git branch name (e.g. "-x") from reaching
// docker/git argv as something option-like. See slugify for the shared
// sanitization rules.
func slugifyBranch(branch string) string {
	return slugify(branch, "branch")
}

// slugifyRepo converts a directory basename into a valid docker repository
// component. Used only on Build's no-git-metadata path, where the
// directory name (never a sanitized git repo name) becomes the tag's
// repository segment directly — directory names are not guaranteed
// docker-clean (spaces, mixed case, and symbols all appear in the wild).
// The git-metadata path is unaffected: a repo name resolved via git stays
// unsanitized, as before. See slugify for the shared sanitization rules.
func slugifyRepo(name string) string {
	return slugify(name, "image")
}

// slugify converts s into a valid docker tag/repository component:
// lowercased, any run of characters outside [a-z0-9._-] collapsed to a
// single '-', and leading/trailing '.'/'-' trimmed (a tag must start with
// an alphanumeric or '_', which every character surviving the loop below
// already satisfies). The result is capped at maxTagLen. An empty or
// fully-invalid input falls back to fallback.
func slugify(s, fallback string) string {
	lower := strings.ToLower(s)

	var b strings.Builder
	lastDash := false
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			lastDash = false
		default:
			// '.', '-', and anything else all collapse to a single '-';
			// runs of separators never produce doubled dashes.
			if !lastDash && b.Len() > 0 {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}

	slug := strings.Trim(b.String(), "-.")
	if slug == "" {
		return fallback
	}
	if len(slug) > maxTagLen {
		slug = strings.TrimRight(slug[:maxTagLen], "-.")
	}
	return slug
}
