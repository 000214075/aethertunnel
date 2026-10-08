//go:build unix

package clientlib

import (
	"os"
	"syscall"
	"testing"
)

// handedOverPipe returns the read end of a pipe and the raw descriptor of its
// write end, whose ownership passes to whoever wraps it (a device built by
// vpn.NewFromFD).
//
// The write end is deliberately not wrapped in an os.File here. Wrapping one
// descriptor in two os.Files gives the process two finalizers for it, and the
// stray one closes a *reused* descriptor later, which shows up as "bad file
// descriptor" in tests that have nothing to do with pipes — a certificate
// written to a temporary file, a temporary directory removed at the end of
// another test.
func handedOverPipe(t *testing.T) (reader *os.File, writerFD uintptr) {
	t.Helper()
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		t.Fatalf("pipe: %v", err)
	}
	reader = os.NewFile(uintptr(fds[0]), "pipe-read")
	if reader == nil {
		t.Fatalf("descriptor %d is not usable", fds[0])
	}
	t.Cleanup(func() { _ = reader.Close() })
	return reader, uintptr(fds[1])
}
