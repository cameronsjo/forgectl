// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package herdrcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/charmbracelet/fang"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

const (
	exitFailed = 1
	exitUsage  = 2
)

// Execute is the entrypoint for the standalone forgectl-herdr plugin binary.
func Execute(ctx context.Context) error {
	env, err := config.CaptureEnvSnapshot()
	if err != nil {
		fmt.Fprintln(os.Stderr, safeText(err.Error()))
		return WithExitCode(err, exitUsage)
	}
	legacyBoundary, err := config.PrepareLegacyMigrationBoundary(env, config.NativeMigrationFS())
	if err != nil {
		fmt.Fprintln(os.Stderr, safeText(err.Error()))
		return WithExitCode(err, exitFailed)
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

	root := newHerdrCmd(deps)
	root.Version = meta.Version
	root.SilenceUsage = true
	root.SilenceErrors = true

	if decErr := cfg.DecodeError(); decErr != nil && !asksForHelpOrVersion(os.Args[1:]) {
		fmt.Fprintln(os.Stderr, safeText(decErr.Error()))
		return WithExitCode(decErr, exitUsage)
	}

	return fang.Execute(
		ctx,
		root,
		fang.WithVersion(meta.Version),
		fang.WithErrorHandler(herdrErrorHandler),
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

func herdrErrorHandler(w io.Writer, styles fang.Styles, err error) {
	var silent *silentCodedError
	if chainAs(err, &silent) {
		return
	}
	fang.DefaultErrorHandler(w, styles, termsafe.Error(err))
}

const textMaxRunes = 1280

func safeText(s string) string {
	return termsafe.SafeLineMax(s, textMaxRunes)
}

func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

func jsonVerdict(err error, asJSON bool) error {
	if err == nil || !asJSON {
		return err
	}
	return &silentCodedError{code: ExitCode(err)}
}

type codedError struct {
	err  error
	code int
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }
func (e *codedError) ExitCode() int { return e.code }

// WithExitCode wraps err so ExitCode(err) reports code instead of 1.
func WithExitCode(err error, code int) error {
	if err == nil {
		return nil
	}
	return &codedError{err: err, code: code}
}

type silentCodedError struct {
	code int
}

func (e *silentCodedError) Error() string { return "" }
func (e *silentCodedError) ExitCode() int { return e.code }

// ExitCode walks err's chain for a codedError or silentCodedError and returns
// its code, or 1 when err carries none.
func ExitCode(err error) int {
	var coded *codedError
	if chainAs(err, &coded) {
		return coded.ExitCode()
	}
	var silent *silentCodedError
	if chainAs(err, &silent) {
		return silent.ExitCode()
	}
	return 1
}

func chainAs[T any](err error, target *T) (found bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Error chain walk panicked; treating the target as absent.",
				"error_type", fmt.Sprintf("%T", err), "target_type", fmt.Sprintf("%T", target), "panic_type", fmt.Sprintf("%T", r))
			found = false
		}
	}()
	return errors.As(err, target)
}
