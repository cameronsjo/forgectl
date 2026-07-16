// env.go is the domain's clipboard-touching entry point: CopyValue (the
// `get` pipeline) and SetFromClipboard (the `set --clipboard` pipeline),
// plus SetValue (the plain stdin/prompt `set` pipeline SetFromClipboard
// shares once it has its own value in hand). This is where the plan's
// structural guarantee lives: a value read out of a Document or off the
// clipboard is handed straight to *clip.Client or writeAtomic and never
// returned to a caller as a string — the CLI layer (a later commit) never
// holds a value it could print.
package env

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/cameronsjo/forgectl/internal/clip"
)

// validKeyPattern documents ValidKey's regex for error messages — kept as
// its own literal (rather than exported off document.go) so this package's
// only cross-file coupling for it is the ValidKey predicate itself.
const validKeyPattern = "[A-Za-z_][A-Za-z0-9_]*"

// errInvalidKey is the shared "invalid key" refusal every value-bearing
// command uses verbatim: it names the RULE a rejected key argument broke,
// never the argument itself. This is what makes a hostile shape like
// `env set KEY=VALUE` or `env get some-typo'd-value` refuse cleanly — the
// offending token never reaches an error string (refusal-message
// discipline, see internal/cli/env.go).
func errInvalidKey() error {
	return fmt.Errorf("key must match %s; values are piped or --clipboard, never argv", validKeyPattern)
}

// errKeyNotFound is the "no such key" refusal, and it names the FILE rather
// than the key that missed. The asymmetry is deliberate: a key the file
// actually holds is safe to name (the file already carries that name, so an
// error repeating it discloses nothing new), but a key that is NOT in the
// file is precisely the token that may not be a key at all. A secret pasted
// into the key slot — `env get sk_live_51H8xY2eZvKYlo2C --clipboard`, an
// utterly ordinary typo — passes ValidKey, because plenty of real API keys
// match [A-Za-z_][A-Za-z0-9_]*. Echoing the miss would then write the secret
// straight into stderr and the session transcript, which is the one outcome
// this whole package exists to prevent. "Not found" is the branch where the
// argument is least likely to be a key name, so it is the branch that must
// stay quietest.
func errKeyNotFound(realPath string) error {
	return fmt.Errorf("no such key in %s (run `forgectl env keys` to list them)", realPath)
}

// Client is the domain entry point for forgectl env's clipboard-touching
// operations. It wraps *clip.Client rather than exec.Runner directly — env
// shells nothing of its own; the clipboard is its only non-filesystem
// effect (mirrors internal/y's ownership of the same *clip.Client type).
type Client struct {
	clip *clip.Client
}

// NewClient builds a Client over the given clipboard client.
func NewClient(clipClient *clip.Client) *Client {
	return &Client{clip: clipClient}
}

// CopyValue resolves key's value in file and copies it to the system
// clipboard. The value is never returned — this is the domain half of
// `get`'s structural "no print path exists" guarantee; the CLI command
// only ever learns whether this call succeeded. allowAnyFile is threaded
// straight through to Locate — see its doc comment and the CLI's
// resolveAllowAnyFile for the --any-file TTY-gated escape hatch.
func (c *Client) CopyValue(ctx context.Context, cwd, file, key string, allowAnyFile bool) error {
	if !ValidKey(key) {
		return errInvalidKey()
	}
	realPath, exists, err := Locate(file, cwd, allowAnyFile)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%s not found", realPath)
	}
	doc, err := parseFile(realPath)
	if err != nil {
		return err
	}
	value, ok := doc.Get(key)
	if !ok {
		return errKeyNotFound(realPath)
	}
	return c.clip.Copy(ctx, value)
}

// SetValue runs the set pipeline (validate → locate → parse → strip → set →
// write) against value directly — the piped-stdin and interactive-prompt
// sources, both already resolved to a plain string by the CLI layer before
// this is called. SetFromClipboard is the clipboard-sourced sibling that
// shares this exact pipeline after its own clip.Paste.
func (c *Client) SetValue(cwd, file, key, value string, allowAnyFile bool) (tightened bool, err error) {
	if !ValidKey(key) {
		return false, errInvalidKey()
	}
	return c.commitSet(cwd, file, key, value, allowAnyFile)
}

// SetFromClipboard pastes the current clipboard contents and runs them
// through the same pipeline as SetValue. Key validation happens BEFORE the
// paste — "ValidKey first, refuse before touching the file or reading
// input" applies to the clipboard source exactly as it does to stdin: a
// hostile key argument must never even trigger a clipboard read.
func (c *Client) SetFromClipboard(ctx context.Context, cwd, file, key string, allowAnyFile bool) (tightened bool, err error) {
	if !ValidKey(key) {
		return false, errInvalidKey()
	}
	value, err := c.clip.Paste(ctx)
	if err != nil {
		return false, err
	}
	return c.commitSet(cwd, file, key, value, allowAnyFile)
}

// commitSet is the shared `set` tail once key is already validated and
// value already sourced: Locate → Parse (an empty Document when the file
// doesn't exist yet) → strip exactly one trailing "\n" or "\r\n" off
// rawValue → refuse an empty result → Document.Set (refuses a duplicate
// key) → Bytes → writeAtomic.
func (c *Client) commitSet(cwd, file, key, rawValue string, allowAnyFile bool) (tightened bool, err error) {
	realPath, exists, err := Locate(file, cwd, allowAnyFile)
	if err != nil {
		return false, err
	}
	doc, err := loadOrEmpty(realPath, exists)
	if err != nil {
		return false, err
	}

	value := stripTrailingNewline(rawValue)
	if value == "" {
		return false, fmt.Errorf("empty value; refusing to set %s to empty — edit the file directly if intended", key)
	}

	if err := doc.Set(key, value); err != nil {
		return false, err
	}
	return writeAtomic(realPath, doc.Bytes())
}

// stripTrailingNewline removes exactly one trailing "\n" or "\r\n" from
// s — what a piped-stdin value or a clipboard paste carries from the
// producing command's own line ending (a Windows-clipboard paste leaves no
// stray \r beyond the one the \r\n pair accounts for). Interior whitespace
// is never touched, and a value with no trailing newline at all (the
// interactive no-echo prompt path) passes through unchanged.
func stripTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\r\n") {
		return s[:len(s)-2]
	}
	if strings.HasSuffix(s, "\n") {
		return s[:len(s)-1]
	}
	return s
}

// loadOrEmpty parses realPath, or returns a fresh empty Document when the
// file doesn't exist yet (Locate already confirmed its parent directory is
// inside the repo — this is the "new file" branch of `set`).
func loadOrEmpty(realPath string, exists bool) (*Document, error) {
	if !exists {
		return &Document{}, nil
	}
	return parseFile(realPath)
}

// parseFile opens and parses realPath. Errors carry the path only.
func parseFile(realPath string) (*Document, error) {
	f, err := os.Open(realPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", realPath, err)
	}
	defer f.Close()
	doc, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", realPath, err)
	}
	return doc, nil
}
