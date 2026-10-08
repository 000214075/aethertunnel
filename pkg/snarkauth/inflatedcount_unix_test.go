//go:build unix

package snarkauth

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// proofChildEnv marks the child half of
// TestAnInflatedCommitmentCountCannotMakeTheProcessAllocateWithoutBound.
const proofChildEnv = "AETHERTUNNEL_INFLATED_COUNT_CHILD"

// The bound is what keeps the decoder's allocation within a constant factor of
// the message, so removing it must bring the fatal allocation back. That is
// proved in a child process whose address space is capped a little above what it
// already uses: the 2^32-point request then fails the way it fails on a server —
// as a runtime fatal error, which would take this test binary down with it if it
// were asked for in-process.
func TestAnInflatedCommitmentCountCannotMakeTheProcessAllocateWithoutBound(t *testing.T) {
	if os.Getenv(proofChildEnv) == "1" {
		inflatedCountChild() // never returns
	}
	cmd := exec.Command(os.Args[0],
		"-test.run=^TestAnInflatedCommitmentCountCannotMakeTheProcessAllocateWithoutBound$", "-test.v")
	cmd.Env = append(os.Environ(), proofChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 3 {
		t.Skipf("this platform does not report the process's mapped size, which the child caps:\n%s", out)
	}
	if err != nil {
		t.Fatalf("the child that verifies a proof declaring 2^32 commitments failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "refused:") {
		t.Fatalf("the child did not report a refusal:\n%s", out)
	}
	if !strings.Contains(string(out), "declares 4294967295 commitment") {
		t.Fatalf("the child's refusal did not name the declared count:\n%s", out)
	}
}

// inflatedCountChild is the child half: it builds the crafted payload, caps its
// own address space, verifies, prints the refusal and exits 0. With the bound
// reverted the verify call dies of the allocation instead, and the exit status
// is the runtime's.
func inflatedCountChild() {
	payload, err := Prove([]byte(proofSecret), []byte(proofContext))
	if err != nil {
		println("child: prove:", err.Error())
		os.Exit(3)
	}
	binary.BigEndian.PutUint32(payload[32+proofCommitmentCount:], 0xFFFFFFFF)

	mapped, err := vmSizeBytes()
	if err != nil {
		println("child: cannot read the mapped size:", err.Error())
		os.Exit(3)
	}
	// Enough headroom that a decode which stays within its message cannot
	// notice the cap, and far below the 274 GB the inflated count asks for.
	limit := syscall.Rlimit{Cur: mapped + 256<<20, Max: ^uint64(0)}
	if err := syscall.Setrlimit(syscall.RLIMIT_AS, &limit); err != nil {
		println("child: cannot cap the address space:", err.Error())
		os.Exit(3)
	}

	err = Verify(payload, []byte(proofSecret), []byte(proofContext))
	if err == nil {
		println("refused: none — the proof was accepted")
		os.Exit(0)
	}
	println("refused:", err.Error())
	os.Exit(0)
}

// vmSizeBytes is the process's mapped address space, from the one file the
// kernel reports it in.
func vmSizeBytes() (uint64, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmSize:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb * 1024, nil
	}
	return 0, errors.New("no VmSize line in /proc/self/status")
}
