//go:build unix

package goaltools

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const maxGoalFileBytes = 64 * 1024

const goalDirectoryFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

func RunFile(root, requested string, write bool, content string) (string, error) {
	components, err := validateFilePath(root, requested, write, content)
	if err != nil {
		return "", err
	}
	rootFD, err := unix.Open(root, goalDirectoryFlags, 0)
	if err != nil {
		return "", fmt.Errorf("open goal file root: %w", err)
	}
	defer func() { _ = unix.Close(rootFD) }()
	var rootStat unix.Stat_t
	if err := unix.Fstat(rootFD, &rootStat); err != nil || rootStat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", errors.New("goal file root is not a directory")
	}

	parentFD := rootFD
	parentFDs := []int{rootFD}
	defer func() {
		for _, descriptor := range parentFDs[1:] {
			_ = unix.Close(descriptor)
		}
	}()
	for _, component := range components[:len(components)-1] {
		fd, err := unix.Openat(parentFD, component, goalDirectoryFlags, 0)
		if err != nil {
			return "", fmt.Errorf("open goal file parent: %w", err)
		}
		parentFD = fd
		parentFDs = append(parentFDs, fd)
	}
	name := components[len(components)-1]
	if write {
		if err := writeGoalFile(parentFD, name, []byte(content)); err != nil {
			return "", err
		}
		return "", nil
	}
	file, err := openGoalFile(parentFD, name)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxGoalFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read goal file: %w", err)
	}
	if len(data) > maxGoalFileBytes {
		return "", errors.New("goal file exceeds 64 KiB")
	}
	return string(data), nil
}

func validateFilePath(root, requested string, write bool, content string) ([]string, error) {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, errors.New("goal file root must be absolute and clean")
	}
	if requested == "" || filepath.IsAbs(requested) || filepath.Clean(requested) != requested || requested == "." || strings.ContainsAny(requested, "\x00\r\n\\") {
		return nil, errors.New("goal file path must be a clean relative path")
	}
	if write && (len(content) > maxGoalFileBytes || !utf8.ValidString(content) || strings.ContainsRune(content, 0)) {
		return nil, errors.New("goal file content must be bounded UTF-8 text")
	}
	components := strings.Split(requested, string(filepath.Separator))
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return nil, errors.New("goal file path contains a protected component")
		}
	}
	return components, nil
}

func openGoalFile(parentFD int, name string) (*os.File, error) {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, fs.ErrNotExist
	}
	if err != nil {
		return nil, errors.New("goal file is not a regular non-symlink file")
	}
	file := os.NewFile(uintptr(fd), name)
	var pinned unix.Stat_t
	if err := unix.Fstat(fd, &pinned); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat goal file: %w", err)
	}
	if pinned.Mode&unix.S_IFMT != unix.S_IFREG || pinned.Nlink != 1 || int(pinned.Uid) != os.Geteuid() || pinned.Size > maxGoalFileBytes {
		_ = file.Close()
		return nil, errors.New("goal file must be an owned regular file of at most 64 KiB with one link")
	}
	var named unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil || named.Dev != pinned.Dev || named.Ino != pinned.Ino {
		_ = file.Close()
		return nil, errors.New("goal file changed while opening")
	}
	return file, nil
}

func writeGoalFile(parentFD int, name string, content []byte) error {
	mode := os.FileMode(0o600)
	if existing, err := openGoalFile(parentFD, name); err == nil {
		info, statErr := existing.Stat()
		_ = existing.Close()
		if statErr != nil {
			return fmt.Errorf("stat existing goal file: %w", statErr)
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	tempName, err := goalTempName()
	if err != nil {
		return err
	}
	fd, err := unix.Openat(parentFD, tempName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create private goal file: %w", err)
	}
	temp := os.NewFile(uintptr(fd), tempName)
	removeTemp := true
	defer func() {
		_ = temp.Close()
		if removeTemp {
			_ = unix.Unlinkat(parentFD, tempName, 0)
		}
	}()
	for len(content) > 0 {
		written, err := temp.Write(content)
		if err != nil {
			return fmt.Errorf("write private goal file: %w", err)
		}
		content = content[written:]
	}
	if err := temp.Chmod(mode); err != nil {
		return fmt.Errorf("preserve goal file mode: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close private goal file: %w", err)
	}
	if err := unix.Renameat(parentFD, tempName, parentFD, name); err != nil {
		return fmt.Errorf("install goal file: %w", err)
	}
	removeTemp = false
	return nil
}

func goalTempName() (string, error) {
	bytes := make([]byte, 12)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("create private goal file name: %w", err)
	}
	return ".forgectl-goal-" + hex.EncodeToString(bytes), nil
}
