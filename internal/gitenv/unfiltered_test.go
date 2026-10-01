package gitenv_test

import (
	"errors"
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
// call is never made.
// Mutation: treat every listing error as "no drivers" (the status runs).
func TestRunUnfilteredFailsClosedWhenTheListingFails(t *testing.T) {
	for name, listErr := range map[string]error{
		"bad config":       &exec.CommandError{Name: "git", ExitCode: 128, Err: errors.New("exit status 128")},
		"exit 1 w/ output": &exec.CommandError{Name: "git", ExitCode: 1, Output: "filter.x.clean", Err: errors.New("exit status 1")},
		"not a command":    errors.New("context canceled"),
	} {
		t.Run(name, func(t *testing.T) {
			f := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
				if gitenvtest.FilterListing(args) {
					return "", listErr
				}
				return "", nil
			}}
			if _, err := gitenv.RunUnfiltered(t.Context(), f, gitenv.Bin, "/repo", "status"); err == nil {
				t.Error("RunUnfiltered returned no error for a failed listing")
			}
			if len(f.Calls) != 1 {
				t.Errorf("calls = %v, want the listing alone", f.Calls)
			}
		})
	}
}

// The overrides precede the caller's -C and arguments, after Local's own
// options, and the listing runs in the same directory under Local.
// Mutation: list without -C dir (the listing reads another repository).
func TestRunUnfilteredBuildsTheCall(t *testing.T) {
	f := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if gitenvtest.FilterListing(args) {
			return "filter.lfs.clean\x00filter.lfs.process\x00filter.a.b.smudge\x00", nil
		}
		return "", nil
	}}
	if _, err := gitenv.RunUnfiltered(t.Context(), f, "/pinned/git", "/repo", "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 2 {
		t.Fatalf("calls = %v, want the listing and the status", f.Calls)
	}
	wantList := append(gitenv.Args(gitenv.Local), "-C", "/repo", "config", "-z", "--name-only", "--get-regexp", `^filter\..*\.(clean|smudge|process)$`)
	if got := f.Calls[0]; got.Name != "/pinned/git" || !slices.Equal(got.Args, wantList) {
		t.Errorf("listing = %s %v, want /pinned/git %v", got.Name, got.Args, wantList)
	}
	want := append(gitenv.Args(gitenv.Local),
		"-c", "filter.a.b.clean=", "-c", "filter.a.b.smudge=", "-c", "filter.a.b.process=",
		"-c", "filter.lfs.clean=", "-c", "filter.lfs.smudge=", "-c", "filter.lfs.process=",
		"-C", "/repo", "status", "--porcelain")
	if got := f.Calls[1]; got.Name != "/pinned/git" || !slices.Equal(got.Args, want) {
		t.Errorf("status = %s %v, want /pinned/git %v", got.Name, got.Args, want)
	}
	if f.Calls[1].Env["GIT_ALLOW_PROTOCOL"] != "" || f.Calls[1].Env == nil {
		t.Errorf("status env %v is not Local's", f.Calls[1].Env)
	}
}
