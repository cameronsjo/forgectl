//go:build !unix

package docs

// openNonblock is 0 where there is no FIFO to open nonblocking.
const openNonblock = 0
