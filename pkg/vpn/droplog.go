package vpn

import (
	"sync/atomic"
	"time"
)

// dropLog counts dropped packets and writes the line they stood for at most
// once per dropLogInterval, so a peer (or the local IP stack) emitting packets
// at line rate cannot write one log line per packet and bury everything else
// the process logs. The drop counters themselves keep every packet.
//
// add's check-store-swap is not atomic across callers: one dropLog belongs to
// the single goroutine that owns the drop (the loop that reads the packet),
// and two goroutines sharing one would split the count and log a zero total.
type dropLog struct {
	count atomic.Uint64
	at    atomic.Int64
}

// dropLogInterval is the minimum spacing between two lines about the same drop.
const dropLogInterval = 30 // seconds

// add records n dropped packets and, when the last line about this drop is
// older than the interval, writes one carrying the total since that line. The
// first drop writes at once, so a single bad packet is still logged in full.
func (d *dropLog) add(n uint64, write func(total uint64)) {
	d.count.Add(n)
	now := time.Now().Unix()
	if now-d.at.Load() < dropLogInterval {
		return
	}
	d.at.Store(now)
	write(d.count.Swap(0))
}
