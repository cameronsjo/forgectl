//go:build unix

package goal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
	"github.com/cameronsjo/forgectl/internal/surface/goalcontainer"
)

type cappedBuffer struct {
	bytes.Buffer
	maximum  int
	exceeded bool
}

func (buffer *cappedBuffer) Write(contents []byte) (int, error) {
	remaining := buffer.maximum - buffer.Len()
	if len(contents) > remaining {
		buffer.exceeded = true
		if remaining > 0 {
			_, _ = buffer.Buffer.Write(contents[:remaining])
		}
		return len(contents), nil
	}
	return buffer.Buffer.Write(contents)
}

func cleanEnvironment(environment []string) []string {
	cleaned := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, exists := strings.Cut(entry, "=")
		if !exists || strings.HasPrefix(name, "GIT_") || strings.HasPrefix(name, "FORGECTL_") || name == "CLAUDECODE" || strings.HasPrefix(name, "CLAUDE_CODE_") {
			continue
		}
		cleaned = append(cleaned, entry)
	}
	return cleaned
}

func gitCommand(ctx context.Context, directory string, arguments ...string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	argv := []string{"--no-pager", "--literal-pathspecs", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-c", "protocol.allow=never", "-c", "core.quotePath=false", "-C", directory}
	argv = append(argv, arguments...)
	command := exec.CommandContext(ctx, "/usr/bin/git", argv...) //nolint:gosec // G204: fixed Git executable and typed arguments, hooks and protocols disabled.
	command.Env = append(cleanEnvironment(os.Environ()), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = time.Second
	output, diagnostic := &cappedBuffer{maximum: 4 * 1024 * 1024}, &cappedBuffer{maximum: 16 * 1024}
	command.Stdout, command.Stderr = output, diagnostic
	err := command.Run()
	if output.exceeded || diagnostic.exceeded {
		return nil, -1, errors.New("git output exceeded the bounded repository inspection limit")
	}
	if err == nil {
		return output.Bytes(), 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return output.Bytes(), exit.ExitCode(), fmt.Errorf("git %s: %s", arguments[0], strings.TrimSpace(diagnostic.String()))
	}
	return nil, -1, fmt.Errorf("git %s could not run: %w", arguments[0], err)
}

func git(ctx context.Context, directory string, arguments ...string) ([]byte, error) {
	output, _, err := gitCommand(ctx, directory, arguments...)
	return output, err
}

func controlledDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || int(stat.Uid) != os.Geteuid() || info.Mode().Perm()&0o022 != 0 {
		return errors.New("repository directory must be real, owned by this user, and not group/world writable")
	}
	return nil
}

func Prepare(ctx context.Context, spec Spec) (Prepared, error) {
	absolute, err := filepath.Abs(spec.Repository)
	if err != nil {
		return Prepared{}, err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return Prepared{}, err
	}
	if err := controlledDirectory(canonical); err != nil {
		return Prepared{}, err
	}
	root, err := git(ctx, canonical, "rev-parse", "--show-toplevel")
	if err != nil {
		return Prepared{}, errors.New("select an existing local Git checkout; this flow does not clone remote repositories")
	}
	spec.Repository = strings.TrimSpace(string(root))
	if err := controlledDirectory(spec.Repository); err != nil {
		return Prepared{}, err
	}
	if err := spec.Validate(); err != nil {
		return Prepared{}, err
	}
	if spec.Isolation != nil {
		if _, err := goalcontainer.Inspect(ctx, *spec.Isolation); err != nil {
			return Prepared{}, fmt.Errorf("container isolation prerequisite: %w", err)
		}
	}
	head, err := git(ctx, spec.Repository, "rev-parse", "HEAD")
	if err != nil {
		return Prepared{}, err
	}
	revision := strings.TrimSpace(string(head))
	if !revisionPattern.MatchString(revision) {
		return Prepared{}, errors.New("repository has no supported committed HEAD")
	}
	if spec.BaseRevision != "" && spec.BaseRevision != revision {
		return Prepared{}, errors.New("source HEAD changed since approval; review the new base before starting")
	}
	spec.BaseRevision = revision
	branch, exitCode, err := gitCommand(ctx, spec.Repository, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil && exitCode != 1 {
		return Prepared{}, err
	}
	branchName := strings.TrimSpace(string(branch))
	if exitCode == 1 {
		branchName = "(detached)"
	}
	status, err := git(ctx, spec.Repository, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return Prepared{}, err
	}
	transport := "Tool and verification network access is disabled; OpenAI model transport remains enabled."
	if spec.Team != nil {
		transport = "Tool and verification network access is disabled; each approved native model provider remains enabled without fallback."
	}
	warnings := []string{transport, "Cleanup confirms only owned native process groups; daemonized children may survive cancellation.", "No commit, push, merge, deployment, installation or permission expansion is included.", "Maximum runtime is not a token or dollar spending cap."}
	if spec.Isolation != nil {
		warnings[1] = "Tools and checks use leased Linux containers: cleanup covers every tool descendant. Native model processes retain separate process-group cleanup."
		warnings = append(warnings, "Linux tool/check semantics; no host-command fallback. Runtime socket: "+spec.Isolation.Socket, "Approved local container image: "+spec.Isolation.Image, "Start verifies the image helper, shared mount paths and non-root access before model work; missing tools or a failed boundary refuse execution.")
	}
	if spec.Team != nil {
		if spec.Isolation == nil {
			warnings = append(warnings, "Native teams require Codex as a separate, credential-free execution host, plus every selected harness and its native credentials.")
		}
		warnings = append(warnings, "An unavailable tool sandbox refuses the run before model work. Native teams remain experimental; inspect retained review, checks and cleanup evidence before accepting a result.")
	}
	if len(status) > 0 {
		warnings = append(warnings, "Source checkout is dirty: only committed HEAD is copied; local edits and untracked files are excluded.")
	}
	if slices.Contains(spec.AllowedPaths, ".") {
		warnings = append(warnings, "Repository-wide scope: new and existing source files may change; Git/agent metadata and .env files remain excluded.")
	}
	return Prepared{Spec: spec, SourceBranch: branchName, SourceDirty: len(status) > 0, Warnings: warnings}, nil
}

type sourceSnapshot struct {
	Head            string `json:"head"`
	Branch          string `json:"branch"`
	Status          string `json:"status"`
	PatchSHA256     string `json:"patch_sha256"`
	IndexSHA256     string `json:"index_sha256"`
	UntrackedSHA256 string `json:"untracked_sha256"`
}

func snapshotSource(ctx context.Context, repository string) (sourceSnapshot, error) {
	head, err := git(ctx, repository, "rev-parse", "HEAD")
	if err != nil {
		return sourceSnapshot{}, err
	}
	branch, code, err := gitCommand(ctx, repository, "symbolic-ref", "--quiet", "HEAD")
	if err != nil && code != 1 {
		return sourceSnapshot{}, err
	}
	status, err := git(ctx, repository, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return sourceSnapshot{}, err
	}
	patch, err := git(ctx, repository, "diff", "--no-ext-diff", "--no-textconv", "--binary", "HEAD", "--")
	if err != nil {
		return sourceSnapshot{}, err
	}
	index, err := git(ctx, repository, "diff", "--cached", "--no-ext-diff", "--no-textconv", "--binary", "HEAD", "--")
	if err != nil {
		return sourceSnapshot{}, err
	}
	untracked, err := snapshotUntracked(ctx, repository)
	if err != nil {
		return sourceSnapshot{}, err
	}
	return sourceSnapshot{Head: strings.TrimSpace(string(head)), Branch: strings.TrimSpace(string(branch)), Status: string(status), PatchSHA256: Digest(patch), IndexSHA256: Digest(index), UntrackedSHA256: untracked}, nil
}

func snapshotUntracked(ctx context.Context, repository string) (string, error) {
	listing, err := git(ctx, repository, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	files := splitNull(listing)
	slices.Sort(files)
	hash := sha256.New()
	var total int64
	for _, relative := range files {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		path := filepath.Join(repository, relative)
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(hash, "%s\x00%d\x00%d\x00", relative, info.Mode(), info.Size())
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return "", err
			}
			_, _ = io.WriteString(hash, target)
			continue
		}
		if !info.Mode().IsRegular() {
			return "", errors.New("source has an untracked special file; cannot verify isolation")
		}
		total += info.Size()
		if total > 256*1024*1024 {
			return "", errors.New("source untracked files exceed the 256 MiB isolation-check bound")
		}
		descriptor, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return "", err
		}
		file := os.NewFile(uintptr(descriptor), path)
		opened, statErr := file.Stat()
		if statErr != nil || !os.SameFile(info, opened) {
			_ = file.Close()
			return "", errors.New("source untracked file changed during isolation check")
		}
		size, readErr := io.Copy(hash, io.LimitReader(file, info.Size()+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || size != info.Size() {
			return "", errors.New("source untracked file could not be read consistently")
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func containsPath(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func prepareWorkspace(ctx context.Context, store Store, state State) (sourceSnapshot, error) {
	before, err := snapshotSource(ctx, state.Spec.Repository)
	if err != nil {
		return sourceSnapshot{}, err
	}
	if before.Head != state.Spec.BaseRevision {
		return sourceSnapshot{}, errors.New("source HEAD changed after confirmation; no worktree was created")
	}
	if containsPath(state.Spec.Repository, store.Root) || containsPath(store.Root, state.Spec.Repository) || containsPath(store.Root, state.Worktree) || containsPath(state.Worktree, store.Root) {
		return sourceSnapshot{}, errors.New("source, runtime and implementation directories must be disjoint")
	}
	parent := filepath.Dir(state.Worktree)
	parentSpec := privdir.Spec{Base: filepath.Dir(parent), Leaf: filepath.Base(parent), Mode: 0o700}
	parentFD, err := privdir.OpenChecked(parentSpec)
	if errors.Is(err, privdir.ErrAbsent) {
		parentSpec.Create = true
		parentSpec.AncestorMode = 0o700
		parentFD, err = privdir.Pin(parentSpec)
	}
	if err != nil {
		return sourceSnapshot{}, err
	}
	defer func() { _ = unix.Close(parentFD) }()
	if _, err := os.Lstat(state.Worktree); !errors.Is(err, fs.ErrNotExist) {
		return sourceSnapshot{}, errors.New("generated worktree destination already exists or cannot be inspected")
	}
	// termsafe:allow-raw-json private source snapshot, never terminal output
	encoded, err := json.Marshal(before)
	if err != nil {
		return sourceSnapshot{}, err
	}
	if err := store.WriteArtifact(state.ID, "source-before.json", encoded); err != nil {
		return sourceSnapshot{}, err
	}
	if _, err := git(ctx, state.Spec.Repository, "worktree", "add", "-b", state.Branch, state.Worktree, state.Spec.BaseRevision); err != nil {
		return sourceSnapshot{}, fmt.Errorf("worktree creation failed; retained partial branch/path for inspection: %w", err)
	}
	if err := controlledDirectory(state.Worktree); err != nil {
		return sourceSnapshot{}, err
	}
	if err := os.Chmod(state.Worktree, 0o700); err != nil { //nolint:gosec // G302: owner search permission is required on the new private directory.
		return sourceSnapshot{}, err
	}
	gitEntry, err := os.Lstat(filepath.Join(state.Worktree, ".git"))
	if err != nil || !gitEntry.Mode().IsRegular() {
		return sourceSnapshot{}, errors.New("created workspace is not a linked Git worktree")
	}
	current, err := snapshotSource(ctx, state.Spec.Repository)
	if err != nil {
		return sourceSnapshot{}, err
	}
	if current != before {
		return sourceSnapshot{}, errors.New("source checkout changed during worktree preparation; goal stopped")
	}
	if err := assertWorkspaceBase(ctx, state); err != nil {
		return sourceSnapshot{}, err
	}
	status, err := git(ctx, state.Worktree, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return sourceSnapshot{}, err
	}
	if len(status) > 0 {
		return sourceSnapshot{}, errors.New("new workspace is not clean")
	}
	return before, nil
}

func assertWorkspaceBase(ctx context.Context, state State) error {
	head, err := git(ctx, state.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	branch, err := git(ctx, state.Worktree, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(head)) != state.Spec.BaseRevision || strings.TrimSpace(string(branch)) != state.Branch {
		return errors.New("implementation changed HEAD or its assigned branch; result is not accepted")
	}
	return nil
}

type patchSnapshot struct {
	Bytes  []byte
	SHA256 string
	Files  []string
}

func snapshotPatch(ctx context.Context, state State) (patchSnapshot, error) {
	if err := assertWorkspaceBase(ctx, state); err != nil {
		return patchSnapshot{}, err
	}
	changed, err := git(ctx, state.Worktree, "diff", "--no-renames", "--name-only", "-z", "HEAD", "--")
	if err != nil {
		return patchSnapshot{}, err
	}
	untracked, err := git(ctx, state.Worktree, "ls-files", "--others", "-z")
	if err != nil {
		return patchSnapshot{}, err
	}
	files := splitNull(changed)
	newFiles := splitNull(untracked)
	files = append(files, newFiles...)
	slices.Sort(files)
	files = slices.Compact(files)
	if len(files) == 0 {
		return patchSnapshot{}, errors.New("implementation produced no patch")
	}
	if len(files) > 256 {
		return patchSnapshot{}, errors.New("patch exceeds 256 changed files")
	}
	for _, file := range files {
		if !Allowed(file, state.Spec.AllowedPaths) {
			return patchSnapshot{}, fmt.Errorf("out-of-scope changed path: %s", file)
		}
		current := state.Worktree
		for _, component := range strings.Split(file, "/") {
			current = filepath.Join(current, component)
			info, err := os.Lstat(current)
			if errors.Is(err, fs.ErrNotExist) {
				break
			}
			if err != nil {
				return patchSnapshot{}, err
			}
			if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
				return patchSnapshot{}, errors.New("changed paths must not traverse or create symlinks or special files")
			}
		}
	}
	patch, err := git(ctx, state.Worktree, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--binary", "--full-index", "HEAD", "--")
	if err != nil {
		return patchSnapshot{}, err
	}
	slices.Sort(newFiles)
	for _, file := range newFiles {
		addition, code, err := gitCommand(ctx, state.Worktree, "diff", "--no-index", "--no-ext-diff", "--no-textconv", "--binary", "--full-index", "--", "/dev/null", file)
		if err != nil && code != 1 {
			return patchSnapshot{}, err
		}
		if code != 1 {
			return patchSnapshot{}, errors.New("new file did not produce a verifiable patch")
		}
		patch = append(patch, addition...)
		if len(patch) > 4*1024*1024 {
			return patchSnapshot{}, errors.New("patch exceeds 4 MiB")
		}
	}
	return patchSnapshot{Bytes: patch, SHA256: Digest(patch), Files: files}, nil
}

func splitNull(contents []byte) []string {
	entries := strings.Split(string(contents), "\x00")
	return slices.DeleteFunc(entries, func(entry string) bool { return entry == "" })
}
