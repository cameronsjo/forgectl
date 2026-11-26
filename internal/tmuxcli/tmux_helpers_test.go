// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package tmuxcli

import (
	"sort"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func keysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func liveServer() *exec.FakeRunner {
	const sep = "\x1f"
	return &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "list-sessions":
			return strings.Join([]string{"123", "456", "$1", "main", "1", "1", "1700000000", "/w"}, sep), nil
		case "list-windows":
			return strings.Join([]string{"123", "456", "@3", "$1", "main", "0", "editor", "1", "1"}, sep), nil
		}
		return "", nil
	}}
}
