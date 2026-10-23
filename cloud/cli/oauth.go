package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// clientID is the server's built-in public client for the CLI.
const clientID = "cli"

// DeviceAuth is a device authorization response (RFC 8628).
type DeviceAuth struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

func postForm(ctx context.Context, hc *http.Client, endpoint string, form url.Values, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("%s: %s", endpoint, resp.Status)
	}
	return resp.StatusCode, nil
}

// StartDevice begins a device sign-in at server.
func StartDevice(ctx context.Context, hc *http.Client, server string) (*DeviceAuth, error) {
	var d DeviceAuth
	status, err := postForm(ctx, hc, server+"/oidc/device", url.Values{"client_id": {clientID}, "scope": {"openid"}}, &d)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || d.DeviceCode == "" {
		return nil, fmt.Errorf("device authorization failed (%d)", status)
	}
	return &d, nil
}

// PollDevice waits until the user approves or denies d, or it expires.
func PollDevice(ctx context.Context, hc *http.Client, server string, d *DeviceAuth) (*tokenResponse, error) {
	interval := time.Duration(max(d.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(d.ExpiresIn) * time.Second)
	for {
		var t tokenResponse
		if _, err := postForm(ctx, hc, server+"/oidc/token", url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":   {clientID},
			"device_code": {d.DeviceCode},
		}, &t); err != nil {
			return nil, err
		}
		switch t.Error {
		case "":
			return &t, nil
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			return nil, errors.New("sign-in was denied")
		case "expired_token":
			return nil, errors.New("the code expired; run musql auth login again")
		default:
			return nil, fmt.Errorf("sign-in failed: %s %s", t.Error, t.Description)
		}
		if d.ExpiresIn > 0 && time.Now().After(deadline) {
			return nil, errors.New("the code expired; run musql auth login again")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// refresh trades a refresh token for new tokens; the server rotates it, so
// the old one is spent whatever happens next.
func refresh(ctx context.Context, hc *http.Client, server, rt string) (*tokenResponse, error) {
	var t tokenResponse
	if _, err := postForm(ctx, hc, server+"/oidc/token", url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {rt},
	}, &t); err != nil {
		return nil, err
	}
	if t.Error != "" {
		return nil, errNotSignedIn
	}
	return &t, nil
}

// revoke asks the server to forget a refresh token (RFC 7009).
func revoke(ctx context.Context, hc *http.Client, server, rt string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/oidc/revoke",
		strings.NewReader(url.Values{"client_id": {clientID}, "token": {rt}, "token_type_hint": {"refresh_token"}}.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revoke: %s", resp.Status)
	}
	return nil
}

func (c *Config) setTokens(t *tokenResponse) {
	c.AccessToken, c.RefreshToken = t.AccessToken, t.RefreshToken
	c.Expiry = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
}
