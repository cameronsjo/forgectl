package cli

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/cameronsjo/forgectl/internal/config"
)

type configWriterOps struct {
	parent       configParentOps
	atomic       atomicWriteOps
	ensureParent func(string, configParentOps) ([]string, error)
	withLock     func(string, func() error) error
	readFile     func(string) ([]byte, error)
	writeConfig  func(string, []byte, atomicWriteOps) (commitState, error)
}

func nativeConfigWriterOps() configWriterOps {
	return configWriterOps{
		parent:       nativeConfigParentOps(),
		atomic:       nativeAtomicWriteOps(),
		ensureParent: ensureNormalConfigParent,
		withLock:     config.WithFileLock,
		readFile:     config.ReadPath,
		writeConfig:  writeConfigAtomicWithOps,
	}
}

// updateConfigLocked is the sole normal-writer read/modify/write primitive.
// It creates the parent before the sibling lock, rereads and strictly decodes
// under the lock, renders once in memory, and performs at most one atomic
// replacement. The low-level writer never relocks itself.
func updateConfigLocked(path string, ops configWriterOps, render func([]byte) ([]byte, error)) (configAction, error) {
	if _, err := ops.ensureParent(path, ops.parent); err != nil {
		return configUnchanged, err
	}
	action := configUnchanged
	err := ops.withLock(path, func() error {
		raw, _, err := readLockedConfig(path, ops.readFile)
		if err != nil {
			return fmt.Errorf("strictly decode config under writer lock: %w", err)
		}
		next, err := render(bytes.Clone(raw))
		if err != nil {
			return err
		}
		if bytes.Equal(raw, next) {
			return nil
		}
		state, err := ops.writeConfig(path, next, ops.atomic)
		if state >= commitRenamed {
			action = configWritten
		}
		if err != nil {
			return err
		}
		if state != commitDurable {
			return fmt.Errorf("config replacement did not become durable")
		}
		return nil
	})
	return action, err
}

func visibleWithoutDirectoryDurability(action configAction, err error) bool {
	return action == configWritten && errors.Is(err, errDirectoryDurabilityUnsupported)
}

func refuseConfigMutationForLegacyBoundary(boundary *config.LegacyMigrationBoundary) error {
	if boundary != nil && boundary.Status == config.BoundaryRefused {
		// Secure legacy mutation remains unsupported on !unix, but that
		// capability refusal must not disable the documented developer-only
		// ordinary config writers on those platforms.
		if errors.Is(boundary.Refusal, config.ErrLegacyMigrationUnsupported) && normalWriterAllowsUnsupportedLegacyMutation() {
			return nil
		}
		return boundary.Refusal
	}
	return nil
}
