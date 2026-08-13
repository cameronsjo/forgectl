//go:build unix

package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/BurntSushi/toml"
	"golang.org/x/sys/unix"
)

type nativeMigrationFS struct{}

func NativeMigrationFS() MigrationFS { return nativeMigrationFS{} }

func (nativeMigrationFS) mutationSupported() bool { return true }

type unixLegacyProbe struct {
	parentFD int
	path     string
	base     string
	pre      unix.Stat_t
	exists   bool
	regular  bool
	owned    bool
	// hooks are nil in production and allow deterministic, per-attempt tests
	// to move the source at exact capture barriers without package globals.
	beforeOpen   func()
	afterOpen    func()
	betweenReads func()
}

func (p *unixLegacyProbe) Exists() bool  { return p != nil && p.exists }
func (p *unixLegacyProbe) Regular() bool { return p != nil && p.regular }

func (p *unixLegacyProbe) Close() error {
	if p == nil || !p.owned {
		return nil
	}
	p.owned = false
	return unix.Close(p.parentFD)
}

func (nativeMigrationFS) probe(path string) (legacyProbe, error) {
	parent := filepath.Dir(path)
	fd, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return &unixLegacyProbe{}, nil
		}
		return nil, fmt.Errorf("inspect legacy config parent: %w", err)
	}
	p := &unixLegacyProbe{parentFD: fd, path: path, base: filepath.Base(path), owned: true}
	if err := unix.Fstatat(fd, p.base, &p.pre, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return p, nil
		}
		_ = p.Close()
		return nil, fmt.Errorf("inspect legacy config entry: %w", err)
	}
	p.exists = true
	p.regular = p.pre.Mode&unix.S_IFMT == unix.S_IFREG
	return p, nil
}

func identityFromUnixStat(st *unix.Stat_t) FileIdentity {
	return FileIdentity{Device: uint64(st.Dev), Inode: uint64(st.Ino)}
}

func identityFromFileInfo(info os.FileInfo) (FileIdentity, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return FileIdentity{}, fmt.Errorf("file metadata has unexpected type %T", info.Sys())
	}
	return FileIdentity{Device: uint64(st.Dev), Inode: uint64(st.Ino)}, nil
}

func statMatchesInfo(st *unix.Stat_t, info os.FileInfo) bool {
	id, err := identityFromFileInfo(info)
	if err != nil {
		return false
	}
	return identityFromUnixStat(st) == id && int64(st.Size) == info.Size() &&
		os.FileMode(st.Mode).Perm() == info.Mode().Perm() && st.Mode&unix.S_IFMT == unix.S_IFREG
}

type unixLegacySnapshot struct {
	parentFD int
	file     *os.File
	base     string
	path     string
	meta     stableFileMetadata
	closed   bool
}

func stableRead(file *os.File, betweenReads func()) ([]byte, stableFileMetadata, error) {
	beforeInfo, err := file.Stat()
	if err != nil {
		return nil, stableFileMetadata{}, fmt.Errorf("stat legacy source before read: %w", err)
	}
	before, err := metadataFromInfo(beforeInfo)
	if err != nil {
		return nil, stableFileMetadata{}, err
	}
	if !beforeInfo.Mode().IsRegular() {
		return nil, stableFileMetadata{}, ErrLegacyNonRegular
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, stableFileMetadata{}, fmt.Errorf("rewind legacy source: %w", err)
	}
	first, err := io.ReadAll(file)
	if err != nil {
		return nil, stableFileMetadata{}, fmt.Errorf("read legacy source: %w", err)
	}
	if betweenReads != nil {
		betweenReads()
	}
	middleInfo, err := file.Stat()
	if err != nil {
		return nil, stableFileMetadata{}, fmt.Errorf("stat legacy source after read: %w", err)
	}
	middle, err := metadataFromInfo(middleInfo)
	if err != nil {
		return nil, stableFileMetadata{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, stableFileMetadata{}, fmt.Errorf("rewind legacy source for comparison: %w", err)
	}
	comparison, err := io.ReadAll(file)
	if err != nil {
		return nil, stableFileMetadata{}, fmt.Errorf("compare legacy source: %w", err)
	}
	afterInfo, err := file.Stat()
	if err != nil {
		return nil, stableFileMetadata{}, fmt.Errorf("stat legacy source after comparison: %w", err)
	}
	after, err := metadataFromInfo(afterInfo)
	if err != nil {
		return nil, stableFileMetadata{}, err
	}
	if !sameStableMetadata(before, middle) || !sameStableMetadata(middle, after) || !bytes.Equal(first, comparison) || int64(len(first)) != after.Size {
		return nil, stableFileMetadata{}, ErrLegacyDrift
	}
	return first, after, nil
}

func (p *unixLegacyProbe) Capture() (*LegacySnapshot, error) {
	if p == nil || !p.owned || !p.exists || !p.regular {
		return nil, ErrLegacyDrift
	}
	if p.beforeOpen != nil {
		p.beforeOpen()
	}
	fd, err := unix.Openat(p.parentFD, p.base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open legacy migration source: %w", ErrLegacyDrift)
	}
	file := os.NewFile(uintptr(fd), p.path)
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()

	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !statMatchesInfo(&p.pre, openedInfo) {
		return nil, ErrLegacyDrift
	}
	if p.afterOpen != nil {
		p.afterOpen()
	}
	var post unix.Stat_t
	if err := unix.Fstatat(p.parentFD, p.base, &post, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		identityFromUnixStat(&post) != identityFromUnixStat(&p.pre) || !statMatchesInfo(&post, openedInfo) {
		return nil, ErrLegacyDrift
	}

	data, meta, err := stableRead(file, p.betweenReads)
	if err != nil {
		return nil, err
	}
	if err := unix.Fstatat(p.parentFD, p.base, &post, unix.AT_SYMLINK_NOFOLLOW); err != nil || identityFromUnixStat(&post) != meta.Identity || !statMatchesInfo(&post, openedInfo) {
		return nil, ErrLegacyDrift
	}
	var launch LaunchConfig
	if _, err := toml.Decode(string(data), &launch); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLegacyMalformed, err)
	}

	plat := &unixLegacySnapshot{
		parentFD: p.parentFD,
		file:     file,
		base:     p.base,
		path:     p.path,
		meta:     meta,
	}
	p.owned = false // transfer the pinned parent descriptor to the snapshot.
	closeFile = false
	return &LegacySnapshot{
		Data:     data,
		Launch:   launch,
		Identity: meta.Identity,
		Mode:     meta.Mode,
		platform: plat,
	}, nil
}

func (s *unixLegacySnapshot) Revalidate(expected []byte) error {
	if s == nil || s.closed || s.file == nil {
		return ErrLegacyDrift
	}
	data, meta, err := stableRead(s.file, nil)
	if err != nil || !bytes.Equal(data, expected) || !sameStableMetadata(meta, s.meta) {
		return ErrLegacyDrift
	}
	var named unix.Stat_t
	if err := unix.Fstatat(s.parentFD, s.base, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return ErrLegacySourceMissing
		}
		return ErrLegacyDrift
	}
	if identityFromUnixStat(&named) != s.meta.Identity || named.Mode&unix.S_IFMT != unix.S_IFREG || int64(named.Size) != s.meta.Size {
		return ErrLegacyDrift
	}
	return nil
}

type unixBackupAllocation struct {
	parentFD  int
	parent    string
	name      string
	path      string
	identity  FileIdentity
	writer    *os.File
	validated *os.File
	validMeta stableFileMetadata
}

func createExclusiveBackup(parentFD int, parent, name string) (*os.File, FileIdentity, error) {
	fd, err := unix.Openat(parentFD, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, FileIdentity{}, err
	}
	f := os.NewFile(uintptr(fd), filepath.Join(parent, name))
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, FileIdentity{}, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, FileIdentity{}, ErrLegacyNonRegular
	}
	id, err := identityFromFileInfo(info)
	if err != nil {
		_ = f.Close()
		return nil, FileIdentity{}, err
	}
	return f, id, nil
}

func (s *unixLegacySnapshot) AllocateBackup() (*BackupAllocation, error) {
	stable := s.base + ".bak"
	writer, identity, err := createExclusiveBackup(s.parentFD, filepath.Dir(s.path), stable)
	name := stable
	if errors.Is(err, unix.EEXIST) {
		const maxAttempts = 100
		for range maxAttempts {
			var random [8]byte
			if _, randErr := rand.Read(random[:]); randErr != nil {
				return nil, fmt.Errorf("generate backup suffix: %w", randErr)
			}
			name = stable + "." + hex.EncodeToString(random[:])
			writer, identity, err = createExclusiveBackup(s.parentFD, filepath.Dir(s.path), name)
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("allocate legacy backup: %w", err)
	}
	platform := &unixBackupAllocation{
		parentFD: s.parentFD,
		parent:   filepath.Dir(s.path),
		name:     name,
		path:     filepath.Join(filepath.Dir(s.path), name),
		identity: identity,
		writer:   writer,
	}
	return &BackupAllocation{Name: name, Path: platform.path, Identity: identity, platform: platform}, nil
}

func writeAll(file *os.File, data []byte) error {
	for len(data) > 0 {
		n, err := file.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (b *unixBackupAllocation) Write(data []byte) error {
	if b.writer == nil {
		return ErrBackupIdentityLost
	}
	return writeAll(b.writer, data)
}

func (b *unixBackupAllocation) SetPrivateMode(source os.FileMode) error {
	if b.writer == nil {
		return ErrBackupIdentityLost
	}
	info, err := b.writer.Stat()
	if err != nil {
		return err
	}
	mode := info.Mode().Perm() & source.Perm() & 0o600
	return b.writer.Chmod(mode)
}

func (b *unixBackupAllocation) SyncFile() error {
	if b.writer == nil {
		return ErrBackupIdentityLost
	}
	return b.writer.Sync()
}

func (b *unixBackupAllocation) CloseWriter() error {
	if b.writer == nil {
		return nil
	}
	err := b.writer.Close()
	b.writer = nil
	return err
}

func (b *unixBackupAllocation) SyncParent() error {
	return unix.Fsync(b.parentFD)
}

func (b *unixBackupAllocation) Validate(expected []byte) error {
	fd, err := unix.Openat(b.parentFD, b.name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return ErrBackupDrift
	}
	f := os.NewFile(uintptr(fd), b.path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return ErrBackupDrift
	}
	id, err := identityFromFileInfo(info)
	if err != nil || id != b.identity {
		_ = f.Close()
		return ErrBackupDrift
	}
	data, meta, err := stableRead(f, nil)
	if err != nil || !bytes.Equal(data, expected) || meta.Identity != b.identity {
		_ = f.Close()
		return ErrBackupDrift
	}
	if b.validated != nil {
		_ = b.validated.Close()
	}
	b.validated = f
	b.validMeta = meta
	return nil
}

func (b *unixBackupAllocation) Revalidate(expected []byte) error {
	if b.validated == nil {
		return ErrBackupDrift
	}
	data, meta, err := stableRead(b.validated, nil)
	if err != nil || !bytes.Equal(data, expected) || !sameStableMetadata(meta, b.validMeta) {
		return ErrBackupDrift
	}
	var named unix.Stat_t
	if err := unix.Fstatat(b.parentFD, b.name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		identityFromUnixStat(&named) != b.identity || named.Mode&unix.S_IFMT != unix.S_IFREG || int64(named.Size) != meta.Size {
		return ErrBackupDrift
	}
	return nil
}

func (b *unixBackupAllocation) CleanupPartial() error {
	if b.writer != nil {
		_ = b.CloseWriter()
	}
	var named unix.Stat_t
	if err := unix.Fstatat(b.parentFD, b.name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		identityFromUnixStat(&named) != b.identity || named.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrBackupIdentityLost
	}
	if err := unix.Unlinkat(b.parentFD, b.name, 0); err != nil {
		return fmt.Errorf("unlink owned partial backup: %w", err)
	}
	if err := unix.Fsync(b.parentFD); err != nil {
		return fmt.Errorf("sync legacy parent after partial cleanup: %w", err)
	}
	return nil
}

func (b *unixBackupAllocation) Close() error {
	var errs []error
	if b.writer != nil {
		errs = append(errs, b.CloseWriter())
	}
	if b.validated != nil {
		errs = append(errs, b.validated.Close())
		b.validated = nil
	}
	return errors.Join(errs...)
}

func (s *unixLegacySnapshot) UnlinkNamedSource() error {
	if err := unix.Unlinkat(s.parentFD, s.base, 0); err != nil {
		return fmt.Errorf("unlink named legacy source: %w", err)
	}
	return nil
}

func (s *unixLegacySnapshot) SyncParent() error {
	if err := unix.Fsync(s.parentFD); err != nil {
		return fmt.Errorf("sync legacy parent: %w", err)
	}
	return nil
}

func (s *unixLegacySnapshot) Close() error {
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	var errs []error
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := unix.Close(s.parentFD); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (nativeMigrationFS) loadReadOnly(path string) (LaunchConfig, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return LaunchConfig{}, ErrNoLegacyLaunch
		}
		return LaunchConfig{}, fmt.Errorf("open legacy config read-only: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close() //nolint:errcheck
	info, err := f.Stat()
	if err != nil {
		return LaunchConfig{}, fmt.Errorf("stat legacy config read-only: %w", err)
	}
	if !info.Mode().IsRegular() {
		return LaunchConfig{}, ErrLegacyNonRegular
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return LaunchConfig{}, fmt.Errorf("read legacy config read-only: %w", err)
	}
	var launch LaunchConfig
	if _, err := toml.Decode(string(data), &launch); err != nil {
		return LaunchConfig{}, fmt.Errorf("%w: %v", ErrLegacyMalformed, err)
	}
	return launch, nil
}
