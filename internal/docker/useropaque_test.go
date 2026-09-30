package docker

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/redact/redacttest"
)

// realDockerClient returns a Client over the real exec Runner whose `docker`
// is a stub that exits 3, and the buffer the debug log goes to.
func realDockerClient(t *testing.T) (*Client, *bytes.Buffer) {
	t.Helper()
	bin := t.TempDir()
	stub := filepath.Join(bin, "docker")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil { //nolint:gosec // G306: the stub must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return New(exec.OSRunner{}, WithLastTagPath(filepath.Join(t.TempDir(), "last-tag.json"))), &logs
}

// assertNoSecret fails when the debug log or err carries the corpus secret,
// and when the Runner never logged the docker argv (so the case proved
// nothing).
func assertNoSecret(t *testing.T, argv []string, logs *bytes.Buffer, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%q: expected the stub docker to fail", argv)
	}
	if !strings.Contains(logs.String(), "Preparing to run interactive command.") {
		t.Fatalf("%q: the Runner logged no argv, so the case proves nothing:\n%s", argv, logs.String())
	}
	if strings.Contains(logs.String(), redacttest.Secret) || strings.Contains(err.Error(), redacttest.Secret) {
		t.Errorf("%q: credential rendered:\nerror: %v\nlog:\n%s", argv, err, logs.String())
	}
	logs.Reset()
}

// The #749 review corpus through Build's ExtraArgs, Run's container args and
// Shell's command, on the real Runner: no row's secret reaches the debug log
// or the error.
//
// Mutation: drop the exec.WithOpaqueArgs line from Build (or Run, or Shell)
// and that subtest goes red.
func TestUserArgv_NeverRendersTheCorpusSecret(t *testing.T) {
	t.Run("build", func(t *testing.T) {
		c, logs := realDockerClient(t)
		for _, argv := range redacttest.Corpus {
			_, err := c.Build(context.Background(), BuildOptions{ContextDir: t.TempDir(), ExtraArgs: argv[1:]})
			assertNoSecret(t, argv, logs, err)
		}
	})
	t.Run("run", func(t *testing.T) {
		c, logs := realDockerClient(t)
		for _, argv := range redacttest.Corpus {
			assertNoSecret(t, argv, logs, c.Run(context.Background(), RunOptions{Tag: "img:1", Args: argv}))
		}
	})
	t.Run("shell", func(t *testing.T) {
		c, logs := realDockerClient(t)
		n := 0
		for _, argv := range redacttest.Corpus {
			for _, a := range argv {
				// An option-like shell is refused before the Runner runs.
				if !strings.Contains(a, redacttest.Secret) || strings.HasPrefix(a, "-") {
					continue
				}
				n++
				assertNoSecret(t, []string{a}, logs, c.Shell(context.Background(), ShellOptions{Tag: "img:1", Shell: a}))
			}
		}
		if n < 40 {
			t.Fatalf("only %d corpus elements reached Shell", n)
		}
	})
}
