// Package mobile exposes the tunnel client to mobile platforms.
//
// An embedding app calls [Run] with the contents of a client configuration in
// TOML form, on its own thread: Run blocks until [Stop] is called or the client
// gives up on a fatal error. The configuration is the same file the
// command-line client reads, so an app can ship one and edit it the same way.
// The package builds for android and ios like the rest of the module and needs
// no cgo.
//
// Two platform notes for a full client binary, as opposed to the library: an
// android executable needs -ldflags=-checklinkname=0, because the interface
// enumeration helper pion/webrtc pulls in uses go:linkname; an ios executable
// additionally requires Apple's cgo toolchain (gomobile or Xcode), which is the
// iOS linker's requirement for any Go program.
package mobile

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"

	"github.com/aethertunnel/aethertunnel/pkg/clientlib"
	"github.com/aethertunnel/aethertunnel/pkg/config"
)

var (
	mu     sync.Mutex
	cancel context.CancelFunc
)

// Run parses configTOML as a client configuration and runs the tunnel client on
// the calling thread until Stop is called or a fatal error shows up. Warnings
// collected while parsing are written to the standard logger; a configuration
// that fails validation aborts with the error before anything is started.
//
// Only one client runs at a time per process; a second Run while one is active
// returns an error.
func Run(configTOML string) error {
	mu.Lock()
	if cancel != nil {
		mu.Unlock()
		return errors.New("mobile: a client is already running; call Stop first")
	}
	ctx, stop := context.WithCancel(context.Background())
	cancel = stop
	mu.Unlock()

	defer func() {
		mu.Lock()
		cancel = nil
		mu.Unlock()
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

	return clientlib.Run(ctx, cfg, logger)
}

// Stop cancels the client started by Run and is safe to call when nothing runs.
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
