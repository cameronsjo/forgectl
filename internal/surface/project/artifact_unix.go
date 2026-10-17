//go:build unix

package project

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var artifactAfterOpenHook func(string)

type artifactOpened struct {
	rootParentFD int
	rootFD       int
	parentFD     int
	rootName     string
	rootStat     unix.Stat_t
	dirNames     []string
	dirStats     []unix.Stat_t
	file         *os.File
	leaf         string
}

func (opened *artifactOpened) close() {
	if opened == nil {
		return
	}
	if opened.file != nil {
		_ = opened.file.Close()
		opened.file = nil
	}
	if opened.parentFD >= 0 && opened.parentFD != opened.rootFD {
		_ = unix.Close(opened.parentFD)
		opened.parentFD = -1
	}
	if opened.rootFD >= 0 {
		_ = unix.Close(opened.rootFD)
		opened.rootFD = -1
	}
	if opened.rootParentFD >= 0 {
		_ = unix.Close(opened.rootParentFD)
		opened.rootParentFD = -1
	}
}

func verifyArtifact(ctx context.Context, worktree, path, expectedSHA256 string) (ArtifactDigest, error) {
	if ctx == nil {
		return ArtifactDigest{}, fmt.Errorf("%w: context is nil", ErrInvalidInput)
	}
	relative, err := artifactInputs(worktree, path, expectedSHA256)
	if err != nil {
		return ArtifactDigest{}, err
	}
	if err := ctx.Err(); err != nil {
		return ArtifactDigest{}, err
	}

	opened, err := openArtifact(worktree, relative)
	if err != nil {
		return ArtifactDigest{}, err
	}
	defer opened.close()

	var before unix.Stat_t
	if err := unix.Fstat(int(opened.file.Fd()), &before); err != nil {
		return ArtifactDigest{}, artifactConflict("stat artifact", err)
	}
	if err := validateArtifactStat(&before); err != nil {
		return ArtifactDigest{}, err
	}
	if err := opened.revalidate(&before); err != nil {
		return ArtifactDigest{}, err
	}
	if artifactAfterOpenHook != nil {
		artifactAfterOpenHook(filepath.Join(worktree, filepath.FromSlash(relative)))
	}
	if err := ctx.Err(); err != nil {
		return ArtifactDigest{}, err
	}

	hasher := sha256.New()
	buffer := make([]byte, 32*1024)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return ArtifactDigest{}, err
		}
		remaining := maxArtifactBytes - size
		if remaining <= 0 {
			break
		}
		chunk := buffer
		if int64(len(chunk)) > remaining {
			chunk = chunk[:remaining]
		}
		read, readErr := opened.file.Read(chunk)
		if read > 0 {
			size += int64(read)
			if _, err := hasher.Write(buffer[:read]); err != nil {
				return ArtifactDigest{}, artifactConflict("hash artifact", err)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
				return ArtifactDigest{}, readErr
			}
			return ArtifactDigest{}, artifactConflict("read artifact", readErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return ArtifactDigest{}, err
	}

	var after unix.Stat_t
	if err := unix.Fstat(int(opened.file.Fd()), &after); err != nil {
		return ArtifactDigest{}, artifactConflict("stat artifact after read", err)
	}
	if !sameArtifactStat(&before, &after) || int64(after.Size) != size {
		return ArtifactDigest{}, artifactConflict("artifact changed while hashing", nil)
	}
	if err := opened.revalidate(&after); err != nil {
		return ArtifactDigest{}, err
	}

	digest := hex.EncodeToString(hasher.Sum(nil))
	if digest != expectedSHA256 {
		return ArtifactDigest{}, artifactConflict("artifact sha256 does not match expected digest", nil)
	}
	return ArtifactDigest{RelativePath: filepath.Clean(relative), Size: size, SHA256: digest}, nil
}

func openArtifact(worktree, relative string) (*artifactOpened, error) {
	worktree = filepath.Clean(worktree)
	rootParentFD, err := unix.Open(filepath.Dir(worktree), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, artifactConflict("open worktree parent", err)
	}
	rootName := filepath.Base(worktree)
	rootFD, err := unix.Openat(rootParentFD, rootName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		_ = unix.Close(rootParentFD)
		return nil, artifactConflict("open worktree", err)
	}
	opened := &artifactOpened{rootParentFD: rootParentFD, rootFD: rootFD, parentFD: rootFD, rootName: rootName}
	if err := unix.Fstat(rootFD, &opened.rootStat); err != nil {
		opened.close()
		return nil, artifactConflict("stat worktree", err)
	}
	if opened.rootStat.Mode&unix.S_IFMT != unix.S_IFDIR {
		opened.close()
		return nil, artifactConflict("worktree is not a directory", nil)
	}
	components := strings.Split(relative, string(filepath.Separator))
	for index, component := range components[:len(components)-1] {
		childFD, err := unix.Openat(opened.parentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			opened.close()
			return nil, artifactConflict("open artifact directory", err)
		}
		var childStat unix.Stat_t
		if err := unix.Fstat(childFD, &childStat); err != nil {
			_ = unix.Close(childFD)
			opened.close()
			return nil, artifactConflict("stat artifact directory", err)
		}
		if index > 0 {
			_ = unix.Close(opened.parentFD)
		}
		opened.dirNames = append(opened.dirNames, component)
		opened.dirStats = append(opened.dirStats, childStat)
		opened.parentFD = childFD
	}
	leaf := components[len(components)-1]
	fd, err := unix.Openat(opened.parentFD, leaf, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		opened.close()
		return nil, artifactConflict("open artifact", err)
	}
	opened.file = os.NewFile(uintptr(fd), filepath.Join(worktree, filepath.FromSlash(relative)))
	opened.leaf = leaf
	return opened, nil
}

func (opened *artifactOpened) revalidate(expectedLeaf *unix.Stat_t) error {
	var namedRoot unix.Stat_t
	if err := unix.Fstatat(opened.rootParentFD, opened.rootName, &namedRoot, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return artifactConflict("stat worktree name", err)
	}
	if !sameArtifactIdentity(&opened.rootStat, &namedRoot) {
		return artifactConflict("worktree name changed", nil)
	}
	parentFD := opened.rootFD
	for index, name := range opened.dirNames {
		childFD, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			if parentFD != opened.rootFD {
				_ = unix.Close(parentFD)
			}
			return artifactConflict("reopen artifact directory", err)
		}
		var childStat unix.Stat_t
		if err := unix.Fstat(childFD, &childStat); err != nil {
			_ = unix.Close(childFD)
			if parentFD != opened.rootFD {
				_ = unix.Close(parentFD)
			}
			return artifactConflict("stat reopened artifact directory", err)
		}
		if !sameArtifactIdentity(&opened.dirStats[index], &childStat) {
			_ = unix.Close(childFD)
			if parentFD != opened.rootFD {
				_ = unix.Close(parentFD)
			}
			return artifactConflict("artifact directory changed", nil)
		}
		if parentFD != opened.rootFD {
			_ = unix.Close(parentFD)
		}
		parentFD = childFD
	}
	if err := namedArtifactMatches(parentFD, opened.leaf, expectedLeaf); err != nil {
		if parentFD != opened.rootFD {
			_ = unix.Close(parentFD)
		}
		return err
	}
	if parentFD != opened.rootFD {
		_ = unix.Close(parentFD)
	}
	return nil
}

func validateArtifactStat(stat *unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return artifactConflict("artifact is not a regular file", nil)
	}
	if stat.Nlink != 1 {
		return artifactConflict("artifact is hard-linked", nil)
	}
	if stat.Size < 0 || stat.Size > maxArtifactBytes {
		return artifactConflict("artifact exceeds size limit", nil)
	}
	return nil
}

func namedArtifactMatches(parentFD int, leaf string, expected *unix.Stat_t) error {
	var named unix.Stat_t
	if err := unix.Fstatat(parentFD, leaf, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return artifactConflict("stat artifact name", err)
	}
	if !sameArtifactStat(expected, &named) {
		return artifactConflict("artifact name changed", nil)
	}
	return nil
}

func sameArtifactStat(left, right *unix.Stat_t) bool {
	return sameArtifactIdentity(left, right) && left.Nlink == right.Nlink && left.Size == right.Size && left.Mtim == right.Mtim && left.Ctim == right.Ctim
}

func sameArtifactIdentity(left, right *unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino && left.Mode&unix.S_IFMT == right.Mode&unix.S_IFMT
}

func artifactConflict(message string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrConflict, message)
	}
	return fmt.Errorf("%w: %s: %w", ErrConflict, message, cause)
}
