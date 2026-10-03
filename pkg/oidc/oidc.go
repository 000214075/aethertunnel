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
func (v *Verifier) signingKey(kid string) (crypto.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if key, ok := v.keys[kid]; ok && time.Since(v.fetchedAt) < jwksRefreshInterval {
		return key, nil
	}
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
	if err := v.getJSON(context.Background(), v.jwksURL, &set); err != nil {
		return nil, fmt.Errorf("oidc: fetch the signing keys: %w", err)
	}
	if len(set.Keys) == 0 {
		return nil, errors.New("oidc: the issuer publishes no signing keys")
	}
	keys := make(map[string]crypto.PublicKey, len(set.Keys))
	for _, jwk := range set.Keys {
		key, err := publicKeyFromJWK(jwk.Kty, jwk.Crv, jwk.N, jwk.E, jwk.X, jwk.Y)
		if err != nil {
			return nil, fmt.Errorf("oidc: signing key %q is unusable: %w", jwk.Kid, err)
		}
		keys[jwk.Kid] = key
	}
	v.keys = keys
	v.fetchedAt = time.Now()
	key, ok := keys[kid]
	if !ok {
		return nil, fmt.Errorf("oidc: the issuer publishes no key named %q", kid)
	}
	return key, nil
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
		exponent := uint64(0)
		for _, b := range exponentBytes {
			exponent = exponent<<8 | uint64(b)
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
	return json.NewDecoder(response.Body).Decode(into)
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
