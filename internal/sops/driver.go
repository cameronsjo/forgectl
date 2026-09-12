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

	"github.com/cameronsjo/forgectl/internal/env"
	"github.com/cameronsjo/forgectl/internal/exec"
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
// `key: '<the secret>'`; Runner logs stderr at Error level, which survives any
// log-level setting and can be pointed at a file, and retains it on an error
// fang renders. This seam can render neither.
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
	leaf := segments[len(segments)-1]
	if cleartext, reason := rules.WouldStoreCleartext(leaf); cleartext {
		// Names the rule and the file, never the path — the sops grammar
		// admits plenty of real credential shapes, so a token pasted into the
		// key slot reaches here.
		return OutcomeUnspecified, fmt.Errorf("refusing to write into %s: %s, so the value would be stored in the clear", target.Rel(), reason)
	}

	work, err := newWorkDir(target)
	if err != nil {
		return OutcomeUnspecified, err
	}
	defer work.cleanup()

	if err := work.stage(before, value); err != nil {
		return OutcomeUnspecified, err
	}

	editor, err := selfEditorCommand()
	if err != nil {
		return OutcomeUnspecified, err
	}

	editCtx, cancel := context.WithTimeout(ctx, editDeadline)
	defer cancel()

	res, runErr := c.runner.RunSensitive(editCtx, exec.SensitiveCommand{
		Kind: exec.KindSopsEdit,
		Path: exec.Secret(sopsBin),
		Args: []exec.Arg{exec.EndOfOptions(), exec.Opaque(target.Abs())},
		Env: []exec.EnvMutation{
			exec.ReplaceSopsEditor(editor),
			exec.ReplaceSopsWorkdir(work.dir),
			exec.ReplaceSopsPath(strings.Join(segments, ".")),
			exec.ReplaceSopsNonce(work.nonce),
		},
		StdoutCap: outputCap,
		StderrCap: outputCap,
	})

	// sops' own output must never reach a rendered error: a YAML parse error
	// quotes the offending line, and the offending line carries the value.
	// It is written to a 0600 file inside the work directory instead, and the
	// path is named so an operator can read it deliberately.
	logPath, logErr := work.captureOutput(res)

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
			return OutcomeUnspecified, fmt.Errorf("sops refused the edit and the file could NOT be restored: %w", restoreErr)
		}
		// The editor's own refusal, when there is one, is the actionable
		// message — a typo'd block name is the commonest mistake and its
		// reason lives only in the child. Every relayed message originates in
		// forgectl and names a rule rather than an argument; sops' own output
		// is still never surfaced.
		if reason := work.readEditorError(); reason != "" {
			return OutcomeUnspecified, fmt.Errorf("%s — %s is unchanged", reason, target.Rel())
		}
		return OutcomeUnspecified, sopsRefusalError(target, logPath, logErr)
	}

	outcome, err := c.verify(ctx, sopsBin, target, segments, value, work)
	if err != nil {
		if restoreErr := work.restore(target); restoreErr != nil {
			return OutcomeUnspecified, fmt.Errorf("%w — and the file could NOT be restored: %v", err, restoreErr)
		}
		return OutcomeUnspecified, err
	}
	return outcome, nil
}

// sopsUnchangedExit is sops' status for "File has not changed, exiting."
// Measured on 3.13.3, and confirmed to return immediately rather than hang.
const sopsUnchangedExit = 200

// sopsRefusalError is the fixed message for a sops failure. It names the log
// path and nothing else; see captureOutput for why.
func sopsRefusalError(target env.Target, logPath string, logErr error) error {
	if logErr != nil || logPath == "" {
		return fmt.Errorf("sops refused the edit — %s is unchanged", target.Rel())
	}
	return fmt.Errorf("sops refused the edit — %s is unchanged; details in %s", target.Rel(), logPath)
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
		return OutcomeUnspecified, errors.New("the write could not be verified — the file has been restored")
	}

	landed, err := os.ReadFile(landedPath) //nolint:gosec // G304: a path this process created inside its own 0700 work dir
	if err != nil {
		return OutcomeUnspecified, errors.New("the write could not be verified — the file has been restored")
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
		return OutcomeUnspecified, errors.New("the value that landed does not match what was supplied — the file has been restored")
	}

	after, err := env.ReadTarget(target)
	if err != nil {
		return OutcomeUnspecified, err
	}
	if err := assertLineEncrypted(after, segments[len(segments)-1]); err != nil {
		return OutcomeUnspecified, err
	}

	return work.readOutcome(), nil
}

// assertLineEncrypted requires the key's line in the re-read ciphertext to
// carry a sops ENC marker.
func assertLineEncrypted(doc []byte, leaf string) error {
	for _, raw := range strings.Split(string(doc), "\n") {
		trimmed := strings.TrimSpace(raw)
		if !strings.HasPrefix(trimmed, leaf+":") {
			continue
		}
		if strings.Contains(trimmed, "ENC[AES256_GCM,") {
			return nil
		}
		// The line is NOT ciphertext. Say so without quoting the line, which
		// by definition holds the plaintext.
		return errors.New("the value was written in the clear rather than encrypted — the file has been restored")
	}
	return errors.New("the written key could not be found in the re-read file — the file has been restored")
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
// backup, and the outcome for one run.
type workDir struct {
	dir    string
	nonce  string
	backup string
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
	dir, err := os.MkdirTemp(parent, ".forgectl-sops-")
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

// captureOutput writes sops' streams to a 0600 file and returns its path.
//
// Never rendered, never logged, never wrapped into a returned error: a YAML
// parse error from sops quotes the offending line, which is `key: '<the
// secret>'`. Its stderr also carries the operator's home path, which is a
// second reason. Writing it down under 0600 keeps it available for a
// deliberate read while keeping it out of a transcript.
func (w *workDir) captureOutput(res exec.SensitiveResult) (string, error) {
	stdout, _ := res.Stdout.CopyBytesForParse()
	stderr, _ := res.Stderr.CopyBytesForParse()
	if len(stdout) == 0 && len(stderr) == 0 {
		return "", nil
	}

	// Outside the work directory, because the work directory is removed on
	// the way out and this file has to outlive it to be worth naming.
	f, err := os.CreateTemp("", "forgectl-sops-output-")
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	if err := f.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := f.Write(stderr); err != nil {
		return "", err
	}
	if _, err := f.Write(stdout); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// readOutcome reports what __sops-edit recorded, defaulting to replaced.
//
// The default matters: the rc=200 "file has not changed" path means the editor
// wrote identical bytes, so it never recorded anything — and the key
// provably already holds this value, which is a replacement that happened to
// be a no-op rather than an addition.
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

func (w *workDir) cleanup() { _ = os.RemoveAll(w.dir) }
