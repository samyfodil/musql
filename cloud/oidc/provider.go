// Package oidc is musql cloud's OpenID Connect provider: it signs people in
// with an email and password kept in the state database and issues EdDSA
// JWTs. Authorization code with PKCE (S256 only) serves the console, the
// device flow serves the CLI.
package oidc

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/samyfodil/musql/cloud/state"
)

// Token uses, carried in the "token_use" claim so one kind of token is never
// accepted as another.
const (
	UseAccess  = "access"
	UseID      = "id"
	UseSession = "session"
	UseDB      = "db"
)

// Built-in clients.
const (
	ClientConsole = "console"
	ClientCLI     = "cli"
)

const (
	accessTTL   = time.Hour
	refreshTTL  = 30 * 24 * time.Hour
	sessionTTL  = 12 * time.Hour
	codeTTL     = time.Minute
	deviceTTL   = 10 * time.Minute
	devicePoll  = 5
	sessionName = "musql_session"
)

// Claims are every claim this provider puts in a token.
type Claims struct {
	jwt.Claims
	Use      string `json:"token_use"`
	ClientID string `json:"client_id,omitempty"`
	Scope    string `json:"scope,omitempty"`
	Email    string `json:"email,omitempty"`
	Name     string `json:"name,omitempty"`
	Nonce    string `json:"nonce,omitempty"`
	// Database tokens.
	DB     string `json:"db,omitempty"`
	Group  string `json:"group,omitempty"`
	Access string `json:"access,omitempty"`
	Gen    int64  `json:"gen,omitempty"`
}

type key struct {
	kid  string
	priv ed25519.PrivateKey
}

// Provider is the OIDC provider.
type Provider struct {
	store  *state.Store
	issuer string
	active key
	pub    map[string]ed25519.PublicKey
	secure bool
}

// New returns a provider for issuer (the public URL, no trailing slash),
// creating a signing key and the built-in clients when missing.
func New(ctx context.Context, store *state.Store, issuer string) (*Provider, error) {
	issuer = strings.TrimRight(issuer, "/")
	p := &Provider{store: store, issuer: issuer, pub: map[string]ed25519.PublicKey{}, secure: strings.HasPrefix(issuer, "https://")}
	keys, err := store.SigningKeys(ctx)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		k := &state.SigningKey{KID: state.NewID(), PrivateKey: priv.Seed(), CreatedAt: time.Now()}
		if err := store.AddSigningKey(ctx, k); err != nil {
			return nil, err
		}
		keys = []*state.SigningKey{k}
	}
	// Every key stays trusted: database tokens may never expire, so a key
	// that signed one must still verify it. The newest unretired key signs.
	for _, k := range keys {
		if len(k.PrivateKey) != ed25519.SeedSize {
			return nil, fmt.Errorf("oidc: signing key %s is malformed", k.KID)
		}
		priv := ed25519.NewKeyFromSeed(k.PrivateKey)
		p.pub[k.KID] = priv.Public().(ed25519.PublicKey)
		if p.active.priv == nil && k.RetiredAt.IsZero() {
			p.active = key{kid: k.KID, priv: priv}
		}
	}
	if p.active.priv == nil {
		return nil, errors.New("oidc: every signing key is retired")
	}
	for _, c := range []*state.Client{
		{ID: ClientConsole, RedirectURIs: []string{issuer + "/console/callback"}, Public: true},
		{ID: ClientCLI, Public: true},
	} {
		if err := store.PutClient(ctx, c); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// Issuer is the provider's issuer URL.
func (p *Provider) Issuer() string { return p.issuer }

// Sign signs c, filling in the issuer and issue time.
func (p *Provider) Sign(c *Claims) (string, error) {
	c.Issuer = p.issuer
	c.IssuedAt = jwt.NewNumericDate(time.Now())
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: p.active.priv},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), p.active.kid))
	if err != nil {
		return "", err
	}
	return jwt.Signed(sig).Claims(c).Serialize()
}

// ErrInvalidToken is any token that does not verify.
var ErrInvalidToken = errors.New("oidc: invalid token")

// Verify checks a token's signature, issuer, expiry and use.
func (p *Provider) Verify(token, use string) (*Claims, error) {
	t, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil || len(t.Headers) != 1 {
		return nil, ErrInvalidToken
	}
	pub, ok := p.pub[t.Headers[0].KeyID]
	if !ok {
		return nil, ErrInvalidToken
	}
	var c Claims
	if err := t.Claims(pub, &c); err != nil {
		return nil, ErrInvalidToken
	}
	if err := c.ValidateWithLeeway(jwt.Expected{Issuer: p.issuer, Time: time.Now()}, 30*time.Second); err != nil {
		return nil, ErrInvalidToken
	}
	if c.Use != use || c.Subject == "" && use != UseDB {
		return nil, ErrInvalidToken
	}
	return &c, nil
}

// VerifyAccess checks an access token and returns the enabled user it names.
func (p *Provider) VerifyAccess(ctx context.Context, token string) (*state.User, error) {
	c, err := p.Verify(token, UseAccess)
	if err != nil {
		return nil, err
	}
	return p.activeUser(ctx, c.Subject)
}

func (p *Provider) activeUser(ctx context.Context, id string) (*state.User, error) {
	u, err := p.store.UserByID(ctx, id)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return nil, ErrInvalidToken
		}
		return nil, err
	}
	if u.Disabled {
		return nil, ErrInvalidToken
	}
	return u, nil
}

// Handler serves the provider's endpoints: /.well-known/openid-configuration,
// /oidc/* and /login.
func (p *Provider) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /oidc/jwks", p.jwks)
	mux.HandleFunc("GET /oidc/authorize", p.authorize)
	mux.HandleFunc("POST /oidc/token", p.token)
	mux.HandleFunc("POST /oidc/device", p.deviceAuthorization)
	mux.HandleFunc("GET /oidc/device/verify", p.deviceVerifyPage)
	mux.HandleFunc("POST /oidc/device/verify", p.deviceVerify)
	mux.HandleFunc("GET /oidc/userinfo", p.userinfo)
	mux.HandleFunc("POST /oidc/userinfo", p.userinfo)
	mux.HandleFunc("GET /oidc/logout", p.logout)
	mux.HandleFunc("POST /oidc/logout", p.logout)
	mux.HandleFunc("GET /login", p.loginPage)
	mux.HandleFunc("POST /login", p.login)
	return mux
}

func (p *Provider) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                p.issuer,
		"authorization_endpoint":                p.issuer + "/oidc/authorize",
		"token_endpoint":                        p.issuer + "/oidc/token",
		"device_authorization_endpoint":         p.issuer + "/oidc/device",
		"userinfo_endpoint":                     p.issuer + "/oidc/userinfo",
		"jwks_uri":                              p.issuer + "/oidc/jwks",
		"end_session_endpoint":                  p.issuer + "/oidc/logout",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token", deviceGrant},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"EdDSA"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{"openid", "profile", "email", "offline_access"},
		"claims_supported":                      []string{"sub", "email", "name"},
	})
}

func (p *Provider) jwks(w http.ResponseWriter, r *http.Request) {
	var set jose.JSONWebKeySet
	for kid, pub := range p.pub {
		set.Keys = append(set.Keys, jose.JSONWebKey{Key: pub, KeyID: kid, Algorithm: string(jose.EdDSA), Use: "sig"})
	}
	writeJSON(w, http.StatusOK, set)
}

// session returns the signed-in user from the session cookie, or nil.
func (p *Provider) session(r *http.Request) *state.User {
	ck, err := r.Cookie(sessionName)
	if err != nil {
		return nil
	}
	c, err := p.Verify(ck.Value, UseSession)
	if err != nil {
		return nil
	}
	u, err := p.activeUser(r.Context(), c.Subject)
	if err != nil {
		return nil
	}
	return u
}

func (p *Provider) setSession(w http.ResponseWriter, u *state.User) error {
	tok, err := p.Sign(&Claims{Claims: jwt.Claims{Subject: u.ID, Expiry: jwt.NewNumericDate(time.Now().Add(sessionTTL))}, Use: UseSession})
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionName, Value: tok, Path: "/", HttpOnly: true, Secure: p.secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL / time.Second)})
	return nil
}

// localPath reports whether next is a path on this server, so a login cannot
// be used to redirect elsewhere.
func localPath(next string) bool {
	return strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") && !strings.HasPrefix(next, "/\\")
}

func (p *Provider) toLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/login?next="+urlQueryEscape(r.URL.RequestURI()), http.StatusFound)
}

// dummyHash is checked against when an email is unknown, so a login takes
// as long whether or not the account exists.
var dummyHash = state.HashPassword("musql-cloud-no-such-user")

// Authenticate checks an email and password.
func (p *Provider) Authenticate(ctx context.Context, email, password string) (*state.User, error) {
	u, err := p.store.UserByEmail(ctx, email)
	if errors.Is(err, state.ErrNotFound) {
		state.VerifyPassword(dummyHash, password)
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, err
	}
	if !state.VerifyPassword(u.PasswordHash, password) || u.Disabled {
		return nil, ErrInvalidToken
	}
	return u, nil
}

func (p *Provider) loginPage(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if !localPath(next) {
		next = "/console/"
	}
	render(w, http.StatusOK, loginTmpl, map[string]any{"Next": next})
}

func (p *Provider) login(w http.ResponseWriter, r *http.Request) {
	next := r.PostFormValue("next")
	if !localPath(next) {
		next = "/console/"
	}
	u, err := p.Authenticate(r.Context(), r.PostFormValue("email"), r.PostFormValue("password"))
	if err != nil {
		render(w, http.StatusUnauthorized, loginTmpl, map[string]any{"Next": next, "Email": r.PostFormValue("email"), "Error": "Wrong email or password."})
		return
	}
	if err := p.setSession(w, u); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (p *Provider) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionName, Value: "", Path: "/", HttpOnly: true, Secure: p.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	if to := r.FormValue("post_logout_redirect_uri"); to != "" && strings.HasPrefix(to, p.issuer+"/") {
		http.Redirect(w, r, to, http.StatusFound)
		return
	}
	render(w, http.StatusOK, messageTmpl, map[string]any{"Title": "Signed out", "Message": "You are signed out."})
}

func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	client, err := p.store.Client(r.Context(), q.Get("client_id"))
	if err != nil {
		render(w, http.StatusBadRequest, messageTmpl, map[string]any{"Title": "Sign-in failed", "Message": "Unknown client."})
		return
	}
	redirect := q.Get("redirect_uri")
	if !slices.Contains(client.RedirectURIs, redirect) {
		// Never redirect to an unregistered URI, not even with an error.
		render(w, http.StatusBadRequest, messageTmpl, map[string]any{"Title": "Sign-in failed", "Message": "The redirect URI is not registered for this client."})
		return
	}
	fail := func(code, desc string) {
		v := urlValues{"error": code, "error_description": desc, "state": q.Get("state")}
		http.Redirect(w, r, redirect+sep(redirect)+v.encode(), http.StatusFound)
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only code is supported")
		return
	}
	challenge := q.Get("code_challenge")
	if q.Get("code_challenge_method") != "S256" || len(challenge) != 43 {
		fail("invalid_request", "PKCE with S256 is required")
		return
	}
	u := p.session(r)
	if u == nil || q.Get("prompt") == "login" {
		if q.Get("prompt") == "none" {
			fail("login_required", "not signed in")
			return
		}
		if q.Get("prompt") == "login" {
			q.Del("prompt")
			r.URL.RawQuery = q.Encode()
		}
		p.toLogin(w, r)
		return
	}
	code, err := p.store.CreateAuthCode(r.Context(), state.AuthCode{
		ClientID: client.ID, UserID: u.ID, RedirectURI: redirect, Challenge: challenge,
		Scope: q.Get("scope"), Nonce: q.Get("nonce"),
	}, codeTTL)
	if err != nil {
		fail("server_error", "")
		return
	}
	v := urlValues{"code": code, "state": q.Get("state"), "iss": p.issuer}
	http.Redirect(w, r, redirect+sep(redirect)+v.encode(), http.StatusFound)
}

const deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

func tokenError(w http.ResponseWriter, code, desc string) {
	status := http.StatusBadRequest
	if code == "invalid_client" {
		status = http.StatusUnauthorized
	}
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	clientID := r.PostFormValue("client_id")
	if _, err := p.store.Client(ctx, clientID); err != nil {
		tokenError(w, "invalid_client", "unknown client")
		return
	}
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		c, err := p.store.TakeAuthCode(ctx, r.PostFormValue("code"))
		if err != nil || c.ClientID != clientID || c.RedirectURI != r.PostFormValue("redirect_uri") || !pkceOK(c.Challenge, r.PostFormValue("code_verifier")) {
			tokenError(w, "invalid_grant", "invalid code, redirect URI or verifier")
			return
		}
		p.issue(w, r, c.UserID, clientID, c.Scope, c.Nonce, "")
	case "refresh_token":
		rt, err := p.store.UseRefreshToken(ctx, r.PostFormValue("refresh_token"))
		if err != nil || rt.ClientID != clientID {
			tokenError(w, "invalid_grant", "invalid refresh token")
			return
		}
		p.issue(w, r, rt.UserID, clientID, rt.Scope, "", rt.Family)
	case deviceGrant:
		d, err := p.store.PollDevice(ctx, r.PostFormValue("device_code"))
		switch {
		case err != nil || d.ClientID != clientID:
			tokenError(w, "invalid_grant", "unknown device code")
		case d.Expired:
			tokenError(w, "expired_token", "")
		case d.Status == state.DeviceDenied:
			tokenError(w, "access_denied", "")
		case d.Status == state.DevicePending:
			tokenError(w, "authorization_pending", "")
		default:
			p.issue(w, r, d.UserID, clientID, d.Scope, "", "")
		}
	default:
		tokenError(w, "unsupported_grant_type", "")
	}
}

// pkceOK checks an RFC 7636 S256 verifier against its challenge.
func pkceOK(challenge, verifier string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]) == challenge
}

// issue writes a token response: an access token, a rotated refresh token
// (in family, or a new one), and an ID token when openid was asked for.
func (p *Provider) issue(w http.ResponseWriter, r *http.Request, userID, clientID, scope, nonce, family string) {
	ctx := r.Context()
	u, err := p.activeUser(ctx, userID)
	if err != nil {
		tokenError(w, "invalid_grant", "user is disabled or gone")
		return
	}
	exp := jwt.NewNumericDate(time.Now().Add(accessTTL))
	access, err := p.Sign(&Claims{Claims: jwt.Claims{Subject: u.ID, Audience: jwt.Audience{clientID}, Expiry: exp, ID: state.NewID()},
		Use: UseAccess, ClientID: clientID, Scope: scope})
	if err != nil {
		tokenError(w, "server_error", "")
		return
	}
	refresh, err := p.store.CreateRefreshToken(ctx, state.RefreshToken{Family: family, UserID: u.ID, ClientID: clientID, Scope: scope}, refreshTTL)
	if err != nil {
		tokenError(w, "server_error", "")
		return
	}
	resp := map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessTTL / time.Second),
		"refresh_token": refresh,
		"scope":         scope,
	}
	if hasScope(scope, "openid") {
		id, err := p.Sign(&Claims{Claims: jwt.Claims{Subject: u.ID, Audience: jwt.Audience{clientID}, Expiry: exp},
			Use: UseID, Email: u.Email, Name: u.Name, Nonce: nonce})
		if err != nil {
			tokenError(w, "server_error", "")
			return
		}
		resp["id_token"] = id
	}
	writeJSON(w, http.StatusOK, resp)
}

func (p *Provider) userinfo(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	u, err := p.VerifyAccess(r.Context(), tok)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sub": u.ID, "email": u.Email, "name": u.Name})
}

// userCodeAlphabet has no vowels or look-alikes (RFC 8628 §6.1).
const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

func newUserCode() string {
	b := make([]byte, 8)
	rand.Read(b)
	out := make([]byte, 0, 9)
	for i, c := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, userCodeAlphabet[int(c)%len(userCodeAlphabet)])
	}
	return string(out)
}

// normalizeUserCode accepts what a person types: any case, with or without
// the dash or spaces.
func normalizeUserCode(s string) string {
	var b []byte
	for _, c := range strings.ToUpper(s) {
		if strings.ContainsRune(userCodeAlphabet, c) {
			b = append(b, byte(c))
		}
	}
	if len(b) != 8 {
		return ""
	}
	return string(b[:4]) + "-" + string(b[4:])
}

func (p *Provider) deviceAuthorization(w http.ResponseWriter, r *http.Request) {
	clientID := r.PostFormValue("client_id")
	if clientID != ClientCLI {
		if _, err := p.store.Client(r.Context(), clientID); err != nil {
			tokenError(w, "invalid_client", "unknown client")
			return
		}
	}
	for range 5 {
		uc := newUserCode()
		dc, err := p.store.CreateDeviceCode(r.Context(), uc, clientID, r.PostFormValue("scope"), deviceTTL)
		if errors.Is(err, state.ErrExists) {
			continue
		}
		if err != nil {
			tokenError(w, "server_error", "")
			return
		}
		verify := p.issuer + "/oidc/device/verify"
		writeJSON(w, http.StatusOK, map[string]any{
			"device_code":               dc,
			"user_code":                 uc,
			"verification_uri":          verify,
			"verification_uri_complete": verify + "?user_code=" + uc,
			"expires_in":                int(deviceTTL / time.Second),
			"interval":                  devicePoll,
		})
		return
	}
	tokenError(w, "server_error", "")
}

func (p *Provider) deviceVerifyPage(w http.ResponseWriter, r *http.Request) {
	if p.session(r) == nil {
		p.toLogin(w, r)
		return
	}
	render(w, http.StatusOK, deviceTmpl, map[string]any{"Code": r.URL.Query().Get("user_code")})
}

func (p *Provider) deviceVerify(w http.ResponseWriter, r *http.Request) {
	u := p.session(r)
	if u == nil {
		http.Redirect(w, r, "/login?next=/oidc/device/verify", http.StatusSeeOther)
		return
	}
	code := normalizeUserCode(r.PostFormValue("user_code"))
	if _, err := p.store.DeviceByUserCode(r.Context(), code); code == "" || err != nil {
		render(w, http.StatusBadRequest, deviceTmpl, map[string]any{"Code": r.PostFormValue("user_code"), "Error": "That code is unknown or has expired."})
		return
	}
	approve := r.PostFormValue("action") == "approve"
	if err := p.store.DecideDevice(r.Context(), code, u.ID, approve); err != nil {
		render(w, http.StatusBadRequest, deviceTmpl, map[string]any{"Error": "That code is unknown or has expired."})
		return
	}
	msg := "The device is signed in. You can close this page."
	if !approve {
		msg = "The request was denied."
	}
	render(w, http.StatusOK, messageTmpl, map[string]any{"Title": "Device sign-in", "Message": msg})
}

func hasScope(scope, want string) bool { return slices.Contains(strings.Fields(scope), want) }

func sep(u string) string {
	if strings.Contains(u, "?") {
		return "&"
	}
	return "?"
}
