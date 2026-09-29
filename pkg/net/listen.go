package net

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
)

// The Windows socket codes for the same conditions. They live here as numbers because
// Go's syscall.EADDRINUSE on Windows is a synthetic APPLICATION_ERROR value that never
// compares equal to the real WSAEADDRINUSE (10048) the network stack reports, so
// errors.Is against it cannot fire there. On Linux these numbers mean other things,
// which is why the branch below is guarded by runtime.GOOS.
const (
	wsaEADDRINUSE    = syscall.Errno(10048)
	wsaEACCES        = syscall.Errno(10013)
	wsaEADDRNOTAVAIL = syscall.Errno(10049)
)

// ListenCause names why a socket could not be bound, for the conditions an operator
// can act on, and returns "" for anything else.
//
// It exists because the same failure reads very differently on the two platforms. On
// Unix the kernel's own message is the diagnosis — "bind: address already in use" — but
// Windows has no text for these codes, so the whole message is a number to look up:
//
//	listen on 127.0.0.1:34807: listen tcp 127.0.0.1:34807: bind: winapi error #10048
//
// The conditions below are what almost every refusal to bind turns out to be, and they
// are the ones a reader cannot guess from a number: something is already listening
// there, or the address cannot be bound at all.
func ListenCause(err error) string {
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return "the address is already in use (another process is listening on it)"
	case errors.Is(err, syscall.EACCES):
		return "permission denied (on Unix a port below 1024 needs privileges; on Windows " +
			"the port may be reserved, excluded from the dynamic range, or claimed by another user)"
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		return "this host has no such address"
	}
	if runtime.GOOS == "windows" {
		switch {
		case errors.Is(err, wsaEADDRINUSE):
			return "the address is already in use (another process is listening on it)"
		case errors.Is(err, wsaEACCES):
			return "permission denied (on Unix a port below 1024 needs privileges; on Windows " +
				"the port may be reserved, excluded from the dynamic range, or claimed by another user)"
		case errors.Is(err, wsaEADDRNOTAVAIL):
			return "this host has no such address"
		}
	}
	return ""
}

// ListenError wraps a failed bind: the cause from ListenCause when there is one, and
// net's own error otherwise, in both cases naming the address that was asked for.
func ListenError(addr string, err error) error {
	if err == nil {
		return nil
	}
	if cause := ListenCause(err); cause != "" {
		return fmt.Errorf("listen on %s: %s", addr, cause)
	}
	return fmt.Errorf("listen on %s: %w", addr, err)
}
