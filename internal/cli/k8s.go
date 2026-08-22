package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	forgexec "github.com/cameronsjo/forgectl/internal/exec"
	k8spkg "github.com/cameronsjo/forgectl/internal/k8s"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

var k8sModule = module.Manifest{
	Name: "k8s",
	Tier: module.TierExtension,
	New:  newK8sCmd,
}

// k8sOutputIsTerminal inspects Cobra's actual output sink. Tests replace the
// seam; production does not assume os.Stdout if an embedder redirected it.
var k8sOutputIsTerminal = func(out io.Writer) bool {
	fdWriter, ok := out.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(fdWriter.Fd()))
}

func newK8sCmd(deps module.Deps) *cobra.Command {
	streamer, _ := deps.Runner.(forgexec.StreamingRunner)
	return newK8sCmdForClient(k8spkg.New(streamer))
}

func newK8sCmdForClient(client *k8spkg.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "k8s",
		Short: "Focused Kubernetes helpers",
		Long: `k8s provides small wrappers around ordinary kubectl arguments. It does not
define deployment manifests, choose pods, or manage cluster configuration.`,
	}
	cmd.AddCommand(newK8sLogsCmd(client))
	return cmd
}

type k8sLogsInvocation struct {
	level       k8spkg.Level
	color       string
	kubectlArgs []string
	help        bool
}

func newK8sLogsCmd(client *k8spkg.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs [forgectl flags] <kubectl logs args...>",
		Short: "Stream terminal-safe kubectl logs with severity filtering",
		Long: `logs forwards ordinary resource, selector, namespace, container, and follow
arguments to kubectl logs, then safely streams its output. Recognized top-level
JSON "level" or "severity" values are colorized on a terminal and can be
filtered by a floor. Plain text and unrecognized JSON always pass through.

  forgectl k8s logs deployment/api -f --all-containers
  forgectl k8s logs -n prod -l app=api -f --log-level warn
  forgectl k8s logs --color always pod/api

forgectl consumes --log-level and --color wherever they appear before the
first --. Use -- to pass a same-named flag to kubectl. Color defaults to auto
and is disabled when stdout is not a terminal or NO_COLOR is present.`,
		// kubectl owns almost the entire flag vocabulary, so Cobra must not
		// reject or reorder those tokens. parseK8sLogsArgs removes only the two
		// documented forgectl flags and forwards everything else exactly.
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(cmd *cobra.Command, args []string) error {
			invocation, err := parseK8sLogsArgs(args)
			if err != nil {
				return err
			}
			if invocation.help {
				return cmd.Help()
			}

			color := invocation.color == "always"
			if invocation.color == "auto" {
				_, noColor := os.LookupEnv("NO_COLOR")
				color = !noColor && k8sOutputIsTerminal(cmd.OutOrStdout())
			}

			err = client.Logs(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), invocation.kubectlArgs, k8spkg.LogsOptions{
				MinLevel: invocation.level,
				Color:    color,
			})
			if err == nil || errors.Is(err, cmd.Context().Err()) {
				return err
			}
			var commandErr *forgexec.CommandError
			if errors.As(err, &commandErr) && commandErr.ExitCode > 0 {
				return WithExitCode(err, commandErr.ExitCode)
			}
			return err
		},
	}
	return cmd
}

func parseK8sLogsArgs(args []string) (k8sLogsInvocation, error) {
	invocation := k8sLogsInvocation{level: k8spkg.LevelTrace, color: "auto"}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		invocation.help = true
		return invocation, nil
	}
	recognizeHelperFlags := true
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if recognizeHelperFlags && arg == "--" {
			recognizeHelperFlags = false
			continue
		}
		if recognizeHelperFlags && (arg == "--log-level" || arg == "--color") {
			if i+1 >= len(args) {
				return invocation, fmt.Errorf("%s requires a value", termsafe.SafeLine(arg))
			}
			i++
			if err := setK8sLogsHelperFlag(&invocation, arg, args[i]); err != nil {
				return invocation, err
			}
			continue
		}
		if recognizeHelperFlags {
			if name, value, ok := strings.Cut(arg, "="); ok && (name == "--log-level" || name == "--color") {
				if err := setK8sLogsHelperFlag(&invocation, name, value); err != nil {
					return invocation, err
				}
				continue
			}
		}
		invocation.kubectlArgs = append(invocation.kubectlArgs, arg)
	}
	if len(invocation.kubectlArgs) == 0 {
		return invocation, errors.New("kubectl logs requires a resource or selector argument")
	}
	return invocation, nil
}

func setK8sLogsHelperFlag(invocation *k8sLogsInvocation, name, value string) error {
	switch name {
	case "--log-level":
		level, err := k8spkg.ParseLevel(value)
		if err != nil {
			return err
		}
		invocation.level = level
		return nil
	case "--color":
		switch value {
		case "auto", "always", "never":
			invocation.color = value
			return nil
		default:
			return fmt.Errorf("unknown color mode %s (want auto, always, or never)", termsafe.QuoteText(value))
		}
	default:
		return fmt.Errorf("unknown forgectl k8s logs flag %s", termsafe.QuoteText(name))
	}
}
