//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package mail

import (
	"errors"
	"os"
)

func lockedPrivateMailbox(box Mailbox, fn func(*Tx) error) error {
	return ErrNativeReplyUnavailable
}

func (store *privateMailbox) openRead(name string) (*os.File, error) {
	return nil, errors.New("private mailbox descriptors are unsupported on this platform")
}

func (store *privateMailbox) openAppend(name string) (*os.File, error) {
	return nil, errors.New("private mailbox descriptors are unsupported on this platform")
}

func (store *privateMailbox) worker(name string) (Worker, error) {
	return Worker{}, ErrNativeReplyUnavailable
}
