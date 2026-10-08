// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/skill"
)

// skillExitUsage is the exit code for a refused --skill invocation.
const skillExitUsage = 2

// addSkillFlags declares the root's --skill and --install flags. The root's
// RunE (rootRun) acts on them; real flags put --skill in --help and keep
// `--install` from parsing as a verb.
func addSkillFlags(root *cobra.Command) {
	root.Flags().Bool("skill", false, "print the agent skill (--install <dir> writes it)")
	// Hidden: --skill's description names it, and --help is held to a size bound
	// (forgectl#1086) a second flag row would break.
	root.Flags().String("install", "", "with --skill: write the skill to this absolute directory")
	_ = root.Flags().MarkHidden("install")
}

// runRootSkill handles `forgectl --skill [--install <dir>]`. It reports
// handled=false when --skill is absent, so the root falls through to its usual
// help. --install without --skill is a usage error rather than a silent no-op.
func runRootSkill(cmd *cobra.Command, _ []string) (handled bool, err error) {
	wantSkill, _ := cmd.Flags().GetBool("skill")
	installDir, _ := cmd.Flags().GetString("install")
	installSet := cmd.Flags().Changed("install")

	if !wantSkill {
		if installSet {
			return true, WithExitCode(fmt.Errorf("--install only works with --skill: forgectl --skill --install <absolute-dir>"), skillExitUsage)
		}
		return false, nil
	}
	if !installSet {
		_, err := fmt.Fprint(cmd.OutOrStdout(), skill.Text())
		return true, err
	}
	if installDir == "" {
		return true, WithExitCode(fmt.Errorf("--install needs a directory: forgectl --skill --install <absolute-dir>"), skillExitUsage)
	}
	_, err = skill.Install(installDir)
	if err != nil {
		return true, WithExitCode(fmt.Errorf("%s", safeText(err.Error())), skillExitUsage)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "installed forgectl skill to %s\n", safeText(installDir))
	return true, err
}

// rootRun is the root command's RunE: the skill flags first, else the usual
// help-and-fail that keeps a headless bare invoke from reading as success.
func rootRun(cmd *cobra.Command, args []string) error {
	if handled, err := runRootSkill(cmd, args); handled {
		return err
	}
	return showRootHelp(cmd, args)
}
