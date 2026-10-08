package gitenv_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv"
	"github.com/cameronsjo/forgectl/internal/gitenv/gitenvtest"
)

// RunUnfiltered runs a status that would re-hash two stat-dirty files through
// the repository's own clean and process filters, on real git, and neither
// runs (#977).
// Mutations: have RunUnfiltered pass args without the overrides (the "evil"
// clean filter runs); drop "process" from filterDriverVars (the "dot.ted"
// process filter runs); take the driver name from the left in
// filterDriverNames (filter.dot.clean is overridden, "dot.ted" runs).
func TestRunUnfilteredRunsNoFilterDriver(t *testing.T) {
	gitenvtest.NewFilterCanary(t).AssertLive(t)

	c := gitenvtest.NewFilterCanary(t)
	out, err := gitenv.RunUnfiltered(t.Context(), exec.OSRunner{}, gitenv.Bin, c.Dir, "status", "--porcelain")
	if err != nil {
		t.Fatalf("RunUnfiltered status: %v", err)
	}
	if c.Ran(t) {
		t.Fatal("a filter driver the repository defines ran during an unfiltered status")
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("status = %q, want a clean tree: the files match their blobs byte for byte", out)
	}
}

// A driver name holding '=' cannot be overridden with -c, since git splits
// the argument at its first '='. RunUnfiltered then refuses to run the call
// at all, rather than run it with that driver live.
// Mutation: drop the '=' check in filterDriverNames (the status runs, and
// "a=b" fires the canary).
func TestRunUnfilteredRefusesADriverItCannotOverride(t *testing.T) {
	gitenvtest.NewFilterCanary(t, [2]string{"z.txt", "a=b"}).AssertLive(t)

	c := gitenvtest.NewFilterCanary(t, [2]string{"z.txt", "a=b"})
	if _, err := gitenv.RunUnfiltered(t.Context(), exec.OSRunner{}, gitenv.Bin, c.Dir, "status", "--porcelain"); err == nil {
		t.Error("RunUnfiltered ran a status with a driver it cannot override")
	}
	if c.Ran(t) {
		t.Fatal("the driver named a=b ran")
	}
}

// A listing that fails for any reason but "no match" is an error, and the
// call is never made.
// Mutation: treat every listing error as "no drivers" (the status runs).
func TestRunUnfilteredFailsClosedWhenTheListingFails(t *testing.T) {
	for name, listErr := range map[string]error{
		"bad config":       &exec.CommandError{Name: "git", ExitCode: 128, Err: errors.New("exit status 128")},
		"exit 1 w/ output": &exec.CommandError{Name: "git", ExitCode: 1, Output: "filter.x.clean", Err: errors.New("exit status 1")},
		"not a command":    errors.New("context canceled"),
	} {
		for _, failing := range []string{"config", "ls-files"} {
			t.Run(name+"/"+failing, func(t *testing.T) {
				f := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
					if gitenvtest.FilterListing(args) {
						if slices.Contains(args, failing) {
							return "", listErr
						}
						return gitenvtest.AnswerListing(name, args)
					}
					return "", nil
				}}
				if _, err := gitenv.RunUnfiltered(t.Context(), f, gitenv.Bin, "/repo", "status"); err == nil {
					t.Error("RunUnfiltered returned no error for a failed listing")
				}
				for _, c := range f.Calls {
					if !gitenvtest.FilterListing(c.Args) {
						t.Errorf("RunUnfiltered ran %v after a failed listing", c.Args)
					}
				}
			})
		}
	}
}

// listingFake answers RunUnfiltered's listings per repository directory:
// drivers[dir] is the driver listing's output there ("" for no match), and
// gitlinks[dir] the ls-files output. Any other call succeeds.
func listingFake(drivers, gitlinks map[string]string) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if !gitenvtest.FilterListing(args) {
			return "", nil
		}
		stripped := gitenvtest.Strip(args)
		dir := ""
		if len(stripped) >= 2 && stripped[0] == "-C" {
			dir = stripped[1]
		}
		if slices.Contains(args, "ls-files") {
			return gitlinks[dir], nil
		}
		if drivers[dir] == "" {
			return gitenvtest.AnswerListing(name, args)
		}
		return drivers[dir], nil
	}}
}

// gitlink is one ls-files --stage entry for a submodule at p.
func gitlink(p string) string { return "160000 " + strings.Repeat("b", 40) + " 0\t" + p + "\x00" }

// mkRepoDir makes dir/.git a directory holding a HEAD, which is what
// RunUnfiltered checks a submodule's .git names.
func mkRepoDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The overrides precede the caller's -C and arguments, after Local's own
// options, and the listings run in the same directory under Local. Every
// listed driver is blanked, whatever scope defined it.
// Mutation: list without -C dir (the listing reads another repository).
func TestRunUnfilteredBuildsTheCall(t *testing.T) {
	f := listingFake(map[string]string{
		"/repo": "filter.lfs.clean\x00filter.lfs.process\x00filter.a.b.smudge\x00",
	}, map[string]string{"/repo": "100644 " + strings.Repeat("a", 40) + " 0\tfile\x00"})
	if _, err := gitenv.RunUnfiltered(t.Context(), f, "/pinned/git", "/repo", "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 3 {
		t.Fatalf("calls = %v, want the two listings and the status", f.Calls)
	}
	wantList := append(gitenv.Args(gitenv.Local), "-C", "/repo", "config", "-z", "--name-only", "--get-regexp", `^filter\..*\.(clean|smudge|process)$`)
	if got := f.Calls[0]; got.Name != "/pinned/git" || !slices.Equal(got.Args, wantList) {
		t.Errorf("driver listing = %s %v, want /pinned/git %v", got.Name, got.Args, wantList)
	}
	wantSubs := append(gitenv.Args(gitenv.Local), "-C", "/repo", "ls-files", "-z", "--stage", "--", ":/")
	if got := f.Calls[1]; got.Name != "/pinned/git" || !slices.Equal(got.Args, wantSubs) {
		t.Errorf("submodule listing = %s %v, want /pinned/git %v", got.Name, got.Args, wantSubs)
	}
	want := append(gitenv.Args(gitenv.Local),
		"-c", "filter.a.b.clean=", "-c", "filter.a.b.smudge=", "-c", "filter.a.b.process=",
		"-c", "filter.lfs.clean=", "-c", "filter.lfs.smudge=", "-c", "filter.lfs.process=",
		"-C", "/repo", "status", "--porcelain")
	if got := f.Calls[2]; got.Name != "/pinned/git" || !slices.Equal(got.Args, want) {
		t.Errorf("status = %s %v, want /pinned/git %v", got.Name, got.Args, want)
	}
	if f.Calls[2].Env["GIT_ALLOW_PROTOCOL"] != "" || f.Calls[2].Env == nil {
		t.Errorf("status env %v is not Local's", f.Calls[2].Env)
	}
}

// RunUnfiltered lists the drivers of every populated submodule, nested
// ones included, and blanks them in the one call: git passes its -c options
// on to the child git status runs in each submodule. A gitlink with no .git
// at its path is not entered, by status or by the listing. A gitfile is
// followed to the repository it names.
// Mutations: skip the submodule walk ("s" stays live); walk only the top
// level ("n" stays live); list an unpopulated gitlink (a listing in "u").
func TestRunUnfilteredListsEverySubmodulesDrivers(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "s")
	nested := filepath.Join(sub, "n")
	mkRepoDir(t, nested)
	mkRepoDir(t, filepath.Join(root, "modules-s"))
	if err := os.MkdirAll(filepath.Join(root, "u"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: ../modules-s/.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := listingFake(
		map[string]string{root: "filter.top.clean\x00", sub: "filter.s.clean\x00", nested: "filter.n.process\x00"},
		map[string]string{root: gitlink("s") + gitlink("u") + gitlink("gone"), sub: gitlink("n")},
	)
	if _, err := gitenv.RunUnfiltered(t.Context(), f, gitenv.Bin, root, "status"); err != nil {
		t.Fatal(err)
	}
	last := f.Calls[len(f.Calls)-1].Args
	for _, want := range []string{"filter.top.clean=", "filter.s.process=", "filter.n.clean="} {
		if !slices.Contains(last, want) {
			t.Errorf("status %v does not blank %s", last, want)
		}
	}
	for _, c := range f.Calls {
		if slices.Contains(c.Args, filepath.Join(root, "u")) || slices.Contains(c.Args, filepath.Join(root, "gone")) {
			t.Errorf("listed an unpopulated gitlink: %v", c.Args)
		}
	}
}

// Every shape the walk cannot follow safely refuses the call, and no status
// runs: submodules nested past the bound; more repositories than the cap;
// two paths to one repository; a gitlink path or .git that is a symbolic
// link; a .git that names no repository; ls-files output that is not index
// entries.
// Mutations: drop the depth check, the cap, the visited set, either
// symlink check, or the HEAD check, or skip a malformed entry instead of
// refusing: that row's call runs.
func TestRunUnfilteredRefusesWhatItCannotWalk(t *testing.T) {
	type row struct {
		dir      string
		gitlinks map[string]string
	}
	rows := map[string]func(t *testing.T) row{
		"too deep": func(t *testing.T) row {
			deep := t.TempDir()
			links := map[string]string{}
			d := deep
			for range 40 {
				links[d] = gitlink("s")
				d = filepath.Join(d, "s")
				mkRepoDir(t, d)
			}
			return row{deep, links}
		},
		"too many": func(t *testing.T) row {
			wide := t.TempDir()
			var links strings.Builder
			for i := range 300 {
				name := fmt.Sprintf("s%d", i)
				mkRepoDir(t, filepath.Join(wide, name))
				links.WriteString(gitlink(name))
			}
			return row{wide, map[string]string{wide: links.String()}}
		},
		"one repository twice": func(t *testing.T) row {
			root := t.TempDir()
			mkRepoDir(t, filepath.Join(root, "m"))
			for _, s := range []string{"a", "b"} {
				if err := os.MkdirAll(filepath.Join(root, s), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, s, ".git"), []byte("gitdir: ../m/.git\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			return row{root, map[string]string{root: gitlink("a") + gitlink("b")}}
		},
		"symlinked gitlink": func(t *testing.T) row {
			root := t.TempDir()
			mkRepoDir(t, filepath.Join(root, "real"))
			if err := os.Symlink("real", filepath.Join(root, "s")); err != nil {
				t.Skipf("symlink: %v", err)
			}
			return row{root, map[string]string{root: gitlink("s")}}
		},
		"symlinked .git": func(t *testing.T) row {
			root := t.TempDir()
			mkRepoDir(t, filepath.Join(root, "real"))
			if err := os.MkdirAll(filepath.Join(root, "s"), 0o750); err != nil {
				t.Fatal(err)
			}
			// The link names a valid gitfile, so only the symlink check refuses it.
			gitfile := filepath.Join(root, "gitfile")
			if err := os.WriteFile(gitfile, []byte("gitdir: "+filepath.Join(root, "real", ".git")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(gitfile, filepath.Join(root, "s", ".git")); err != nil {
				t.Skipf("symlink: %v", err)
			}
			return row{root, map[string]string{root: gitlink("s")}}
		},
		"empty .git directory": func(t *testing.T) row {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "s", ".git"), 0o750); err != nil {
				t.Fatal(err)
			}
			return row{root, map[string]string{root: gitlink("s")}}
		},
		"gitfile naming nothing": func(t *testing.T) row {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "s"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "s", ".git"), []byte("not a gitfile\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return row{root, map[string]string{root: gitlink("s")}}
		},
		"malformed": func(*testing.T) row {
			return row{"/repo", map[string]string{"/repo": "160000 no tab here\x00"}}
		},
	}
	for name, build := range rows {
		t.Run(name, func(t *testing.T) {
			tc := build(t)
			f := listingFake(nil, tc.gitlinks)
			if _, err := gitenv.RunUnfiltered(t.Context(), f, gitenv.Bin, tc.dir, "status"); err == nil {
				t.Error("RunUnfiltered ran the call")
			}
			for _, c := range f.Calls {
				if !gitenvtest.FilterListing(c.Args) {
					t.Errorf("RunUnfiltered ran %v", c.Args)
				}
			}
		})
	}
}

// Two gitlinks that are symbolic links to the superproject's own working
// tree would make the walk revisit it without end, doubling at each level
// (about 2^33 git processes before the depth bound). On real git the call
// is refused at once.
// Mutation: drop the gitlink symlink check (with the visited set still in
// place the walk refuses later; without either, it does not return within
// the deadline).
func TestRunUnfilteredRefusesSubmodulesThatLoopBack(t *testing.T) {
	gitenvtest.RequireGit(t)
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	gitenvtest.Git(t, dir, "init", "-q", "-b", "main")
	gitenvtest.Git(t, dir, "commit", "-q", "--allow-empty", "-m", "control")
	head := gitenvtest.Git(t, dir, "rev-parse", "HEAD")
	for _, s := range []string{"s1", "s2"} {
		gitenvtest.Git(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+head+","+s)
		if err := os.Symlink(".", filepath.Join(dir, s)); err != nil {
			t.Skipf("symlink: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, err := gitenv.RunUnfiltered(ctx, exec.OSRunner{}, gitenv.Bin, dir, "status", "--porcelain")
	if err == nil {
		t.Error("RunUnfiltered ran a status through submodules that loop back")
	}
	if ctx.Err() != nil || time.Since(start) > 10*time.Second {
		t.Errorf("RunUnfiltered took %v to refuse", time.Since(start))
	}
}

// RunUnfilteredAlso lists each extra working tree too, and skips one that is
// not there, which worktree remove still prunes.
// Mutations: ignore also (the worktree's "wt" stays live); list a missing
// path (its listing fails, and the remove is refused).
func TestRunUnfilteredAlsoListsTheWorkingTreesItReaches(t *testing.T) {
	wt := t.TempDir()
	missing := filepath.Join(t.TempDir(), "gone")
	f := listingFake(map[string]string{wt: "filter.wt.clean\x00"}, nil)
	f.RunFunc = func(next func(string, []string) (string, error)) func(string, []string) (string, error) {
		return func(name string, args []string) (string, error) {
			if slices.Contains(args, missing) && gitenvtest.FilterListing(args) {
				return "", &exec.CommandError{Name: name, Args: args, ExitCode: 128, Err: errors.New("exit status 128")}
			}
			return next(name, args)
		}
	}(f.RunFunc)
	if _, err := gitenv.RunUnfilteredAlso(t.Context(), f, gitenv.Bin, "", []string{wt, missing}, "worktree", "remove", "--", wt); err != nil {
		t.Fatal(err)
	}
	last := f.Calls[len(f.Calls)-1].Args
	want := append(gitenv.Args(gitenv.Local), "-c", "filter.wt.clean=", "-c", "filter.wt.smudge=", "-c", "filter.wt.process=", "worktree", "remove", "--", wt)
	if !slices.Equal(last, want) {
		t.Errorf("remove = %v, want %v", last, want)
	}
}

// A repository's own config can name a program for git-lfs to run, through
// lfs.extension.<name>.clean, and git-lfs runs it inside the operator's
// global filter.lfs.process. So the operator's driver is blanked as well:
// on real git with git-lfs, a stat-dirty LFS file's status never runs the
// repository's extension, and reads as a failure, not a clean tree.
// Mutations: leave "lfs" unblanked, or blank only the drivers local and
// worktree scope define, as ebcf6d7 did: the extension runs.
func TestRunUnfilteredBlanksTheOperatorsLFSDriver(t *testing.T) {
	gitenvtest.RequireGit(t)
	if _, err := osexec.LookPath("git-lfs"); err != nil {
		t.Skipf("git-lfs is not on PATH: %v", err)
	}
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	conf := "[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n\tsmudge = git-lfs smudge -- %f\n\tprocess = git-lfs filter-process\n\trequired = true\n"
	if err := os.WriteFile(global, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	build := func() (dir, canary string) {
		base := t.TempDir()
		dir, canary = filepath.Join(base, "repo"), filepath.Join(base, "canary")
		if err := os.Mkdir(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{"x.bin": "payload\n", ".gitattributes": "x.bin filter=lfs diff=lfs merge=lfs -text\n"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		gitenvtest.Git(t, dir, "init", "-q", "-b", "main")
		gitenvtest.Git(t, dir, "add", ".")
		gitenvtest.Git(t, dir, "commit", "-q", "-m", "control")
		evil := filepath.Join(base, "evil.sh")
		if err := os.WriteFile(evil, []byte("#!/bin/sh\ntouch '"+canary+"'\ncat\n"), 0o700); err != nil { //nolint:gosec // G306: an executable canary
			t.Fatal(err)
		}
		gitenvtest.Git(t, dir, "config", "lfs.extension.evil.clean", evil+" %f")
		gitenvtest.Git(t, dir, "config", "lfs.extension.evil.smudge", "cat")
		gitenvtest.Git(t, dir, "config", "lfs.extension.evil.priority", "0")
		later := time.Now().Add(time.Hour)
		if err := os.Chtimes(filepath.Join(dir, "x.bin"), later, later); err != nil {
			t.Fatal(err)
		}
		return dir, canary
	}
	fired := func(canary string) bool { return gitenvtest.FilterCanary{Path: canary}.Ran(t) }

	liveDir, liveCanary := build()
	_ = gitenv.Command(t.Context(), gitenv.Local, "-C", liveDir, "status", "--porcelain").Run()
	if !fired(liveCanary) {
		t.Fatal("a Local status ran no lfs extension; the fixture cannot fire, so a passing test would prove nothing")
	}

	dir, canary := build()
	_, err := gitenv.RunUnfiltered(t.Context(), exec.OSRunner{}, gitenv.Bin, dir, "status", "--porcelain")
	if fired(canary) {
		t.Fatal("the repository's lfs extension ran through the operator's git-lfs filter during an unfiltered status")
	}
	if err == nil {
		t.Error("status succeeded with the required lfs filter blanked; want a failure the callers read as unknown")
	}
}

// A status in a superproject runs each populated submodule's own drivers,
// nested ones included, through the child git it starts there; on real git,
// none of them runs.
// Mutations: skip the submodule walk, or walk only the top level: the
// canary runs.
func TestRunUnfilteredRunsNoSubmoduleFilterDriver(t *testing.T) {
	gitenvtest.NewSubmoduleFilterCanary(t).AssertLive(t)

	c := gitenvtest.NewSubmoduleFilterCanary(t)
	out, err := gitenv.RunUnfiltered(t.Context(), exec.OSRunner{}, gitenv.Bin, c.Dir, "status", "--porcelain")
	if err != nil {
		t.Fatalf("RunUnfiltered status: %v", err)
	}
	if c.Ran(t) {
		t.Fatal("a submodule's own filter driver ran during an unfiltered status")
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("status = %q, want a clean tree", out)
	}
}
