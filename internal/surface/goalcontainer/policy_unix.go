//go:build unix

package goalcontainer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	Isolation      Spec
	Role           string
	Directory      string
	Worktree       string
	GitDirectories []string
	ControlDir     string
	PatchPath      string
	WritableRoots  []string
}

func (config Config) validate() error {
	if err := config.Isolation.Validate(); err != nil {
		return err
	}
	if !slices.Contains([]string{"coordinator", "implementation", "reviewer"}, config.Role) {
		return errors.New("container execution requires a known role")
	}
	for _, directory := range append([]string{config.Directory, config.Worktree, config.ControlDir}, config.GitDirectories...) {
		if err := canonicalDirectory(directory); err != nil {
			return err
		}
	}
	if len(config.GitDirectories) < 1 || len(config.GitDirectories) > 2 {
		return errors.New("container execution requires exact Git/common metadata directories")
	}
	if overlaps(config.Directory, config.Worktree) || overlaps(config.ControlDir, config.Directory) || overlaps(config.ControlDir, config.Worktree) {
		return errors.New("container runtime, worktree and control directories must be disjoint")
	}
	for _, directory := range config.GitDirectories {
		if overlaps(directory, config.Directory) || overlaps(directory, config.Worktree) || overlaps(directory, config.ControlDir) {
			return errors.New("container Git metadata overlaps writable or control state")
		}
	}
	if !cleanPath(config.PatchPath) || filepath.Base(config.PatchPath) != "result.patch" || overlaps(config.PatchPath, config.Directory) || overlaps(config.PatchPath, config.Worktree) || overlaps(config.PatchPath, config.ControlDir) {
		return errors.New("container frozen patch must be a separate supervisor-owned result.patch")
	}
	for _, directory := range config.WritableRoots {
		if directory != config.Directory && directory != config.Worktree {
			return errors.New("container writable ceiling includes an unsupported directory")
		}
	}
	if !slices.Contains(config.WritableRoots, config.Directory) || (config.Role == "coordinator" && slices.Contains(config.WritableRoots, config.Worktree)) {
		return errors.New("container writable ceiling does not match its role")
	}
	info, err := os.Lstat(config.ControlDir)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		return errors.New("container receipts require a private control directory")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Geteuid() {
		return errors.New("container receipts must belong to the current user")
	}
	return nil
}

func canonicalDirectory(directory string) error {
	if !cleanPath(directory) {
		return errors.New("container mount directories must be absolute clean paths")
	}
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil || canonical != directory {
		return errors.New("container mount directories must exist without symlink components")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return errors.New("container mount directory is unavailable")
	}
	return root.Close()
}

func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}
func overlaps(first, second string) bool { return within(first, second) || within(second, first) }

func (host *Host) request(argv []string, cwd string, roots []string, nonce string, deadline time.Time) (createRequest, error) {
	if len(argv) < 1 || len(argv) > 64 || strings.TrimSpace(argv[0]) == "" || strings.HasPrefix(argv[0], "-") {
		return createRequest{}, errors.New("container execution requires bounded nonempty argv")
	}
	total := 0
	for _, argument := range argv {
		total += len(argument)
		if len(argument) > 64*1024 || strings.ContainsRune(argument, 0) || total > 128*1024 {
			return createRequest{}, errors.New("container argv exceeds its bounds")
		}
	}
	if !cleanPath(cwd) || !slices.Contains(roots, cwd) || !slices.Contains(roots, host.config.Directory) {
		return createRequest{}, errors.New("container execution cwd must match a requested writable root")
	}
	for _, root := range roots {
		if !slices.Contains(host.config.WritableRoots, root) {
			return createRequest{}, errors.New("container request exceeds its role's writable ceiling")
		}
	}
	for _, directory := range []string{host.config.Directory, host.config.Worktree} {
		if err := canonicalDirectory(directory); err != nil {
			return createRequest{}, err
		}
	}
	mounts := []mount{bind(host.config.Worktree, !slices.Contains(roots, host.config.Worktree)), bind(host.config.Directory, false)}
	gitDirectories := slices.Clone(host.config.GitDirectories)
	slices.SortFunc(gitDirectories, func(first, second string) int { return len(first) - len(second) })
	for _, directory := range gitDirectories {
		if err := canonicalDirectory(directory); err != nil {
			return createRequest{}, err
		}
		covered := slices.ContainsFunc(mounts, func(existing mount) bool { return existing.ReadOnly && within(directory, existing.Source) })
		if !covered {
			mounts = append(mounts, bind(directory, true))
		}
	}
	gitEntry := filepath.Join(host.config.Worktree, ".git")
	if err := regularOwnedFile(gitEntry); err != nil {
		return createRequest{}, errors.New("container worktree requires a regular, owned .git entry")
	}
	mounts = append(mounts, bind(gitEntry, true))
	if _, err := os.Lstat(host.config.PatchPath); !errors.Is(err, os.ErrNotExist) {
		if err := regularOwnedFile(host.config.PatchPath); err != nil {
			return createRequest{}, errors.New("container frozen patch is not a regular owned file")
		}
		mounts = append(mounts, bind(host.config.PatchPath, true))
	}
	arguments := []string{"surface", "project", "goal", "_container-init", strconv.Itoa(os.Geteuid()), strconv.Itoa(os.Getegid()), strconv.FormatInt(deadline.UnixNano(), 10), cwd}
	arguments = append(arguments, argv...)
	return createRequest{
		Image: host.config.Isolation.Image, User: "0:0", Entrypoint: []string{HelperPath}, Cmd: arguments,
		Env: workloadEnvironment(filepath.Join(host.config.Directory, "cache")), WorkingDir: "/tmp",
		AttachStdin: true, AttachStdout: true, AttachStderr: true, OpenStdin: true, StdinOnce: true,
		Healthcheck: healthcheck{Test: []string{"NONE"}}, Labels: map[string]string{"io.forgectl.goal-nonce": nonce, "io.forgectl.goal-role": host.config.Role, "io.forgectl.goal-owner": digest([]byte(host.config.ControlDir))},
		HostConfig: policy{NetworkMode: "none", ReadonlyRootfs: true, CapDrop: []string{"ALL"}, CapAdd: []string{"SETUID", "SETGID"}, SecurityOpt: []string{"no-new-privileges"}, PidsLimit: 128, Memory: 1024 * 1024 * 1024, MemorySwap: 1024 * 1024 * 1024, NanoCpus: 1000000000, RestartPolicy: restartPolicy{Name: "no"}, Tmpfs: map[string]string{"/tmp": "rw,nosuid,nodev,size=134217728,mode=1777"}, Mounts: mounts, CgroupnsMode: "private", IpcMode: "private"},
	}, nil
}

func bind(path string, readOnly bool) mount {
	return mount{Type: "bind", Source: path, Target: path, ReadOnly: readOnly, BindOptions: bindOptions{Propagation: "rprivate", NonRecursive: true}}
}

func regularOwnedFile(path string) error {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return errors.New("file path includes a symlink")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("file is not regular")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Geteuid() || owner.Nlink != 1 {
		return errors.New("file is not owned or has multiple links")
	}
	return nil
}

func workloadEnvironment(cache string) []string {
	return []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/tmp", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TZ=UTC", "TMPDIR=/tmp", "GOCACHE=" + cache, "XDG_CONFIG_HOME=/tmp", "XDG_CACHE_HOME=/tmp", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=", "GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1=/dev/null"}
}

func verifyIdentity(observed inspection, request createRequest, name, identity string) error {
	if !identityPattern.MatchString(observed.ID) || (identity != "" && observed.ID != identity) || observed.Name != "/"+name || observed.Image != request.Image {
		return errors.New("container identity does not match this owned operation")
	}
	for key, value := range request.Labels {
		if observed.Config.Labels[key] != value {
			return errors.New("container ownership labels do not match this operation")
		}
	}
	return nil
}

func verifyPolicy(observed inspection, request createRequest) error {
	expected := request.HostConfig
	actual := observed.HostConfig
	if !reflect.DeepEqual(actual, expected) {
		return errors.New("container effective runtime policy differs from its approved policy")
	}
	actualConfig := observed.Config
	if actualConfig.User != request.User || actualConfig.WorkingDir != request.WorkingDir || actualConfig.Tty || !actualConfig.OpenStdin || !actualConfig.StdinOnce || !actualConfig.AttachStdin || !actualConfig.AttachStdout || !actualConfig.AttachStderr || !reflect.DeepEqual(actualConfig.Entrypoint, request.Entrypoint) || !reflect.DeepEqual(actualConfig.Cmd, request.Cmd) || !reflect.DeepEqual(actualConfig.Healthcheck, request.Healthcheck) {
		return errors.New("container effective process configuration differs from the approved helper")
	}
	expectedEnv, actualEnv := slices.Clone(request.Env), slices.Clone(actualConfig.Env)
	slices.Sort(expectedEnv)
	slices.Sort(actualEnv)
	if !slices.Equal(expectedEnv, actualEnv) || len(observed.Mounts) != len(expected.Mounts) {
		return errors.New("container environment or mount count differs from the approved boundary")
	}
	for _, wanted := range expected.Mounts {
		found := false
		for _, got := range observed.Mounts {
			if got.Type == "bind" && got.Source == wanted.Source && got.Destination == wanted.Target && got.RW != wanted.ReadOnly && got.Propagation == "rprivate" {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("container mount policy differs at %s", wanted.Target)
		}
	}
	return nil
}
