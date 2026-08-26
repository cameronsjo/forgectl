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
	return newK8sCmdForClient(k8spkg.New(streamer), deps.Runner)
}

func newK8sCmdForClient(client *k8spkg.Client, runner forgexec.Runner) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "k8s",
		Short: "Focused Kubernetes helpers",
		Long: `k8s provides small wrappers around ordinary kubectl arguments. It does not
define deployment manifests, choose pods, or manage cluster configuration beyond
reading and switching the current context's namespace (ns).`,
	}
	cmd.AddCommand(newK8sLogsCmd(client))
	cmd.AddCommand(newK8sNsCmd(runner))
	return cmd
}

// newK8sNsCmd wraps the current-context namespace: no args reads it (falling
// back to "default" the same way kubectl itself treats an unset namespace),
// one arg sets it. It is deliberately narrower than the logs command above —
// plain cobra flag parsing, no forgectl-owned flags — because both kubectl
// invocations it wraps take a single, unambiguous argument.
func newK8sNsCmd(runner forgexec.Runner) *cobra.Command {
	return &cobra.Command{
		Use:   "ns [namespace]",
		Short: "Get or set the current kubectl context's namespace",
		Long: `ns reports the current context's namespace, or switches it when given one.

  forgectl k8s ns          print the current namespace (default when unset)
  forgectl k8s ns staging  switch the current context to the staging namespace`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				_, err := runner.Run(cmd.Context(), "kubectl", "config", "set-context", "--current", "--namespace="+args[0])
				return wrapK8sCommandError(err)
			}
			out, err := runner.Run(cmd.Context(), "kubectl", "config", "view", "--minify", "-o", "jsonpath={..namespace}")
			if err != nil {
				return wrapK8sCommandError(err)
			}
			namespace := strings.TrimSpace(out)
			if namespace == "" {
				namespace = "default"
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), termsafe.SafeLine(namespace))
			return err
		},
	}
}

// wrapK8sCommandError opts `ns` into kubectl's real exit code, mirroring the
// logs command's handling in newK8sLogsCmd's RunE above.
func wrapK8sCommandError(err error) error {
	if err == nil {
		return nil
	}
	var commandErr *forgexec.CommandError
	if errors.As(err, &commandErr) && commandErr.ExitCode > 0 {
		return WithExitCode(err, commandErr.ExitCode)
	}
	return err
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
			return wrapK8sCommandError(err)
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
