package server

import (
	"fmt"
	"strings"
	"testing"
)

// A proxy name reaches the exposition as a label value; the Prometheus text
// parser only accepts \\, \" and \n as escapes, so anything Go's %q would render
// as \t or \xNN makes the whole scrape unreadable.
func TestPromLabelsEscapeWhatPrometheusAccepts(t *testing.T) {
	cases := map[string]string{
		"plain":         `"plain"`,
		"quote\"inside": `"quote\"inside"`,
		`back\slash`:    `"back\\slash"`,
		"line\nbreak":   `"line\nbreak"`,
		"tab\there":     "\"tab\there\"",
		"nul\x00byte":   "\"nul\x00byte\"",
	}
	for value, want := range cases {
		if got := promLabel(value); got != want {
			t.Errorf("promLabel(%q) = %q, want %q", value, got, want)
		}
	}
}

// The cap has to hold when nothing can be pruned too: a name whose series does
// not fit is counted on an entry nobody stores, rather than growing every scrape.
func TestThePerTunnelSeriesCapHoldsWhenNothingCanBePruned(t *testing.T) {
	m := newMetrics()
	for i := 0; i < maxTunnelSeries; i++ {
		m.streamOpened(fmt.Sprintf("busy-%d", i))
	}
	if got := len(m.perTunnel); got != maxTunnelSeries {
		t.Fatalf("the map holds %d entries before the cap is hit, want %d", got, maxTunnelSeries)
	}
	// Every series has a stream open, so a prune frees nothing.
	overflow := m.streamOpened("overflow")
	if overflow == nil {
		t.Fatal("the stream's counters have nowhere to go")
	}
	if _, ok := m.lookupTunnel("overflow"); ok {
		t.Error("the exposition grew past its cap")
	}
	if got := len(m.perTunnel); got > maxTunnelSeries {
		t.Fatalf("the per-tunnel map holds %d entries, want at most %d", got, maxTunnelSeries)
	}
}

// A stream that was open when the name was withdrawn, and that ends after the
// name is registered again, must not be subtracted from the new generation's
// series: it was counted against the entry that went with the withdrawal, and a
// lookup by name drove the fresh gauge to -1 for the rest of the process.
func TestAStreamClosedAfterReregistrationDoesNotSkewTheNewSeries(t *testing.T) {
	m := newMetrics()
	old := m.streamOpened("web")
	m.forgetTunnel("web")
	fresh := m.streamOpened("web")
	m.streamClosed(old)
	if got := fresh.streamsActive.Load(); got != 1 {
		t.Fatalf("the new series reports %d active streams, want 1", got)
	}
	if got := m.streamsActive.Load(); got != 1 {
		t.Fatalf("the global gauge reports %d active streams, want 1", got)
	}
}

// A client may register a fresh proxy name as often as it likes; the per-tunnel
// map has to stay bounded or one client grows it, and every scrape, forever.
func TestThePerTunnelSeriesAreBounded(t *testing.T) {
	m := newMetrics()
	for i := 0; i < maxTunnelSeries+500; i++ {
		m.tunnel(fmt.Sprintf("name-%d", i))
	}
	m.mu.RLock()
	held := len(m.perTunnel)
	m.mu.RUnlock()
	if held > maxTunnelSeries {
		t.Fatalf("the per-tunnel map holds %d entries, want at most %d", held, maxTunnelSeries)
	}
}

// A proxy that is unpublished must not keep a permanent family in the exposition.
func TestForgetTunnelDropsTheSeries(t *testing.T) {
	m := newMetrics()
	m.tunnel("gone").streamsTotal.Add(3)
	m.tunnel("here").streamsTotal.Add(1)
	m.forgetTunnel("gone")

	rendered := m.Render()
	// Match the label, not the bare word: a HELP string mentioning a visitor that
	// "had gone" is not a series.
	if strings.Contains(rendered, `tunnel="gone"`) {
		t.Error("the series of a forgotten tunnel is still in the exposition")
	}
	if !strings.Contains(rendered, `tunnel="here"`) {
		t.Error("forgetting one tunnel dropped another")
	}
}

// A proxy that has been unpublished has no series any more, and closing one of its
// streams must not put it back: streamClosed used to create the entry in order to
// decrement it, which left a permanent gauge of -1 for a name that no longer
// exists, in every scrape, forever.
func TestAClosedStreamDoesNotRecreateAForgottenSeries(t *testing.T) {
	m := newMetrics()
	series := m.streamOpened("gone")
	m.forgetTunnel("gone")
	m.streamClosed(series)

	rendered := m.Render()
	if strings.Contains(rendered, `tunnel="gone"`) {
		t.Error("a closed stream recreated the series of a proxy that was unpublished")
	}
	if !strings.Contains(rendered, "aethertunnel_streams_active 0") {
		t.Error("the global gauge did not come back to zero when the stream closed")
	}
}
