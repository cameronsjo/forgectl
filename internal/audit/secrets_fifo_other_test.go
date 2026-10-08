//go:build !unix || aix || illumos || solaris

package audit

import "errors"

func mkfifo(string) error { return errors.New("no mkfifo on this platform") }
