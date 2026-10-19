//go:build unix

package goal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/surface/goaltools"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

func (engine *execution) toolHandler(role *nativeRole, roots []string) goaltools.Handler {
	approvedRoots := append([]string(nil), roots...)
	return func(ctx context.Context, operation string, arguments json.RawMessage) (any, error) {
		state := engine.snapshot()
		if role.isClosed() {
			return nil, errors.New("native role has stopped")
		}
		if err := engine.record(Event{Time: time.Now().UTC(), Kind: "goal-tool", Role: role.Name, Summary: operation}); err != nil {
			return nil, err
		}
		switch operation {
		case "goal_exec":
			var input struct {
				Argv    []string `json:"argv"`
				Timeout int      `json:"timeout_seconds,omitempty"`
			}
			if goaltools.Decode(arguments, &input) != nil || len(input.Argv) < 1 || len(input.Argv) > 64 || input.Argv[0] == "" || strings.HasPrefix(input.Argv[0], "-") || input.Timeout < 0 || input.Timeout > 120 {
				return nil, errors.New("goal_exec requires bounded argv and at most 120 seconds")
			}
			for _, argument := range input.Argv {
				if len(argument) > 64*1024 || !utf8.ValidString(argument) || strings.ContainsRune(argument, 0) {
					return nil, errors.New("goal_exec argument exceeds its bound")
				}
			}
			if input.Timeout == 0 {
				input.Timeout = 60
			}
			return role.Executor.Exec(ctx, input.Argv, state.Worktree, approvedRoots, time.Duration(input.Timeout)*time.Second)
		case "goal_read", "goal_write":
			var input struct {
				Path    string  `json:"path"`
				Content *string `json:"content,omitempty"`
			}
			if goaltools.Decode(arguments, &input) != nil || (operation == "goal_read" && input.Content != nil) || (operation == "goal_write" && input.Content == nil) {
				return nil, errors.New("goal file arguments are invalid")
			}
			write := operation == "goal_write"
			if write && role.Name != "implementation" {
				return nil, errors.New("only the implementation role may write source")
			}
			path, err := resolveToolPath(state, input.Path, write)
			if err != nil {
				return nil, err
			}
			helperRoot := state.Worktree
			helperPath, err := filepath.Rel(state.Worktree, path)
			if !write && path == filepath.Join(state.RunDir, "result.patch") {
				helperRoot = state.RunDir
				helperPath = filepath.Base(path)
			}
			if err != nil || helperPath == "." || filepath.IsAbs(helperPath) {
				return nil, errors.New("goal file path could not be bound to its fixed root")
			}
			argv := []string{engine.self, "surface", "project", "goal", "_file", "--root", helperRoot, "--path", helperPath}
			if write {
				if !utf8.ValidString(*input.Content) || len(*input.Content) > 64*1024 || strings.ContainsRune(*input.Content, 0) {
					return nil, errors.New("goal_write content must be UTF-8 text of at most 64 KiB")
				}
				argv = append(argv, "--write", "--content", *input.Content)
			}
			result, err := role.Executor.Exec(ctx, argv, state.Worktree, approvedRoots, 15*time.Second)
			if err != nil {
				return nil, err
			}
			if result.ExitCode != 0 {
				return nil, fmt.Errorf("goal file operation was refused: %s", result.Stderr)
			}
			return map[string]any{"path": path, "content": result.Stdout, "written": write}, nil
		case "goal_mail":
			var input struct {
				To   string `json:"to"`
				Body string `json:"body"`
			}
			if goaltools.Decode(arguments, &input) != nil || engine.service == nil {
				return nil, errors.New("goal_mail arguments or mailbox are unavailable")
			}
			if _, err := messageKind(mail.Message{From: role.Name, To: input.To, Body: input.Body}, state.ID); err != nil {
				return nil, err
			}
			return engine.service.Queue(ctx, role.Name, input.To, input.Body, mail.PriorityNext)
		case "goal_inbox":
			if goaltools.Decode(arguments, &struct{}{}) != nil || engine.service == nil {
				return nil, errors.New("goal_inbox accepts no arguments")
			}
			return engine.service.Messages(func(entry mail.Entry) bool {
				return entry.Msg.To == role.Name && entry.Msg.ToRunID == role.Member.RunID
			})
		default:
			return nil, errors.New("unsupported goal tool")
		}
	}
}

func resolveToolPath(state State, requested string, write bool) (string, error) {
	if requested == "" || len(requested) > 4096 || !utf8.ValidString(requested) || strings.ContainsAny(requested, "\x00\r\n\\") {
		return "", errors.New("goal file path is invalid")
	}
	patch := filepath.Join(state.RunDir, "result.patch")
	if !write && requested == patch {
		if err := safeToolFile(patch, false); err != nil {
			return "", err
		}
		return patch, nil
	}
	relative := requested
	if filepath.IsAbs(relative) {
		var err error
		relative, err = filepath.Rel(state.Worktree, requested)
		if err != nil {
			return "", errors.New("goal file path is outside the isolated worktree")
		}
	}
	if filepath.Clean(relative) != relative || !Allowed(filepath.ToSlash(relative), []string{"."}) || relative == "." {
		return "", errors.New("goal file path is protected or outside the isolated worktree")
	}
	if write && !Allowed(filepath.ToSlash(relative), state.Spec.AllowedPaths) {
		return "", errors.New("goal file write exceeds the approved literal paths")
	}
	components := strings.Split(relative, string(filepath.Separator))
	current := state.Worktree
	for index, component := range components {
		current = filepath.Join(current, component)
		if index == len(components)-1 {
			if err := safeToolFile(current, write); err != nil {
				return "", err
			}
			break
		}
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("goal file parent must be an existing directory without symlinks")
		}
	}
	return current, nil
}

func safeToolFile(path string, allowNew bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && allowNew {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64*1024 {
		return errors.New("goal file must be a regular file of at most 64 KiB, without symlinks")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
		return errors.New("goal file must not be hard-linked")
	}
	return nil
}
