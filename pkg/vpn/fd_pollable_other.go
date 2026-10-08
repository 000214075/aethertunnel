//go:build !unix

package vpn

// setNonblockFD is a no-op where the runtime's poller has no equivalent: the
// platforms this builds for hand over a descriptor that Close already reaches
// (or never call NewFromFD at all).
func setNonblockFD(uintptr) error { return nil }
