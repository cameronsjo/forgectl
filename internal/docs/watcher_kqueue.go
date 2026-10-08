//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package docs

// kqueueListing reports that fsnotify watches through kqueue here. The tag
// list is fsnotify v1.10.1's backend_kqueue.go. Only that backend learns of
// a new directory entry by listing the directory, and only that listing can
// stop short and hide the docs after an unopenable entry (forgectl#895).
const kqueueListing = true
