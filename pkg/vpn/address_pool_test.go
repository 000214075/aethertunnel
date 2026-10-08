package vpn

import (
	"net"
	"strings"
	"testing"
)

// The address count used to be computed in the platform's own int: on a
// 32-bit build a /0 or a /1 wrapped the count negative and the pool refused
// a subnet that has plenty of addresses, with a message naming a negative
// count. The widest subnet the pool's arithmetic can express is a /1, and
// both a /1 and everything narrower now behave the same on every platform.
func TestTheWidestAddressPoolIsASlashOne(t *testing.T) {
	pool, err := NewPool("10.0.0.0/1")
	if err != nil {
		t.Fatalf("NewPool refused a /1: %v", err)
	}
	if got := pool.Size(); got != 2147483645 {
		t.Fatalf("a /1 pool holds %d address(es), want 2147483645", got)
	}
	first, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// network + server are reserved, so the first lease is 0.0.0.2.
	if !first.Equal(net.IPv4(0, 0, 0, 2)) {
		t.Fatalf("the first lease is %s, want 0.0.0.2", first)
	}
}

// A /0 spans the whole IPv4 space, which the pool's uint32 first/last
// arithmetic cannot express; it is refused with the count it really has
// rather than with the nonsense the old arithmetic produced.
func TestASlashZeroPoolIsRefusedWithTheTrueCount(t *testing.T) {
	_, err := NewPool("0.0.0.0/0")
	if err == nil {
		t.Fatal("a /0 pool was accepted")
	}
	if !strings.Contains(err.Error(), "4294967296") {
		t.Fatalf("the error %q does not name the true address count", err)
	}
}
