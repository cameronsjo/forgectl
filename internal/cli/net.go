package cli

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	netpkg "github.com/cameronsjo/forgectl/internal/net"
)

// newNetCmd builds `forgectl net` — mirrors newLaunchCmd/newWorkflowCmd in
// building its own exec.Runner rather than sharing the tmux/projects client
// lifecycle (net has no TUI-side consumer).
func newNetCmd(cfg config.Config) *cobra.Command {
	client := netpkg.New(exec.OSRunner{}, netpkg.WithNetConfig(cfg.Net))
	return newNetCmdForClient(client)
}

// newNetCmdForClient builds the command over an already-constructed client —
// split out so tests can inject a fake-wired *net.Client (mirrors
// newProjectsListCmd(client)) without changing newNetCmd's cfg-based signature.
func newNetCmdForClient(client *netpkg.Client) *cobra.Command {
	var refresh bool
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "net",
		Short: "Check cached internal-network reachability",
		Long: `net reports whether the configured internal endpoint (the [net] section
of config.toml) answers a TCP probe, caching the answer for net.ttl_seconds
(default 60s) so repeated calls don't re-dial on every invocation.

  forgectl net             show the cached (or freshly probed) answer
  forgectl net --refresh   force a new probe, bypassing the cache
  forgectl net --json      machine-readable output for scripting`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			var (
				status netpkg.Status
				err    error
			)
			if refresh {
				status, err = client.Refresh(ctx)
			} else {
				status, err = client.Status(ctx)
			}
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			age := time.Since(status.CheckedAt).Round(time.Second)

			if asJSON {
				enc := json.NewEncoder(out)
				return enc.Encode(netStatusJSON{
					Reachable:  status.Reachable,
					CheckedAt:  status.CheckedAt,
					AgeSeconds: int(age.Seconds()),
				})
			}

			word := "unreachable"
			if status.Reachable {
				word = "reachable"
			}
			fmt.Fprintf(out, "%s (checked %s ago)\n", word, age)
			return nil
		},
	}
	cmd.Flags().BoolVar(&refresh, "refresh", false, "force a fresh probe, bypassing the cache")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON to stdout")
	return cmd
}

// netStatusJSON is the --json wire shape for `forgectl net`.
type netStatusJSON struct {
	Reachable  bool      `json:"reachable"`
	CheckedAt  time.Time `json:"checkedAt"`
	AgeSeconds int       `json:"ageSeconds"`
}
