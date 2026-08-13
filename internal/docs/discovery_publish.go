package docs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
)

// PublishServerInfo installs one immutable record for info's generation and
// returns the lease that owns it.
//
// The publication is no-replace: installing the final name fails when that name
// already exists rather than overwriting it. Ordinary rename would succeed
// there, and succeeding is the bug — it would silently replace a live sibling's
// record with this one, which is the same class of failure forgectl#277 exists
// to fix, just moved from shutdown to startup.
func PublishServerInfo(dir string, info ServerInfo) (Publication, error) {
	return publishServerInfo(productionDiscoveryRuntime(), dir, info)
}

func publishServerInfo(rt discoveryRuntime, dir string, info ServerInfo) (Publication, error) {
	// Revalidate rather than trusting the caller: a record that reached disk
	// without passing this gate would be one discovery then refuses to parse,
	// which reads as "the server is not running".
	if err := validateServerInfo(info, rt.localIP); err != nil {
		return Publication{}, err
	}
	payload, err := encodeRecord(info)
	if err != nil {
		return Publication{}, err
	}

	dirHandle, err := rt.openDir(dir, true)
	if err != nil {
		return Publication{}, fmt.Errorf("open the docs discovery directory: %w", sanitizeFSError(err))
	}

	warning, err := installRecord(dirHandle, info.Generation, payload)
	if err != nil {
		dirHandle.Close() //nolint:errcheck // already failing; nothing was installed
		return Publication{}, err
	}
	// The lease inherits the open handle. It must not reopen the path later:
	// the directory it was told to own is the one it holds, and re-resolving
	// the name is exactly how a removal ends up pointed somewhere else.
	return Publication{
		Lease:   &ServerLease{dir: dirHandle, name: recordFileName(info.Generation)},
		Warning: warning,
	}, nil
}

// installRecord writes the temp, installs the final name no-replace, and syncs
// the directory.
//
// It owns the single translation of an already-exists errno into
// ErrGenerationCollision. Doing it here rather than in each platform installer
// is what keeps the serving loop's retry predicate from drifting per platform —
// one errno mapping, one retryable error, one place to be wrong.
func installRecord(dir discoveryDir, generation string, payload []byte) (warning error, err error) {
	tempName, err := writeRecordBytes(dir, payload)
	if err != nil {
		return nil, err
	}

	installWarning, err := dir.InstallNoReplace(tempName, recordFileName(generation))
	if err != nil {
		discardTemp(dir, tempName)
		if errors.Is(err, fs.ErrExist) {
			return nil, ErrGenerationCollision
		}
		return nil, fmt.Errorf("install the docs discovery record: %w", sanitizeFSError(err))
	}

	var warnings []error
	if installWarning != nil {
		warnings = append(warnings, installWarning)
	}
	if err := dir.Sync(); err != nil {
		// The record is already visible. Reporting this as a publication
		// failure would tell the caller there is no record while another
		// process may already be reading one.
		warnings = append(warnings, fmt.Errorf("sync the docs discovery directory: %w", sanitizeFSError(err)))
	}
	return errors.Join(warnings...), nil
}

// writeRecordBytes creates a private temp, fills it completely, and returns its
// name. Every failure path removes the temp it owns and leaves no final record.
func writeRecordBytes(dir discoveryDir, payload []byte) (string, error) {
	file, name, err := dir.CreateTemp()
	if err != nil {
		return "", fmt.Errorf("create the docs discovery record: %w", sanitizeFSError(err))
	}

	fail := func(op string, cause error) (string, error) {
		file.Close() //nolint:errcheck // already failing
		discardTemp(dir, name)
		return "", fmt.Errorf("%s the docs discovery record: %w", op, sanitizeFSError(cause))
	}

	// Chmod before the bytes, never after: the payload can carry a bearer
	// token, and a file that is briefly group-readable has already leaked it.
	if err := file.Chmod(0o600); err != nil {
		return fail("secure", err)
	}
	n, err := file.Write(payload)
	if err == nil && n != len(payload) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fail("write", err)
	}
	if err := file.Sync(); err != nil {
		return fail("sync", err)
	}
	if err := file.Close(); err != nil {
		discardTemp(dir, name)
		return "", fmt.Errorf("close the docs discovery record: %w", sanitizeFSError(err))
	}
	return name, nil
}

// discardTemp removes a temp this publisher created and nothing else. It is
// deliberately silent: it runs only on paths that are already returning a
// failure, and a leftover hidden temp is inert — readers ignore every name that
// is not an authoritative record.
func discardTemp(dir discoveryDir, name string) {
	_ = dir.Remove(name)
}

// ServerLease owns exactly one published record name.
//
// It holds the directory handle it published through rather than the path it
// published to. That is the ownership claim: removal goes to the same
// descriptor the install went to, so replacing the directory afterwards cannot
// redirect the removal, and the lease can never reach a name it did not write.
type ServerLease struct {
	dir  discoveryDir
	name string
	once sync.Once
	err  error
}

// Close removes this lease's record and releases the directory handle.
//
// Absence is success. A record can legitimately be gone by the time shutdown
// runs — an operator cleaning up by hand is the ordinary case — and treating
// that as a failure would put a warning on every such exit while changing
// nothing about the outcome.
//
// The result is cached under sync.Once so concurrent or repeated closes agree,
// and so no second removal is ever attempted against a name that may by then
// belong to a different generation.
func (l *ServerLease) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if err := l.dir.Remove(l.name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			l.err = fmt.Errorf("remove the docs discovery record: %w", sanitizeFSError(err))
		}
		if err := l.dir.Close(); err != nil {
			l.err = errors.Join(l.err, fmt.Errorf("close the docs discovery directory: %w", sanitizeFSError(err)))
		}
	})
	return l.err
}
