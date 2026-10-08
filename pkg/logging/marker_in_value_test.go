package logging

import (
	"bytes"
	"strings"
	"testing"
)

// A plain Printf prints whatever the caller hands it, and callers hand it
// peer-controlled bytes. Those bytes may spell the level marker; they are data,
// so the line keeps the level a plain Printf means (info) and every byte stays
// in the line. The whole-line scan used to take a mid-line marker for real and
// could drop or re-level the line.
func TestAPrintedValueThatSpellsAMarkerKeepsItsLevelAndItsBytes(t *testing.T) {
	var buf bytes.Buffer
	logger, err := New(&buf, Options{Level: "info"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Printf("peer %s asked to forward", "a\x000\x00b")
	if out := buf.String(); !strings.Contains(out, "a\x000\x00b") {
		t.Fatalf("the line carrying a marker-spelling value was dropped or altered: %q", out)
	}

	// A level the package writes itself is still a level: the marker at the
	// start of the message is the package's own, not data that wandered in.
	Warnf(logger, "careful: %s", "slow peer")
	if out := buf.String(); !strings.Contains(out, "careful") {
		t.Fatalf("a warn line disappeared: %q", out)
	}
}
