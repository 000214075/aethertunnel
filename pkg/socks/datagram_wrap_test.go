package socks

import (
	"errors"
	"testing"
)

// An empty host is invalid, not long. WrapUDPDatagram's ":80" — what
// net.SplitHostPort leaves of an address written without a host — used to be
// reported as a name that is too long, sending an operator after a name that was
// never sent. The reader's side of the same mislabel was already fixed.
func TestWrappingADatagramWithNoHostIsRefusedAsEmpty(t *testing.T) {
	if _, err := WrapUDPDatagram(":80", []byte("payload")); !errors.Is(err, ErrEmptyAddress) {
		t.Fatalf("WrapUDPDatagram(:80) = %v, want ErrEmptyAddress", err)
	}
	// A name that really is too long keeps the other error.
	long := ""
	for i := 0; i < 256; i++ {
		long += "a"
	}
	if _, err := WrapUDPDatagram(long+":80", nil); !errors.Is(err, ErrAddressTooLong) {
		t.Fatalf("a 256-byte name = %v, want ErrAddressTooLong", err)
	}
}
