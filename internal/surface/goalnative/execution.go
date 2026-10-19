package goalnative

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type CommandHost struct{ session *Session }

func StartCommandHost(ctx context.Context, config Config) (*CommandHost, error) {
	config.ExecutionOnly = true
	config.Model = ""
	session, err := Start(ctx, config)
	if err != nil {
		return nil, err
	}
	return &CommandHost{session: session}, nil
}

func (host *CommandHost) Exec(ctx context.Context, argv []string, cwd string, roots []string, timeout time.Duration) (CommandResult, error) {
	confined, err := confinedCommand(argv)
	if err != nil {
		return CommandResult{}, err
	}
	for _, root := range roots {
		if pathWithin(root, host.session.config.ControlDir) || pathWithin(host.session.config.ControlDir, root) {
			return CommandResult{}, errors.New("execution roots overlap private host state")
		}
		approved := false
		for _, allowed := range host.session.config.WritableRoots {
			if pathWithin(root, allowed) {
				approved = true
			}
		}
		if !approved {
			return CommandResult{}, errors.New("execution roots exceed the host boundary")
		}
	}
	environment := make(map[string]string)
	for _, root := range roots {
		if root == cwd {
			continue
		}
		temporary, cache := filepath.Join(root, "tmp"), filepath.Join(root, "cache")
		if temporaryInfo, err := os.Lstat(temporary); err != nil || !temporaryInfo.IsDir() {
			continue
		}
		if cacheInfo, err := os.Lstat(cache); err != nil || !cacheInfo.IsDir() {
			continue
		}
		environment["TMPDIR"], environment["GOCACHE"] = temporary, cache
		break
	}
	return host.session.exec(ctx, confined, cwd, roots, timeout, environment)
}

func (host *CommandHost) Close(ctx context.Context) error { return host.session.Close(ctx) }

func executionMethod(method string) bool {
	switch method {
	case "initialize", "config/read", "command/exec", "command/exec/terminate":
		return true
	default:
		return false
	}
}

func CredentialFreeEnvironment(environment []string, home string) []string {
	result := []string{"HOME=" + home, "CODEX_HOME=" + home, "XDG_CONFIG_HOME=" + home, "XDG_CACHE_HOME=" + home, "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if !found || strings.ContainsRune(value, 0) {
			continue
		}
		switch key {
		case "PATH", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "TZ", "GOMAXPROCS", "GOTOOLCHAIN", "TMPDIR", "GOCACHE":
			if key == "PATH" {
				result[4] = entry
			} else {
				result = append(result, entry)
			}
		}
	}
	return result
}

func prepareExecutionEnvironment(config Config) ([]string, error) {
	home := filepath.Join(config.ControlDir, "execution-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return nil, errors.New("execution host requires a fresh private configuration home")
	}
	if err := ensurePrivateDir(home); err != nil {
		return nil, err
	}
	environment := config.Environment
	if environment == nil {
		environment = os.Environ()
	}
	return CredentialFreeEnvironment(environment, home), nil
}
