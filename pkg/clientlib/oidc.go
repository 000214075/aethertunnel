package clientlib

// The [oidc] client side: an access token fetched from the identity provider
// with the client-credentials grant, presented in place of the static auth
// token on every (re)connect. The token is cached until shortly before its
// expiry; a provider that names no expiry gets asked again on every connect,
// the way frp's non-caching token source behaves.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// oidcAccessToken returns the client's access token, fetching a fresh one when
// the cached one is within thirty seconds of its expiry.
func (c *client) oidcAccessToken(ctx context.Context) (string, error) {
	options := c.cfg.OIDC
	if options == nil || options.TokenEndpointURL == "" {
		return "", errors.New("oidc: no token endpoint is configured")
	}
	c.oidcMu.Lock()
	defer c.oidcMu.Unlock()
	if c.oidcToken != "" && time.Now().Before(c.oidcExpiry.Add(-30*time.Second)) {
		return c.oidcToken, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	if options.Audience != "" {
		form.Set("audience", options.Audience)
	}
	if options.Scope != "" {
		form.Set("scope", options.Scope)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, options.TokenEndpointURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.SetBasicAuth(options.ClientID, options.ClientSecret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("oidc: the token request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 256))
		return "", fmt.Errorf("oidc: the token endpoint answered %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		return "", fmt.Errorf("oidc: the token answer is unreadable: %w", err)
	}
	if answer.AccessToken == "" {
		return "", errors.New("oidc: the token answer carries no access_token")
	}
	if answer.ExpiresIn > 0 {
		c.oidcToken = answer.AccessToken
		c.oidcExpiry = time.Now().Add(time.Duration(answer.ExpiresIn) * time.Second)
		c.logger.Printf("oidc: fetched an access token (expires in %ds)", answer.ExpiresIn)
	} else {
		c.logger.Printf("oidc: fetched an access token with no expiry; it is fetched again on every reconnect")
	}
	return answer.AccessToken, nil
}
