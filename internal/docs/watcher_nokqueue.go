//go:build !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package docs

// kqueueListing reports that fsnotify watches through kqueue here; see
// watcher_kqueue.go. Other backends report each new entry on its own, so an
// in-tree Create that is not a doc arms nothing (forgectl#936).
const kqueueListing = false
