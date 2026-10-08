//go:build !unix

package clientlib

import (
	"os"
	"testing"
)

// handedOverPipe returns the read end of a pipe and the write end's descriptor.
// Platforms without syscall.Pipe keep the os.Pipe wrapper on the write end: the
// descriptor is a handle there, and the double close this helper exists to
// avoid has not been seen there.
func handedOverPipe(t *testing.T) (reader *os.File, writerFD uintptr) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return reader, writer.Fd()
}
