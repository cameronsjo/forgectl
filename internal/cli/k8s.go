package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
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
	cmd.AddCommand(newK8sExecCmd(runner))
	cmd.AddCommand(newK8sInspectCmd(runner))
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
				namespace := strings.TrimSpace(args[0])
				if namespace == "" {
					return errors.New("namespace must not be empty")
				}
				_, err := runner.Run(cmd.Context(), "kubectl", "config", "set-context", "--current", "--namespace="+namespace)
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

// wrapK8sCommandError opts every k8s subcommand into kubectl's real exit
// code. Run and RunStreaming fail with a *forgexec.CommandError, handled by
// the first branch. RunInteractive (exec's seam) returns cmd.Run()'s error
// unwrapped — a bare *exec.ExitError — so the second branch unwraps that
// directly rather than expecting a shape RunInteractive never produces.
func wrapK8sCommandError(err error) error {
	if err == nil {
		return nil
	}
	var commandErr *forgexec.CommandError
	if errors.As(err, &commandErr) && commandErr.ExitCode > 0 {
		return WithExitCode(err, commandErr.ExitCode)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return WithExitCode(err, exitErr.ExitCode())
	}
	return err
}

// k8sWorkloadPattern is the charset kubectl itself accepts for a resource
// kind or name (a DNS-1123-ish subdomain segment). It is the second half of
// validateK8sWorkload's check — the first half (rejecting a leading '-'
// before any parsing) is what actually closes the flag-injection path; this
// pattern additionally keeps a validated name safe to interpolate into the
// events field selector below (no comma, no '=').
var k8sWorkloadPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// validateK8sWorkload parses and validates a "<kind>/<name>" workload
// reference before any part of it reaches kubectl argv or the events field
// selector. A check that only confirms "contains a slash, has a name" would
// accept a value like "--server=https://attacker.example/x" — kubectl then
// parses that as its own global flag, redirecting the call (and its bearer
// token) to an attacker-chosen server. Rejecting a leading '-' outright, and
// constraining both halves to kubectl's own charset, closes that off.
func validateK8sWorkload(workload string) (name string, err error) {
	invalid := fmt.Errorf("workload reference %s must be kind/name (e.g. deployment/api)", termsafe.QuoteText(workload))
	if strings.HasPrefix(workload, "-") {
		return "", invalid
	}
	kind, name, ok := strings.Cut(workload, "/")
	if !ok || !k8sWorkloadPattern.MatchString(kind) || !k8sWorkloadPattern.MatchString(name) {
		return "", invalid
	}
	return name, nil
}

// newK8sExecCmd wraps `kubectl exec` argv verbatim. Unlike logs, exec has no
// forgectl-owned flags to recognize: an interactive exec session is kubectl's
// terminal to own, not forgectl's to filter or colorize, so the child gets
// the real stdin/stdout/stderr via RunInteractive rather than the captured
// streams termsafe and severity filtering rely on elsewhere in this file.
func newK8sExecCmd(runner forgexec.Runner) *cobra.Command {
	return &cobra.Command{
		Use:   "exec <kubectl exec args...>",
		Short: "Run kubectl exec with the terminal wired through directly",
		Long: `exec forwards its arguments to kubectl exec unchanged and hands the child
process the real stdin, stdout, and stderr — required for an interactive shell
or TTY session. Output is not passed through forgectl's terminal-safety or
severity filtering: kubectl owns the terminal for the duration of the call.

  forgectl k8s exec -it pod/api -- sh
  forgectl k8s exec pod/api -c sidecar -- cat /etc/hostname

There are no forgectl-owned flags to consume; every argument forwards to
kubectl exec exactly as given, and every argument is operator-trusted:
forgectl does not vet it, and a global kubectl flag carrying a credential
(--token, a bearer URL) may surface in forgectl's error output or debug logs
on failure. Prefer a kubeconfig context over passing credentials as flags.`,
		// kubectl owns the entire flag vocabulary here, including flags that
		// collide with Cobra's own (-c, -i, -t), so Cobra must not parse them.
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
				return cmd.Help()
			}
			if len(args) == 0 {
				return errors.New("kubectl exec requires a resource and command")
			}
			kubectlArgs := append([]string{"exec"}, args...)
			err := runner.RunInteractive(cmd.Context(), "kubectl", kubectlArgs...)
			return wrapK8sCommandError(err)
		},
	}
}

// newK8sInspectCmd runs the describe/get/events triple issue #399 proposes
// against a single workload, in the shape of one call rather than three. It
// stays inside the same boundary as logs and exec: the caller names a
// "<kind>/<name>" workload (kubectl's own reference format), and inspect
// invents nothing about which pod that resolves to.
func newK8sInspectCmd(runner forgexec.Runner) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <kind>/<name> [kubectl flags...]",
		Short: "Run describe, get -o wide, and events for one workload",
		Long: `inspect runs the describe/get/events triple against a single workload in one
call: kubectl describe, kubectl get -o wide, and kubectl get events filtered
to that workload's name. The first argument must be a "<kind>/<name>"
reference (e.g. deployment/api); any remaining arguments (namespace, context,
...) are forwarded to all three kubectl calls unchanged.

Forwarded arguments are operator-trusted input, never externally-derived
data — forgectl does not vet them, and a global kubectl flag carrying a
credential may surface in forgectl's error output or debug logs on failure.

  forgectl k8s inspect deployment/api
  forgectl k8s inspect pod/api-7f6c9 -n prod`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
				return cmd.Help()
			}
			if len(args) == 0 {
				return errors.New("kubectl inspect requires a kind/name workload reference")
			}
			workload := args[0]
			name, err := validateK8sWorkload(workload)
			if err != nil {
				return err
			}
			return runK8sInspectTriple(cmd.Context(), runner, cmd.OutOrStdout(), workload, name, args[1:])
		},
	}
}

// runK8sInspectTriple issues describe, get -o wide, and get events in that
// fixed order, stopping at the first failure so a caller never sees events
// for a workload whose describe just failed to resolve.
func runK8sInspectTriple(ctx context.Context, runner forgexec.Runner, out io.Writer, workload, name string, extra []string) error {
	describeArgs := append([]string{"describe", workload}, extra...)
	if err := runK8sInspectSection(ctx, runner, out, "describe", describeArgs); err != nil {
		return err
	}

	getArgs := append([]string{"get", workload, "-o", "wide"}, extra...)
	if err := runK8sInspectSection(ctx, runner, out, "get -o wide", getArgs); err != nil {
		return err
	}

	eventsArgs := append([]string{"get", "events", "--field-selector", "involvedObject.name=" + name}, extra...)
	return runK8sInspectSection(ctx, runner, out, "events", eventsArgs)
}

// runK8sInspectSection runs one kubectl call, captures its stdout, and
// prints it under a labeled banner with every line passed through
// termsafe.SafeLine — inspect captures rather than streams, so it uses the
// ordinary Runner.Run seam instead of exec/logs's StreamingRunner.
func runK8sInspectSection(ctx context.Context, runner forgexec.Runner, out io.Writer, title string, args []string) error {
	result, err := runner.Run(ctx, "kubectl", args...)
	if err != nil {
		return wrapK8sCommandError(err)
	}
	if _, err := fmt.Fprintf(out, "== %s ==\n", title); err != nil {
		return err
	}
	return writeK8sSafeLines(out, result)
}

// writeK8sSafeLines prints text one physical line at a time so real line
// breaks survive while each line's content is rendered through
// termsafe.SafeLine — SafeLine itself collapses a multi-line string onto one
// inert line, which is right for the single-value ns command above but wrong
// for a multi-line describe/get/events block.
func writeK8sSafeLines(out io.Writer, text string) error {
	if text == "" {
		return nil
	}
	for _, line := range strings.Split(text, "\n") {
		if _, err := fmt.Fprintln(out, termsafe.SafeLine(line)); err != nil {
			return err
		}
	}
	return nil
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
