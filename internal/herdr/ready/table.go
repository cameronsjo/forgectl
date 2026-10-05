package ready

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

//go:embed predicates.toml
var defaultPredicates []byte

// tableVersion is the predicate file format this build reads.
const tableVersion = 1

// maxTableBytes caps an override file. The default table is a few KiB.
const maxTableBytes = 64 << 10

// ErrTable reports a predicate table that cannot be used.
var ErrTable = errors.New("ready: predicate table")

// Table holds the compiled predicates for every known harness.
type Table struct {
	blocking  []screen
	harnesses map[string]harness
}

type harness struct {
	agent       string
	prompt      *regexp.Regexp
	promptFirst bool
	placeholder *regexp.Regexp
	blocking    []screen
}

type screen struct {
	name string
	any  []*regexp.Regexp
}

func (s screen) matches(text string) bool {
	for _, re := range s.any {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

type fileScreen struct {
	Name string   `toml:"name"`
	Any  []string `toml:"any"`
}

type fileHarness struct {
	Agent       string       `toml:"agent"`
	Prompt      string       `toml:"prompt"`
	PromptFirst bool         `toml:"prompt_first"`
	Placeholder string       `toml:"placeholder"`
	Blocking    []fileScreen `toml:"blocking"`
}

type fileTable struct {
	Version  int                    `toml:"version"`
	Blocking []fileScreen           `toml:"blocking"`
	Harness  map[string]fileHarness `toml:"harness"`
}

// Default returns the table built into forgectl.
func Default() (*Table, error) {
	return Parse(defaultPredicates)
}

// Load returns the table at path when that file exists, and the built-in
// table when it does not. path must be inside forgectl's own config
// directory; the caller resolves it, never from a worktree or repo, so a
// branch cannot mark its own trust dialog as ready.
//
// The file replaces the built-in table whole. It must be a regular file (not
// a symlink), not writable by group or others, and at most 64 KiB.
func Load(path string) (*Table, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default()
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrTable, path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file (mode %s)", ErrTable, path, info.Mode().Type())
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%w: %s is writable by group or others (mode %s); chmod go-w it", ErrTable, path, info.Mode().Perm())
	}
	if info.Size() > maxTableBytes {
		return nil, fmt.Errorf("%w: %s is %d bytes, the limit is %d", ErrTable, path, info.Size(), maxTableBytes)
	}
	f, err := os.Open(path) //nolint:gosec // G304: path is forgectl's own config-dir override, Lstat-checked above as a regular, non-group-writable file
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrTable, path, err)
	}
	defer func() { _ = f.Close() }() // read-only; a close error loses nothing
	// The open follows symlinks, so prove it opened the file Lstat checked: a
	// swap between the two would otherwise read whatever the link points at.
	opened, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrTable, path, err)
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("%w: %s changed between check and open", ErrTable, path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxTableBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrTable, path, err)
	}
	if len(data) > maxTableBytes {
		return nil, fmt.Errorf("%w: %s grew past %d bytes while being read", ErrTable, path, maxTableBytes)
	}
	t, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

// Parse compiles a predicate table. It refuses an unknown version, unknown
// keys, a harness without an agent or prompt, a screen without a name or
// patterns, and any pattern that does not compile.
func Parse(data []byte) (*Table, error) {
	var f fileTable
	meta, err := toml.Decode(string(data), &f)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTable, err)
	}
	if extra := meta.Undecoded(); len(extra) > 0 {
		keys := make([]string, len(extra))
		for i, k := range extra {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("%w: unknown keys %s", ErrTable, strings.Join(keys, ", "))
	}
	if f.Version != tableVersion {
		return nil, fmt.Errorf("%w: version %d, this build reads %d", ErrTable, f.Version, tableVersion)
	}
	if len(f.Harness) == 0 {
		return nil, fmt.Errorf("%w: no [harness.*] tables", ErrTable)
	}
	t := &Table{harnesses: make(map[string]harness, len(f.Harness))}
	if t.blocking, err = compileScreens("blocking", f.Blocking); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(f.Harness))
	for name := range f.Harness {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fh := f.Harness[name]
		if fh.Agent == "" {
			return nil, fmt.Errorf("%w: harness %q has no agent", ErrTable, name)
		}
		if fh.Prompt == "" {
			return nil, fmt.Errorf("%w: harness %q has no prompt pattern", ErrTable, name)
		}
		prompt, err := regexp.Compile(fh.Prompt)
		if err != nil {
			return nil, fmt.Errorf("%w: harness %q prompt: %w", ErrTable, name, err)
		}
		blocking, err := compileScreens("harness."+name+".blocking", fh.Blocking)
		if err != nil {
			return nil, err
		}
		h := harness{agent: fh.Agent, prompt: prompt, promptFirst: fh.PromptFirst, blocking: blocking}
		if fh.Placeholder != "" {
			if h.placeholder, err = regexp.Compile(fh.Placeholder); err != nil {
				return nil, fmt.Errorf("%w: harness %q placeholder: %w", ErrTable, name, err)
			}
		}
		t.harnesses[name] = h
	}
	return t, nil
}

func compileScreens(where string, in []fileScreen) ([]screen, error) {
	out := make([]screen, 0, len(in))
	for i, s := range in {
		if s.Name == "" {
			return nil, fmt.Errorf("%w: %s[%d] has no name", ErrTable, where, i)
		}
		if len(s.Any) == 0 {
			return nil, fmt.Errorf("%w: %s %q has no patterns", ErrTable, where, s.Name)
		}
		sc := screen{name: s.Name, any: make([]*regexp.Regexp, 0, len(s.Any))}
		for _, p := range s.Any {
			re, err := regexp.Compile(p)
			if err != nil {
				return nil, fmt.Errorf("%w: %s %q: %w", ErrTable, where, s.Name, err)
			}
			sc.any = append(sc.any, re)
		}
		out = append(out, sc)
	}
	return out, nil
}
