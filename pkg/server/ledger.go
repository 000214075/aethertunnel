package server

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/ledger"
)

// ledgerStore is the server's bandwidth ledger together with the operator-facing
// metadata the dashboard publishes.
//
// The ledger is optional. When [ledger].enabled is false the server keeps none and
// every call site skips accounting, so a deployment that does not want a usage
// record pays nothing for it.
type ledgerStore struct {
	*ledger.Ledger

	path      string
	keyFile   string
	publicKey ed25519.PublicKey
}

// openLedger opens the ledger named by the [ledger] section, creating the signing
// key on first use.
//
// The key file holds the hex-encoded 32-byte Ed25519 seed. Generating it here
// rather than asking the operator to run a key-gen command keeps a first start
// working out of the box; the file is written 0600, and the public key is printed
// so the operator can record it before any entry is published.
func openLedger(cfg *config.Config, logger *log.Logger) (*ledgerStore, error) {
	if !cfg.Ledger.Enabled {
		return nil, nil
	}

	key, created, err := loadLedgerKey(cfg.Ledger.SigningKey)
	if err != nil {
		return nil, err
	}

	chain, err := ledger.New(cfg.Ledger.Path, key)
	if err != nil {
		return nil, err
	}

	store := &ledgerStore{
		Ledger:    chain,
		path:      cfg.Ledger.Path,
		keyFile:   cfg.Ledger.SigningKey,
		publicKey: key.PublicKey(),
	}
	if created {
		logger.Printf("bandwidth ledger: generated a signing key at %s", store.keyFile)
	}
	logger.Printf("bandwidth ledger: %s (%d entries, head %s, public key %s)",
		store.path, chain.Len(), shortHash(chain.Head()), store.PublicKeyHex())
	return store, nil
}

// loadLedgerKey reads the Ed25519 seed from path, generating and storing a new one
// when the file does not exist. The second return value reports whether the key was
// created.
func loadLedgerKey(path string) (*ledger.SigningKey, bool, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		seed, decodeErr := hex.DecodeString(strings.TrimSpace(string(data)))
		if decodeErr != nil {
			return nil, false, fmt.Errorf("ledger.signing_key_file %s: %w", path, decodeErr)
		}
		key, keyErr := ledger.SigningKeyFromSeed(seed)
		if keyErr != nil {
			return nil, false, fmt.Errorf("ledger.signing_key_file %s: %w", path, keyErr)
		}
		return key, false, nil
	case !os.IsNotExist(err):
		return nil, false, fmt.Errorf("ledger.signing_key_file %s: %w", path, err)
	}

	key, err := ledger.NewSigningKey()
	if err != nil {
		return nil, false, err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, false, fmt.Errorf("ledger.signing_key_file %s: %w", path, err)
		}
	}
	encoded := []byte(hex.EncodeToString(key.Seed()) + "\n")
	// O_EXCL so two servers started at once cannot both claim the same key file and
	// then sign different chains with different keys.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			// Another process won the race; adopt its key.
			return loadLedgerKey(path)
		}
		return nil, false, fmt.Errorf("ledger.signing_key_file %s: %w", path, err)
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, false, fmt.Errorf("ledger.signing_key_file %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return nil, false, fmt.Errorf("ledger.signing_key_file %s: %w", path, err)
	}
	return key, true, nil
}

// PublicKeyHex is the verification key in hex, the form an auditor needs.
func (l *ledgerStore) PublicKeyHex() string {
	if l == nil || len(l.publicKey) != ed25519.PublicKeySize {
		return ""
	}
	return hex.EncodeToString(l.publicKey)
}

// Path is the JSONL file the chain is appended to.
func (l *ledgerStore) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// recordSessionUsage appends one entry per proxy the session published, using the
// byte counters the proxy accumulated before it was torn down.
//
// It runs on the control connection's teardown path, after the proxy members have
// been removed but before the process forgets them, so a client's usage is billed
// exactly once even when the server is shutting down.
// ledgerSettle bounds how long a session's teardown waits for its streams to finish before
// the ledger entry is written. Streams that are still running when their control connection
// goes away end within a round trip, so this only has to cover that, and it keeps a
// misbehaving stream from holding a teardown open.
const ledgerSettle = 2 * time.Second

// waitForStreamsToFinish waits, up to limit, for the streams this session is still carrying
// to release.
//
// A stream adds its bytes to the tunnel after its pipe returns, and recordSessionUsage reads
// those counters, so writing the entry while a stream is in flight under-reports the session:
// measured on macOS CI, where a test that leaves while a 29-byte stream is finishing recorded
// 0 bytes in and 0 bytes out.
func (s *Server) waitForStreamsToFinish(session *Session, limit time.Duration) {
	deadline := time.Now().Add(limit)
	for session.ActiveStreams() > 0 && time.Now().Before(deadline) {
		time.Sleep(drainPollInterval)
	}
}

func (s *Server) recordSessionUsage(session *Session) {
	if s.ledger == nil {
		return
	}
	for _, member := range session.Tunnels() {
		in := member.BytesIn.Load()
		out := member.BytesOut.Load()
		entry, err := s.ledger.Append(session.ID, member.Name, in, out)
		if err != nil {
			s.logger.Printf("bandwidth ledger: cannot record %s/%s: %v", session.ID, member.Name, err)
			continue
		}
		s.logger.Printf("bandwidth ledger: recorded entry %d for client %s proxy %q (%d bytes in, %d bytes out, chain head %s)",
			entry.Index, session.ID, member.Name, in, out, shortHash(entry.Hash))
	}
}

// currentUsage summarises the live sessions, which is what has been used since the
// last entry was appended rather than what the chain already accounts for.
func (s *Server) currentUsage() map[string]int64 {
	usage := map[string]int64{}
	for _, member := range s.tunnels.List() {
		usage[member.Session.ID] += member.BytesIn.Load() + member.BytesOut.Load()
	}
	return usage
}

// Close flushes the chain to disk. It is a no-op when the ledger is disabled, so
// the shutdown path does not have to test for that.
func (l *ledgerStore) Close() error {
	if l == nil {
		return nil
	}
	return l.Ledger.Close()
}

// renderLedger is the payload of GET /api/ledger.
func (s *Server) renderLedger(limit int) map[string]any {
	if s.ledger == nil {
		return map[string]any{"enabled": false}
	}

	entries := s.ledger.Entries()
	if limit <= 0 {
		limit = 200
	}
	if limit > len(entries) {
		limit = len(entries)
	}
	recent := entries[len(entries)-limit:]

	totals := map[string]map[string]any{}
	for id, sum := range ledger.Summarise(entries) {
		totals[id] = map[string]any{
			"bytes_in":  sum.BytesIn,
			"bytes_out": sum.BytesOut,
			"entries":   sum.Entries,
		}
	}

	return map[string]any{
		"enabled":       true,
		"path":          s.ledger.Path(),
		"public_key":    s.ledger.PublicKeyHex(),
		"count":         len(entries),
		"head":          s.ledger.Head(),
		"returned":      len(recent),
		"entries":       recent,
		"totals":        totals,
		"unbilled_live": s.currentUsage(),
	}
}

// shortHash abbreviates a hex hash for log lines and API responses.
func shortHash(hash string) string {
	if hash == "" {
		return "(empty)"
	}
	if len(hash) <= 16 {
		return hash
	}
	return hash[:16] + "…"
}

// VerifyLedgerFile checks a ledger file against a public key and returns the entry
// count and the per-client totals. It is what the server's -verify-ledger flag
// calls, and it needs only the public key, never the signing key.
//
// keyText is either a hex Ed25519 public key or the path of a signing key file, in
// which case the matching public key is derived from its seed.
func VerifyLedgerFile(path, keyText string) (int, map[string]ledger.Totals, string, error) {
	pub, err := parseVerificationKey(keyText)
	if err != nil {
		return 0, nil, "", err
	}
	chain, err := ledger.ReadFile(path)
	if err != nil {
		return 0, nil, "", err
	}
	verified, err := ledger.Verify(chain, pub)
	if err != nil {
		return verified, nil, "", err
	}
	head := ""
	if len(chain) > 0 {
		head = chain[len(chain)-1].Hash
	}
	return verified, ledger.Summarise(chain), head, nil
}

// parseVerificationKey accepts a hex public key or the path of a key file.
func parseVerificationKey(text string) (ed25519.PublicKey, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("a verification key is required: pass the server's public key in hex, or its signing key file")
	}
	if raw, err := hex.DecodeString(text); err == nil && len(raw) == ed25519.PublicKeySize {
		return ed25519.PublicKey(raw), nil
	}
	// Not a hex public key, so treat it as a file holding the private seed.
	if _, err := os.Stat(text); err != nil {
		return nil, fmt.Errorf("%q is neither a %d-byte hex public key nor a readable key file: %w",
			text, ed25519.PublicKeySize, err)
	}
	key, _, err := loadLedgerKey(text)
	if err != nil {
		return nil, err
	}
	return key.PublicKey(), nil
}
