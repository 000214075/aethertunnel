package vpn

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
)

func TestNewFromFDCarriesBytesBothWays(t *testing.T) {
	reader, writerFD := handedOverPipe(t)

	device, err := NewFromFD(writerFD, "pipe", 0)
	if err != nil {
		t.Fatalf("NewFromFD: %v", err)
	}
	if got := device.Name(); got != "pipe" {
		t.Fatalf("Name is %q, want %q", got, "pipe")
	}
	if got := device.MTU(); got != DefaultMTU {
		t.Fatalf("MTU is %d, want the default %d", got, DefaultMTU)
	}
	// An fd device accepts the server's address through AssignAddress like any
	// other device; it records it instead of ioctl-ing a shell-owned interface.
	if err := AssignAddress(device, "10.7.0.2", "255.255.255.0"); err != nil {
		t.Fatalf("AssignAddress: %v", err)
	}

	packet := []byte{0x45, 0x00, 0x00, 0x14, 1, 2, 3, 4}
	if n, err := device.Write(packet); err != nil || n != len(packet) {
		t.Fatalf("Write: %d, %v", n, err)
	}
	got := make([]byte, len(packet)+8)
	n, err := io.ReadFull(reader, got[:len(packet)])
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got[:n], packet) {
		t.Fatalf("round trip is % X, want % X", got[:n], packet)
	}

	if err := device.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := device.Write(packet); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Write after Close: %v, want a closed-file error", err)
	}
}

func TestNewFromFDNamesAnUnnamedDevice(t *testing.T) {
	_, writerFD := handedOverPipe(t)

	device, err := NewFromFD(writerFD, "", 0)
	if err != nil {
		t.Fatalf("NewFromFD: %v", err)
	}
	if got := device.Name(); got != "tun" {
		t.Fatalf("Name is %q, want %q", got, "tun")
	}
}

func TestNewFromFDRefusesAnOutOfRangeMTU(t *testing.T) {
	if _, err := NewFromFD(0, "tun", MinMTU-1); err == nil {
		t.Fatal("NewFromFD accepted an MTU below the minimum")
	}
	if _, err := NewFromFD(0, "tun", MaxMTU+1); err == nil {
		t.Fatal("NewFromFD accepted an MTU above the maximum")
	}
}
