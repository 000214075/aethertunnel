// Package net holds the byte-movement helpers shared by the client and server.
package net

import (
	"io"
	"sync"
	"time"
)

// bufferPool recycles the buffers used when copying between connections. A tunnel
// moves a lot of bytes through short-lived connections, so allocating a fresh
// 32 KiB buffer for every direction of every stream shows up in the profile.
var bufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 32*1024)
		return &buf
	},
}

// halfCloser is implemented by *net.TCPConn and *net.UnixConn.
type halfCloser interface {
	CloseWrite() error
}

// Pipe copies bytes in both directions between a and b until both directions have
// finished, then closes both.
//
// The details matter:
//   - each direction is closed independently with CloseWrite, so a protocol that
//     half-closes (HTTP/1.0, several database and SSH-style flows) is not
//     truncated when the first direction finishes;
//   - idleTimeout, when non-zero, is applied as a read deadline so a peer that
//     stops talking without closing cannot pin goroutines and sockets forever;
//   - each direction reports its own byte count, which is what the traffic
//     counters and the per-stream log line use.
func Pipe(a, b io.ReadWriteCloser, idleTimeout time.Duration) (aToB, bToA int64) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		aToB = copyDirection(b, a, idleTimeout)
		closeWrite(b)
	}()
	go func() {
		defer wg.Done()
		bToA = copyDirection(a, b, idleTimeout)
		closeWrite(a)
	}()

	wg.Wait()
	a.Close()
	b.Close()
	return aToB, bToA
}

func copyDirection(dst io.Writer, src io.Reader, idleTimeout time.Duration) int64 {
	bufPtr := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(bufPtr)
	buf := *bufPtr

	var total int64
	for {
		if idleTimeout > 0 {
			if conn, ok := src.(interface{ SetReadDeadline(time.Time) error }); ok {
				if err := conn.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
					return total
				}
			}
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total
			}
			if written != n {
				return total
			}
		}
		if readErr != nil {
			return total
		}
	}
}

func closeWrite(conn io.ReadWriteCloser) {
	if hc, ok := conn.(halfCloser); ok {
		_ = hc.CloseWrite()
		return
	}
	_ = conn.Close()
}

// HalfClose closes the write side of conn if it supports it, otherwise the whole
// connection.
func HalfClose(conn io.ReadWriteCloser) { closeWrite(conn) }
