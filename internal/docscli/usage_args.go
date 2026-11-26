// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package docscli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

func usageArgError(cmd *cobra.Command, args []string, err error) error {
	if err == nil || !isCountError(err) {
		return err
	}
	path := strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
	required, optional := usageShape(cmd.Use)
	usage := "usage: " + cmd.UseLine()
	most := len(required) + len(optional)
	switch {
	case len(args) < len(required):
		return fmt.Errorf("%s: missing %s; %s", path, strings.Join(required[len(args):], " "), usage)
	case most == 0 && len(args) > 0:
		return fmt.Errorf("%s: takes no arguments, got %s; %s", path, termsafe.QuoteArgMax(args[0], termsafe.ArgEchoMaxRunes), usage)
	case len(args) > most && !usageVariadic(cmd.Use) && len(optional) > 0:
		return fmt.Errorf("%s: takes at most %s, got %d; %s", path, plural(most, "argument", "arguments"), len(args), usage)
	case len(args) > most && !usageVariadic(cmd.Use):
		return fmt.Errorf("%s: unexpected argument %s after %s; %s", path, termsafe.QuoteArgMax(args[most], termsafe.ArgEchoMaxRunes), strings.Join(required, " "), usage)
	}
	return fmt.Errorf("%s: wrong number of arguments (%d); %s", path, len(args), usage)
}

func isCountError(err error) bool {
	var coded *codedError
	var silent *silentCodedError
	if errors.As(err, &coded) || errors.As(err, &silent) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "arg(s)") || strings.HasPrefix(msg, "unknown command ")
}

func usageShape(use string) (required, optional []string) {
	fields := strings.Fields(use)
	if len(fields) == 0 {
		return nil, nil
	}
	prev := ""
	depth := 0
	for _, f := range fields[1:] {
		if f == "|" {
			break
		}
		opens := strings.Count(f, "[")
		switch {
		case depth == 0 && strings.HasPrefix(prev, "-"):
		case depth == 0 && opens == 0 && isPlaceholder(f):
			required = append(required, f)
		case depth == 0 && opens > 0 && strings.HasPrefix(f, "[") && !strings.HasPrefix(f, "[-"):
			optional = append(optional, f)
		}
		depth += opens - strings.Count(f, "]")
		prev = f
	}
	return required, optional
}

func usageVariadic(use string) bool { return strings.Contains(use, "...") }

func isPlaceholder(tok string) bool {
	if strings.HasPrefix(tok, "<") {
		return true
	}
	if len(tok) < 2 {
		return false
	}
	for _, r := range tok {
		if (r < 'A' || r > 'Z') && r != '_' {
			return false
		}
	}
	return true
}

func plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return strconv.Itoa(n) + " " + singular
	}
	return strconv.Itoa(n) + " " + pluralForm
}
