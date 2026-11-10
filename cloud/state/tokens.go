package state

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// PlatformToken is an API token acting as the user who created it. Only its
// hash is kept.
type PlatformToken struct {
	ID         string
	UserID     string
	Name       string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// PlatformTokenPrefix marks a platform token, so a bearer is told apart from
// a JWT without trying both.
const PlatformTokenPrefix = "mpt_"

// CreatePlatformToken creates a token and returns it with its secret.
func (s *Store) CreatePlatformToken(ctx context.Context, userID, name string) (*PlatformToken, string, error) {
	secret := PlatformTokenPrefix + NewSecret()
	t := &PlatformToken{ID: NewID(), UserID: userID, Name: name, CreatedAt: unix(now())}
	_, err := s.exec(ctx, `INSERT INTO platform_tokens (id, user_id, name, hash, created_at) VALUES (?, ?, ?, ?, ?)`,
		t.ID, t.UserID, t.Name, HashToken(secret), t.CreatedAt.Unix())
	if err != nil {
		return nil, "", err
	}
	return t, secret, nil
}

// PlatformTokenUser returns the user a platform token secret acts as.
func (s *Store) PlatformTokenUser(ctx context.Context, secret string) (*User, error) {
	if !strings.HasPrefix(secret, PlatformTokenPrefix) {
		return nil, ErrNotFound
	}
	var id, userID string
	if err := s.q().QueryRowContext(ctx, `SELECT id, user_id FROM platform_tokens WHERE hash = ?`, HashToken(secret)).Scan(&id, &userID); err != nil {
		return nil, notFound(err)
	}
	if _, err := s.exec(ctx, `UPDATE platform_tokens SET last_used_at = ? WHERE id = ?`, now(), id); err != nil {
		return nil, err
	}
	return s.UserByID(ctx, userID)
}

// PlatformTokens lists a user's platform tokens.
func (s *Store) PlatformTokens(ctx context.Context, userID string) ([]*PlatformToken, error) {
	rows, err := s.q().QueryContext(ctx, `SELECT id, user_id, name, created_at, last_used_at FROM platform_tokens WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PlatformToken
	for rows.Next() {
		var t PlatformToken
		var created, used int64
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &created, &used); err != nil {
			return nil, err
		}
		t.CreatedAt, t.LastUsedAt = unix(created), unix(used)
		out = append(out, &t)
	}
	return out, rows.Err()
}

// RevokePlatformToken deletes one of a user's platform tokens.
func (s *Store) RevokePlatformToken(ctx context.Context, userID, id string) error {
	return one(s.exec(ctx, `DELETE FROM platform_tokens WHERE id = ? AND user_id = ?`, id, userID))
}

// RefreshToken is a stored refresh token.
type RefreshToken struct {
	Family   string
	UserID   string
	ClientID string
	Scope    string
}

// CreateRefreshToken stores a new refresh token in family (a new family when
// empty) and returns its secret.
func (s *Store) CreateRefreshToken(ctx context.Context, rt RefreshToken, ttl time.Duration) (string, error) {
	if rt.Family == "" {
		rt.Family = NewID()
	}
	secret := NewSecret()
	_, err := s.exec(ctx, `INSERT INTO refresh_tokens (hash, family, user_id, client_id, scope, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		HashToken(secret), rt.Family, rt.UserID, rt.ClientID, rt.Scope, time.Now().Add(ttl).Unix())
	return secret, err
}

// UseRefreshToken marks a refresh token used and returns it. Presenting a
// token already used means it was copied: its whole family is revoked.
func (s *Store) UseRefreshToken(ctx context.Context, secret string) (*RefreshToken, error) {
	var rt RefreshToken
	reused := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var used bool
		var exp int64
		h := HashToken(secret)
		if err := tx.QueryRow(`SELECT family, user_id, client_id, scope, used, expires_at FROM refresh_tokens WHERE hash = ?`, h).
			Scan(&rt.Family, &rt.UserID, &rt.ClientID, &rt.Scope, &used, &exp); err != nil {
			return notFound(err)
		}
		if used {
			if _, err := tx.Exec(`DELETE FROM refresh_tokens WHERE family = ?`, rt.Family); err != nil {
				return err
			}
			reused = true
			return nil
		}
		if exp < now() {
			return ErrNotFound
		}
		_, err := tx.Exec(`UPDATE refresh_tokens SET used = 1 WHERE hash = ?`, h)
		return err
	})
	if err != nil {
		return nil, err
	}
	if reused {
		return nil, ErrNotFound
	}
	return &rt, nil
}

// RevokeRefreshToken deletes the whole family of a refresh token issued to
// clientID. An unknown token, or another client's, is not an error.
func (s *Store) RevokeRefreshToken(ctx context.Context, secret, clientID string) error {
	_, err := s.exec(ctx, `DELETE FROM refresh_tokens WHERE family IN (SELECT family FROM refresh_tokens WHERE hash = ? AND client_id = ?)`,
		HashToken(secret), clientID)
	return err
}

// RevokeUserRefreshTokens revokes every refresh token a user holds.
func (s *Store) RevokeUserRefreshTokens(ctx context.Context, userID string) error {
	_, err := s.exec(ctx, `DELETE FROM refresh_tokens WHERE user_id = ?`, userID)
	return err
}

// SigningKey is an Ed25519 key the provider signs with.
type SigningKey struct {
	KID        string
	PrivateKey []byte
	CreatedAt  time.Time
	RetiredAt  time.Time
	// MaxExp is the latest expiry (unix seconds) of any token the key
	// signed, 0 when it has signed none, and -1 when a token it signed may
	// never expire.
	MaxExp int64
}

// SigningKeys returns every signing key, newest first.
func (s *Store) SigningKeys(ctx context.Context) ([]*SigningKey, error) {
	rows, err := s.q().QueryContext(ctx, `SELECT kid, private_key, created_at, retired_at, max_exp FROM signing_keys ORDER BY created_at DESC, kid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SigningKey
	for rows.Next() {
		var k SigningKey
		var created, retired int64
		if err := rows.Scan(&k.KID, &k.PrivateKey, &created, &retired, &k.MaxExp); err != nil {
			return nil, err
		}
		k.CreatedAt, k.RetiredAt = unix(created), unix(retired)
		out = append(out, &k)
	}
	return out, rows.Err()
}

// AddSigningKey stores a key, ErrExists when its kid is stored already.
func (s *Store) AddSigningKey(ctx context.Context, k *SigningKey) error {
	_, err := s.exec(ctx, `INSERT INTO signing_keys (kid, private_key, created_at, max_exp) VALUES (?, ?, ?, ?)`, k.KID, k.PrivateKey, k.CreatedAt.Unix(), k.MaxExp)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

// RetireSigningKey marks a key as no longer signing.
func (s *Store) RetireSigningKey(ctx context.Context, kid string, at time.Time) error {
	return one(s.exec(ctx, `UPDATE signing_keys SET retired_at = ? WHERE kid = ?`, at.Unix(), kid))
}

// SetSigningKeyMaxExp records the latest expiry of the tokens a key signed.
func (s *Store) SetSigningKeyMaxExp(ctx context.Context, kid string, maxExp int64) error {
	return one(s.exec(ctx, `UPDATE signing_keys SET max_exp = ? WHERE kid = ?`, maxExp, kid))
}

// DeleteSigningKey removes a key, which stops verifying what it signed.
func (s *Store) DeleteSigningKey(ctx context.Context, kid string) error {
	return one(s.exec(ctx, `DELETE FROM signing_keys WHERE kid = ?`, kid))
}

// Client is an OIDC client.
type Client struct {
	ID           string
	RedirectURIs []string
	Public       bool
}

// PutClient creates or replaces a client.
func (s *Store) PutClient(ctx context.Context, c *Client) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM oidc_clients WHERE id = ?`, c.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO oidc_clients (id, redirect_uris, public) VALUES (?, ?, ?)`, c.ID, strings.Join(c.RedirectURIs, "\n"), c.Public)
		return err
	})
}

// Client returns a client.
func (s *Store) Client(ctx context.Context, id string) (*Client, error) {
	var c Client
	var uris string
	if err := s.q().QueryRowContext(ctx, `SELECT id, redirect_uris, public FROM oidc_clients WHERE id = ?`, id).Scan(&c.ID, &uris, &c.Public); err != nil {
		return nil, notFound(err)
	}
	if uris != "" {
		c.RedirectURIs = strings.Split(uris, "\n")
	}
	return &c, nil
}

// AuthCode is an authorization code's grant.
type AuthCode struct {
	ClientID    string
	UserID      string
	RedirectURI string
	Challenge   string
	Scope       string
	Nonce       string
}

// CreateAuthCode stores a code and returns its secret.
func (s *Store) CreateAuthCode(ctx context.Context, c AuthCode, ttl time.Duration) (string, error) {
	secret := NewSecret()
	_, err := s.exec(ctx, `INSERT INTO auth_codes (hash, client_id, user_id, redirect_uri, challenge, scope, nonce, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		HashToken(secret), c.ClientID, c.UserID, c.RedirectURI, c.Challenge, c.Scope, c.Nonce, time.Now().Add(ttl).Unix())
	return secret, err
}

// TakeAuthCode consumes a code: it can be redeemed once.
func (s *Store) TakeAuthCode(ctx context.Context, secret string) (*AuthCode, error) {
	var c AuthCode
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var exp int64
		h := HashToken(secret)
		if err := tx.QueryRow(`SELECT client_id, user_id, redirect_uri, challenge, scope, nonce, expires_at FROM auth_codes WHERE hash = ?`, h).
			Scan(&c.ClientID, &c.UserID, &c.RedirectURI, &c.Challenge, &c.Scope, &c.Nonce, &exp); err != nil {
			return notFound(err)
		}
		if _, err := tx.Exec(`DELETE FROM auth_codes WHERE hash = ?`, h); err != nil {
			return err
		}
		if exp < now() {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Device code states.
const (
	DevicePending  = "pending"
	DeviceApproved = "approved"
	DeviceDenied   = "denied"
)

// DeviceCode is a device authorization in progress.
type DeviceCode struct {
	UserCode string
	ClientID string
	Scope    string
	UserID   string
	Status   string
	Expired  bool
}

// CreateDeviceCode stores a device authorization and returns the device
// code's secret. userCode must be unique among pending ones.
func (s *Store) CreateDeviceCode(ctx context.Context, userCode, clientID, scope string, ttl time.Duration) (string, error) {
	if _, err := s.exec(ctx, `DELETE FROM device_codes WHERE expires_at < ?`, now()); err != nil {
		return "", err
	}
	secret := NewSecret()
	_, err := s.exec(ctx, `INSERT INTO device_codes (device_hash, user_code, client_id, scope, expires_at) VALUES (?, ?, ?, ?, ?)`,
		HashToken(secret), userCode, clientID, scope, time.Now().Add(ttl).Unix())
	if isUnique(err) {
		return "", ErrExists
	}
	return secret, err
}

// DeviceByUserCode returns a pending, unexpired device authorization.
func (s *Store) DeviceByUserCode(ctx context.Context, userCode string) (*DeviceCode, error) {
	var d DeviceCode
	var exp int64
	if err := s.q().QueryRowContext(ctx, `SELECT user_code, client_id, scope, user_id, status, expires_at FROM device_codes WHERE user_code = ?`, userCode).
		Scan(&d.UserCode, &d.ClientID, &d.Scope, &d.UserID, &d.Status, &exp); err != nil {
		return nil, notFound(err)
	}
	if exp < now() || d.Status != DevicePending {
		return nil, ErrNotFound
	}
	return &d, nil
}

// DecideDevice approves (for userID) or denies a pending authorization.
func (s *Store) DecideDevice(ctx context.Context, userCode, userID string, approve bool) error {
	status := DeviceDenied
	if approve {
		status = DeviceApproved
	}
	return one(s.exec(ctx, `UPDATE device_codes SET status = ?, user_id = ? WHERE user_code = ? AND status = ? AND expires_at >= ?`,
		status, userID, userCode, DevicePending, now()))
}

// PollDevice returns a device authorization by its device code; an approved
// or denied one is consumed, so it yields tokens once.
func (s *Store) PollDevice(ctx context.Context, secret string) (*DeviceCode, error) {
	var d DeviceCode
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var exp int64
		h := HashToken(secret)
		if err := tx.QueryRow(`SELECT user_code, client_id, scope, user_id, status, expires_at FROM device_codes WHERE device_hash = ?`, h).
			Scan(&d.UserCode, &d.ClientID, &d.Scope, &d.UserID, &d.Status, &exp); err != nil {
			return notFound(err)
		}
		d.Expired = exp < now()
		if d.Status != DevicePending || d.Expired {
			_, err := tx.Exec(`DELETE FROM device_codes WHERE device_hash = ?`, h)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &d, nil
}
