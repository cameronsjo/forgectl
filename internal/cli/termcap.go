package cli

import "github.com/cameronsjo/forgectl/internal/termsafe"

// The capped text boundary for internal/cli (#913). Every value a text
// printer in this package writes is escaped AND bounded: termsafe.SafeLine
// alone makes a value inert but leaves its length to whoever wrote it, and a
// disk-, server- or subprocess-sourced value has a length nobody at the
// terminal chose (#891, #894). Text printers reach termsafe through the
// helpers below, one per field class; TestTextPrintersUseCappedHelpers pins
// that no other function in the package uses an uncapped termsafe primitive
// (SafeLine, QuoteText, QuotePathIfUnsafe, or a *Max call without a positive
// constant cap), except its allowlisted entries, each with its reason.
//
// Each cap counts escaped OUTPUT runes (SafeLineMax) or, for paths, input
// runes (QuotePathMax), and a cut value says it was cut. --json surfaces carry
// every value whole through termsafe.JSONEncoder; nothing here is for them.

const (
	// labelMaxRunes caps a short identifier: a session id, project, model,
	// machine, branch, a PR or issue ref, a state, a version, a host, a
	// repo owner or name. 64 holds a UUID, a repo slug or a model id whole.
	labelMaxRunes = 64

	// titleMaxRunes caps a one-line title a document, a PR, an issue or a
	// task gave itself: a frontmatter `title:`, an H1, a gh title. 256
	// shows any realistic heading whole.
	titleMaxRunes = 256

	// snippetMaxRunes caps a search-match snippet. Postgres ts_headline with
	// MaxWords=20 bounds words, not characters, so one long word passes
	// through whole (#891). 320 is docs search's snippet cap
	// (docsSearchSnippetRunes) and holds 20 ordinary words plus the <<>>
	// match markers several times over.
	snippetMaxRunes = 320

	// textMaxRunes caps a line of free text: a reason, an error, a detail, a
	// note, a whole composed line. It is larger than a title because an
	// error text can carry up to two capped paths (termsafe.PathEchoMaxRunes
	// each) and still needs its own words around them.
	textMaxRunes = 1280
)

// safeLabel is a short identifier made terminal-safe and bounded for a line
// of text output.
func safeLabel(s string) string {
	return termsafe.SafeLineMax(s, labelMaxRunes)
}

// safeTitle is a title made terminal-safe and bounded for a line of text
// output.
func safeTitle(s string) string {
	return termsafe.SafeLineMax(s, titleMaxRunes)
}

// safeSnippet is a match snippet made terminal-safe and bounded for a line of
// text output.
func safeSnippet(s string) string {
	return termsafe.SafeLineMax(s, snippetMaxRunes)
}

// safeText is a line of free text made terminal-safe and bounded for text
// output.
func safeText(s string) string {
	return termsafe.SafeLineMax(s, textMaxRunes)
}

// safePath renders a path for text output: quoted, escaped, and capped at
// termsafe.PathEchoMaxRunes input runes (#894). A longer path is cut in the
// middle so its file name survives, the same rule every other path echo in
// forgectl follows.
func safePath(s string) string {
	return termsafe.QuotePathMax(s, termsafe.PathEchoMaxRunes)
}

// safeColumnPath renders a path for a fixed-width text column: escaped and
// capped like safePath, but unquoted, so an ordinary row's column does not
// shift by two quotes (#913). The ellipsis of a cut path is not quoted apart
// from the path text; a sink that needs that uses safePath.
func safeColumnPath(s string) string {
	return termsafe.SafePathMax(s, termsafe.PathEchoMaxRunes)
}
