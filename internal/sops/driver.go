package sops

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cameronsjo/forgectl/internal/env"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// editDeadline bounds the `sops` edit call.
//
// It is a real control, not politeness. sops re-invokes its editor forever on
// a document it cannot parse, and while __sops-edit's own validation and
// once-only counter are the primary brakes, neither runs if sops fails to
// parse the file for a reason the editor never touched. Measured: 36,851
// invocations in about three minutes, so a minute is generous for a
// single-key edit and still bounds the pathological case.
const editDeadline = 60 * time.Second

// extractDeadline bounds the read-back. A decrypt of one value is fast; a KMS
// round-trip is the slow case worth allowing for.
const extractDeadline = 30 * time.Second

// outputCap bounds each captured stream. The runner's own ceiling is 64KiB and
// this narrows it: nothing sops says on a successful edit is long, and the
// interesting failure produced megabytes.
const outputCap = 16 << 10

// nonceBytes is the per-run nonce's length. Twenty bytes of base32 is well
// past guessable for a value that lives for one subprocess.
const nonceBytes = 20

// Client writes one scalar into a SOPS file.
type Client struct {
	runner exec.SensitiveRunner
}

// NewClient builds a Client over the sensitive execution seam.
//
// It takes SensitiveRunner rather than Runner deliberately. sops' stderr
// quotes the offending line of a document it failed to parse, and that line is
// `key: '<the secret>'`; Runner logs stderr at Error level, recorded at every
// enabled log_level (log_level defaults to off) and can be pointed at a file,
// and retains it on an error fang renders. This seam can render neither.
func NewClient(runner exec.SensitiveRunner) *Client {
	return &Client{runner: runner}
}

// SetValue writes value at path in the target SOPS file and proves it landed
// encrypted before reporting success.
//
// The sequence, and why each step is where it is:
//
//  1. Refuse if `sops` is absent, before anything else — a missing binary
//     should not look like a file problem.
//  2. Validate the path and the value BEFORE touching the file or reading
//     input, the same ordering internal/env's `set` defends.
//  3. Take the file lock. Everything below happens under it.
//  4. Read the file once, under the lock, and run the SOPS checks against
//     THOSE BYTES — never a re-open by name.
//  5. Refuse a key the file's own rules would store in cleartext.
//  6. Create the work directory as a SIBLING of the target, never $TMPDIR:
//     os.Rename across filesystems returns EXDEV, so a $TMPDIR backup would
//     fail to restore in exactly the error paths it exists for, while the
//     message claimed it succeeded.
//  7. Write the value to a 0600 file and the nonce beside it.
//  8. Run sops, interpreting three outcomes rather than two.
//  9. Read back and verify — including that the line is ciphertext.
//  10. Restore on any failure from step 8 on, and PROVE the restore.
func (c *Client) SetValue(ctx context.Context, target env.Target, rawPath, rawValue string) (Outcome, error) {
	// A nil runner means nobody wired the sensitive seam. Refusing is the
	// only safe answer: the obvious fallback is exec.Runner, and that is the
	// seam whose stderr logging is the reason this package does not use it.
	// Failing closed here keeps a wiring mistake from quietly becoming a
	// secret in a log file.
	if c == nil || c.runner == nil {
		return OutcomeUnspecified, errors.New("the sops writer has no execution seam wired; this is a forgectl bug, please report it")
	}

	sopsBin, err := osexec.LookPath("sops")
	if err != nil {
		return OutcomeUnspecified, errors.New("sops not found on PATH; install it with 'brew install sops'")
	}

	segments, err := ParsePath(rawPath)
	if err != nil {
		return OutcomeUnspecified, err
	}
	value, err := NormalizeValue(rawValue)
	if err != nil {
		return OutcomeUnspecified, err
	}

	var outcome Outcome
	lockErr := env.WithFileLock(target, func() error {
		var err error
		outcome, err = c.setLocked(ctx, sopsBin, target, segments, value)
		return err
	})
	if lockErr != nil {
		return OutcomeUnspecified, lockErr
	}
	return outcome, nil
}

// setLocked is the body that runs while the file lock is held.
func (c *Client) setLocked(ctx context.Context, sopsBin string, target env.Target, segments []string, value string) (Outcome, error) {
	before, err := env.ReadTarget(target)
	if err != nil {
		return OutcomeUnspecified, err
	}

	// Both checks run against the bytes just read, not a re-open. Re-opening
	// by name between the check and the use is how the final path component
	// gets swapped underneath a decision.
	if !IsSOPSFile(before) {
		return OutcomeUnspecified, fmt.Errorf("refusing %s: it has no top-level sops: block, so it is not a SOPS document", target.Rel())
	}
	rules, err := ReadPlaintextRules(before)
	if err != nil {
		return OutcomeUnspecified, fmt.Errorf("refusing %s: %w", target.Rel(), err)
	}
	// The WHOLE path, not the leaf: sops applies these rules to a key and its
	// entire subtree, so an ancestor decides the outcome. See
	// WouldStoreCleartext for the measurement.
	if cleartext, reason := rules.WouldStoreCleartext(segments); cleartext {
		// Names the rule and the file, never the path — the sops grammar
		// admits plenty of real credential shapes, so a token pasted into the
		// key slot reaches here.
		return OutcomeUnspecified, fmt.Errorf("refusing to write into %s: %s, so the value would be stored in the clear", target.Rel(), reason)
	}

	// The guard is armed BEFORE the work directory exists and released AFTER
	// it is removed: defers run last-in first-out, so release (deferred first)
	// runs after cleanup on every path, a panic included. A guarded signal
	// (SIGINT, SIGTERM, SIGHUP, SIGQUIT, SIGABRT on unix) anywhere in between
	// removes the directory and then terminates the process. Between
	// beginMutation and settle it first moves the ciphertext backup out beside
	// the target, so the next run refuses and points at it. See signal.go.
	guard := armPlaintextGuard()
	defer guard.release()

	work, err := guard.track(func() (*workDir, error) { return newWorkDir(target) })
	if err != nil {
		return OutcomeUnspecified, err
	}
	defer guard.cleanup()

	if err := work.stage(before, value); err != nil {
		return OutcomeUnspecified, err
	}

	editor, err := selfEditorCommand()
	if err != nil {
		return OutcomeUnspecified, err
	}

	editCtx, cancel := context.WithTimeout(ctx, editDeadline)
	defer cancel()

	// From here until settle, the target may hold something other than the
	// backup, so a signal keeps the backup (see signal.go).
	guard.beginMutation()
	res, runErr := c.runner.RunSensitive(editCtx, exec.SensitiveCommand{
		Kind: exec.KindSopsEdit,
		Path: exec.Secret(sopsBin),
		Args: []exec.Arg{exec.MustFixed("--disable-version-check"), exec.EndOfOptions(), exec.Opaque(target.Abs())},
		Env: []exec.EnvMutation{
			exec.ReplaceSopsEditor(editor),
			exec.ReplaceSopsWorkdir(work.dir),
			exec.ReplaceSopsPath(strings.Join(segments, ".")),
			exec.ReplaceSopsNonce(work.nonce),
			// sops' decrypted copy of the whole document goes in the work
			// directory, where the guard and the leftover scan cover it,
			// rather than in $TMPDIR, where sops leaves it on SIGHUP and
			// SIGQUIT (cameronsjo/forgectl#560).
			exec.ReplaceSopsTmpdir(work.dir),
		},
		StdoutCap: outputCap,
		StderrCap: outputCap,
	})

	switch {
	case runErr == nil:
		// Proceed to verification.
	case res.ExitCode == sopsUnchangedExit:
		// sops exits 200 with "File has not changed, exiting." when the
		// editor handed back identical bytes — which happens whenever the key
		// already holds this value. That is success, not failure, and it is
		// still verified below. Reporting it as an error would fail every
		// idempotent re-run.
	default:
		if restoreErr := work.restore(target); restoreErr != nil {
			return OutcomeUnspecified, restoreFailed(errors.New("sops refused the edit"), restoreErr, target, guard.keepBackup())
		}
		guard.settle()
		// The editor's own refusal, when there is one, is the actionable
		// message — a typo'd block name is the commonest mistake and its
		// reason lives only in the child. Every relayed message originates in
		// forgectl and names a rule rather than an argument; sops' own output
		// is still never surfaced.
		if reason := work.readEditorError(); reason != "" {
			return OutcomeUnspecified, fmt.Errorf("%s — %s is unchanged", reason, target.Rel())
		}
		return OutcomeUnspecified, sopsRefusalError(target)
	}

	// The staged plaintext has served its purpose the moment sops returns, so
	// it goes now rather than at cleanup. The work directory is a sibling of
	// the target and therefore INSIDE the repository. Before the signal guard,
	// a Ctrl-C during a KMS round-trip skipped the deferred cleanup and left
	// `value` (0600, the secret verbatim) where `git add -A` will commit it —
	// reproduced on the first of forty kill attempts. The guard now removes
	// the directory on the catchable terminating signals; shrinking the window
	// still matters, because SIGKILL, SIGSTOP, a power loss and any signal the
	// guard does not cover run no handler at all. What they leave behind is
	// refused, never swept, by the next write's leftover scan (internal/env).
	work.discardStagedValue()

	outcome, err := c.verify(ctx, sopsBin, target, segments, value, work)
	if err != nil {
		if restoreErr := work.restore(target); restoreErr != nil {
			return OutcomeUnspecified, restoreFailed(err, restoreErr, target, guard.keepBackup())
		}
		guard.settle()
		return OutcomeUnspecified, fmt.Errorf("%w — the file has been restored", err)
	}
	guard.settle()
	return outcome, nil
}

// sopsUnchangedExit is sops' status for "File has not changed, exiting."
// Measured on 3.13.3, and confirmed to return immediately rather than hang.
const sopsUnchangedExit = 200

// sopsRefusalError is the fixed message for a sops failure the editor did not
// explain.
//
// sops' own output is neither shown nor saved. Its YAML parse errors quote the
// offending line, which is `key: '<the secret>'`, and its stderr carries the
// operator's home path. It used to be written to a 0600 file under $TMPDIR
// and named here, but a file has to outlive the run to be worth naming, and
// nothing ever removed it, so every failed run left one more copy of output
// that could quote plaintext (cameronsjo/forgectl#652). What reaches this path
// is sops failing on its own terms, usually a key it cannot use, and a
// decrypt with stdout discarded reproduces that without writing anything.
func sopsRefusalError(target env.Target) error {
	return fmt.Errorf("sops refused the edit — %s is unchanged. Its output is withheld because it can quote the file's plaintext; running `sops decrypt` on the file with stdout discarded shows why it failed", target.Rel())
}

// restoreFailed is the error for a failed run whose restore also failed. kept
// is where the ciphertext backup now is (plaintextGuard.keepBackup), or ""
// when it could not be kept. It is named so the operator has a way back other
// than git (cameronsjo/forgectl#652).
func restoreFailed(cause, restoreErr error, target env.Target, kept string) error {
	if kept == "" {
		return fmt.Errorf("%w — and %s could NOT be restored: %v. Its backup could not be kept either; restore it from git", cause, target.Rel(), restoreErr)
	}
	shown := filepath.Base(kept)
	if rel, err := filepath.Rel(filepath.Dir(target.Abs()), kept); err == nil {
		shown = rel
	}
	shown = termsafe.QuotePath(filepath.Join(filepath.Dir(target.Rel()), shown))
	return fmt.Errorf("%w — and %s could NOT be restored: %v. Its ciphertext from before this run is kept at %s; restore from it or from git, then delete it", cause, target.Rel(), restoreErr, shown)
}

// verify proves the write landed, and landed ENCRYPTED.
//
// Three assertions in order, and the third is the one that matters most: a
// decrypt round-trip alone passes happily against a value stored in
// cleartext, which is precisely the failure step 5 exists to prevent. Checking
// that the re-read ciphertext line carries an ENC[ marker is what makes this
// verifier able to go red on that.
func (c *Client) verify(ctx context.Context, sopsBin string, target env.Target, segments []string, value string, work *workDir) (Outcome, error) {
	landedPath := filepath.Join(work.dir, "landed")

	extractCtx, cancel := context.WithTimeout(ctx, extractDeadline)
	defer cancel()

	_, runErr := c.runner.RunSensitive(extractCtx, exec.SensitiveCommand{
		Kind: exec.KindSopsExtract,
		Path: exec.Secret(sopsBin),
		Args: []exec.Arg{
			exec.MustFixed("--decrypt"),
			exec.MustFixed("--disable-version-check"),
			exec.MustFixed("--extract"),
			exec.Opaque(JoinExtract(segments)),
			exec.MustFixed("--output"),
			exec.Opaque(landedPath),
			exec.EndOfOptions(),
			exec.Opaque(target.Abs()),
		},
		StdoutCap: outputCap,
		StderrCap: outputCap,
	})
	// A verification command that FAILED TO RUN must never read as a clean
	// result, so a non-zero exit here is a refusal rather than a skipped
	// check.
	if runErr != nil {
		return OutcomeUnspecified, errors.New("the write could not be verified")
	}

	landed, err := os.ReadFile(landedPath) //nolint:gosec // G304: a path this process created inside its own 0700 work dir
	// Removed as soon as it is read: it is a second plaintext copy of the
	// secret, in a directory that sits inside the repository.
	work.discardLandedValue()
	if err != nil {
		return OutcomeUnspecified, errors.New("the write could not be verified")
	}
	// Byte-exact, with no trailing-newline strip: measured, `sops --decrypt
	// --extract --output` writes a scalar with NO terminator (an 8-byte value
	// produces an 8-byte file), so stripping one would mask a real
	// single-byte corruption.
	if len(landed) == 0 || !bytes.Equal(landed, []byte(value)) {
		// No digests. A SHA-256 of the intended plaintext, printed into a
		// transcript that gets committed, is offline-verifiable — for a
		// password, a PIN, or any value from a guessable set it IS the value.
		// It is also not actionable.
		return OutcomeUnspecified, errors.New("the value that landed does not match what was supplied")
	}

	after, err := env.ReadTarget(target)
	if err != nil {
		return OutcomeUnspecified, err
	}
	if err := assertEncryptedAtPath(after, segments); err != nil {
		return OutcomeUnspecified, err
	}

	return work.readOutcome(), nil
}

// assertEncryptedAtPath requires the scalar at exactly path in the re-read
// ciphertext to carry a sops ENC marker.
//
// # Why this resolves the path instead of scanning lines
//
// The first version scanned for a line whose trimmed text began with
// `leaf + ":"`, anywhere in the document, and that made the assertion
// DOCUMENT-ORDER DEPENDENT. A same-named key elsewhere that happened to be
// encrypted satisfied it. Proven by reordering one write: with an encrypted
// `app.token` above a cleartext `notes_unencrypted.token`, the scan matched
// app's line and the run reported success with the secret in plaintext; move
// the cleartext line first and the same write correctly went red.
//
// So the check that the design calls the one that matters most was the check
// that could not be made to go red on demand. It resolves the path properly
// now. The prefix match was sloppy too — `leaf+":"` also matches
// `token:anything`.
func assertEncryptedAtPath(doc []byte, path []string) error {
	var root yaml.Node
	if err := yaml.Unmarshal(doc, &root); err != nil {
		return errors.New("the re-read file does not parse as YAML")
	}

	node := &root
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	for _, segment := range path {
		next := mappingValue(node, segment)
		if next == nil {
			return errors.New("the written key could not be found in the re-read file")
		}
		node = next
	}

	if node.Kind != yaml.ScalarNode {
		return errors.New("the written key is not a scalar in the re-read file")
	}
	if strings.HasPrefix(node.Value, "ENC[AES256_GCM,") {
		return nil
	}
	// NOT ciphertext. Said without quoting the value, which by definition
	// holds the plaintext this refusal exists to report.
	return errors.New("the value was written in the clear rather than encrypted")
}

// mappingValue returns the value node for key in a mapping node, or nil.
//
// A yaml.v3 mapping stores Content as alternating key, value pairs, so this
// steps by two. Anything that is not a mapping has no keys to look up, which
// is a miss rather than an error — the caller reports the path as unfound.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// executablePath is a seam over os.Executable.
//
// It exists for the integration tests, which must point the editor at a
// forgectl built from the CURRENT tree rather than at the test binary
// os.Executable actually names — a test binary has no __sops-edit subcommand,
// so without this the tests could only exercise the driver against whatever
// forgectl happened to be installed, which is the wrong thing to test.
var executablePath = os.Executable

// selfEditorCommand builds the EDITOR value: this executable's resolved path
// plus the hidden subcommand, shell-quoted.
//
// os.Executable, never argv[0] — the caller controls argv[0], and Homebrew
// links forgectl into bin/ through a symlink that must be resolved so sops
// spawns the real binary. sops shell-word-splits EDITOR and honours quotes,
// measured on 3.13.3, so a path containing a space survives single-quoting.
func selfEditorCommand() (string, error) {
	self, err := executablePath()
	if err != nil {
		return "", errors.New("could not resolve this executable's own path")
	}
	resolved, err := filepath.EvalSymlinks(self)
	if err != nil {
		resolved = self
	}
	return "'" + strings.ReplaceAll(resolved, "'", `'\''`) + "' __sops-edit", nil
}

// workDir is the private 0700 directory holding the value, the nonce, the
// backup, and the outcome for one run. It is also the edit call's TMPDIR, so
// sops' decrypted copy of the whole document lives here while the editor runs.
type workDir struct {
	dir    string
	nonce  string
	backup string
	// keep is where an interrupted run leaves the backup: beside the target,
	// scoped to it, so the next run's leftover scan finds it.
	keep string
}

// newWorkDir creates the directory as a SIBLING of the target.
//
// Not $TMPDIR, and that is not a preference: os.Rename across filesystems
// returns EXDEV, so a backup in $TMPDIR would fail to restore in exactly the
// error paths a backup exists for — while a message said it had succeeded. A
// sibling is also what keeps sops' own upward walk for .sops.yaml reaching the
// same rules it would from the target itself.
func newWorkDir(target env.Target) (*workDir, error) {
	parent := filepath.Dir(target.Abs())
	// Scoped to the target, so the next run's leftover scan (internal/env,
	// under this same lock) can attribute a directory a SIGKILL left behind.
	dir, err := os.MkdirTemp(parent, target.SopsWorkDirPattern())
	if err != nil {
		return nil, fmt.Errorf("create a work directory beside %s: %w", target.Rel(), err)
	}
	// 0700, not 0600: a directory needs its execute bit to be entered at all,
	// which is what gosec's file-oriented rule does not model.
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: 0700 on a DIRECTORY; the execute bit is required
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("secure the work directory beside %s: %w", target.Rel(), err)
	}

	buf := make([]byte, nonceBytes)
	if _, err := rand.Read(buf); err != nil {
		_ = os.RemoveAll(dir)
		return nil, errors.New("could not generate a nonce")
	}

	return &workDir{
		dir:    dir,
		nonce:  base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf),
		backup: filepath.Join(dir, "backup"),
		keep:   target.SopsBackupPath(),
	}, nil
}

// stage writes the ciphertext backup, the value, and the nonce.
func (w *workDir) stage(before []byte, value string) error {
	if err := os.WriteFile(w.backup, before, 0o600); err != nil {
		return errors.New("could not write the backup")
	}
	// No added newline: the read-back comparison is byte-exact, and a
	// terminator here would make every value fail it.
	if err := os.WriteFile(filepath.Join(w.dir, "value"), []byte(value), 0o600); err != nil {
		return errors.New("could not stage the value")
	}
	if err := os.WriteFile(filepath.Join(w.dir, "nonce"), []byte(w.nonce), 0o600); err != nil {
		return errors.New("could not stage the nonce")
	}
	return nil
}

// readOutcome reports what __sops-edit recorded, defaulting to replaced.
//
// The rc=200 "file has not changed" path is NOT why the default exists, which
// an earlier version of this comment got backwards. The editor does run on
// that path — identical bytes are what it produced — and it records `result`
// before writing the document, so a real value is there to read.
//
// The default fires only when the child died before recording, or the file is
// unreadable. Reporting "replaced" then is a guess about a write of unknown
// shape, and it is the safer guess: verify has already proven the value is
// present and encrypted at the requested path, so the only question left is
// whether the key was new, and calling a new key "replaced" understates
// rather than overstates what happened.
func (w *workDir) readOutcome() Outcome {
	recorded, err := os.ReadFile(filepath.Join(w.dir, "result"))
	if err != nil {
		return OutcomeReplaced
	}
	if strings.TrimSpace(string(recorded)) == OutcomeAdded.String() {
		return OutcomeAdded
	}
	return OutcomeReplaced
}

// maxEditorErrorBytes bounds the relayed message. It comes from a forgectl
// process, so it is trusted in origin, but reading an unbounded file into an
// error string is a habit worth not forming.
const maxEditorErrorBytes = 4096

// readEditorError returns the refusal __sops-edit recorded, or "".
//
// The content is trimmed to one line: every message that reaches here is a
// single-sentence refusal from forgectl's own code, and collapsing anything
// else keeps a surprise out of a rendered error.
func (w *workDir) readEditorError() string {
	data, err := os.ReadFile(filepath.Join(w.dir, "error"))
	if err != nil || len(data) == 0 {
		return ""
	}
	if len(data) > maxEditorErrorBytes {
		data = data[:maxEditorErrorBytes]
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSpace(line)
}

// restore puts the backup back and PROVES it, by digest, before returning
// success. A restore that reports success without checking is the one thing
// worse than no restore at all.
func (w *workDir) restore(target env.Target) error {
	backup, err := os.ReadFile(w.backup)
	if err != nil {
		return errors.New("the backup is unreadable")
	}
	if err := env.WriteTarget(target, backup); err != nil {
		return err
	}
	after, err := env.ReadTarget(target)
	if err != nil {
		return err
	}
	if sha256.Sum256(after) != sha256.Sum256(backup) {
		return errors.New("the restored file does not match the backup")
	}
	return nil
}

// discardStagedValue removes the plaintext value file. Errors are dropped: the
// deferred cleanup removes the whole directory anyway, so this is about
// shortening the window, not about being the only remover.
func (w *workDir) discardStagedValue() {
	_ = os.Remove(filepath.Join(w.dir, "value"))
}

// discardLandedValue removes the decrypted read-back. Same reasoning as
// discardStagedValue — it is a second plaintext copy of the same secret and
// has no reason to outlive the comparison it exists for.
func (w *workDir) discardLandedValue() {
	_ = os.Remove(filepath.Join(w.dir, "landed"))
}

// preserveBackup moves the ciphertext backup out of the work directory to
// keep, and reports whether it did. The two are in the same directory, so
// neither step can fail with EXDEV.
//
// It never replaces an existing file. Under the lock, the leftover scan has
// already refused any earlier backup, so one appearing now was put there
// outside the lock, and it is evidence too. The move is a hard link followed
// by an unlink, because link(2) fails with EEXIST rather than replacing, so
// the no-clobber check and the move are one atomic step. The Lstat-then-rename
// it replaced left a window in which a file created at keep was overwritten
// (cameronsjo/forgectl#652). On a filesystem that refuses hard links, it
// falls back to that check-then-rename, which is still no worse than losing
// the backup. A failed unlink leaves two links to one ciphertext, which
// exposes nothing.
func (w *workDir) preserveBackup() bool {
	if w.keep == "" {
		return false
	}
	err := os.Link(w.backup, w.keep)
	switch {
	case err == nil:
		_ = os.Remove(w.backup)
		return true
	case errors.Is(err, os.ErrExist):
		return false
	}
	if _, err := os.Lstat(w.backup); err != nil {
		return false
	}
	if _, err := os.Lstat(w.keep); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	return os.Rename(w.backup, w.keep) == nil
}

// pruneToBackup removes every entry of the work directory except the
// ciphertext backup, and reports whether the backup is now the ONLY thing
// left. A false result means the caller must remove the whole directory: a
// directory kept for its backup must never also keep a plaintext file.
func (w *workDir) pruneToBackup() bool {
	backupName := filepath.Base(w.backup)
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() != backupName {
			_ = os.RemoveAll(filepath.Join(w.dir, e.Name()))
		}
	}
	entries, err = os.ReadDir(w.dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != backupName || !entries[0].Type().IsRegular() {
		return false
	}
	return true
}

func (w *workDir) cleanup() { _ = os.RemoveAll(w.dir) }
