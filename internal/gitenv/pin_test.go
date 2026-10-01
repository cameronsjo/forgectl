package gitenv_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

const (
	modulePath       = "github.com/cameronsjo/forgectl"
	gitenvImportPath = modulePath + "/internal/gitenv"
)

// skippedPackages are production directories the pin does not scan, each with
// its reason.
var skippedPackages = map[string]string{
	"internal/gitenv":            "the helper itself: its Command is the one place git is named to os/exec",
	"internal/gitenv/gitenvtest": "test support: AssertLive runs an unhardened git on purpose, to prove the canary can fire, and only tests import it",
	"internal/redact/redacttest": "test support: a corpus of argv shapes, git's among them, that redaction tests render; nothing in it runs",
}

// gitConstantAllowlist is every function that may hold the constant string
// "git" in an expression, keyed "pkgdir/file.go:Func", with the exact count
// and the reason. None of them runs it.
var gitConstantAllowlist = map[string]struct {
	uses   int
	reason string
}{
	"internal/cli/status.go:var": {1, "statusSectionLabels: the status report's section label, printed by both human views, never run"},
}

// transportAllowlist is every function that may run git under
// gitenv.Transport, which keeps the operator's transports, with the exact
// count and the reason it needs them.
var transportAllowlist = map[string]struct {
	uses   int
	reason string
}{
	"internal/sandbox/sandbox.go:Sandbox":           {2, "clones the repository under review, or adds a worktree from the operator's own repository, which may be a partial clone that fetches what the checkout needs"},
	"internal/projects/gitea.go:cloneFromGitea":     {1, "clones over SSH; ext:: and fd:: stay refused at the call"},
	"internal/projects/gitea.go:cloneBareFromURL":   {1, "bare-clones over SSH; ext:: and fd:: stay refused at the call"},
	"internal/projects/worktree.go:Client.Worktree": {1, "fetches origin into the bare clone it just made"},
	"internal/projects/worktree.go:defaultBranch":   {1, "`git remote show origin` asks the remote for its default branch"},
	"internal/projects/pull.go:Client.PullAll":      {1, "`git pull --rebase` in the operator's own checkouts"},
	"internal/branch/branch.go:Client.deleteRemote": {1, "`git push --delete` removes the branch on the remote"},
	"internal/projects/github.go:cloneRepo":         {1, "gitenv.Unset(Transport): the variables gh's environment loses, since gh runs git clone itself (ghGitAllowlist)"},
	"internal/projects/github.go:cloneBareRepo":     {1, "as cloneRepo"},
}

// ghGitAllowlist is every function that may start gh with a subcommand that
// runs git itself (ghGitSubcommands), outside internal/gitenv, with the exact
// count and how that git is hardened instead (#978).
var ghGitAllowlist = map[string]struct {
	uses   int
	reason string
}{
	"internal/projects/github.go:cloneRepo":     {1, "`gh repo clone` keeps gh's credential handling; gh's environment loses gitenv.Unset(Transport), so GIT_WORK_TREE and GIT_INDEX_FILE cannot redirect its git clone. core.fsmonitor is not pinned: gh forwards it as `git clone -c`, which persists it in the new repository"},
	"internal/projects/github.go:cloneBareRepo": {1, "as cloneRepo, for the bare clone"},
}

// shellScriptAllowlist is every function that may start a shell with a
// script that is not a constant, which the pin cannot read for git, with the
// exact count and the reason.
var shellScriptAllowlist = map[string]struct {
	uses   int
	reason string
}{
	"internal/bless/bless.go:InstallAnchor": {1, "anchorInstallScript is a package var built at init from the AnchorPath constant and filepath.Dir of it, so it is not a Go constant; it runs mkdir, chown, chmod and printf, and no git"},
}

// ghGitSubcommands are the gh arguments that make gh run git: repo clone,
// repo fork --clone, gist clone, pr checkout, repo sync.
var ghGitSubcommands = map[string]bool{"clone": true, "--clone": true, "checkout": true, "sync": true}

// shells are executables whose arguments are a script, which can run git.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "cmd": true, "cmd.exe": true, "powershell": true, "powershell.exe": true, "pwsh": true, "pwsh.exe": true}

// gitWord matches git as a word in a script: `git status`, `cd x && git`,
// not github or digit.
var gitWord = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])git([^A-Za-z0-9_-]|$)`)

// execNamePos is, for each function or method name that starts a process,
// the index of the argument that names the executable.
var execNamePos = map[string]int{
	"Command":             0, // os/exec
	"CommandContext":      1, // os/exec
	"StartProcess":        0, // os
	"Exec":                0, // syscall
	"Run":                 1, // internal/exec Runner
	"RunInteractive":      1,
	"RunDiscardingStdout": 1,
	"RunWithInput":        2,
	"RunWithEnv":          2,
	"RunWithEnvFiltered":  3,
	"RunStreaming":        4,
}

// scriptFlags are the shell options whose next argument is the script:
// sh -c, cmd /c and /k, PowerShell -Command and -EncodedCommand.
var scriptFlags = map[string]bool{"-c": true, "/c": true, "/C": true, "/k": true, "/K": true, "-Command": true, "-command": true, "-EncodedCommand": true}

// gitBinName matches the name of a variable, field or method that holds the
// git executable: gitBin, c.gitBinary(), gitExe.
var gitBinName = regexp.MustCompile(`(?i)^git_?(bin|binary|exe)$`)

// gitLike matches the source of a non-constant executable name that is git's
// by its own name: gitBin, c.gitBinary(), gitPath.
var gitLike = regexp.MustCompile(`(?i)git`)

// finding is one violation or counted use, keyed by its enclosing function.
type finding struct {
	pos, fn, kind string
}

// TestProductionGitGoesThroughGitenv is #944's pin: no production code runs
// git except through internal/gitenv, and only the allowlisted functions use
// its Transport profile. It type-checks each production package for linux,
// darwin and windows (imports are empty stand-ins; only constants need to
// resolve) and flags:
//
//   - a process start (os/exec Command/CommandContext, os.StartProcess,
//     syscall.Exec, or a Runner method) whose executable is a constant that
//     names git ("git", "/usr/bin/git") or an expression spelled with "git"
//     (gitBin, c.gitBinary());
//   - a process start whose executable is held in a variable that an
//     exec.LookPath of git (or anything derived from it) set in the same
//     function, whatever the variable is called;
//   - a shell (sh, bash, cmd, …), started directly or behind a wrapper
//     (`sudo sh -c`), with a constant script that runs git as a word
//     (`sh -c "git status"`), or with a script that is not a constant,
//     outside shellScriptAllowlist;
//   - a process start that hands the git executable (gitenv.Bin, a git
//     path, gitBin, a LookPath of git) to the program it starts as an
//     argument: `env X=1 git`, `sh -c 'exec "$0"' git`;
//   - gh started with a subcommand that runs git itself (clone, checkout,
//     sync) outside ghGitAllowlist, or over its count;
//   - any other constant expression equal to "git" outside
//     gitConstantAllowlist, so `name := "git"` ahead of a Run is caught
//     where its use would not be;
//   - a use of gitenv.Transport outside transportAllowlist, or over its
//     count.
//
// Mutations that turn it red: `c.run.Run(ctx, "git", "status")` anywhere in
// internal/; `run.Run(ctx, gitBin, ...)` in projects' gitStatus;
// `exec.CommandContext(ctx, "git", ...)` in internal/env; switching
// pr/session.go's rev-parse to gitenv.Transport; deleting a transportAllowlist
// entry (its use is then unlisted) or a use behind it (the entry goes stale).
func TestProductionGitGoesThroughGitenv(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := productionDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	var all []finding
	scanned := map[string]bool{}
	for _, rel := range dirs {
		dir := filepath.Join(root, filepath.FromSlash(rel))
		for _, goos := range []string{"linux", "darwin", "windows"} {
			ctx := build.Default
			ctx.GOOS = goos
			bp, err := ctx.ImportDir(dir, 0)
			var noGo *build.NoGoError
			if errors.As(err, &noGo) {
				continue
			}
			if err != nil {
				t.Fatalf("list %s for %s: %v", rel, goos, err)
			}
			scanned[rel] = true
			files := make([]*ast.File, 0, len(bp.GoFiles))
			for _, name := range bp.GoFiles {
				f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
				if err != nil {
					t.Fatal(err)
				}
				files = append(files, f)
			}
			for _, f := range scan(fset, path.Join(modulePath, rel), rel, files) {
				if seen[f.pos+f.kind] {
					continue
				}
				seen[f.pos+f.kind] = true
				all = append(all, f)
			}
		}
	}
	for _, want := range []string{"internal/pr", "internal/projects", "internal/env", "internal/branch"} {
		if !scanned[want] {
			t.Fatalf("%s was not scanned; the walk is broken, not the tree clean", want)
		}
	}
	for _, msg := range verdicts(all) {
		t.Error(msg)
	}
}

// verdicts turns findings into failure messages against the allowlists.
func verdicts(all []finding) []string {
	var out []string
	constants, transports, ghGits, scripts := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	for _, f := range all {
		switch f.kind {
		case "exec":
			out = append(out, fmt.Sprintf("%s (%s) runs git without internal/gitenv; call gitenv.Run or gitenv.RunBin with a profile", f.pos, f.fn))
		case "constant":
			constants[f.fn]++
		case "transport":
			transports[f.fn]++
		case "gh-git":
			ghGits[f.fn]++
		case "git-arg":
			out = append(out, fmt.Sprintf("%s (%s) hands the git executable to another program, which runs it without internal/gitenv", f.pos, f.fn))
		case "shell-script":
			scripts[f.fn]++
		}
	}
	check := func(kind string, counts map[string]int, allow map[string]struct {
		uses   int
		reason string
	}, fix string) {
		var keys []string
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, fn := range keys {
			entry, ok := allow[fn]
			if !ok {
				out = append(out, fmt.Sprintf("%s: %d %s use(s), not allowlisted; %s", fn, counts[fn], kind, fix))
				continue
			}
			if counts[fn] != entry.uses {
				out = append(out, fmt.Sprintf("%s: %d %s use(s), allowlisted for %d (%s)", fn, counts[fn], kind, entry.uses, entry.reason))
			}
		}
		for fn, entry := range allow {
			if strings.TrimSpace(entry.reason) == "" {
				out = append(out, fmt.Sprintf("%s allowlist entry %s carries no reason", kind, fn))
			}
			if counts[fn] == 0 {
				out = append(out, fmt.Sprintf("%s allowlist entry %s matches no use; delete it", kind, fn))
			}
		}
	}
	check("constant \"git\"", constants, gitConstantAllowlist, "name git through gitenv.Run, which runs gitenv.Bin")
	check("gitenv.Transport", transports, transportAllowlist, "use gitenv.Local unless the call reaches a remote, and then allowlist it with the reason")
	check("shell-script", scripts, shellScriptAllowlist, "a shell given a script the pin cannot read may run git; pass a constant script, or allowlist it with the reason")
	check("gh-runs-git", ghGits, ghGitAllowlist, "gh runs git outside internal/gitenv; remove gitenv.Unset(gitenv.Transport, os.Environ()) from its environment and allowlist it with the reason")
	sort.Strings(out)
	return out
}

// scan type-checks files as the package pkgPath (rel from the module root)
// with every import an empty stand-in, and returns its findings.
func scan(fset *token.FileSet, pkgPath, rel string, files []*ast.File) []finding {
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
	conf := types.Config{Importer: stubImporter{}, Error: func(error) {}}
	_, _ = conf.Check(pkgPath, fset, files, info)

	var out []finding
	for _, file := range files {
		fileName := path.Join(rel, filepath.Base(fset.Position(file.Pos()).Filename))
		for _, decl := range file.Decls {
			fn := fileName + ":" + declName(decl)
			looked := lookedUpGit(info, decl)
			execName := map[ast.Expr]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id := calleeIdent(call.Fun)
				if id == nil {
					return true
				}
				pos, ok := execNamePos[id.Name]
				if !ok || pos >= len(call.Args) {
					return true
				}
				arg := call.Args[pos]
				if namesGit(info, arg) || mentions(info, arg, looked) {
					execName[arg] = true
					out = append(out, finding{pos: fset.Position(arg.Pos()).String(), fn: fn, kind: "exec"})
					return true
				}
				base := constBase(info, arg)
				// A wrapper (sudo, env, nice) may start the shell: sudo sh -c.
				inShell := shells[base]
				rest := call.Args[pos+1:]
				for i, a := range rest {
					if passesGit(info, a, looked) {
						out = append(out, finding{pos: fset.Position(a.Pos()).String(), fn: fn, kind: "git-arg"})
						return true
					}
					v, ok := constString(info, a)
					switch {
					case !ok:
					case !inShell && shells[constBase(info, a)]:
						inShell = true
					case inShell && isScriptFlag(v) && i+1 < len(rest) && !isConstString(info, rest[i+1]):
						out = append(out, finding{pos: fset.Position(rest[i+1].Pos()).String(), fn: fn, kind: "shell-script"})
						return true
					case inShell && gitWord.MatchString(v):
						out = append(out, finding{pos: fset.Position(a.Pos()).String(), fn: fn, kind: "exec"})
						return true
					case (base == "gh" || base == "gh.exe") && ghGitSubcommands[v]:
						out = append(out, finding{pos: fset.Position(a.Pos()).String(), fn: fn, kind: "gh-git"})
						return true
					}
				}
				// A shell's or a wrapper's arguments spread from a slice
				// (sh args..., sudo args...) may hold any script, or git
				// itself; the pin cannot read them unless the slice is a
				// literal of constant strings.
				if (inShell || wrappers[base]) && call.Ellipsis.IsValid() && len(rest) > 0 && !constStrings(info, rest[len(rest)-1]) {
					out = append(out, finding{pos: fset.Position(rest[len(rest)-1].Pos()).String(), fn: fn, kind: "shell-script"})
				}
				return true
			})
			ast.Inspect(decl, func(n ast.Node) bool {
				switch e := n.(type) {
				case *ast.SelectorExpr:
					if isGitenvTransport(info, e) {
						out = append(out, finding{pos: fset.Position(e.Pos()).String(), fn: fn, kind: "transport"})
					}
				case ast.Expr:
					if execName[e] {
						return false
					}
					if tv, ok := info.Types[e]; ok && tv.Value != nil && tv.Value.Kind() == constant.String && constant.StringVal(tv.Value) == "git" {
						out = append(out, finding{pos: fset.Position(e.Pos()).String(), fn: fn, kind: "constant"})
						return false
					}
				}
				return true
			})
		}
	}
	return out
}

// namesGit reports whether arg, an executable name, is git's: a constant
// whose base name is git, or an expression whose source spells git.
func namesGit(info *types.Info, arg ast.Expr) bool {
	if tv, ok := info.Types[arg]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
		base := path.Base(strings.ReplaceAll(constant.StringVal(tv.Value), `\`, "/"))
		return base == "git" || strings.EqualFold(base, "git.exe")
	}
	var names []string
	ast.Inspect(arg, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			names = append(names, id.Name)
		}
		return true
	})
	return gitLike.MatchString(strings.Join(names, " "))
}

// passesGit reports whether e, an argument after the executable, holds the
// git executable: gitenv.Bin, a constant whose base name is git, a name
// gitBinName matches, or a variable a LookPath of git set (looked). A
// wrapper (env, nice, xargs, a shell's "$0") given it runs git outside
// internal/gitenv.
func passesGit(info *types.Info, e ast.Expr, looked map[types.Object]bool) bool {
	if mentions(info, e, looked) {
		return true
	}
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.SelectorExpr:
			found = isGitenvSel(info, x, "Bin") || gitBinName.MatchString(x.Sel.Name)
		case *ast.Ident:
			found = gitBinName.MatchString(x.Name)
		case ast.Expr:
			if v, ok := constString(info, x); ok {
				found = isGitExecutable(v)
			}
		}
		return !found
	})
	return found
}

// absPath matches an absolute path: /usr/bin/git, C:\Git\git.exe, \\host\git.
var absPath = regexp.MustCompile(`^(/|[A-Za-z]:[\\/]|\\\\)`)

// isGitExecutable reports whether the argument v names the git executable:
// git or git.exe alone, or an absolute or ./- or ../-relative path ending in
// one. A bare relative path whose last segment is git, as in `gh api
// repos/o/r/git`, does not.
func isGitExecutable(v string) bool {
	isGit := func(s string) bool { return s == "git" || strings.EqualFold(s, "git.exe") }
	if isGit(v) {
		return true
	}
	slashed := strings.ReplaceAll(v, `\`, "/")
	explicit := absPath.MatchString(v) || strings.HasPrefix(slashed, "./") || strings.HasPrefix(slashed, "../")
	return explicit && isGit(path.Base(slashed))
}

// bundledScriptFlag matches a shell option bundle that holds c, whose next
// argument is then the script: -lc, -ec, -xec.
var bundledScriptFlag = regexp.MustCompile(`^-[A-Za-z]*c[A-Za-z]*$`)

// isScriptFlag reports whether v is a shell option whose next argument is
// the script.
func isScriptFlag(v string) bool {
	return scriptFlags[v] || bundledScriptFlag.MatchString(v)
}

// wrappers are executables that run their arguments as another command:
// sudo git, env X=1 git, nice git. A spread through one is flagged as a
// shell's is. A wrapper outside this list, or one reached through a
// non-constant name, is not seen.
var wrappers = map[string]bool{"sudo": true, "doas": true, "env": true, "nice": true, "nohup": true, "timeout": true, "xargs": true, "setsid": true, "stdbuf": true, "ionice": true, "chrt": true, "taskset": true, "time": true, "command": true, "exec": true, "busybox": true}

// constStrings reports whether e is a constant string, or a slice literal
// whose elements all are: []string{"-c", "true"}.
func constStrings(info *types.Info, e ast.Expr) bool {
	if isConstString(info, e) {
		return true
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return false
	}
	for _, el := range lit.Elts {
		if !isConstString(info, el) {
			return false
		}
	}
	return true
}

// isConstString reports whether e is a constant string.
func isConstString(info *types.Info, e ast.Expr) bool {
	_, ok := constString(info, e)
	return ok
}

// constString is e's value when e is a constant string.
func constString(info *types.Info, e ast.Expr) (string, bool) {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

// constBase is the lower-cased base name of a constant executable name, or
// "" when e is not a constant.
func constBase(info *types.Info, e ast.Expr) string {
	v, ok := constString(info, e)
	if !ok {
		return ""
	}
	return strings.ToLower(path.Base(strings.ReplaceAll(v, `\`, "/")))
}

// lookedUpGit returns the variables in decl holding an exec.LookPath (or any
// LookPath-named function) of git, and every variable assigned from an
// expression that mentions one, to a fixed point: `p, _ :=
// exec.LookPath(gitenv.Bin); abs, _ := filepath.Abs(p)` marks p and abs.
func lookedUpGit(info *types.Info, decl ast.Decl) map[types.Object]bool {
	marked := map[types.Object]bool{}
	tainted := func(e ast.Expr) bool {
		if call, ok := e.(*ast.CallExpr); ok {
			if id := calleeIdent(call.Fun); id != nil && strings.EqualFold(id.Name, "LookPath") && len(call.Args) > 0 && namesGit(info, call.Args[0]) {
				return true
			}
		}
		return mentions(info, e, marked)
	}
	mark := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		if !ok {
			return false
		}
		obj := info.Defs[id]
		if obj == nil {
			obj = info.Uses[id]
		}
		if obj == nil || marked[obj] {
			return false
		}
		marked[obj] = true
		return true
	}
	for changed := true; changed; {
		changed = false
		ast.Inspect(decl, func(n ast.Node) bool {
			var lhs, rhs []ast.Expr
			switch s := n.(type) {
			case *ast.AssignStmt:
				lhs, rhs = s.Lhs, s.Rhs
			case *ast.ValueSpec:
				for _, name := range s.Names {
					lhs = append(lhs, name)
				}
				rhs = s.Values
			default:
				return true
			}
			switch {
			case len(rhs) == 1 && len(lhs) >= 1:
				// One value, or one call's several results: the first is
				// the path; an error beside it is not.
				if tainted(rhs[0]) && mark(lhs[0]) {
					changed = true
				}
			case len(rhs) == len(lhs):
				for i := range rhs {
					if tainted(rhs[i]) && mark(lhs[i]) {
						changed = true
					}
				}
			}
			return true
		})
	}
	return marked
}

// mentions reports whether e names any variable in marked.
func mentions(info *types.Info, e ast.Expr, marked map[types.Object]bool) bool {
	if len(marked) == 0 {
		return false
	}
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && marked[info.Uses[id]] {
			found = true
		}
		return !found
	})
	return found
}

// isGitenvTransport reports whether sel is gitenv.Transport.
func isGitenvTransport(info *types.Info, sel *ast.SelectorExpr) bool {
	return isGitenvSel(info, sel, "Transport")
}

// isGitenvSel reports whether sel is gitenv.<name>.
func isGitenvSel(info *types.Info, sel *ast.SelectorExpr, name string) bool {
	if sel.Sel.Name != name {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	pkg, ok := info.Uses[x].(*types.PkgName)
	return ok && pkg.Imported().Path() == gitenvImportPath
}

// stubImporter answers every import with an empty, complete package, so a
// package type-checks far enough to resolve its own constants and its import
// names without loading the dependency graph.
type stubImporter struct{}

func (stubImporter) Import(p string) (*types.Package, error) {
	return stubImporter{}.ImportFrom(p, "", 0)
}

func (stubImporter) ImportFrom(p, _ string, _ types.ImportMode) (*types.Package, error) {
	name := path.Base(p)
	if strings.HasPrefix(name, "v") && strings.Trim(name[1:], "0123456789") == "" && name != "v" {
		name = path.Base(path.Dir(p))
	}
	name = strings.NewReplacer("-", "_", ".", "_").Replace(name)
	pkg := types.NewPackage(p, name)
	pkg.MarkComplete()
	return pkg, nil
}

// declName is "Func" for a function, "Recv.Method" for a method, and "var",
// "const" or "type" for a general declaration.
func declName(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil || len(d.Recv.List) == 0 {
			return d.Name.Name
		}
		recv := d.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}
		if idx, ok := recv.(*ast.IndexExpr); ok {
			recv = idx.X
		}
		if id, ok := recv.(*ast.Ident); ok {
			return id.Name + "." + d.Name.Name
		}
		return d.Name.Name
	case *ast.GenDecl:
		return d.Tok.String()
	}
	return "?"
}

// calleeIdent is the identifier a call's function position names.
func calleeIdent(fun ast.Expr) *ast.Ident {
	switch f := fun.(type) {
	case *ast.Ident:
		return f
	case *ast.SelectorExpr:
		return f.Sel
	case *ast.ParenExpr:
		return calleeIdent(f.X)
	case *ast.IndexExpr:
		return calleeIdent(f.X)
	}
	return nil
}

// productionDirs lists every module directory holding Go files, relative to
// root in slash form, minus testdata, hidden directories (worktrees under
// .claude), and skippedPackages.
func productionDirs(root string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if p != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" || name == "node_modules") {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, skip := skippedPackages[rel]; skip {
			return nil
		}
		dirs = append(dirs, rel)
		return nil
	})
	return dirs, err
}

// TestPinFlagsEveryBypass runs the scanner over hostile source, so the pin's
// red is watched, not assumed. Mutation: make namesGit return false, or drop
// the "constant" or "transport" case in scan: a case here goes unflagged.
// For #978's rows: make lookedUpGit return nothing (looked, lookedVar), drop
// the shells case (shell, shellExe), or the gh case (ghClone, ghCheckout).
// For #985's: make passesGit return false (envBin through binArg); drop the
// scriptFlags case (shellVar, sudoShellVar, cmdVar); start inShell only at
// a shell executable (sudoShellVar, sudoShellGit); match scriptFlags only
// (bundled); drop the spread check (spread); match a constant git by
// path.Base alone (fineFour's `repos/o/r/git` is flagged); let a later
// constant in passesGit reset found (inSlice's gitenv.Bin is missed);
// match only absolute paths (dotSlash); check spreads only in a shell
// (sudoSpread); treat a constant slice literal as unreadable (fineFive is
// flagged).
func TestPinFlagsEveryBypass(t *testing.T) {
	src := `package p

import (
	"context"
	"os/exec"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/gitenv"
)

type runner interface {
	Run(context.Context, string, ...string) (string, error)
	RunWithEnvFiltered(context.Context, map[string]string, []string, string, ...string) (string, error)
}

const gitName = "git"

func literal(ctx context.Context, r runner) { _, _ = r.Run(ctx, "git", "status") }
func absolute() { _ = exec.Command("/usr/bin/git", "status") }
func pinned(ctx context.Context, r runner, gitBin string) { _, _ = r.Run(ctx, gitBin, "status") }
func viaConst(ctx context.Context) { _ = exec.CommandContext(ctx, gitName, "status") }
func viaVar(ctx context.Context, r runner) { name := "git"; _, _ = r.Run(ctx, name) }
func filtered(ctx context.Context, r runner) { _, _ = r.RunWithEnvFiltered(ctx, nil, nil, "git") }
func transport(ctx context.Context, r runner) { _, _ = gitenv.Run(ctx, r, gitenv.Transport, "fetch") }
func fine(ctx context.Context, r runner) { _, _ = gitenv.Run(ctx, r, gitenv.Local, "status"); _, _ = r.Run(ctx, "gh", "repo") }
func looked(ctx context.Context, r runner) { p, _ := exec.LookPath(gitenv.Bin); abs, _ := filepath.Abs(p); _, _ = r.Run(ctx, abs, "status") }
func lookedVar(ctx context.Context, r runner) { var p, _ = exec.LookPath(gitenv.Bin); _, _ = r.RunWithEnvFiltered(ctx, nil, nil, p) }
func shell(ctx context.Context, r runner) { _, _ = r.Run(ctx, "sh", "-c", "cd /x && git status") }
func shellExe(ctx context.Context) { _ = exec.CommandContext(ctx, "/bin/bash", "-c", "git fetch") }
func ghClone(ctx context.Context, r runner) { _, _ = r.Run(ctx, "gh", "repo", "clone", "o/r") }
func ghCheckout(ctx context.Context, r runner) { _, _ = r.RunWithEnvFiltered(ctx, nil, nil, "gh", "pr", "checkout", "1") }
func envBin(ctx context.Context, r runner) { _, _ = r.Run(ctx, "env", "GIT_DIR=x", gitenv.Bin, "status") }
func dollarZero(ctx context.Context) { _ = exec.CommandContext(ctx, "sh", "-c", "exec \"$0\" status", gitenv.Bin) }
func xargsPath(ctx context.Context, r runner) { _, _ = r.Run(ctx, "xargs", "/usr/bin/git") }
func lookedArg(ctx context.Context, r runner) { p, _ := exec.LookPath(gitenv.Bin); _, _ = r.Run(ctx, "nice", p, "status") }
func binArg(ctx context.Context, r runner, gitBin string) { _, _ = r.Run(ctx, "nice", "-n5", gitBin) }
func shellVar(ctx context.Context, r runner, script string) { _, _ = r.Run(ctx, "sh", "-c", script) }
func sudoShellVar(ctx context.Context, r runner, script string) { _, _ = r.Run(ctx, "/usr/bin/sudo", "/bin/sh", "-c", script) }
func cmdVar(script string) { _ = exec.Command("cmd.exe", "/c", script) }
func sudoShellGit(ctx context.Context, r runner) { _, _ = r.Run(ctx, "sudo", "bash", "-c", "git pull") }
func bundled(ctx context.Context, r runner, script string) { _, _ = r.Run(ctx, "bash", "-lc", script) }
func spread(args []string) { _ = exec.Command("sh", args...) }
func dotSlash(ctx context.Context, r runner) { _, _ = r.Run(ctx, "nice", "./git", "status") }
func sudoSpread(ctx context.Context, r runner, args []string) { _, _ = r.Run(ctx, "sudo", args...) }
func fineFive(ctx context.Context, r runner) { _ = exec.Command("sh", []string{"-c", "true"}...); _, _ = r.Run(ctx, "env", []string{"A=1", "true"}...) }
func inSlice(ctx context.Context, r runner) { _, _ = r.Run(ctx, "nice", append([]string{gitenv.Bin}, "status")...) }
func fineFour(ctx context.Context, r runner, args []string) { _, _ = r.Run(ctx, "gh", "api", "repos/o/r/git"); _, _ = r.Run(ctx, "tmux", args...); _, _ = r.Run(ctx, "bash", "-lc", "true") }
func fineThree(ctx context.Context, r runner, arg, github string) { _, _ = r.Run(ctx, "sh", "-c", "echo hi", "_", arg); _, _ = r.Run(ctx, "gh", "api", "repos/o/r/git/refs"); _, _ = r.Run(ctx, "tmux", "send-keys", github); _, _ = r.Run(ctx, "sh", "-e", "-c", "true") }
func fineToo(ctx context.Context, r runner) { p, _ := exec.LookPath("tmux"); _, _ = r.Run(ctx, p); _, _ = r.Run(ctx, "sh", "-c", "echo github digit"); _, _ = r.Run(ctx, "gh", "pr", "view") }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, fd := range scan(fset, "example.com/p", "p", []*ast.File{f}) {
		got[fd.fn] = append(got[fd.fn], fd.kind)
	}
	want := map[string][]string{
		"p/p.go:const":      {"constant"},
		"p/p.go:literal":    {"exec"},
		"p/p.go:absolute":   {"exec"},
		"p/p.go:pinned":     {"exec"},
		"p/p.go:viaConst":   {"exec"},
		"p/p.go:viaVar":     {"constant"},
		"p/p.go:filtered":   {"exec"},
		"p/p.go:transport":  {"transport"},
		"p/p.go:looked":     {"exec"},
		"p/p.go:lookedVar":  {"exec"},
		"p/p.go:shell":      {"exec"},
		"p/p.go:shellExe":   {"exec"},
		"p/p.go:ghClone":    {"gh-git"},
		"p/p.go:ghCheckout": {"gh-git"},
		// #985 nit: the git executable handed to a wrapper, and a shell
		// script the pin cannot read.
		"p/p.go:envBin":       {"git-arg"},
		"p/p.go:dollarZero":   {"git-arg"},
		"p/p.go:xargsPath":    {"git-arg"},
		"p/p.go:lookedArg":    {"git-arg"},
		"p/p.go:binArg":       {"git-arg"},
		"p/p.go:shellVar":     {"shell-script"},
		"p/p.go:sudoShellVar": {"shell-script"},
		"p/p.go:cmdVar":       {"shell-script"},
		"p/p.go:sudoShellGit": {"exec"},
		"p/p.go:bundled":      {"shell-script"},
		"p/p.go:spread":       {"shell-script"},
		"p/p.go:inSlice":      {"git-arg"},
		"p/p.go:dotSlash":     {"git-arg"},
		"p/p.go:sudoSpread":   {"shell-script"},
	}
	for fn, kinds := range want {
		if strings.Join(got[fn], ",") != strings.Join(kinds, ",") {
			t.Errorf("%s: findings %v, want %v", fn, got[fn], kinds)
		}
	}
	for fn, kinds := range got {
		if _, ok := want[fn]; !ok {
			t.Errorf("%s: unexpected findings %v", fn, kinds)
		}
	}
	if msgs := verdicts([]finding{{pos: "x", fn: "p/p.go:transport", kind: "transport"}}); len(msgs) == 0 {
		t.Error("an unlisted gitenv.Transport use produced no verdict")
	}
	if msgs := verdicts([]finding{{pos: "x", fn: "p/p.go:envBin", kind: "git-arg"}}); len(msgs) == 0 {
		t.Error("the git executable handed to a wrapper produced no verdict")
	}
	if msgs := verdicts([]finding{{pos: "x", fn: "p/p.go:shellVar", kind: "shell-script"}}); !slices.ContainsFunc(msgs, func(m string) bool { return strings.HasPrefix(m, "p/p.go:shellVar: 1 shell-script") }) {
		t.Errorf("an unlisted non-constant shell script produced no verdict: %v", msgs)
	}
	if msgs := verdicts([]finding{{pos: "x", fn: "p/p.go:ghClone", kind: "gh-git"}}); !slices.ContainsFunc(msgs, func(m string) bool { return strings.HasPrefix(m, "p/p.go:ghClone: 1 gh-runs-git") }) {
		t.Errorf("an unlisted gh git subcommand produced no verdict: %v", msgs)
	}
}
