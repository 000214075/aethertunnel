package oidc

// A minimal OIDC token verifier for [oidc]-authenticated sessions: the server
// discovers the issuer's JWKS once, caches the keys, and verifies the JWT the
// client presents in place of the static auth token — signature (RS256, PS256,
// ES256 family), issuer and audience claims, and expiry. It mirrors what frp's
// auth.oidc does with coreos/go-oidc, without the dependency.

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// supportedAlgorithms lists the JWS algorithms the verifier accepts. Anything
// else in a token's header is refused, including "none" — an unauthenticated
// JWT is not a credential.
var supportedAlgorithms = map[string]bool{
	"RS256": true, "RS384": true, "RS512": true,
	"PS256": true, "PS384": true, "PS512": true,
	"ES256": true, "ES384": true, "ES512": true,
}

// jwksRefreshInterval is how long a fetched key set stays trusted before the
// next verification goes back to the issuer.
const jwksRefreshInterval = 5 * time.Minute

// unknownKeyRefreshInterval bounds how often a kid the cached set does not name may
// force a refetch. An unknown kid is the normal shape of a signing-key rotation, so
// waiting out jwksRefreshInterval would refuse every token issued under the new key
// for five minutes. It is short, and a variable so a test can exercise the refetch
// without sleeping; the interval is still long enough that a flood of tokens with
// made-up kids cannot buy an HTTP round trip per token under the verifier's lock.
var unknownKeyRefreshInterval = 30 * time.Second

// keyFetchTimeout bounds one JWKS GET for callers whose own client has no
// timeout, which the two constructors here always set but WithClient can
// replace with anything. Without it the fetch ran on context.Background() and a
// caller waited on the fetch gate for as long as the issuer kept the connection
// open without answering. A variable so a test can shorten it.
var keyFetchTimeout = 10 * time.Second

// leeway accepts a small clock skew on expiry and not-before claims.
const leeway = 30 * time.Second

// Verifier verifies OIDC access tokens (JWTs) against one issuer.
type Verifier struct {
	issuer   string
	audience string
	// skipIssuer and skipExpiry mirror frp's auth.oidc switches for issuers
	// whose claims do not line up with what an operator runs.
	skipIssuer bool
	skipExpiry bool

	jwksURL string
	client  *http.Client

	mu        sync.Mutex
	keys      map[string]crypto.PublicKey
	fetchedAt time.Time
	// attemptAt is when a fetch was last tried, successful or not. fetchedAt only
	// moves on success, so an issuer that cannot be reached left the freshness
	// check false forever: every connection presenting a random kid bought another
	// HTTP round trip, and an unreachable issuer was asked once per token.
	attemptAt time.Time
	// lastFetchErr is why the most recent fetch failed, nil after a successful
	// one: the cooldown that follows a failure has to say the set could not be
	// read, instead of claiming the issuer publishes no such key when the
	// fetcher never got a set to look in.
	lastFetchErr error
	// fetching is non-nil while one caller is fetching the key set. The fetch runs
	// outside mu so it cannot stall the verifications whose keys are already cached:
	// a token with a made-up kid used to hold the lock for the whole HTTP round trip,
	// which is a stall any unauthenticated connection could ask for. Callers that
	// arrive while a fetch is in flight wait for it and re-read the cache.
	fetching chan struct{}
}

// Option tunes a Verifier.
type Option func(*Verifier)

// WithJWKSURL overrides the discovery document's jwks_uri.
func WithJWKSURL(url string) Option {
	return func(v *Verifier) { v.jwksURL = url }
}

// WithClient sets the HTTP client the discovery and JWKS fetches go through
// (tests point it at a local issuer).
func WithClient(client *http.Client) Option {
	return func(v *Verifier) { v.client = client }
}

// WithSkipIssuer turns the issuer-claim check off.
func WithSkipIssuer() Option {
	return func(v *Verifier) { v.skipIssuer = true }
}

// WithSkipExpiry turns the expiry check off.
func WithSkipExpiry() Option {
	return func(v *Verifier) { v.skipExpiry = true }
}

// NewVerifier discovers the issuer's JWKS endpoint and returns a verifier. The
// discovery document is fetched here, so a misconfigured issuer fails fast at
// startup instead of at the first login.
func NewVerifier(ctx context.Context, issuer, audience string, options ...Option) (*Verifier, error) {
	v := &Verifier{
		issuer:   issuer,
		audience: audience,
		client:   &http.Client{Timeout: 10 * time.Second},
		keys:     map[string]crypto.PublicKey{},
	}
	for _, option := range options {
		option(v)
	}
	if v.jwksURL == "" {
		wellKnown := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
		discovery := struct {
			JWKSURI string `json:"jwks_uri"`
		}{}
		if err := v.getJSON(ctx, wellKnown, &discovery); err != nil {
			return nil, fmt.Errorf("oidc: discover %s: %w", issuer, err)
		}
		if discovery.JWKSURI == "" {
			return nil, fmt.Errorf("oidc: the discovery document of %s names no jwks_uri", issuer)
		}
		v.jwksURL = discovery.JWKSURI
	}
	return v, nil
}

// NewVerifierWithJWKS builds a verifier from an explicit JWKS URL, skipping
// discovery — for issuers that publish no well-known document.
func NewVerifierWithJWKS(jwksURL, issuer, audience string, options ...Option) (*Verifier, error) {
	v := &Verifier{
		issuer:   issuer,
		audience: audience,
		jwksURL:  jwksURL,
		client:   &http.Client{Timeout: 10 * time.Second},
		keys:     map[string]crypto.PublicKey{},
	}
	for _, option := range options {
		option(v)
	}
	return v, nil
}

// Verify checks one token's signature and claims.
func (v *Verifier) Verify(token string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("oidc: the token is not a JWT")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("oidc: the token header is unreadable: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return fmt.Errorf("oidc: the token header is unreadable: %w", err)
	}
	if !supportedAlgorithms[header.Alg] {
		return fmt.Errorf("oidc: the token's algorithm %q is not accepted", header.Alg)
	}
	if header.Kid == "" {
		return errors.New("oidc: the token names no signing key")
	}

	signing := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("oidc: the token signature is unreadable: %w", err)
	}
	key, err := v.signingKey(header.Kid)
	if err != nil {
		return err
	}
	digest, hashed := hashFor(header.Alg)
	if err := verifySignature(header.Alg, key, digest, hashed, []byte(signing), signature); err != nil {
		return fmt.Errorf("oidc: the token signature does not verify: %w", err)
	}

	claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("oidc: the token claims are unreadable: %w", err)
	}
	var claims struct {
		Issuer string `json:"iss"`
		Aud    any    `json:"aud"`
		Exp    int64  `json:"exp"`
		Nbf    int64  `json:"nbf"`
	}
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		return fmt.Errorf("oidc: the token claims are unreadable: %w", err)
	}
	now := time.Now()
	if !v.skipIssuer && !v.issuerEmpty() && claims.Issuer != v.issuer {
		return fmt.Errorf("oidc: the token names issuer %q, want %q", claims.Issuer, v.issuer)
	}
	if v.audience != "" && !audienceContains(claims.Aud, v.audience) {
		return fmt.Errorf("oidc: the token's audience does not include %q", v.audience)
	}
	if !v.skipExpiry {
		if claims.Exp == 0 {
			return errors.New("oidc: the token carries no expiry")
		}
		if now.After(time.Unix(claims.Exp, 0).Add(leeway)) {
			return errors.New("oidc: the token is expired")
		}
	}
	if claims.Nbf != 0 && now.Add(leeway).Before(time.Unix(claims.Nbf, 0)) {
		return errors.New("oidc: the token is not valid yet")
	}
	return nil
}

func (v *Verifier) issuerEmpty() bool { return v.issuer == "" }

// audienceContains accepts the claim in either form the spec allows: one
// string or a list.
func audienceContains(claim any, audience string) bool {
	switch value := claim.(type) {
	case string:
		return value == audience
	case []any:
		for _, entry := range value {
			if text, ok := entry.(string); ok && text == audience {
				return true
			}
		}
	}
	return false
}

// signingKey returns the signing key for a token's kid, refreshing the cached
// JWKS when the key is unknown or stale. One refresh per verification is
// enough: a token signed by a key the issuer does not publish will not verify
// no matter how many times the set is re-read.
//
// The fetch itself runs without the lock. Holding it across the HTTP round trip let
// one unauthenticated connection — any token carrying a kid the cache does not name —
// stall every other verification, including the ones whose keys were already cached,
// for as long as the issuer took to answer.
func (v *Verifier) signingKey(kid string) (crypto.PublicKey, error) {
	for {
		v.mu.Lock()
		if key, ok := v.keys[kid]; ok && time.Since(v.fetchedAt) < jwksRefreshInterval {
			v.mu.Unlock()
			return key, nil
		}
		_, known := v.keys[kid]
		if v.fetching != nil {
			// Another caller is already fetching. Waiting has to come before the
			// cooldown below, because that caller stamped attemptAt a moment ago: a
			// token carrying the same newly rotated kid read the fresh stamp as
			// "too soon to ask again" and refused a key the issuer really
			// publishes — exactly the rotation the gate exists for.
			wait := v.fetching
			v.mu.Unlock()
			<-wait
			continue
		}
		// A kid the cached set does not name is how a rotation looks from here, so it
		// earns a refetch on the short cooldown. A kid that is cached but stale waits
		// out the full interval, serving its cached key whatever a refresh in between
		// answers: a key that is already published does not depend on how recently the
		// set was read, and re-reading it more often only loads the issuer.
		cooldown := unknownKeyRefreshInterval
		if known {
			cooldown = jwksRefreshInterval
		}
		if time.Since(v.attemptAt) < cooldown {
			// Read while the lock is still held: lastFetchErr is written under
			// mu by the caller that fetched, and this read runs after Unlock.
			lastFetchErr := v.lastFetchErr
			if known {
				// A key the issuer published does not stop working because one
				// refresh failed: serving the cached copy keeps the issuer's
				// own revocation latency (one refresh interval) instead of
				// turning a single failed fetch into a login outage that lasts
				// as long as the cooldown does.
				key := v.keys[kid]
				v.mu.Unlock()
				return key, nil
			}
			v.mu.Unlock()
			// A failed fetch never saw a key set, so it cannot know which keys the
			// issuer publishes: naming the fetch failure keeps a rotation that
			// coincides with an issuer hiccup from being misread as a wrong kid.
			if lastFetchErr != nil {
				return nil, fmt.Errorf("oidc: the key set could not be refreshed to look up %q: %v", kid, lastFetchErr)
			}
			return nil, fmt.Errorf("oidc: the issuer publishes no key named %q", kid)
		}
		// This caller does the fetch. The gate is published before the unlocked
		// fetch so a second caller waits instead of starting one of its own, and
		// attemptAt is stamped now so a failure is not retried on every token.
		v.fetching = make(chan struct{})
		v.attemptAt = time.Now()
		v.mu.Unlock()
		break
	}

	return v.fetchAndInstall(kid)
}

// fetchAndInstall fetches the key set, installs it and returns the key for kid. It
// releases the fetch gate whatever happens, including a panic: leaving it set would
// park every later caller that needs a fetch on a channel nobody will close. The
// gate is why only one caller fetches at a time.
func (v *Verifier) fetchAndInstall(kid string) (crypto.PublicKey, error) {
	defer v.releaseFetch()
	keys, err := v.fetchKeys()
	if err != nil {
		v.mu.Lock()
		v.lastFetchErr = err
		// The fetch this caller performed failed, but a key the issuer already
		// published keeps verifying: refusing here would cost one verification
		// per refresh interval for as long as the issuer is unreachable, spent
		// on a key the caller was about to be handed anyway.
		if key, ok := v.keys[kid]; ok {
			v.mu.Unlock()
			return key, nil
		}
		v.mu.Unlock()
		return nil, err
	}
	v.mu.Lock()
	v.keys = keys
	v.fetchedAt = time.Now()
	v.lastFetchErr = nil
	key, ok := keys[kid]
	v.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("oidc: the issuer publishes no key named %q", kid)
	}
	return key, nil
}

func (v *Verifier) releaseFetch() {
	v.mu.Lock()
	if v.fetching != nil {
		close(v.fetching)
		v.fetching = nil
	}
	v.mu.Unlock()
}

// fetchKeys reads and decodes the issuer's key set. It runs without the verifier's
// lock, and under keyFetchTimeout so a caller's own client cannot make this wait
// forever.
func (v *Verifier) fetchKeys() (map[string]crypto.PublicKey, error) {
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			Crv string `json:"crv"`
			N   string `json:"n"`
			E   string `json:"e"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), keyFetchTimeout)
	defer cancel()
	if err := v.getJSON(ctx, v.jwksURL, &set); err != nil {
		return nil, fmt.Errorf("oidc: fetch the signing keys: %w", err)
	}
	if len(set.Keys) == 0 {
		return nil, errors.New("oidc: the issuer publishes no signing keys")
	}
	keys := make(map[string]crypto.PublicKey, len(set.Keys))
	// A key this code cannot build — an OKP or oct entry beside the RSA/EC
	// signing keys, which issuers publish — is skipped rather than fatal:
	// failing the whole set would leave no key installed at all and stop
	// every login until the issuer republished, where skipping only costs
	// the tokens whose kid names the unusable entry.
	var unusable []string
	for _, jwk := range set.Keys {
		key, err := publicKeyFromJWK(jwk.Kty, jwk.Crv, jwk.N, jwk.E, jwk.X, jwk.Y)
		if err != nil {
			unusable = append(unusable, fmt.Sprintf("%q: %v", jwk.Kid, err))
			continue
		}
		keys[jwk.Kid] = key
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("oidc: the issuer publishes no usable signing keys: %s", strings.Join(unusable, "; "))
	}
	return keys, nil
}

// publicKeyFromJWK builds a Go key from the two shapes a signing key takes:
// RSA (n, e) and elliptic curve (crv, x, y).
func publicKeyFromJWK(kty, crv, nModulus, eExponent, xCoord, yCoord string) (crypto.PublicKey, error) {
	decode := func(value string) ([]byte, error) {
		return base64.RawURLEncoding.DecodeString(value)
	}
	switch kty {
	case "RSA":
		modulus, err := decode(nModulus)
		if err != nil {
			return nil, err
		}
		exponentBytes, err := decode(eExponent)
		if err != nil {
			return nil, err
		}
		// An RSA exponent is a small number — 65537 is three bytes, and RFC 7518
		// wants the minimum number of octets. Decoding byte by byte into a uint64
		// silently wrapped past eight bytes, and int() of that went negative: the
		// entry became a key that simply never verified, reported as a generic
		// signature failure. E is an int, so the bound is what fits an int
		// everywhere: on a 32-bit build, anything larger wraps negative again.
		// The entry is refused by name instead.
		exponent := uint64(0)
		for _, b := range exponentBytes {
			exponent = exponent<<8 | uint64(b)
		}
		if exponent > math.MaxInt32 {
			return nil, errors.New("the exponent is malformed")
		}
		if exponent == 0 {
			return nil, errors.New("the exponent is empty")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(exponent)}, nil
	case "EC":
		curve := func() elliptic.Curve {
			switch crv {
			case "P-256":
				return elliptic.P256()
			case "P-384":
				return elliptic.P384()
			case "P-521":
				return elliptic.P521()
			}
			return nil
		}()
		if curve == nil {
			return nil, fmt.Errorf("curve %q is not supported", crv)
		}
		x, err := decode(xCoord)
		if err != nil {
			return nil, err
		}
		y, err := decode(yCoord)
		if err != nil {
			return nil, err
		}
		return &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}, nil
	default:
		return nil, fmt.Errorf("key type %q is not supported", kty)
	}
}

// hashFor picks the digest a JWS algorithm signs with.
func hashFor(alg string) (hash.Hash, crypto.Hash) {
	switch {
	case strings.HasSuffix(alg, "512"):
		return sha512.New(), crypto.SHA512
	case strings.HasSuffix(alg, "384"):
		return sha512.New384(), crypto.SHA384
	default:
		return sha256.New(), crypto.SHA256
	}
}

// verifySignature checks the signature with the key family the algorithm
// names.
func verifySignature(alg string, key crypto.PublicKey, digest hash.Hash, hashed crypto.Hash, signed, signature []byte) error {
	digest.Write(signed)
	sum := digest.Sum(nil)
	switch {
	case strings.HasPrefix(alg, "RS"):
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("the key is not RSA")
		}
		return rsa.VerifyPKCS1v15(rsaKey, hashed, sum, signature)
	case strings.HasPrefix(alg, "PS"):
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("the key is not RSA")
		}
		return rsa.VerifyPSS(rsaKey, hashed, sum, signature, nil)
	case strings.HasPrefix(alg, "ES"):
		ecdsaKey, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("the key is not elliptic curve")
		}
		size := (ecdsaKey.Curve.Params().BitSize + 7) / 8
		if len(signature) != 2*size {
			return errors.New("the signature has the wrong length for the curve")
		}
		r := new(big.Int).SetBytes(signature[:size])
		s := new(big.Int).SetBytes(signature[size:])
		if !ecdsa.Verify(ecdsaKey, sum, r, s) {
			return errors.New("the signature does not match")
		}
		return nil
	default:
		return fmt.Errorf("algorithm %q is not accepted", alg)
	}
}

// maxJSONResponse bounds the body one endpoint may make this verifier read. The
// issuer is remote, and a decode with no bound grows whatever it sends into this
// process's memory; a key set or a discovery document is far smaller than this.
const maxJSONResponse = 1 << 20

// getJSON fetches one JSON document with a plain GET.
func (v *Verifier) getJSON(ctx context.Context, url string, into any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	response, err := v.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 256))
		return fmt.Errorf("the endpoint answered %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(io.LimitReader(response.Body, maxJSONResponse)).Decode(into)
}

// SignToken is a test helper on the client side of the same story: it signs a
// JWT with the given headers and claims, using the RS or ES family the test
// asks for. Production clients never sign tokens; they fetch them from an
// issuer.
func SignToken(alg string, kid string, key crypto.PrivateKey, header map[string]string, claims map[string]any) (string, error) {
	build := func(part any) (string, error) {
		raw, err := json.Marshal(part)
		if err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(raw), nil
	}
	full := map[string]string{"alg": alg, "kid": kid, "typ": "JWT"}
	for name, value := range header {
		full[name] = value
	}
	head, err := build(full)
	if err != nil {
		return "", err
	}
	body, err := build(claims)
	if err != nil {
		return "", err
	}
	signing := head + "." + body
	digest, hashed := hashFor(alg)
	digest.Write([]byte(signing))
	sum := digest.Sum(nil)

	var signature []byte
	switch {
	case strings.HasPrefix(alg, "RS"):
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return "", errors.New("the key is not RSA")
		}
		signature, err = rsa.SignPKCS1v15(rand.Reader, rsaKey, hashed, sum)
	case strings.HasPrefix(alg, "ES"):
		ecdsaKey, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return "", errors.New("the key is not elliptic curve")
		}
		r, s, err2 := ecdsa.Sign(rand.Reader, ecdsaKey, sum)
		if err2 != nil {
			return "", err2
		}
		size := (ecdsaKey.Curve.Params().BitSize + 7) / 8
		signature = make([]byte, 2*size)
		r.FillBytes(signature[:size])
		s.FillBytes(signature[size:])
	default:
		return "", fmt.Errorf("algorithm %q is not supported for signing", alg)
	}
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
