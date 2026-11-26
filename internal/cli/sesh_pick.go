// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

var errSeshUnsafeCandidate = errors.New("refusing to hand sesh a name containing '#': sesh passes it to tmux unescaped, where tmux would expand #(...) as a command")

func seshPick(ctx context.Context, client *tmux.Client, name string) error {
	if strings.Contains(name, "#") {
		return fmt.Errorf("%w: %s", errSeshUnsafeCandidate, termsafe.QuotePath(name))
	}
	paths, err := seshResolvedPaths(name, os.UserHomeDir)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", errSeshUnsafeCandidate, termsafe.QuotePath(name), err)
	}
	for _, p := range paths {
		if strings.Contains(p, "#") {
			return fmt.Errorf("%w: %s resolves to a path containing '#'",
				errSeshUnsafeCandidate, termsafe.QuotePath(name))
		}
	}
	return client.Pick(ctx, name)
}

func seshResolvedPaths(name string, userHomeDir func() (string, error)) ([]string, error) {
	path := os.ExpandEnv(name)
	if strings.HasPrefix(path, "~") {
		home, err := userHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home directory: %w", termsafe.Error(err))
		}
		path = strings.Replace(path, "~", home, 1)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return []string{path}, nil
	}
	paths := []string{abs}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		paths = append(paths, resolved)
	}
	return paths, nil
}
