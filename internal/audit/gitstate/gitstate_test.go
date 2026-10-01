package gitstate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/audit"
	fexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv"
)

// TestStatus_Argv pins the one git call: hardened through gitenv.Local (its
// global options lead the argv and its environment pins are set), -C the
// repo, ls-files with -z -t --cached --others --exclude-standard, and every
// path a :(literal) pathspec after --.
//
// Mutations that turn it red: run it under gitenv.Transport; drop -z, -t or
// --exclude-standard; drop the :(literal) prefix or the "--".
func TestStatus_Argv(t *testing.T) {
	fr := &fexec.FakeRunner{RunFunc: func(string, []string) (string, error) { return "", nil }}
	if _, err := Status(context.Background(), fr, "/p/repo", []string{".env", "a/*[x]/.env"}); err != nil {
		t.Fatal(err)
	}
	if len(fr.Calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(fr.Calls))
	}
	c := fr.Calls[0]
	want := append(gitenv.Args(gitenv.Local), "-C", "/p/repo",
		"ls-files", "-z", "-t", "--cached", "--others", "--exclude-standard", "--",
		":(literal).env", ":(literal)a/*[x]/.env")
	if c.Name != gitenv.Bin || !slices.Equal(c.Args, want) {
		t.Errorf("argv = %s %q\nwant  %s %q", c.Name, c.Args, gitenv.Bin, want)
	}
	if v, ok := c.Env["GIT_ALLOW_PROTOCOL"]; !ok || v != "" {
		t.Errorf("env = %v, want gitenv.Local's GIT_ALLOW_PROTOCOL= pin", c.Env)
	}
}

// TestStatus_ParsesTags: "H" (and any other index tag) is tracked, "?" is
// untracked, and an omitted path is ignored. A name holding a newline or a
// space survives the -z split.
func TestStatus_ParsesTags(t *testing.T) {
	out := "H .env\x00? sub/.env\x00C mod/.env\x00? odd\nname/.env\x00H both\x00? both\x00"
	fr := &fexec.FakeRunner{RunFunc: func(string, []string) (string, error) { return out, nil }}
	got, err := Status(context.Background(), fr, "/r", []string{".env", "sub/.env", "mod/.env", "odd\nname/.env", "both", "gone"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]audit.GitState{
		".env": audit.GitTracked, "sub/.env": audit.GitUntracked, "mod/.env": audit.GitTracked,
		"odd\nname/.env": audit.GitUntracked, "both": audit.GitTracked,
	}
	if len(got) != len(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%q = %v, want %v", k, got[k], v)
		}
	}
}

// TestStatus_Batches: past the batch bounds the paths split across calls,
// each path asked exactly once, and a failed batch fails the whole call.
//
// Mutation that turns it red: return the partial map on a batch error.
func TestStatus_Batches(t *testing.T) {
	var rels []string
	for i := 0; i < maxBatchPaths*2+5; i++ {
		rels = append(rels, fmt.Sprintf("d%05d/.env", i))
	}
	rels = append(rels, strings.Repeat("x", maxBatchBytes)+"/.env") // rides alone
	var asked []string
	fr := &fexec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		i := slices.Index(args, "--")
		n := 0
		for _, a := range args[i+1:] {
			asked = append(asked, strings.TrimPrefix(a, ":(literal)"))
			n += len(a) + 1
		}
		if len(args[i+1:]) > maxBatchPaths || (len(args[i+1:]) > 1 && n > maxBatchBytes) {
			t.Errorf("batch of %d paths, %d bytes, past the bounds", len(args[i+1:]), n)
		}
		return "", nil
	}}
	if _, err := Status(context.Background(), fr, "/r", rels); err != nil {
		t.Fatal(err)
	}
	if len(fr.Calls) < 4 || !slices.Equal(asked, rels) {
		t.Errorf("calls=%d, asked %d paths in order? %v; want every path once across >=4 batches", len(fr.Calls), len(asked), slices.Equal(asked, rels))
	}

	n := 0
	failing := &fexec.FakeRunner{RunFunc: func(string, []string) (string, error) {
		n++
		if n == 2 {
			return "", errors.New("boom")
		}
		return "H d00000/.env\x00", nil
	}}
	if got, err := Status(context.Background(), failing, "/r", rels); err == nil || got != nil {
		t.Errorf("a failed batch returned %v, %v; want nil and the error", got, err)
	}
}

// TestStatus_LiveGit runs the real git, hardened, against a repo with a
// tracked, an untracked, and an ignored .env, and a name holding glob
// characters, which :(literal) must match only as itself.
func TestStatus_LiveGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...) //nolint:gosec,noctx // G204: test setup running git with fixed arguments
		cmd.Env = gitenv.Env(gitenv.Local, os.Environ())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel string) {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	write(".gitignore")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".env.local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	write(".env")
	write(".env.local")
	write("new/.env")
	write("g/[a]/.env")
	write("g/a/.env")
	run("add", ".env", ".gitignore", "g/a/.env")
	run("commit", "-qm", "init")
	got, err := Status(context.Background(), fexec.OSRunner{}, repo, []string{".env", ".env.local", "new/.env", "g/[a]/.env"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]audit.GitState{".env": audit.GitTracked, "new/.env": audit.GitUntracked, "g/[a]/.env": audit.GitUntracked}
	if len(got) != len(want) {
		t.Errorf("got %v, want %v (.env.local ignored, g/a/.env never asked)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%q = %v, want %v", k, got[k], v)
		}
	}
}
