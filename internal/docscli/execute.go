// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package docscli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/fang"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Execute is the entrypoint for the standalone forgectl-docs plugin binary.
func Execute(ctx context.Context) error {
	var legacyBoundary *config.LegacyMigrationBoundary
	env, err := config.CaptureEnvSnapshot()
	if err != nil {
		fmt.Fprintln(os.Stderr, safeText(err.Error()))
		return WithExitCode(err, 2)
	}
	legacyBoundary, err = config.PrepareLegacyMigrationBoundary(env, config.NativeMigrationFS())
	if err != nil {
		fmt.Fprintln(os.Stderr, safeText(err.Error()))
		return WithExitCode(err, 1)
	}
	defer legacyBoundary.Close() //nolint:errcheck

	var cfg config.Config
	if !errors.Is(legacyBoundary.Refusal, config.ErrLegacyPathControl) {
		cfg = config.LoadPath(legacyBoundary.ConfigPath)
	}
	closer := config.SetupLogger(cfg)
	defer closer.Close() //nolint:errcheck

	deps := module.Deps{
		Cfg:            cfg,
		Runner:         exec.OSRunner{},
		LegacyBoundary: legacyBoundary,
	}

	root := newDocsCmd(deps)
	root.Version = meta.Version
	root.SilenceUsage = true
	root.SilenceErrors = true

	if decErr := cfg.DecodeError(); decErr != nil && !asksForHelpOrVersion(os.Args[1:]) {
		// Honor --json error contract when config.toml fails to parse.
		args := os.Args[1:]
		if sub, _, findErr := root.Find(args); findErr == nil && sub != nil && argvWantsJSONIn(sub, args) {
			return docsFail(sub, sub.CommandPath(), "", decErr, 2, true)
		}
		fmt.Fprintln(os.Stderr, safeText(decErr.Error()))
		return WithExitCode(decErr, 2)
	}

	return fang.Execute(
		ctx,
		root,
		fang.WithVersion(meta.Version),
		fang.WithErrorHandler(docsErrorHandler),
	)
}

func asksForHelpOrVersion(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		switch a {
		case "-h", "--help", "--version", "help":
			return true
		}
	}
	return false
}

func docsErrorHandler(w io.Writer, styles fang.Styles, err error) {
	var silent *silentCodedError
	if chainAs(err, &silent) {
		return
	}
	fang.DefaultErrorHandler(w, styles, termsafe.Error(err))
}
