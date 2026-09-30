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
// follows that import (forgectl#854). There is nothing to skip. The list is
// computed once per test binary.
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
	Module     *struct{ Path string }

	GoFiles, CgoFiles, TestGoFiles, XTestGoFiles        []string
	SFiles, SysoFiles, CFiles, CXXFiles, MFiles, HFiles []string
	FFiles, SwigFiles, SwigCXXFiles                     []string
}

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
	seen := map[string]moduleFile{}
	for _, p := range guardPlatforms {
		for _, cgo := range []string{"0", "1"} {
			cmd := osexec.CommandContext(ctx, "go", "list", "-deps", "-test", "-json", "./...")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GOOS="+p.goos, "GOARCH="+p.goarch, "CGO_ENABLED="+cgo)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				return nil, fmt.Errorf("go list for %s cgo=%s: %w\n%s", p, cgo, err, stderr.String())
			}
			dec := json.NewDecoder(bytes.NewReader(out))
			for {
				var pkg goListPackage
				if err := dec.Decode(&pkg); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					return nil, fmt.Errorf("decode go list for %s cgo=%s: %w", p, cgo, err)
				}
				if pkg.Module == nil || pkg.Module.Path != modulePath {
					continue
				}
				for _, name := range pkg.files() {
					abs := name
					if !filepath.IsAbs(abs) {
						abs = filepath.Join(pkg.Dir, name)
					}
					rel, err := filepath.Rel(root, abs)
					if err != nil {
						return nil, err
					}
					if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						if pkg.isTestMain() {
							continue
						}
						return nil, fmt.Errorf("go list places %s of package %s outside the module root", abs, pkg.ImportPath)
					}
					seen[abs] = moduleFile{rel: filepath.ToSlash(rel), abs: abs}
				}
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
// still fires); a new file importing "unsafe" alone; a file importing "C".
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
// say), which this module's build never compiles, and is not descended into.
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
