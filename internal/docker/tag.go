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
// component: lowercased, any run of characters outside [a-z0-9._-]
// collapsed to a single '-', and leading/trailing '.'/'-' trimmed (a tag
// must start with an alphanumeric or '_', which every character surviving
// the loop below already satisfies). The result is capped at maxTagLen. An
// empty or fully-invalid input falls back to "branch" so deriveTag never
// produces a malformed tag — this is also the sanitizer that keeps a
// hostile git branch name (e.g. "-x") from reaching docker/git argv as
// something option-like.
func slugifyBranch(branch string) string {
	lower := strings.ToLower(branch)

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
		return "branch"
	}
	if len(slug) > maxTagLen {
		slug = strings.TrimRight(slug[:maxTagLen], "-.")
	}
	return slug
}
