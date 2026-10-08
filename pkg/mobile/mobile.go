// Package mobile exposes the tunnel client to mobile platforms.
//
// An embedding app calls [Run] with the contents of a client configuration in
// TOML form, on its own thread: Run blocks until [Stop] is called or the client
// gives up on a fatal error. The configuration is the same file the
// command-line client reads, so an app can ship one and edit it the same way.
// For a layer-3 tunnel the app calls [RunVPN] instead and implements
// [PlatformVPN], which hands over the tun descriptor at the moment the session
// is up. The package builds for android and ios like the rest of the module and
// needs no cgo.
//
// Two platform notes for a full client binary, as opposed to the library: an
// android executable needs -ldflags=-checklinkname=0, because the interface
// enumeration helper pion/webrtc pulls in uses go:linkname; an ios executable
// additionally requires Apple's cgo toolchain (gomobile or Xcode), which is the
// iOS linker's requirement for any Go program. The build produces the AAR and the
// debug APK of the minimal app in mobile/android, and attaches both to each release.
package mobile

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"

	"github.com/aethertunnel/aethertunnel/pkg/clientlib"
	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/logging"
	"github.com/aethertunnel/aethertunnel/pkg/vpn"
)

var (
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
)

// PlatformVPN is the platform half of the layer-3 tunnel: the shell owns the
// tun interface, the client owns the packets.
//
// The split exists because a platform interface is configured at creation time,
// while the tunnel's address is assigned by the server once the session is up.
// OpenTun is therefore called at the moment both facts are known — the session
// is authenticated and the server's answer carries the address — and the
// descriptor it returns must already carry that address and whatever routes the
// shell chose. ProtectSocket runs for every socket the client opens to the
// server, so a full-device route table does not capture the tunnel's own
// traffic; a shell that routes only the tunnel's subnet has no loop to fear and
// may leave the body empty.
type PlatformVPN interface {
	// OpenTun returns the file descriptor of a tun interface carrying address
	// with the given prefix length, plus at least a route to subnet. On Android
	// this is VpnService.Builder.establish(); on iOS, the packet flow of a
	// NetworkExtension provider.
	OpenTun(mtu int32, address string, prefix int32, subnet string) (int32, error)

	// ProtectSocket excludes one of the client's sockets from the VPN's routes.
	ProtectSocket(fd int32)
}

// Run parses configTOML as a client configuration and runs the tunnel client on
// the calling thread until Stop is called or a fatal error shows up. Warnings
// collected while parsing are written to the standard logger; a configuration
// that fails validation aborts with the error before anything is started.
//
// Only one client runs at a time per process; a second Run while one is active
// returns an error.
func Run(configTOML string) error {
	return start(configTOML, nil)
}

// RunVPN is Run with the layer-3 device supplied by the platform shell. The
// configuration must enable [vpn]; the shell's OpenTun is called once per
// session, when the server's address assignment arrives.
func RunVPN(configTOML string, shell PlatformVPN) error {
	return start(configTOML, shell)
}

func start(configTOML string, shell PlatformVPN) error {
	mu.Lock()
	if cancel != nil {
		mu.Unlock()
		return errors.New("mobile: a client is already running; call Stop first")
	}
	ctx, stop := context.WithCancel(context.Background())
	cancel = stop
	done = make(chan struct{})
	mu.Unlock()

	defer func() {
		mu.Lock()
		cancel = nil
		finished := done
		done = nil
		mu.Unlock()
		// Signalled last, after cancel is cleared: Stop waits on this, and a
		// Run that starts right after it must see the bookkeeping reset.
		close(finished)
	}()

	cfg, err := config.LoadString(configTOML, "embedded", config.ValidateOptions{Role: config.RoleClient})
	logger := log.New(os.Stderr, "", log.LstdFlags)
	if cfg != nil {
		for _, warning := range cfg.Warnings {
			logger.Printf("warning: %s", warning)
		}
	}
	if err != nil {
		return err
	}

	// [log] is honored here too: on Android the platform hook has already
	// pointed standard error at logcat, and an empty log.to keeps that; a path
	// is how an app keeps the client's log in its own sandbox.
	logger, closeLog, err := logging.Open(cfg.Log.Options(), os.Stderr)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()

	if shell != nil {
		if !cfg.VPN.Enabled {
			return errors.New("mobile: the configuration does not enable [vpn], so there is no tunnel interface to ask the shell for")
		}
		return clientlib.RunWithShell(ctx, cfg, logger,
			func(mtu int, address string, prefix int, subnet string) (vpn.Device, error) {
				return openShellTun(shell, mtu, address, prefix, subnet)
			},
			func(fd int) {
				shell.ProtectSocket(int32(fd))
			},
		)
	}

	return clientlib.Run(ctx, cfg, logger)
}

// openShellTun asks the shell for the tun descriptor and wraps it. NewFromFD
// takes ownership of the descriptor only when it succeeds, so a failure there
// has to close it here: the shell opened it once, and a tunnel whose setup then
// failed would otherwise leak the descriptor for the life of the app.
func openShellTun(shell PlatformVPN, mtu int, address string, prefix int, subnet string) (vpn.Device, error) {
	fd, err := shell.OpenTun(int32(mtu), address, int32(prefix), subnet)
	if err != nil {
		return nil, err
	}
	device, err := vpn.NewFromFD(uintptr(fd), "tun", mtu)
	if err != nil {
		_ = os.NewFile(uintptr(fd), "tun").Close()
		return nil, err
	}
	return device, nil
}

// Stop cancels the client started by Run or RunVPN and is safe to call when
// nothing runs. It waits for the client to finish unwinding — its log closed,
// its bookkeeping reset — so a Run that starts right after Stop returns is
// accepted rather than told a client is still running.
func Stop() {
	mu.Lock()
	c := cancel
	finished := done
	mu.Unlock()
	if c == nil {
		return
	}
	c()
	<-finished
}
