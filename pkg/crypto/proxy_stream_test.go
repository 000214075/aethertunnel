package crypto

import "testing"

// The per-proxy key is derived from the shared secret and the proxy's name, so
// two proxies on one session cannot read each other's stream and two servers
// that share a token but not a name set derive different keys.
func TestProxyStreamCipherIsBoundToTheSecretAndTheName(t *testing.T) {
	left, err := ProxyStreamCipher("shared-token", "web")
	if err != nil {
		t.Fatalf("ProxyStreamCipher: %v", err)
	}
	again, err := ProxyStreamCipher("shared-token", "web")
	if err != nil {
		t.Fatalf("ProxyStreamCipher: %v", err)
	}
	if !left.Enabled() {
		t.Fatal("the per-proxy cipher is disabled")
	}

	// Both ends derive the same key from the same inputs, or no stream would
	// ever decrypt.
	record, err := left.SealString("a stream record")
	if err != nil {
		t.Fatalf("SealString: %v", err)
	}
	plaintext, err := again.OpenString(record)
	if err != nil {
		t.Fatalf("the two derivations do not agree: %v", err)
	}
	if plaintext != "a stream record" {
		t.Fatalf("the record came back as %q", plaintext)
	}

	other, err := ProxyStreamCipher("shared-token", "other")
	if err != nil {
		t.Fatalf("ProxyStreamCipher: %v", err)
	}
	if _, err := other.OpenString(record); err == nil {
		t.Fatal("another proxy's key opened the record: the name is not part of the derivation")
	}

	elsewhere, err := ProxyStreamCipher("another-token", "web")
	if err != nil {
		t.Fatalf("ProxyStreamCipher: %v", err)
	}
	if _, err := elsewhere.OpenString(record); err == nil {
		t.Fatal("another secret opened the record: the secret is not part of the derivation")
	}
}

// Without both inputs there is no key, and a caller that asked for one anyway
// must be told rather than handed a stream that is quietly in the clear.
func TestProxyStreamCipherRefusesEmptyInputs(t *testing.T) {
	if _, err := ProxyStreamCipher("", "web"); err == nil {
		t.Fatal("an empty shared secret produced a cipher")
	}
	if _, err := ProxyStreamCipher("shared-token", ""); err == nil {
		t.Fatal("an empty proxy name produced a cipher")
	}
}

// The per-proxy layer is a different key from the session key derived from the
// same secret, so the two layers cannot be confused for one another.
func TestProxyStreamCipherIsNotTheSessionCipher(t *testing.T) {
	proxy, err := ProxyStreamCipher("shared-token", "web")
	if err != nil {
		t.Fatalf("ProxyStreamCipher: %v", err)
	}
	session, err := NewCipher(AlgorithmXChaCha20Poly1305, "shared-token", "aethertunnel")
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	record, err := proxy.SealString("a stream record")
	if err != nil {
		t.Fatalf("SealString: %v", err)
	}
	if _, err := session.OpenString(record); err == nil {
		t.Fatal("the session cipher opened a per-proxy record: the two derivations collide")
	}
}
