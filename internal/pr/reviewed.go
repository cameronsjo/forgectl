package pr

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// ReviewedStore is the local, offline reviewed-state authority for `forgectl
// pr`: it maps a PR's host-qualified "host/owner/repo#N" form (reviewedKey) to
// the timestamp it was last marked reviewed.
//
// The host is part of the key (#668): owner/repo#N alone would let a mark on
// one forge dim the same-numbered PR in a same-named repo on another. Marks
// written before that carry no host ("owner/repo#N"); they are read as the
// store's default host (the configured [github] host) and never as "any
// host". A Mark on the default host rewrites its legacy entry under the
// qualified key — a one-way migration on write. A local (offline) session has
// no forge and keeps its unqualified "local/repo#N" key.
//
// Timestamp-not-boolean is load-bearing: dimming and the picker's skip both
// derive from a single comparison — reviewedAt >= the PR's latest activity —
// so any newer activity (a push, comment, label, or review — whatever bumps
// the PR's updatedAt) auto-un-dims the PR with no separate un-dim logic. The
// on-disk shape is a plain JSON object mapping the
// breadcrumb form to an RFC3339 time; a missing, unreadable, or malformed file
// loads as an empty store, never an error (mirrors internal/net's cache).
type ReviewedStore struct {
	path string
	at   map[string]time.Time
	now  func() time.Time

	// defaultHost is what an empty Ref.Host means, and the only host a legacy
	// host-less key is ever read as. Empty is treated as github.com.
	defaultHost string
}

// ReviewedOption configures a ReviewedStore at load time.
type ReviewedOption func(*ReviewedStore)

// WithNow overrides the clock used to stamp Mark — used in tests so the
// reviewedAt timestamp is deterministic (mirrors net.WithNow).
func WithNow(fn func() time.Time) ReviewedOption {
	return func(s *ReviewedStore) { s.now = fn }
}

// WithDefaultHost sets the host an empty Ref.Host means — the configured
// [github] host. It is also the sole host that legacy host-less marks belong to.
func WithDefaultHost(host string) ReviewedOption {
	return func(s *ReviewedStore) { s.defaultHost = host }
}

// storeHost is the host a ref's mark lives under: its own, else the store's
// default. ok is false for a host that fails validation — such a ref cannot
// be keyed safely, so it is never marked and never reads as reviewed.
func (s *ReviewedStore) storeHost(ref Ref) (string, bool) {
	host := ref.Host
	if host == "" {
		host = s.defaultHost
	}
	if host == "" {
		host = defaultGitHubHost
	}
	host = strings.ToLower(host)
	return host, ValidHostSegment(host)
}

// keys returns the qualified key a ref's mark is stored under and, when the
// ref is on the store's default host, the legacy host-less key that may still
// hold an older mark for it. Both are "" when the ref cannot be keyed. A local
// ref has one key: its unqualified String() form.
func (s *ReviewedStore) keys(ref Ref) (qualified, legacy string) {
	if ref.IsLocal() {
		return ref.String(), ""
	}
	host, ok := s.storeHost(ref)
	if !ok {
		return "", ""
	}
	qualified = host + "/" + ref.String()
	def := s.defaultHost
	if def == "" {
		def = defaultGitHubHost
	}
	if host == strings.ToLower(def) {
		legacy = ref.String()
	}
	return qualified, legacy
}

// lookup returns the newest mark among a ref's qualified and legacy keys.
func (s *ReviewedStore) lookup(ref Ref) (time.Time, bool) {
	q, l := s.keys(ref)
	var best time.Time
	found := false
	for _, k := range []string{q, l} {
		if k == "" {
			continue
		}
		if at, ok := s.at[k]; ok && (!found || at.After(best)) {
			best, found = at, true
		}
	}
	return best, found
}

// LoadReviewed reads the reviewed-state store at path. A missing, unreadable,
// or malformed file yields an empty store rather than an error — the same
// corrupt-tolerant model internal/net uses for its cache, so a hand-mangled
// file degrades to "nothing reviewed yet" instead of blinding the dashboards.
func LoadReviewed(path string, opts ...ReviewedOption) *ReviewedStore {
	// Clean once, here, so persist's kernel stats, its resolver and this
	// load all name the same file: readReviewedFile cleans, and an uncleaned
	// "cfglink/../x" would otherwise be written physically but read lexically.
	if path != "" {
		path = filepath.Clean(path)
	}
	s := &ReviewedStore{
		path: path,
		at:   make(map[string]time.Time),
		now:  time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	data, err := readReviewedFile(path)
	if err != nil {
		if errors.Is(err, errRecordNotRegular) {
			slog.Warn("Ignoring a pr-reviewed store that is not a regular file; starting empty.", "path", path)
		}
		return s // missing / unreadable / not a regular file → empty
	}
	var at map[string]time.Time
	if err := json.Unmarshal(data, &at); err != nil {
		slog.Warn("Ignoring unreadable pr-reviewed store; starting empty.", "path", path, "error", err)
		return s // malformed → empty
	}
	if at != nil {
		s.at = at
	}
	slog.Debug("Successfully loaded reviewed store.", "path", path, "count", len(s.at))
	return s
}

// readReviewedFile reads the store without blocking in the open: a FIFO
// planted at path would block a plain os.ReadFile forever (forgectl#765). The
// open is O_NONBLOCK (openNonblock, which clears it before returning), and the
// descriptor is Fstat'ed and read only if it is a regular file.
//
// It is deliberately not readRecordFile, in two ways. A symlink is followed:
// the store lives under the user's config dir, where dotfile managers link
// files in, and persist writes through the link. And the read is not capped at
// the 8 KiB record bound: the store grows by one entry per reviewed PR. Either
// refusal would read a real store as empty, and the next Mark would overwrite
// it with one entry.
func readReviewedFile(path string) ([]byte, error) {
	f, err := openNonblock(filepath.Clean(path), os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, &os.PathError{Op: "read", Path: path, Err: errRecordNotRegular}
	}
	return io.ReadAll(f)
}

// Mark stamps ref as reviewed at the current clock and persists the store.
// The mark is keyed by host; a legacy host-less entry for the same PR on the
// default host is dropped in the same write.
func (s *ReviewedStore) Mark(ref Ref) error {
	q, l := s.keys(ref)
	if q == "" {
		return errors.New("pr: cannot mark reviewed: the PR's host failed validation")
	}
	if l != "" {
		delete(s.at, l)
	}
	return s.MarkKey(q)
}

// MarkKey is Mark for a caller that keys entries by an arbitrary canonical
// string (internal/review's host-qualified "host/owner/repo#N" keys) rather
// than a Ref. The Ref methods delegate here — one write path, two key shapes.
func (s *ReviewedStore) MarkKey(key string) error {
	slog.Debug("Preparing to mark reviewed.", "key", key)
	s.at[key] = s.now()
	if err := s.persist(); err != nil {
		slog.Error("Failed to mark reviewed.", "key", key, "error", err)
		return err
	}
	slog.Info("Successfully marked reviewed.", "key", key)
	return nil
}

// Unmark clears ref's reviewed mark and persists. A ref that was never marked
// is a no-op — no write fires.
// Both the qualified key and, on the default host, the legacy key are cleared.
func (s *ReviewedStore) Unmark(ref Ref) error {
	q, l := s.keys(ref)
	if q == "" {
		return nil
	}
	changed := false
	for _, k := range []string{q, l} {
		if _, ok := s.at[k]; k != "" && ok {
			delete(s.at, k)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := s.persist(); err != nil {
		slog.Error("Failed to unmark reviewed.", "key", q, "error", err)
		return err
	}
	slog.Info("Successfully unmarked reviewed.", "key", q)
	return nil
}

// UnmarkKey is Unmark for string-keyed callers (see MarkKey).
func (s *ReviewedStore) UnmarkKey(key string) error {
	slog.Debug("Preparing to unmark reviewed.", "key", key)
	if _, ok := s.at[key]; !ok {
		slog.Debug("Skipping unmark: entry was not marked reviewed.", "key", key)
		return nil
	}
	delete(s.at, key)
	if err := s.persist(); err != nil {
		slog.Error("Failed to unmark reviewed.", "key", key, "error", err)
		return err
	}
	slog.Info("Successfully unmarked reviewed.", "key", key)
	return nil
}

// IsReviewed reports whether ref was marked reviewed at or after its latest
// activity. A later latestActivity (any new activity that bumps the PR's
// updatedAt) makes a previously-marked PR read as unreviewed again — the
// auto-un-dim falls out of this comparison, so there is no separate un-dim path.
func (s *ReviewedStore) IsReviewed(ref Ref, latestActivity time.Time) bool {
	at, ok := s.lookup(ref)
	return ok && !at.Before(latestActivity)
}

// IsReviewedKey is IsReviewed for string-keyed callers (see MarkKey) — the
// identical timestamp comparison, so issues and PRs share the auto-un-dim
// semantics exactly.
func (s *ReviewedStore) IsReviewedKey(key string, latestActivity time.Time) bool {
	at, ok := s.at[key]
	if !ok {
		return false
	}
	return !at.Before(latestActivity)
}

// ReviewedAt returns the stored reviewed timestamp for ref, if any. It backs
// the CLI-layer "previously marked reviewed" note on an explicit `pr <ref>`
// launch — a note only, never a skip.
func (s *ReviewedStore) ReviewedAt(ref Ref) (time.Time, bool) {
	return s.lookup(ref)
}

// Sync prunes any stored entry whose ref is not in openRefs and persists when
// anything changed. It keeps the store from growing without bound as PRs close
// and merge — `pr reviewed sync` feeds it the current open set.
func (s *ReviewedStore) Sync(openRefs []Ref) error {
	open := make(map[string]bool, len(openRefs))
	hosts := make(map[string]bool)
	for _, r := range openRefs {
		q, l := s.keys(r)
		if q == "" {
			continue
		}
		open[q] = true
		if l != "" {
			open[l] = true
		}
		if !r.IsLocal() {
			host, _ := s.storeHost(r)
			hosts[host] = true
		}
	}
	changed := false
	for key := range s.at {
		if open[key] {
			continue
		}
		// A qualified key is only prunable when its host had refs in this
		// run's open set: a host absent from it was not queried, which is not
		// the same as everything on it closing. A legacy key is the default
		// host's, and the open set is always that host's.
		if host, _, ok := strings.Cut(key, "/"); ok && isQualifiedKey(key) && !hosts[host] {
			continue
		}
		delete(s.at, key)
		changed = true
	}
	if !changed {
		return nil
	}
	return s.persist()
}

// isQualifiedKey reports whether key has the "host/owner/repo#N" shape (two
// slashes before the '#') rather than the legacy "owner/repo#N".
func isQualifiedKey(key string) bool {
	head, _, _ := strings.Cut(key, "#")
	return strings.Count(head, "/") >= 2
}

// SyncKeys is Sync for string-keyed callers (see MarkKey).
func (s *ReviewedStore) SyncKeys(openKeys []string) error {
	slog.Debug("Preparing to sync reviewed store.", "storeSize", len(s.at), "openCount", len(openKeys))
	open := make(map[string]bool, len(openKeys))
	for _, k := range openKeys {
		open[k] = true
	}
	changed := false
	pruned := 0
	for key := range s.at {
		if !open[key] {
			delete(s.at, key)
			changed = true
			pruned++
		}
	}
	if !changed {
		slog.Debug("Skipping sync: no changes to store.", "openCount", len(openKeys))
		return nil
	}
	if err := s.persist(); err != nil {
		slog.Error("Failed to sync reviewed store.", "openCount", len(openKeys), "pruned", pruned, "error", err)
		return err
	}
	slog.Info("Successfully synced reviewed store.", "openCount", len(openKeys), "pruned", pruned)
	return nil
}

// SyncKeysScoped is SyncKeys restricted to a set of active host prefixes: an
// entry is only eligible for pruning when its key's host segment (the
// substring before the first '/' — internal/review's keys are host-qualified,
// "host/owner/repo#N") is in activeHosts. Entries for a host with no active
// source in this run are left untouched, whatever openKeys says — openKeys
// only reflects the sources that actually ran, and a host that was merely
// omitted (a disabled or misconfigured second source, say) is NOT the same
// as "everything on that host closed"; pruning against a partial universe
// would silently wipe out every mark for the omitted host. Callers with a
// single, unqualified key dialect (internal/pr's own "owner/repo#N" keys,
// which carry no host segment at all) should keep using SyncKeys.
func (s *ReviewedStore) SyncKeysScoped(openKeys []string, activeHosts []string) error {
	slog.Debug("Preparing to sync reviewed store (host-scoped).", "storeSize", len(s.at), "openCount", len(openKeys), "activeHosts", activeHosts)
	active := make(map[string]bool, len(activeHosts))
	for _, h := range activeHosts {
		active[h] = true
	}
	open := make(map[string]bool, len(openKeys))
	for _, k := range openKeys {
		open[k] = true
	}
	changed := false
	pruned := 0
	for key := range s.at {
		host, _, _ := strings.Cut(key, "/")
		if !active[host] {
			continue // host has no active source this run — never eligible for pruning
		}
		if !open[key] {
			delete(s.at, key)
			changed = true
			pruned++
		}
	}
	if !changed {
		slog.Debug("Skipping sync: no changes to store.", "openCount", len(openKeys))
		return nil
	}
	if err := s.persist(); err != nil {
		slog.Error("Failed to sync reviewed store.", "openCount", len(openKeys), "pruned", pruned, "error", err)
		return err
	}
	slog.Info("Successfully synced reviewed store.", "openCount", len(openKeys), "pruned", pruned, "activeHosts", activeHosts)
	return nil
}

// persist writes the store to disk, creating the parent dir as needed.
//
// The write is atomic (forgectl#791): the bytes go to a fresh temp file in the
// destination's own directory (os.CreateTemp: O_EXCL, 0600), which is synced
// and then renamed over the destination, and the directory is synced after the
// rename. A crash mid-write leaves the old store whole instead of truncated.
// And the destination is Lstat'ed first: a FIFO, or anything else that is not a
// regular file, is refused before any write, where os.WriteFile would block in
// its open waiting for a reader.
//
// A rename is a new file, not a rewrite, so it differs from the in-place
// os.WriteFile it replaced in four ways. It replaces a read-only (0444) store,
// which the old open for writing refused. It breaks a hardlink: the other name
// keeps the old bytes. It resets the mode to 0600 and the owner to the writer,
// where the old write kept both. And it needs the destination's directory to be
// writable, where the old write needed only the file to be.
//
// A symlink at the store path is resolved first (resolveStoreTarget), and the
// rename replaces the file the link names, never the link. The store lives
// under the user's config dir, where dotfile managers link files in
// (readReviewedFile follows the link for the same reason), and renaming over
// the link would silently turn it into a plain file. A dangling link resolves
// to the file it would create.
//
// THE KERNEL IS THE ARBITER, not the resolver. A load reads s.path through the
// kernel, and any shape where a hand resolver disagrees with it (its
// 40-symlinks-per-lookup cap counts directory links too, for one) would write
// one file while every load read another, or read ELOOP and load empty, and
// the next Mark would then overwrite the store: silent loss. So s.path is
// stat'ed through the kernel before anything is written, and anything but
// success or "does not exist yet" refuses. After the rename it is stat'ed
// again, and must now be the very file just written (os.SameFile); if it is
// not, persist says so rather than claiming the mark. A divergence can
// therefore cost a loud refusal, never a silently lost mark.
//
// The rename is the commit point. A directory fsync that fails after it is
// logged as a durability warning and the mark reports success: the bytes are
// written, and a caller must not print "failed to mark" for a mark that is on
// disk. This deliberately differs from writeRecordAtomic and
// writeRepairLogAtomic, which return that error (worded as written but not
// confirmed durable) because their callers act on it.
func (s *ReviewedStore) persist() error {
	if s.path == "" {
		return errors.New("pr: reviewed store path unset")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("pr: reviewed store path does not resolve; refusing to write it: %w", err)
	}
	dest, err := resolveStore(s.path)
	if err != nil {
		return err
	}
	info, err := os.Lstat(dest)
	switch {
	case err == nil && !info.Mode().IsRegular():
		return &os.PathError{Op: "write", Path: dest, Err: errRecordNotRegular}
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}
	// termsafe:allow-raw-json persisted review timestamp, never command output
	data, err := json.Marshal(s.at)
	if err != nil {
		return err
	}
	written, err := writeFileAtomic(dest, data)
	if err != nil {
		return err
	}
	if serr := syncStoreDir(filepath.Dir(dest)); serr != nil {
		slog.Warn("The pr-reviewed store is written; its durability could not be confirmed.",
			"path", dest, "error", serr)
	}
	got, err := os.Stat(s.path)
	if err != nil || !os.SameFile(got, written) {
		if err == nil {
			err = errStoreNotWritten
		}
		return fmt.Errorf("pr: reviewed store path does not resolve to the file just written; "+
			"%s now holds the store and the mark may not be visible: %w", termsafe.QuotePath(dest), err)
	}
	return nil
}

// errStoreNotWritten is persist's post-write refusal of a store path that
// resolves, but to some file other than the one it wrote.
var errStoreNotWritten = errors.New("it names a different file")

// syncStoreDir fsyncs the store's directory after the rename, so the new name
// is durable, as writeRepairLogAtomic does for the audit log. It is a var only
// so a test can see it run and make it fail.
var syncStoreDir = osRecordFS{}.SyncDir

// resolveStore is resolveStoreTarget, a var only so a test can make the
// resolver disagree with the kernel and see persist's post-write check refuse.
var resolveStore = resolveStoreTarget

// maxStoreLinkHops is the kernel's cap on symlinks followed in one lookup
// (Linux's MAXSYMLINKS). resolveStoreTarget follows at most that many final
// links. Directory links along the way count toward the kernel's cap but not
// this one, which is why persist's kernel stats, not this cap, arbitrate.
const maxStoreLinkHops = 40

// errStoreLinkLoop is resolveStoreTarget's refusal of a link chain longer than
// maxStoreLinkHops.
var errStoreLinkLoop = errors.New("too many levels of symbolic links")

// resolveStoreTarget is the file a write through path would reach, resolved
// PHYSICALLY, as the kernel resolves it for readReviewedFile's open. Resolving
// lexically is wrong: filepath.Clean collapses "dir/.." to the dir's parent by
// name, while the kernel goes to the parent of whatever dir links to. Under a
// stow-folded ~/.config/forgectl -> ~/dotfiles/forgectl holding
// pr-reviewed.json -> ../shared/pr-reviewed.json, a lexical resolve writes
// ~/.config/shared/... while every load reads ~/dotfiles/shared/...
//
// It walks the chain one final link at a time, existing target or dangling
// alike: each hop's DIRECTORY part is resolved with filepath.EvalSymlinks
// before the hop's base name is looked at, and a relative link target is
// appended raw (never Cleaned), so the next hop's EvalSymlinks applies its
// ".." to the physical directory. persist checks the result against the
// kernel on both sides of the write.
func resolveStoreTarget(path string) (string, error) {
	cur := path
	// maxStoreLinkHops links, plus the lookup of the name the last one reaches.
	for range maxStoreLinkHops + 1 {
		rawDir, base := splitLastElem(cur)
		if base == "" || base == "." || base == ".." {
			// A link to a directory-shaped name; resolve the whole thing and
			// let persist's Lstat refuse what it reaches.
			return filepath.EvalSymlinks(cur)
		}
		dir, err := filepath.EvalSymlinks(rawDir)
		if err != nil {
			return "", err
		}
		cur = filepath.Join(dir, base)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return cur, nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return cur, nil
		}
		next, err := os.Readlink(cur)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(next) {
			// Raw, not filepath.Join: Join Cleans, which would collapse a ".."
			// in next against dir by name before EvalSymlinks could see it.
			next = dir + string(filepath.Separator) + next
		}
		cur = next
	}
	return "", &os.PathError{Op: "resolve", Path: path, Err: errStoreLinkLoop}
}

// splitLastElem splits p at its last separator without cleaning either half
// (filepath.Dir Cleans, which would collapse "link/.." by name). A volume name
// (Windows "C:" or a UNC share) stays with the directory half, and a separator
// directly after it keeps the directory rooted. A trailing separator yields an
// empty base.
func splitLastElem(p string) (dir, base string) {
	vol := filepath.VolumeName(p)
	rest := p[len(vol):]
	i := len(rest) - 1
	for i >= 0 && !os.IsPathSeparator(rest[i]) {
		i--
	}
	switch {
	case i < 0 && vol == "":
		return ".", rest
	case i < 0:
		return vol, rest
	case i == 0:
		return vol + rest[:1], rest[1:]
	}
	return vol + rest[:i], rest[i+1:]
}

// writeFileAtomic writes data to a new temp file beside dest and renames it
// over dest, returning the written file's own Fstat for persist's post-write
// identity check. The temp file is removed on any failure before the rename.
func writeFileAtomic(dest string, data []byte) (written fs.FileInfo, err error) {
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".tmp-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if written, err = tmp.Stat(); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err = tmp.Close(); err != nil {
		return nil, err
	}
	if err = os.Rename(tmpName, dest); err != nil {
		return nil, err
	}
	return written, nil
}
