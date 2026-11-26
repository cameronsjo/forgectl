// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package docscli

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const jsonErrTextUnavailable = "error text unavailable: its Error method panicked"

func jsonErrorText(err error) (text string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Error method panicked; its text is withheld.",
				"error_type", fmt.Sprintf("%T", err), "panic_type", fmt.Sprintf("%T", r))
			text = jsonErrTextUnavailable
		}
	}()
	return err.Error()
}

func jsonFlagParsed(cmd *cobra.Command) bool {
	if cmd.Flags().Lookup("json") == nil {
		return false
	}
	v, err := cmd.Flags().GetBool("json")
	return err == nil && v
}

func jsonVerdict(err error, asJSON bool) error {
	if err == nil || !asJSON {
		return err
	}
	return newSilentCodedError(ExitCode(err))
}

var jsonOSArgs = func() []string { return os.Args[1:] }

func argvWantsJSON(cmd *cobra.Command) bool {
	return argvWantsJSONIn(cmd, jsonOSArgs())
}

func argvWantsJSONIn(cmd *cobra.Command, args []string) bool {
	if cmd.Flags().Lookup("json") == nil {
		return false
	}
	seen, want := false, false
scan:
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			break scan
		case a == "--json":
			seen, want = true, true
		case strings.HasPrefix(a, "--json="):
			if v, err := strconv.ParseBool(strings.TrimPrefix(a, "--json=")); err == nil {
				seen, want = true, v
			}
		case takesSeparateValue(cmd, a):
			i++
		}
	}
	if seen {
		return want
	}
	return jsonFlagParsed(cmd)
}

func takesSeparateValue(cmd *cobra.Command, a string) bool {
	switch {
	case strings.HasPrefix(a, "--"):
		if len(a) == 2 || strings.Contains(a, "=") {
			return false
		}
		f := lookupFlag(cmd, a[2:])
		return f != nil && f.NoOptDefVal == ""
	case len(a) > 1 && a[0] == '-':
		return shorthandGroupTakesNext(cmd, a[1:])
	}
	return false
}

func shorthandGroupTakesNext(cmd *cobra.Command, group string) bool {
	for i := 0; i < len(group); i++ {
		f := lookupShorthand(cmd, group[i:i+1])
		if f == nil {
			return false
		}
		if f.NoOptDefVal == "" {
			return i == len(group)-1
		}
	}
	return false
}

func lookupFlag(cmd *cobra.Command, name string) *pflag.Flag {
	if f := cmd.Flags().Lookup(name); f != nil {
		return f
	}
	return cmd.InheritedFlags().Lookup(name)
}

func lookupShorthand(cmd *cobra.Command, ch string) *pflag.Flag {
	if f := cmd.Flags().ShorthandLookup(ch); f != nil {
		return f
	}
	return cmd.InheritedFlags().ShorthandLookup(ch)
}
