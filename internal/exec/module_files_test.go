package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// modulePath is this module's path; a package go list reports under any other
// module (or none, for the standard library) is out of scope.
const modulePath = "github.com/cameronsjo/forgectl"

// moduleFile is one file the go tool compiles into some package of this
// module, by its path relative to the module root (slash-separated) and its
// absolute path.
type moduleFile struct{ rel, abs string }

var (
	compiledOnce  sync.Once
	compiledFiles []moduleFile
	compiledErr   error
)

// moduleCompiledFiles is every file of this module the go tool compiles for
// any platform in guardPlatforms, with cgo off and on, tests included: each
// file `go list -deps -test -json ./...` reports for a package of this
// module, in every compiled-file list it has. It asks the go tool rather than
// walking the tree because the go tool is what decides: a package under a
// dot-directory, under testdata or behind a symlinked directory is outside
// ./... yet compiles the moment something imports it by path, and -deps
// follows that import (forgectl#854). There is nothing to skip. A package
// counts as this module's when its module lives in the tree (goListModule's
// inTree), and every go command runs under pinnedGoEnv. The list is computed
// once per test binary, its go list runs concurrently.
//
// It fails, never skips, when the go tool cannot be run: a guard that goes
// quiet when its input is missing is a green light with no bulb.
func moduleCompiledFiles(t *testing.T) []moduleFile {
	t.Helper()
	compiledOnce.Do(func() { compiledFiles, compiledErr = listCompiledFiles(context.Background()) })
	if compiledErr != nil {
		t.Fatalf("list the module's compiled files: %v", compiledErr)
	}
	return compiledFiles
}

// goListPackage is the part of `go list -json` output moduleCompiledFiles reads.
type goListPackage struct {
	ImportPath string
	Name       string
	Dir        string
	ForTest    string
	Module     *goListModule

	GoFiles, CgoFiles, TestGoFiles, XTestGoFiles        []string
	SFiles, SysoFiles, CFiles, CXXFiles, MFiles, HFiles []string
	FFiles, SwigFiles, SwigCXXFiles                     []string
}

// goListModule is the part of a package's module go list reports.
type goListModule struct {
	Path    string
	Dir     string
	Replace *struct{ Dir string }
}

// inTree reports whether m's source lives under root: this module, or any
// module whose directory (or local replacement directory) sits inside the
// tree, such as a nested module pulled in by a local replace. Keying on the
// directory rather than the module path is what keeps such a module from
// escaping the scan under a path of its own (forgectl#854).
func (m *goListModule) inTree(root string) bool {
	if m == nil {
		return false // the standard library
	}
	if m.Path == modulePath {
		return true
	}
	dirs := []string{m.Dir}
	if m.Replace != nil {
		dirs = append(dirs, m.Replace.Dir)
	}
	return slices.ContainsFunc(dirs, func(dir string) bool { return dir != "" && underRoot(root, dir) })
}

// underRoot reports whether path is root or lies below it.
func underRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// pinnedGoEnv is the environment every go command the guards run gets on top
// of the ambient one, so nothing ambient can change what the go tool reports.
// An ambient GOFLAGS=-tags=x would hide every file tagged !x from go list, and
// with it from both guards; a go.work could swap in other modules; a
// GOTOOLCHAIN switch could run another go. GOENV=off keeps a user go.env
// file from injecting any of those back.
var pinnedGoEnv = []string{"GOFLAGS=", "GOWORK=off", "GO111MODULE=on", "GOTOOLCHAIN=local", "GOENV=off"}

func (p goListPackage) files() []string {
	return slices.Concat(p.GoFiles, p.CgoFiles, p.TestGoFiles, p.XTestGoFiles,
		p.SFiles, p.SysoFiles, p.CFiles, p.CXXFiles, p.MFiles, p.HFiles,
		p.FFiles, p.SwigFiles, p.SwigCXXFiles)
}

// isTestMain reports whether p is the main package go test synthesizes to run
// a package's tests. Its one file is generated into the build cache, outside
// the module, so it is not this module's source.
func (p goListPackage) isTestMain() bool {
	return p.Name == "main" && p.ForTest == "" && strings.HasSuffix(p.ImportPath, ".test")
}

func listCompiledFiles(ctx context.Context) ([]moduleFile, error) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		return nil, err
	}
	if _, err := osexec.LookPath("go"); err != nil {
		return nil, fmt.Errorf("the go tool is not on PATH, and it is the only source of the compiled file set: %w", err)
	}
	// One go list per platform and cgo setting, run concurrently.
	type run struct {
		p   guardPlatform
		cgo string
		out []byte
		err error
	}
	var runs []*run
	for _, p := range guardPlatforms {
		for _, cgo := range []string{"0", "1"} {
			runs = append(runs, &run{p: p, cgo: cgo})
		}
	}
	var wg sync.WaitGroup
	for _, r := range runs {
		wg.Go(func() {
			cmd := osexec.CommandContext(ctx, "go", "list", "-deps", "-test", "-json", "./...")
			cmd.Dir = root
			cmd.Env = append(append(os.Environ(), pinnedGoEnv...), "GOOS="+r.p.goos, "GOARCH="+r.p.goarch, "CGO_ENABLED="+r.cgo)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			r.out, r.err = cmd.Output()
			if r.err != nil {
				r.err = fmt.Errorf("go list for %s cgo=%s: %w\n%s", r.p, r.cgo, r.err, stderr.String())
			}
		})
	}
	wg.Wait()
	seen := map[string]moduleFile{}
	for _, r := range runs {
		if r.err != nil {
			return nil, r.err
		}
		dec := json.NewDecoder(bytes.NewReader(r.out))
		for {
			var pkg goListPackage
			if err := dec.Decode(&pkg); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return nil, fmt.Errorf("decode go list for %s cgo=%s: %w", r.p, r.cgo, err)
			}
			if !pkg.Module.inTree(root) {
				continue
			}
			for _, name := range pkg.files() {
				abs := name
				if !filepath.IsAbs(abs) {
					abs = filepath.Join(pkg.Dir, name)
				}
				if !underRoot(root, abs) {
					if pkg.isTestMain() {
						continue
					}
					return nil, fmt.Errorf("go list places %s of package %s outside the module root", abs, pkg.ImportPath)
				}
				rel, err := filepath.Rel(root, abs)
				if err != nil {
					return nil, err
				}
				seen[abs] = moduleFile{rel: filepath.ToSlash(rel), abs: abs}
			}
		}
	}
	files := make([]moduleFile, 0, len(seen))
	for _, f := range seen {
		files = append(files, f)
	}
	slices.SortFunc(files, func(a, b moduleFile) int { return strings.Compare(a.rel, b.rel) })
	return files, nil
}

// unsafeAllowed is the allowlist of files, relative to the module root, that
// may import "unsafe". It is empty: nothing in the module imports it today
// (forgectl#854), and a new entry must name why the file needs to read memory
// past the type system.
var unsafeAllowed = map[string]string{}

// cgoAllowed is the allowlist of files, relative to the module root, that may
// import "C". It is empty: no file uses cgo today, and every release build
// runs with cgo off, so a cgo file is code no guard type-checks.
var cgoAllowed = map[string]string{}

// hiddenSourceAllowed is the allowlist of Go or assembly source files that may
// sit under a dot-directory or a testdata directory, relative to the module
// root. It is empty: the module has no such fixtures today.
var hiddenSourceAllowed = map[string]bool{}

// TestNoFileReachesPastTheTypeSystem closes the hole the guard tests above
// cannot see (forgectl#854, P-1): every one of them reasons about what Go's
// type system lets a file name, and four things step around it. A
// //go:linkname directive binds a local declaration to any symbol in any
// package, unexported or internal, so another package could call
// (*OSSensitiveRunner).buildCmd and read the argv it assembles; a probe did,
// and got the payload back while every other guard stayed green. An "unsafe"
// import reads any memory, including a sealed Arg's closure, and it is also
// what the compiler requires before it honours a linkname. A cgo file runs C
// with the same reach. An assembly, C or object file links in code that names
// any symbol directly. So, in every file the go tool compiles into this
// module (moduleCompiledFiles), for every guard platform, cgo off and on,
// tests included:
//
//   - no //go:linkname directive, anywhere, a leading byte-order mark or not;
//   - no "unsafe" import outside unsafeAllowed;
//   - no "C" import outside cgoAllowed;
//   - no compiled file that is not Go source.
//
// This is the interim guard; sealing the payload behind a package boundary
// (forgectl#854) does not retire it, because a linkname reaches into an
// internal package too.
//
// Mutations that turn it red, each with a package that imports it: a file
// with `import _ "unsafe"` and `//go:linkname buildCmd
// github.com/cameronsjo/forgectl/internal/exec.(*OSSensitiveRunner).buildCmd`
// placed in internal/, in internal/exec/testdata/, in a dot-directory, or
// behind a symlinked directory (add it to unsafeAllowed and the linkname rule
// still fires); the same file tagged //go:build !zztag, run under an ambient
// GOFLAGS=-tags=zztag; the same file in a nested module .zzmod pulled in by a
// local replace; a new file importing "unsafe" alone; a file importing "C".
func TestNoFileReachesPastTheTypeSystem(t *testing.T) {
	fset := token.NewFileSet()
	parsed, sawExec := 0, false
	var findings []string
	report := func(pos token.Pos, msg string) {
		findings = append(findings, fset.Position(pos).String()+": "+msg)
	}
	for _, f := range moduleCompiledFiles(t) {
		if filepath.Ext(f.rel) != ".go" {
			findings = append(findings, f.rel+": a compiled assembly, C or object file can name any symbol past the type system; forgectl ships none")
			continue
		}
		src, err := os.ReadFile(filepath.Clean(f.abs))
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f.rel, src, parser.ImportsOnly|parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		parsed++
		sawExec = sawExec || filepath.ToSlash(filepath.Dir(f.rel)) == "internal/exec"
		for _, imp := range file.Imports {
			switch p, _ := strconv.Unquote(imp.Path.Value); {
			case p == "unsafe" && unsafeAllowed[f.rel] == "":
				report(imp.Pos(), `imports "unsafe", which reads any memory, a sealed payload included; add the file to unsafeAllowed with a reason only after review`)
			case p == "C" && cgoAllowed[f.rel] == "":
				report(imp.Pos(), `imports "C"; cgo code runs past the type system and no guard type-checks it; add the file to cgoAllowed with a reason only after review`)
			}
		}
		for _, line := range linknameLines(src) {
			findings = append(findings, f.rel+":"+strconv.Itoa(line)+": a //go:linkname directive binds to any symbol, unexported or internal, so it can call buildCmd and read a payload")
		}
	}
	for _, f := range findings {
		t.Error(f)
	}
	if parsed == 0 || !sawExec {
		t.Fatalf("parsed %d Go files (internal/exec among them: %v); the file set is broken, not the module clean", parsed, sawExec)
	}
}

// linknameLines returns the 1-based line of every //go:linkname directive in
// src. ImportsOnly parsing stops at the imports, so this scans the source
// text, where a linkname sits beside a declaration further down. A leading
// byte-order mark is dropped first: the compiler ignores one at the start of
// a file, so a directive on line 1 behind it is live.
func linknameLines(src []byte) []int {
	src = bytes.TrimPrefix(src, []byte("\uFEFF"))
	var lines []int
	for i, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//go:linkname") {
			lines = append(lines, i+1)
		}
	}
	return lines
}

// TestLinknameLinesSeesPastAByteOrderMark pins the directive scan's edge
// cases: a directive behind a leading byte-order mark is found, as is an
// indented one on a later line; a line that merely mentions it is not.
//
// Mutation that turns it red: drop the TrimPrefix (the BOM row finds nothing).
func TestLinknameLinesSeesPastAByteOrderMark(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []int
	}{
		{"behind a BOM on line 1", "\uFEFF//go:linkname f p.f\npackage x\n", []int{1}},
		{"indented, later", "package x\n\n\t//go:linkname f p.f\n", []int{3}},
		{"mentioned in prose", "package x\n// see //go:linkname\n", nil},
	}
	for _, tt := range tests {
		if got := linknameLines([]byte(tt.src)); !slices.Equal(got, tt.want) {
			t.Errorf("%s: linknameLines = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestModuleTreeHidesNoGoSource is the cheap second layer under the go-list
// file set: it walks the module tree and refuses the three places where Go
// source can sit outside ./... yet compile once imported by path, so a hidden
// package is refused even before anything imports it (forgectl#854):
//
//   - a symlink to a directory (or a symlink that does not resolve inside the
//     module), anywhere;
//   - a .go, .s or .S file under a directory whose name starts with "." or is
//     testdata, unless hiddenSourceAllowed names it.
//
// A directory holding its own go.mod is another module (a nested worktree,
// say) and is not descended into. That skip is safe only because nothing can
// pull such a module into this build: go list runs with GOWORK=off, and
// TestGoModPullsInNoLocalModule refuses a local replace and any go.work; a
// nested module that did get in would still be scanned by the go-list layer,
// which keys on where a module lives, not on its path. A go.work file
// anywhere in the walked tree is refused here too.
//
// Mutations that turn it red: a .go file in internal/exec/testdata/zz/; a
// .go file in .zzprobe/zz/; a symlink internal/zzlink pointing at a directory.
func TestModuleTreeHidesNoGoSource(t *testing.T) {
	rootDir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()
	var findings []string
	walked := 0
	err = fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		walked++
		if entry.Type()&fs.ModeSymlink != 0 {
			info, statErr := fs.Stat(fsys, path)
			switch {
			case statErr != nil:
				findings = append(findings, path+": a symlink that does not resolve inside the module; the go tool may still follow it")
			case info.IsDir():
				findings = append(findings, path+": a symlinked directory; the go tool compiles a package behind it when imported by path, outside ./...")
			}
			return nil
		}
		if entry.IsDir() {
			if path != "." {
				if _, statErr := fs.Stat(fsys, path+"/go.mod"); statErr == nil {
					return fs.SkipDir // another module
				}
			}
			return nil
		}
		if entry.Name() == "go.work" {
			findings = append(findings, path+": a go.work can pull another module's source into this build; forgectl uses none")
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".s", ".S":
		default:
			return nil
		}
		hidden := slices.ContainsFunc(strings.Split(filepath.ToSlash(filepath.Dir(path)), "/"), func(dir string) bool {
			return dir == "testdata" || (strings.HasPrefix(dir, ".") && dir != ".")
		})
		if hidden && !hiddenSourceAllowed[path] {
			findings = append(findings, path+": source under a dot-directory or testdata compiles when imported by path, yet ./... never lists it; move it, or add it to hiddenSourceAllowed after review")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
	if walked < 2 {
		t.Fatal("walked nothing; the walk is broken, not the module clean")
	}
}

// localReplaceAllowed is the allowlist of module paths go.mod may replace with
// a local directory. It is empty: go.mod has no replace directive today.
var localReplaceAllowed = map[string]string{}

// TestGoModPullsInNoLocalModule refuses the two ways another module's source
// in this tree could enter the build unseen (forgectl#854): a go.mod replace
// whose target is a local directory (one with no version; go mod edit -json
// reports such a target with an empty Version), outside localReplaceAllowed,
// and a go.work at the module root. The tree walk does not descend into a
// nested module, so without this test such a module would be scanned only by
// the go-list layer.
//
// Mutation that turns it red: `replace example.com/zzmod => ./.zzmod` in
// go.mod, or an empty go.work at the root.
func TestGoModPullsInNoLocalModule(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := osexec.CommandContext(t.Context(), "go", "mod", "edit", "-json") //nolint:gosec // G204: a fixed go command with literal arguments; nothing is tainted
	cmd.Dir = root
	cmd.Env = append(os.Environ(), pinnedGoEnv...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go mod edit -json: %v", err)
	}
	var mod struct {
		Module  struct{ Path string }
		Replace []struct {
			Old struct{ Path string }
			New struct{ Path, Version string }
		}
	}
	if err := json.Unmarshal(out, &mod); err != nil {
		t.Fatal(err)
	}
	if mod.Module.Path != modulePath {
		t.Fatalf("go mod edit -json reports module %q, want %q; the parse is broken", mod.Module.Path, modulePath)
	}
	for _, r := range mod.Replace {
		if r.New.Version == "" && localReplaceAllowed[r.Old.Path] == "" {
			t.Errorf("go.mod replaces %s with the local directory %s; a local module's source enters the build under its own path, so add it to localReplaceAllowed only after review", r.Old.Path, r.New.Path)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "go.work")); err == nil {
		t.Error("go.work exists at the module root; a workspace can pull other modules' source into the build")
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
}
