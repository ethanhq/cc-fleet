package codexproxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// refreshSkew refreshes the access token this long before its JWT exp.
const refreshSkew = 120 * time.Second

// ErrReauth means the stored refresh token is dead (or absent); the caller must
// run `cc-fleet codex login` again. It never means a transient network failure.
var ErrReauth = errors.New("codexproxy: codex login required")

// tokens is the persisted credential chain. The access token is cached in memory
// only; refresh_token + account_id are durable (see tokenStore).
type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
}

// oauthClient performs the device-code login and refresh grants. It owns no state;
// the tokenStore holds the durable chain.
type oauthClient struct {
	http *http.Client
}

func newOAuthClient() *oauthClient {
	return &oauthClient{http: &http.Client{Timeout: 45 * time.Second}}
}

// deviceCode is the start-of-login handle shown to the user.
type deviceCode struct {
	deviceAuthID string
	userCode     string
	verifyURL    string
	interval     time.Duration
	expiresAt    time.Time
}

type deviceCodeResp struct {
	DeviceAuthID string          `json:"device_auth_id"`
	UserCode     string          `json:"user_code"`
	Interval     json.RawMessage `json:"interval"`
	ExpiresIn    int             `json:"expires_in"`
}

// startDeviceLogin begins an OAuth device-code flow; the user authorizes deviceVerifyURL
// with the returned user code, then the caller polls pollDeviceLogin.
func (c *oauthClient) startDeviceLogin(ctx context.Context, now time.Time) (*deviceCode, error) {
	body, _ := json.Marshal(map[string]string{"client_id": oauthClientID})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, deviceUserCodeURL, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgentValue)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device start: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("device start: http %d", resp.StatusCode)
	}
	var dr deviceCodeResp
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		return nil, fmt.Errorf("device start decode: %w", err)
	}
	expiresIn := dr.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 900
	}
	return &deviceCode{
		deviceAuthID: dr.DeviceAuthID,
		userCode:     dr.UserCode,
		verifyURL:    deviceVerifyURL,
		interval:     parseInterval(dr.Interval),
		expiresAt:    now.Add(time.Duration(expiresIn) * time.Second),
	}, nil
}

// errAuthPending is returned by a poll while the user has not yet authorized.
var errAuthPending = errors.New("authorization pending")

type devicePollResp struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
}

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// pollDeviceLogin polls once; it returns errAuthPending until the user authorizes,
// then exchanges the authorization code for the token chain.
func (c *oauthClient) pollDeviceLogin(ctx context.Context, dc *deviceCode) (*tokens, error) {
	body, _ := json.Marshal(map[string]string{"device_auth_id": dc.deviceAuthID, "user_code": dc.userCode})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, deviceTokenURL, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgentValue)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device poll: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusNotFound:
		return nil, errAuthPending
	case resp.StatusCode == http.StatusGone:
		return nil, errors.New("device code expired; restart login")
	case resp.StatusCode/100 != 2:
		return nil, fmt.Errorf("device poll: http %d", resp.StatusCode)
	}
	var pr devicePollResp
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, fmt.Errorf("device poll decode: %w", err)
	}
	return c.exchangeCode(ctx, pr.AuthorizationCode, pr.CodeVerifier)
}

func (c *oauthClient) exchangeCode(ctx context.Context, code, verifier string) (*tokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {deviceRedirectURI},
		"client_id":     {oauthClientID},
		"code_verifier": {verifier},
	}
	tr, err := c.postToken(ctx, form)
	if err != nil {
		return nil, err
	}
	tk := &tokens{AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken, IDToken: tr.IDToken}
	tk.AccountID = accountIDFromTokens(tk)
	return tk, nil
}

// refresh exchanges the refresh token for a new access token (and possibly a
// rotated refresh token). A 401/403 means the chain is dead -> ErrReauth.
func (c *oauthClient) refresh(ctx context.Context, refreshToken string) (*tokens, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {oauthClientID},
		"scope":         {"openid profile email"},
	}
	tr, err := c.postToken(ctx, form)
	if err != nil {
		return nil, err
	}
	rt := tr.RefreshToken
	if rt == "" {
		rt = refreshToken // server omitted a rotation; keep the current one
	}
	tk := &tokens{AccessToken: tr.AccessToken, RefreshToken: rt, IDToken: tr.IDToken}
	tk.AccountID = accountIDFromTokens(tk)
	return tk, nil
}

func (c *oauthClient) postToken(ctx context.Context, form url.Values) (*tokenResp, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgentValue)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token grant: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrReauth
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("token grant: http %d", resp.StatusCode)
	}
	var tr tokenResp
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("token grant decode: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, errors.New("token grant: empty access_token")
	}
	return &tr, nil
}

func parseInterval(raw json.RawMessage) time.Duration {
	secs := 5
	if len(raw) > 0 {
		var n int
		if json.Unmarshal(raw, &n) == nil && n > 0 {
			secs = n
		} else {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				if v, err := time.ParseDuration(s + "s"); err == nil && v > 0 {
					secs = int(v.Seconds())
				}
			}
		}
	}
	return time.Duration(secs+3) * time.Second
}

// jwtClaims base64url-decodes a JWT payload to a claims map. Decode-only; never
// verifies a signature (we only read identity claims from our own tokens).
func jwtClaims(jwt string) map[string]any {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		return nil
	}
	return m
}

// accountIDFromTokens derives chatgpt_account_id via the fallback chain
// id_token claim -> access_token claim. After a refresh the access_token can omit
// the claim, so the id_token is tried first.
func accountIDFromTokens(tk *tokens) string {
	if tk.AccountID != "" {
		return tk.AccountID
	}
	for _, jwt := range []string{tk.IDToken, tk.AccessToken} {
		if claimAccount := accountIDFromClaim(jwtClaims(jwt)); claimAccount != "" {
			return claimAccount
		}
	}
	return ""
}

func accountIDFromClaim(claims map[string]any) string {
	auth, ok := claims[jwtAuthClaim].(map[string]any)
	if !ok {
		return ""
	}
	id, _ := auth["chatgpt_account_id"].(string)
	return id
}

// tokenExpiry reads the exp claim (unix seconds) from an access token JWT.
func tokenExpiry(accessToken string) (time.Time, bool) {
	claims := jwtClaims(accessToken)
	exp, ok := claims["exp"].(float64)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(exp), 0), true
}
