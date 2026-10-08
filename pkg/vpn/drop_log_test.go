package vpn

import "testing"

// The first dropped packet is logged at once with the total it stands for; a
// burst that follows inside the interval stays silent but keeps counting, so
// the next line carries what the quiet one swallowed.
func TestTheDropLogWritesTheFirstDropAtOnceAndThrottlesTheRest(t *testing.T) {
	var d dropLog
	var totals []uint64
	write := func(total uint64) { totals = append(totals, total) }

	d.add(2, write)
	if len(totals) != 1 || totals[0] != 2 {
		t.Fatalf("the first drop wrote %v, want one line carrying 2", totals)
	}

	d.add(3, write)
	if len(totals) != 1 {
		t.Fatalf("a drop inside the interval wrote another line: %v", totals)
	}
	if got := d.count.Load(); got != 3 {
		t.Fatalf("the suppressed drops were lost: count = %d, want 3", got)
	}
}
