package socks

import (
	"errors"
	"strings"
	"testing"
)

// A port that parses but is out of range is not a parse failure: formatting the
// nil error with %w rendered the message as "%!w(<nil>)", which hid the port the
// caller actually wrote.
func TestAnOutOfRangePortIsReportedWithThePort(t *testing.T) {
	_, err := WrapUDPDatagram("example.com:70000", []byte("x"))
	if err == nil {
		t.Fatal("WrapUDPDatagram accepted a port above 65535")
	}
	if !strings.Contains(err.Error(), "70000") {
		t.Fatalf("error %q does not name the port", err)
	}
	if strings.Contains(err.Error(), "%!w") {
		t.Fatalf("error %q carries an unformatted verb", err)
	}
}

// A domain length of zero says the name is empty, and the byte that carries it is
// there: the datagram is not short, so it used to be mislabelled as truncated.
func TestAnEmptyDomainInADatagramIsAnEmptyAddress(t *testing.T) {
	// reserved, reserved, fragment, atyp, domain length 0, port.
	packet := []byte{0, 0, 0, AtypDomain, 0, 0, 80}
	if _, _, err := ParseUDPDatagram(packet); !errors.Is(err, ErrEmptyAddress) {
		t.Fatalf("ParseUDPDatagram with a zero-length domain = %v, want ErrEmptyAddress", err)
	}
}
