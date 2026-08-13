package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// configModule declares the config display core module (ADR-0005).
// ConfigKey is deliberately empty: this module RENDERS the whole config, it
// owns no section — ownership belongs to the domain module each section
// configures.
var configModule = module.Manifest{
	Name:         "config",
	Tier:         module.TierCore,
	GroupAliases: []string{"cfg"},
	New:          newConfigCmd,
}

// redactedKeys names dotted config keys whose VALUE must never be printed.
// The provenance marker still renders, so the command keeps answering "is it
// set?" — which for a credential is the entire useful signal.
//
// sessions.dsn is a Postgres connection string. Its schema comment says the
// DSN SHOULD omit the password (pgx resolves it from ~/.pgpass), but "should"
// is not "does", and this command's output is exactly the kind of thing pasted
// into an issue.
//
// This is an exact dotted-key match, so it can hide a scalar but cannot say
// "hide the values of this map" — map leaves are handled by policy in
// leafValue instead.
var redactedKeys = map[string]bool{
	"sessions.dsn": true,
}

// redactedPlaceholder stands in for a redacted value in both output modes.
const redactedPlaceholder = "(redacted)"

// configEntry is one leaf of config.Config as rendered by the walk: a dotted
// key, its value, and whether the config file actually set it.
type configEntry struct {
	Key string `json:"key"`
	// Group is the leaf's parent path — "net" for net.probe_host,
	// "review.gitea" for review.gitea.host, "" for the three host scalars. It
	// is what the human renderer emits section headers from.
	Group string `json:"group"`
	Value any    `json:"value"`
	Set   bool   `json:"set"`
	// Redacted means Value is NOT the stored value — it was withheld. Two
	// things set it: a redactedKeys hit (Value is the placeholder) and a
	// non-empty map leaf (Value is the key set, values dropped).
	Redacted bool `json:"redacted,omitempty"`

	// display is the human-readable rendering; not part of the JSON contract.
	display string
}

// hostResolvedView holds the two host scalars whose effective value is not
// their stored value: log_level "" means "off", and log_file "" means today's
// dated file under the config dir. The pre-fix command printed ONLY these
// resolved forms; the walk prints what the file says, so both survive rather
// than one replacing the other.
type hostResolvedView struct {
	LogLevel string `json:"log_level"`
	LogFile  string `json:"log_file"`
}

// launchResolvedView is the [launch] block's resolved posture: built-in
// fallbacks applied and the exec target resolved with launch's own precedence
// (FORGECTL_CLAUDE_BIN > binary_path > PATH). The generic walk cannot
// reproduce it — the walk reports what the file says, this reports what
// `forgectl launch` will actually do, which is why the bespoke block survives
// the rewrite.
type launchResolvedView struct {
	Harness string `json:"harness"`
	Model   string `json:"model"`
	// Effort is "" when no level resolved — the model is unmapped and no
	// config layer set one, so launch emits no --effort and Claude Code's own
	// default applies. omitempty keeps that distinct in JSON from a level.
	Effort         string `json:"effort,omitempty"`
	ApprovalPolicy string `json:"approval_policy,omitempty"`
	Sandbox        string `json:"sandbox,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
	AllowDanger    *bool  `json:"allow_danger,omitempty"`
	BinaryLabel    string `json:"binary_label"`
	BinaryPath     string `json:"binary_path"`
	Projects       int    `json:"projects"`
}

// configReport is the --json document. It is the stable surface: the human
// rendering is free to reflow, this is not.
type configReport struct {
	Path           string             `json:"path"`
	Found          bool               `json:"found"`
	PathError      string             `json:"path_error,omitempty"`
	DecodeError    string             `json:"decode_error,omitempty"`
	Unrecognized   []string           `json:"unrecognized"`
	Entries        []configEntry      `json:"entries"`
	HostResolved   hostResolvedView   `json:"host_resolved"`
	LaunchResolved launchResolvedView `json:"launch_resolved"`
}

func newConfigCmd(module.Deps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "config",
		Short:   "Show the active configuration and config file path",
		Aliases: []string{"cfg"},
		RunE: func(cmd *cobra.Command, args []string) error {
			slog.Debug("Preparing to display configuration.")
			out := cmd.OutOrStdout()

			// Describe, not deps.Cfg: the command needs the provenance Load
			// discards, and reading both would risk rendering one decode's
			// values against another's provenance.
			cfg, rep := config.Describe()
			entries := walkConfig(cfg, rep)
			hosts := resolveHostView(cfg)
			resolved := resolveLaunchView(cfg)

			if asJSON {
				return emitConfigJSON(out, entries, rep, hosts, resolved)
			}
			renderConfigText(out, entries, rep, hosts, resolved)

			slog.Info("Successfully displayed configuration.",
				"path", rep.Path, "found", rep.Found, "entries", len(entries),
				"unrecognized", len(rep.Unrecognized))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON to stdout (stable; the human rendering is not)")
	return cmd
}

// walkConfig reflects over cfg and returns one configEntry per leaf field, in
// declaration order, with provenance from rep.
//
// This is the one production use of reflect in forgectl — it appears elsewhere
// only in _test.go, so the convention is broken deliberately. The defect this
// command carried was a hand-maintained render list diverging from
// config.Config (1 of 11 sections printed), and init_cmd.go's initSections was
// a second live instance of the same class. A hand-written renderer fixes
// today's fields and re-arms the trap; a walk makes a new section AND a new
// field inside an existing section appear with no renderer change at all.
func walkConfig(cfg config.Config, rep config.Report) []configEntry {
	var out []configEntry
	walkStruct(reflect.ValueOf(cfg), "", rep, &out)
	return out
}

// walkStruct appends v's leaves to out, then recurses into its nested structs.
// Leaves before nested structs is not cosmetic: it keeps every direct leaf of
// a section under that section's own header, instead of stranding it after a
// subsection header it does not belong to ([launch] project vs
// [launch.defaults]).
func walkStruct(v reflect.Value, prefix string, rep config.Report, out *[]configEntry) {
	t := v.Type()
	var nested []int
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag, bound := tomlFieldName(f)
		if !bound {
			continue
		}
		key := tag
		if prefix != "" {
			key = prefix + "." + tag
		}
		fv := v.Field(i)
		if fv.Kind() == reflect.Struct {
			nested = append(nested, i)
			continue
		}
		display, value := leafValue(fv)
		entry := configEntry{
			Key:     key,
			Group:   prefix,
			Value:   value,
			Set:     rep.IsSet(key),
			display: display,
		}
		switch {
		case redactedKeys[key] && !fv.IsZero():
			entry.Value = redactedPlaceholder
			entry.display = redactedPlaceholder
			entry.Redacted = true
		case fv.Kind() == reflect.Map && fv.Len() > 0:
			// leafValue already dropped the values; say so, so a reader is
			// never left thinking the key set is the whole content.
			entry.Redacted = true
		}
		*out = append(*out, entry)
	}
	for _, i := range nested {
		key, bound := tomlFieldName(t.Field(i))
		if !bound {
			continue
		}
		if prefix != "" {
			key = prefix + "." + key
		}
		walkStruct(v.Field(i), key, rep, out)
	}
}

// tomlFieldName returns the config-file name the decoder binds f to, and
// whether f participates in the file at all.
//
// An empty `toml` tag is NOT "unbound": BurntSushi falls back to the Go field
// name (type_fields.go), matched exactly and then case-insensitively, so an
// untagged `Foo string` is settable as `Foo = "x"` and decodes — which also
// means it never shows up under "unrecognized keys". Skipping it here would
// recreate the invisible-key defect this command exists to remove, one level
// down. Only `toml:"-"` is genuinely out of the file.
//
// The fallback is the belt; TestConfig_EveryBindableFieldIsTagged is the
// suspenders. It has to be both: the fallback restores visibility but not
// provenance, because config.Report.IsSet keys on the file's own spelling.
func tomlFieldName(f reflect.StructField) (string, bool) {
	tag, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
	switch tag {
	case "-":
		return "", false
	case "":
		return f.Name, true
	default:
		return tag, true
	}
}

// redactedMapDisplay renders a map leaf as its key names alone — the shared
// rendering behind the keys-visible/values-withheld policy, used by leafValue's
// reflect.Map arm and by `forgectl launch which`. Callers pass keys already
// sorted; the ordering is the caller's to own because Go randomises map
// iteration and both surfaces must be reproducible.
//
// Note this is deliberately NOT redactedPlaceholder: that "(redacted)" stands
// in for a whole scalar (sessions.dsn), where even the shape is sensitive. A
// map's key names are the useful signal — which variables are injected — and
// only the values need withholding.
func redactedMapDisplay(keys []string) string {
	return "{" + strings.Join(keys, " ") + "}"
}

// leafValue renders one non-struct field for display and for --json. The three
// awkward shapes config.Config carries are handled explicitly:
//
//   - *bool (LaunchDefaults.AllowDanger) — a pointer precisely so an explicit
//     false is distinguishable from unset; nil renders "unset" / JSON null,
//     and must not collapse to false.
//   - map[string]string (LaunchDefaults.Env) — sorted KEYS ONLY, never
//     values. See the reflect.Map arm.
//   - []string ([docs] roots, [review] owners, [update] roster, …) — a
//     bracketed list; a nil slice still renders "[]" and JSON [], never null.
//
// A slice of structs ([[launch.project]]) is summarised by count: its elements
// are override blocks rather than values, and `forgectl launch which` is what
// renders them.
func leafValue(v reflect.Value) (display string, value any) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return "unset", nil
		}
		return leafValue(v.Elem())

	// A map leaf renders its sorted KEYS and never its values. The only one
	// config.Config carries is LaunchDefaults.Env — arbitrary environment
	// injected into the launched harness, and therefore the likeliest home in
	// this file for ANTHROPIC_API_KEY, GH_TOKEN, or the password-bearing
	// FORGECTL_SESSIONS_DSN that redactedKeys already hides one section over.
	// redactedKeys is an exact dotted-key match and cannot express "mask this
	// map's values", so the policy lives here and covers every map leaf,
	// including ones added later.
	//
	// Keys-only preserves exactly what the DSN redaction preserves: is it set,
	// and with what shape. Sorting is load-bearing beyond determinism — Go
	// randomises map iteration, so unsorted output would differ run to run.
	//
	// `forgectl launch which` renders the same map under the same policy,
	// through the same redactedMapDisplay helper — see its env row in
	// internal/cli/launch_which.go. Both surfaces get pasted into issues, and
	// --json makes this one machine-copyable.
	case reflect.Map:
		keys := make([]string, 0, v.Len())
		iter := v.MapRange()
		for iter.Next() {
			keys = append(keys, iter.Key().String())
		}
		sort.Strings(keys)
		return redactedMapDisplay(keys), keys

	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Struct {
			return fmt.Sprintf("%d configured", v.Len()), v.Len()
		}
		items := make([]string, 0, v.Len())
		for i := 0; i < v.Len(); i++ {
			items = append(items, fmt.Sprint(v.Index(i).Interface()))
		}
		return "[" + strings.Join(items, " ") + "]", items

	case reflect.String:
		if v.String() == "" {
			return `""`, ""
		}
		return v.String(), v.String()

	default:
		return fmt.Sprintf("%v", v.Interface()), v.Interface()
	}
}

// resolveHostView reproduces the pre-existing host-scalar resolution: the
// "off" fallback for an empty log_level and config.ResolvedLogPath for
// log_file.
func resolveHostView(cfg config.Config) hostResolvedView {
	level := cfg.LogLevel
	if level == "" {
		level = "off"
	}
	return hostResolvedView{
		LogLevel: level,
		LogFile:  config.ResolvedLogPath(cfg.LogFile),
	}
}

// resolveLaunchView reproduces the pre-existing bespoke [launch] block: the
// resolved defaults profile plus the exec target chosen by launch's own
// precedence. Kept verbatim in content — only its home moved.
func resolveLaunchView(cfg config.Config) launchResolvedView {
	ld := launch.DefaultsProfile(cfg.Launch)
	view := launchResolvedView{
		Harness:     ld.Harness,
		Model:       ld.Model,
		BinaryLabel: "launch.claude_bin",
		Projects:    len(cfg.Launch.Projects),
	}

	var bin string
	var err error
	if ld.Harness == "codex" {
		view.BinaryLabel = "launch.codex_bin"
		view.ApprovalPolicy = ld.ApprovalPolicy
		view.Sandbox = ld.Sandbox
		bin, err = launch.CodexPath(cfg.Launch.Defaults)
	} else {
		// Claude-only: --effort is Claude Code's flag and Codex has no
		// equivalent, so a Codex profile resolves a level it would never emit.
		view.Effort = ld.Effort
		view.PermissionMode = ld.PermissionMode
		allow := ld.AllowDanger
		view.AllowDanger = &allow
		bin, err = launch.ClaudePath(cfg.Launch.Defaults)
	}
	if err != nil {
		bin = fmt.Sprintf("(unresolved: %s)", err)
	}
	view.BinaryPath = bin
	return view
}

// renderConfigText writes the human rendering: the file's own status, then
// every leaf grouped under its section header, then the resolved [launch]
// posture, then any keys the file defines that bound to nothing.
//
// This is the terminal boundary, and the only place config data is altered for
// presentation. Every fragment sourced from the config file, the environment,
// or an OS error is escaped here so it cannot contribute terminal controls;
// code-authored keys, section headers, provenance markers, and layout are
// trusted and pass through untouched. The report data, the --json document,
// and slog attributes stay byte-faithful — escaping is a property of THIS
// writer, not of the values.
func renderConfigText(out io.Writer, entries []configEntry, rep config.Report, hosts hostResolvedView, resolved launchResolvedView) {
	switch {
	case rep.PathErr != nil:
		fmt.Fprintf(out, "config file: (unavailable: %s)\n", termsafe.SafeLine(rep.PathErr.Error()))
	case !rep.Found:
		fmt.Fprintf(out, "config file: %s (not found — using defaults)\n", termsafe.QuotePath(rep.Path))
	default:
		fmt.Fprintf(out, "config file: %s\n", termsafe.QuotePath(rep.Path))
	}
	if rep.DecodeErr != nil {
		fmt.Fprintf(out, "  ! decode error: %s\n", termsafe.SafeLine(rep.DecodeErr.Error()))
		fmt.Fprintf(out, "  ! values below reflect only what parsed before the error\n")
	}
	fmt.Fprintln(out)

	// The host scalars lead: walkStruct emits every leaf of a level before
	// recursing, so the root's own fields always precede the first section.
	n := 0
	for ; n < len(entries) && entries[n].Group == ""; n++ {
		writeEntry(out, entries[n])
	}
	fmt.Fprintf(out, "\nhost scalars resolved (built-in fallbacks applied)\n")
	fmt.Fprintf(out, "  log_level  %s\n", termsafe.SafeLine(hosts.LogLevel))
	fmt.Fprintf(out, "  log_file   %s\n", termsafe.QuotePath(hosts.LogFile))

	// lastTop tracks the top-level section so its header prints exactly once;
	// lastGroup tracks the nested path so [review.gitea] gets its own.
	lastTop, lastGroup := "\x00", "\x00"
	for _, e := range entries[n:] {
		top, _, _ := strings.Cut(e.Group, ".")
		if top != lastTop {
			fmt.Fprintf(out, "\n[%s]\n", top)
			lastTop, lastGroup = top, top
		}
		if e.Group != lastGroup {
			fmt.Fprintf(out, "\n  [%s]\n", e.Group)
			lastGroup = e.Group
		}
		writeEntry(out, e)
	}

	fmt.Fprintf(out, "\n[launch] resolved (built-in fallbacks and binary precedence applied)\n")
	fmt.Fprintf(out, "  launch.harness       %s\n", termsafe.SafeLine(resolved.Harness))
	fmt.Fprintf(out, "  launch.model         %s\n", termsafe.SafeLine(resolved.Model))
	if resolved.Harness == "codex" {
		fmt.Fprintf(out, "  launch.approval      %s\n", termsafe.SafeLine(resolved.ApprovalPolicy))
		fmt.Fprintf(out, "  launch.sandbox       %s\n", termsafe.SafeLine(resolved.Sandbox))
	} else {
		// Spelled out rather than left blank: this block is a fixed field list,
		// so an empty column would read as "unset" when the operative fact is
		// that no flag is emitted and Claude Code's own default governs.
		effort := resolved.Effort
		if effort == "" {
			effort = "(none — claude default)"
		}
		fmt.Fprintf(out, "  launch.effort        %s\n", termsafe.SafeLine(effort))
		fmt.Fprintf(out, "  launch.permission    %s\n", termsafe.SafeLine(resolved.PermissionMode))
		fmt.Fprintf(out, "  launch.allow_danger  %v\n", resolved.AllowDanger != nil && *resolved.AllowDanger)
	}
	// SafeLine, not QuotePath: resolveLaunchView overloads BinaryPath with
	// "(unresolved: <error>)" when resolution fails, so it is not always a path.
	fmt.Fprintf(out, "  %-20s %s\n", resolved.BinaryLabel, termsafe.SafeLine(resolved.BinaryPath))
	fmt.Fprintf(out, "  launch.projects      %d configured\n", resolved.Projects)

	if len(rep.Unrecognized) > 0 {
		fmt.Fprintf(out, "\nunrecognized keys (present in the file, bound to nothing — check spelling and section):\n")
		for _, k := range rep.Unrecognized {
			fmt.Fprintf(out, "  %s\n", termsafe.SafeLine(k))
		}
	}
}

// writeEntry renders one leaf: dotted key, value, provenance marker. The key
// column fits the longest key config.Config carries today
// (launch.defaults.codex_binary_path); a longer one simply pushes its own row.
//
// Only display is escaped. The key is code-authored — it comes from the struct
// tags walkStruct reads — so escaping it would be escaping our own text.
func writeEntry(out io.Writer, e configEntry) {
	fmt.Fprintf(out, "  %-34s %-28s %s\n", e.Key, termsafe.SafeLine(e.display), provenance(e.Set))
}

// provenance renders the set/default marker that distinguishes "the file asked
// for this" from "this is a built-in fallback" — the distinction the whole
// command turns on.
func provenance(set bool) string {
	if set {
		return "(set)"
	}
	return "(default)"
}

// emitConfigJSON writes the stable machine-readable document.
func emitConfigJSON(out io.Writer, entries []configEntry, rep config.Report, hosts hostResolvedView, resolved launchResolvedView) error {
	doc := configReport{
		Path:           rep.Path,
		Found:          rep.Found,
		Unrecognized:   rep.Unrecognized,
		Entries:        entries,
		HostResolved:   hosts,
		LaunchResolved: resolved,
	}
	if doc.Unrecognized == nil {
		doc.Unrecognized = []string{}
	}
	if rep.PathErr != nil {
		doc.PathError = rep.PathErr.Error()
	}
	if rep.DecodeErr != nil {
		doc.DecodeError = rep.DecodeErr.Error()
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}
