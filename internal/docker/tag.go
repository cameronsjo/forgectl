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

// slugifyRepo converts either a git-root or context-directory basename into
// a valid docker repository name component. Neither filesystem-derived name
// is guaranteed docker-clean (spaces, mixed case, and symbols all appear in
// the wild).
//
// This deliberately does NOT share slugify with slugifyBranch: the two
// positions have different grammars, and the tag grammar is the more
// permissive of the pair. See slugifyRepoName for the repository rules.
func slugifyRepo(name string) string {
	return slugifyRepoName(name, "image")
}

// slugifyRepoName converts s into a valid docker *repository name*
// component. That grammar is stricter than a tag's:
//
//	[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*
//
// — it must both start and end with an alphanumeric, and each separator
// between two alphanumeric runs must be exactly one of '.', '_', '__', or a
// run of '-'. A mix is not a separator: "app_-backup" parses as neither,
// even though it is a perfectly valid *tag*. Passing a tag-sanitized string
// into this position is what produced `invalid reference format` straight
// out of docker's own parser.
//
// s is lowercased; every maximal run of non-alphanumeric characters between
// two alphanumerics collapses to a single separator token (see
// repoSeparator), and leading and trailing runs are dropped entirely rather
// than trimmed after the fact — so a directory literally named "_scratch"
// or "app_ backup" survives as something docker accepts. The result is
// capped at maxTagLen and re-trimmed, so truncation can't leave a dangling
// separator either. An empty or fully-invalid input falls back to fallback.
func slugifyRepoName(s, fallback string) string {
	var b strings.Builder
	var pending strings.Builder // separator run seen since the last alphanumeric

	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if b.Len() > 0 && pending.Len() > 0 {
				b.WriteString(repoSeparator(pending.String()))
			}
			pending.Reset()
			b.WriteRune(r)
			continue
		}
		pending.WriteRune(r)
	}

	slug := b.String()
	if len(slug) > maxTagLen {
		slug = strings.TrimRight(slug[:maxTagLen], "-._")
	}
	if slug == "" {
		return fallback
	}
	return slug
}

// repoSeparator maps a run of non-alphanumeric characters onto the single
// separator token the repository grammar accepts in its place. Only a pure
// lone '_', a pure doubled '__', and a pure lone '.' survive as themselves;
// every other run — a longer underscore run, a dot run, or any mix such as
// "_ " — becomes '-', the one separator legal at any repeated length.
func repoSeparator(run string) string {
	switch run {
	case "_", "__", ".":
		return run
	}
	return "-"
}

// slugify converts s into a valid docker *tag* component: lowercased, any
// run of characters outside [a-z0-9._-] collapsed to a single '-', and
// leading/trailing '.'/'-' trimmed (a tag must start with an alphanumeric
// or '_', which every character surviving the loop below already
// satisfies). The result is capped at maxTagLen. An empty or fully-invalid
// input falls back to fallback.
//
// Tag-only: the repository-name grammar is stricter and has its own
// sanitizer (slugifyRepoName).
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
