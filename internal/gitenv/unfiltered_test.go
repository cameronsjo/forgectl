package gitenv_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
// call is never made. A failed scoped listing falls back to the unscoped
// one, which fails the same way.
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
// drivers[dir] is the scoped driver listing's output there ("" for no
// match), and gitlinks[dir] the ls-files output. Any other call succeeds.
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

// The overrides precede the caller's -C and arguments, after Local's own
// options, and the listings run in the same directory under Local. Drivers
// from local, worktree and command scope are blanked; one only global
// config defines is not.
// Mutations: list without -C dir (the listing reads another repository);
// blank global-scope drivers too ("g" is overridden).
func TestRunUnfilteredBuildsTheCall(t *testing.T) {
	f := listingFake(map[string]string{
		"/repo": "local\x00filter.lfs.clean\x00local\x00filter.lfs.process\x00worktree\x00filter.a.b.smudge\x00global\x00filter.g.clean\x00command\x00filter.c.clean\x00",
	}, map[string]string{"/repo": "100644 " + strings.Repeat("a", 40) + " 0\tfile\x00"})
	if _, err := gitenv.RunUnfiltered(t.Context(), f, "/pinned/git", "/repo", "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 3 {
		t.Fatalf("calls = %v, want the two listings and the status", f.Calls)
	}
	wantList := append(gitenv.Args(gitenv.Local), "-C", "/repo", "config", "-z", "--show-scope", "--name-only", "--get-regexp", `^filter\..*\.(clean|smudge|process)$`)
	if got := f.Calls[0]; got.Name != "/pinned/git" || !slices.Equal(got.Args, wantList) {
		t.Errorf("driver listing = %s %v, want /pinned/git %v", got.Name, got.Args, wantList)
	}
	wantSubs := append(gitenv.Args(gitenv.Local), "-C", "/repo", "ls-files", "-z", "--stage", "--", ":/")
	if got := f.Calls[1]; got.Name != "/pinned/git" || !slices.Equal(got.Args, wantSubs) {
		t.Errorf("submodule listing = %s %v, want /pinned/git %v", got.Name, got.Args, wantSubs)
	}
	want := append(gitenv.Args(gitenv.Local),
		"-c", "filter.a.b.clean=", "-c", "filter.a.b.smudge=", "-c", "filter.a.b.process=",
		"-c", "filter.c.clean=", "-c", "filter.c.smudge=", "-c", "filter.c.process=",
		"-c", "filter.lfs.clean=", "-c", "filter.lfs.smudge=", "-c", "filter.lfs.process=",
		"-C", "/repo", "status", "--porcelain")
	if got := f.Calls[2]; got.Name != "/pinned/git" || !slices.Equal(got.Args, want) {
		t.Errorf("status = %s %v, want /pinned/git %v", got.Name, got.Args, want)
	}
	if f.Calls[2].Env["GIT_ALLOW_PROTOCOL"] != "" || f.Calls[2].Env == nil {
		t.Errorf("status env %v is not Local's", f.Calls[2].Env)
	}
}

// A git without --show-scope (before 2.26) refuses the scoped listing with a
// usage error, and a scoped listing that is not scope and key pairs cannot
// say which scope a key came from. Either way every driver the unscoped
// listing names is blanked, the operator's included.
// Mutation: return the scoped listing's error, or no names, instead of
// falling back (the call fails, or "g" stays live).
func TestRunUnfilteredBlanksEveryDriverWhenScopesAreUnknown(t *testing.T) {
	for name, scoped := range map[string]func(string, []string) (string, error){
		"old git": func(name string, args []string) (string, error) {
			return "", &exec.CommandError{Name: name, Args: args, ExitCode: 129, Err: errors.New("exit status 129")}
		},
		"unparsed": func(string, []string) (string, error) { return "local\x00filter.a.clean\x00filter.g.clean\x00", nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
				switch {
				case !gitenvtest.FilterListing(args):
					return "", nil
				case slices.Contains(args, "--show-scope"):
					return scoped(name, args)
				case slices.Contains(args, "config"):
					return "filter.g.clean\x00filter.a.clean\x00", nil
				}
				return "", nil
			}}
			if _, err := gitenv.RunUnfiltered(t.Context(), f, gitenv.Bin, "/repo", "status"); err != nil {
				t.Fatal(err)
			}
			last := f.Calls[len(f.Calls)-1].Args
			for _, want := range []string{"filter.a.clean=", "filter.g.clean="} {
				if !slices.Contains(last, want) {
					t.Errorf("status %v does not blank %s", last, want)
				}
			}
		})
	}
}

// RunUnfiltered lists the drivers of every populated submodule, nested
// ones included, and blanks them in the one call: git passes its -c options
// on to the child git status runs in each submodule. A gitlink with no .git
// at its path is not entered, by status or by the listing.
// Mutations: skip the submodule walk ("s" stays live); walk only the top
// level ("n" stays live); list an unpopulated gitlink (a listing in "u").
func TestRunUnfilteredListsEverySubmodulesDrivers(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "s")
	nested := filepath.Join(sub, "n")
	for _, d := range []string{filepath.Join(nested, ".git"), filepath.Join(root, "u")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	link := func(p string) string { return "160000 " + strings.Repeat("b", 40) + " 0\t" + p + "\x00" }
	if err := os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := listingFake(
		map[string]string{root: "local\x00filter.top.clean\x00", sub: "local\x00filter.s.clean\x00", nested: "local\x00filter.n.process\x00"},
		map[string]string{root: link("s") + link("u") + link("gone"), sub: link("n")},
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

// Submodules nested past the bound, as a symlink loop would make them, and
// ls-files output that is not index entries, both refuse the call.
// Mutations: drop the depth check (the walk ends where the tree does, and
// the status runs); skip a malformed entry instead of refusing.
func TestRunUnfilteredRefusesWhatItCannotWalk(t *testing.T) {
	deep := t.TempDir()
	dirs := map[string]string{}
	d := deep
	for range 40 {
		dirs[d] = "160000 " + strings.Repeat("c", 40) + " 0\ts\x00"
		d = filepath.Join(d, "s")
		if err := os.MkdirAll(filepath.Join(d, ".git"), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct {
		dir      string
		gitlinks map[string]string
	}{
		"too deep":  {deep, dirs},
		"malformed": {"/repo", map[string]string{"/repo": "160000 no tab here\x00"}},
	} {
		t.Run(name, func(t *testing.T) {
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

// RunUnfilteredAlso lists each extra working tree too, and skips one that is
// not there, which worktree remove still prunes.
// Mutations: ignore also (the worktree's "wt" stays live); list a missing
// path (its listing fails, and the remove is refused).
func TestRunUnfilteredAlsoListsTheWorkingTreesItReaches(t *testing.T) {
	wt := t.TempDir()
	missing := filepath.Join(t.TempDir(), "gone")
	f := listingFake(map[string]string{wt: "worktree\x00filter.wt.clean\x00"}, nil)
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

// The operator's own driver, which only global config defines, stays live
// on real git, so a repository routed through git-lfs still reads as clean.
// A name the repository defines too is blanked in every scope.
// Mutations: blank global-scope drivers (the operator's canary stays
// unfired); skip a name some global key also defines ("both" runs the
// repository's program).
func TestRunUnfilteredLeavesTheOperatorsDriversLive(t *testing.T) {
	gitenvtest.NewFilterCanary(t).AssertLive(t)

	c, operator := gitenvtest.NewOperatorFilterCanary(t)
	out, err := gitenv.RunUnfiltered(t.Context(), exec.OSRunner{}, gitenv.Bin, c.Dir, "status", "--porcelain")
	if err != nil {
		t.Fatalf("RunUnfiltered status: %v", err)
	}
	if c.Ran(t) {
		t.Fatal("a filter driver the repository defines ran during an unfiltered status")
	}
	if !(gitenvtest.FilterCanary{Path: operator}).Ran(t) {
		t.Error("the operator's own global driver was switched off")
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("status = %q, want a clean tree", out)
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
