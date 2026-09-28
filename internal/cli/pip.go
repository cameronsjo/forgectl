package cli

import (
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	pippkg "github.com/cameronsjo/forgectl/internal/pip"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// pipModule declares the pip.conf-editor extension (ADR-0005): no config
// section, no alias surface.
var pipModule = module.Manifest{
	Name: "pip",
	Tier: module.TierExtension,
	New:  newPipCmd,
}

// newPipCmd builds `forgectl pip` over the registry Deps.
func newPipCmd(deps module.Deps) *cobra.Command {
	client := pippkg.New(deps.Runner)
	return newPipCmdForClient(client)
}

// newPipCmdForClient builds the command over an already-constructed client —
// split out so tests can inject a *pip.Client pointed at a temp pip.conf
// (mirrors newNetCmdForClient) without needing to go through newPipCmd.
func newPipCmdForClient(client *pippkg.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pip",
		Short: "Comment- and whitespace-preserving pip.conf editor",
		Long: `pip edits pip.conf's index-url entries reversibly: remove comments an
entry out (byte-clean round-trip, no backup sidecar), restore uncomments
whatever remove last tagged. Every subcommand takes --path to target a
pip.conf other than the OS default (` + client.Path() + `).

  forgectl pip remove                          comment out [global] index-url
  forgectl pip remove --key extra-index-url    comment out a different key
  forgectl pip restore                         un-comment everything remove tagged
  forgectl pip show                            print the effective pip.conf
  forgectl pip path                            print the resolved pip.conf path`,
	}
	cmd.AddCommand(
		newPipRemoveCmd(client),
		newPipRestoreCmd(client),
		newPipShowCmd(client),
		newPipPathCmd(client),
	)
	return cmd
}

// resolvePipClient returns client, or a copy pointed at path when path is
// non-empty (the --path flag override).
func resolvePipClient(client *pippkg.Client, path string) *pippkg.Client {
	return client.WithPath(path)
}

// newPipRemoveCmd builds `pip remove`.
func newPipRemoveCmd(client *pippkg.Client) *cobra.Command {
	var section, key, path string
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Comment out a pip.conf entry (reversible)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := resolvePipClient(client, path)
			n, err := c.Remove(cmd.Context(), section, key)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if n == 0 {
				fmt.Fprintf(out, "no [%s] %s entries found in %s\n", section, key, c.Path())
				return nil
			}
			fmt.Fprintf(out, "removed %d [%s] %s %s from %s\n", n, section, key, entryWord(n), c.Path())
			return nil
		},
	}
	cmd.Flags().StringVar(&section, "section", "global", "pip.conf section to edit")
	cmd.Flags().StringVar(&key, "key", "index-url", "entry key to remove")
	cmd.Flags().StringVar(&path, "path", "", "pip.conf path (default: OS-resolved location)")
	return cmd
}

// newPipRestoreCmd builds `pip restore`.
func newPipRestoreCmd(client *pippkg.Client) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Un-comment whatever remove last tagged",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := resolvePipClient(client, path)
			n, err := c.Restore(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if n == 0 {
				fmt.Fprintf(out, "no removed entries to restore in %s\n", c.Path())
				return nil
			}
			fmt.Fprintf(out, "restored %d %s in %s\n", n, entryWord(n), c.Path())
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "pip.conf path (default: OS-resolved location)")
	return cmd
}

// pipShowJSON is the --json wire shape for `pip show`: the resolved path
// (what `pip path` prints) and the active entries of the effective pip.conf
// the human output prints. Entries is [] (never null) for a missing or empty
// file. Comment lines — including entries `pip remove` commented out — are
// not entries and do not appear.
type pipShowJSON struct {
	Path    string         `json:"path"`
	Entries []pipEntryJSON `json:"entries"`
}

// pipEntryJSON is one active `key = value` entry. Key is lowercased, the way
// pip's configparser reads it. Value is the text after the delimiter, with
// continuation lines joined by "\n", and each line passed through
// endpointForDisplay — the redaction `launch doctor` applies to the OTLP
// endpoint — because an index-url is where a registry token rides
// (https://__token__:secret@host/simple, or ?token=…). A JSON payload is
// pasted into agent transcripts verbatim, so the secret-bearing URL parts
// are hidden here even though the human `show` prints the file as-is.
type pipEntryJSON struct {
	Section string `json:"section"`
	Key     string `json:"key"`
	Value   string `json:"value"`
}

// isPipSeparator reports whether r splits a pip list value. pip splits with
// Python's str.split(), which breaks on every Unicode whitespace rune plus the
// ASCII separators \x1c-\x1f. Matching that set exactly matters: a separator
// pip honours but this does not would hand endpointForDisplay two URLs as one
// token, and it only reads the first.
func isPipSeparator(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// redactPipValue applies endpointForDisplay to every separator-delimited
// token, not the line as a whole: pip accepts several URLs on one
// extra-index-url line, and endpointForDisplay only reads the first URL it
// is given, so a second credentialed URL would pass through untouched.
// Separators are kept as written.
func redactPipValue(v string) string {
	var b strings.Builder
	start := -1
	flush := func(end int) {
		if start >= 0 {
			b.WriteString(redactPipToken(v[start:end]))
			start = -1
		}
	}
	for i, r := range v {
		if isPipSeparator(r) {
			flush(i)
			b.WriteRune(r)
			continue
		}
		if start < 0 {
			start = i
		}
	}
	flush(len(v))
	return b.String()
}

// redactPipToken hides one token's userinfo and query. A token that still
// carries an "@" afterwards is one endpointForDisplay could not parse as a
// URL — an unencoded "#" or "/" inside a password stops its host scan
// before the "@" — so the whole token is hidden rather than trusted.
func redactPipToken(tok string) string {
	out := endpointForDisplay(tok)
	if strings.Contains(strings.ReplaceAll(out, "[userinfo hidden]@", ""), "@") {
		return "[value hidden]"
	}
	return out
}

// buildPipShowJSON parses data into the --json wire shape.
func buildPipShowJSON(path string, data []byte) pipShowJSON {
	entries := []pipEntryJSON{}
	for _, l := range pippkg.Parse(data).Lines {
		switch l.Kind {
		case pippkg.KindEntry:
			trimmed := strings.TrimSpace(l.Raw)
			// The key already parsed, so the first delimiter is present;
			// IndexAny finds the same one parseEntryKey chose.
			value := ""
			if i := strings.IndexAny(trimmed, "=:"); i >= 0 {
				value = strings.TrimSpace(trimmed[i+1:])
			}
			entries = append(entries, pipEntryJSON{Section: l.Section, Key: l.Key, Value: redactPipValue(value)})
		case pippkg.KindContinuation:
			if len(entries) == 0 {
				continue
			}
			last := &entries[len(entries)-1]
			cont := redactPipValue(strings.TrimSpace(l.Raw))
			if last.Value == "" {
				last.Value = cont
			} else {
				last.Value += "\n" + cont
			}
		}
	}
	return pipShowJSON{Path: path, Entries: entries}
}

// writePipShowJSON encodes the parsed pip.conf through the sanctioned
// termsafe seam, whose escaping neutralizes any control bytes in the file.
func writePipShowJSON(w io.Writer, path string, data []byte) error {
	enc := termsafe.JSONEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(buildPipShowJSON(path, data))
}

// newPipShowCmd builds `pip show`.
func newPipShowCmd(client *pippkg.Client) *cobra.Command {
	var path string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the effective pip.conf",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := resolvePipClient(client, path)
			data, err := c.Read(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return writePipShowJSON(cmd.OutOrStdout(), c.Path(), data)
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "pip.conf path (default: OS-resolved location)")
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit {"path":...,"entries":[{"section":...,"key":...,"value":...}]} to stdout (URL userinfo and queries hidden; a token in a URL path is not)`)
	return cmd
}

// newPipPathCmd builds `pip path`.
func newPipPathCmd(client *pippkg.Client) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "path",
		Short: "Print the resolved pip.conf path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := resolvePipClient(client, path)
			fmt.Fprintln(cmd.OutOrStdout(), c.Path())
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "pip.conf path (default: OS-resolved location)")
	return cmd
}

// entryWord pluralizes "entry" for a count-driven message.
func entryWord(n int) string {
	if n == 1 {
		return "entry"
	}
	return "entries"
}
