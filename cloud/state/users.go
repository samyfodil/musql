package state

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"
)

// User is a person who can sign in.
type User struct {
	ID           string
	Email        string
	Name         string
	PasswordHash string
	Admin        bool
	Disabled     bool
	CreatedAt    time.Time
}

const userCols = `id, email, name, password_hash, admin, disabled, created_at`

type scanner interface{ Scan(...any) error }

func scanUser(r scanner) (*User, error) {
	var u User
	var created int64
	if err := r.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Admin, &u.Disabled, &created); err != nil {
		return nil, notFound(err)
	}
	u.CreatedAt = unix(created)
	return &u, nil
}

// NormalizeEmail is the form emails are stored and looked up in.
func NormalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// CreateUser creates a user and its personal org (slug from the email's local
// part, made unique), with the user as owner. The user's ID, and its personal
// org's and default group's, derive from the email: the same email created on
// two cluster nodes at once is one user.
func (s *Store) CreateUser(ctx context.Context, email, name, passwordHash string, admin bool) (*User, error) {
	email = NormalizeEmail(email)
	u := &User{ID: derivedID("user", email), Email: email, Name: name, PasswordHash: passwordHash, Admin: admin, CreatedAt: unix(now())}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO users (`+userCols+`) VALUES (?, ?, ?, ?, ?, 0, ?)`,
			u.ID, u.Email, u.Name, u.PasswordHash, u.Admin, u.CreatedAt.Unix()); err != nil {
			if isUnique(err) {
				return ErrExists
			}
			return err
		}
		// A user deleted and created again finds the personal org it left.
		org := derivedID("org", u.ID)
		var kept int
		if err := tx.QueryRow(`SELECT count(*) FROM orgs WHERE id = ?`, org).Scan(&kept); err != nil {
			return err
		}
		if kept > 0 {
			_, err := tx.Exec(`INSERT INTO members (org_id, user_id, role) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, org, u.ID, RoleOwner)
			return err
		}
		slug, err := freeSlug(tx, slugify(strings.SplitN(u.Email, "@", 2)[0]))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO orgs (id, slug, name, personal, created_at) VALUES (?, ?, ?, 1, ?)`, org, slug, slug, now()); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO members (org_id, user_id, role) VALUES (?, ?, ?)`, org, u.ID, RoleOwner); err != nil {
			return err
		}
		return createGroupTx(tx, derivedID("group", org, "default"), org, "default", "", s.dbType)
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}

// UserByID returns a user.
func (s *Store) UserByID(ctx context.Context, id string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// UserByEmail returns a user.
func (s *Store) UserByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email = ?`, NormalizeEmail(email)))
}

// ListUsers returns every user.
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUser writes name, admin and disabled.
func (s *Store) UpdateUser(ctx context.Context, u *User) error {
	return one(s.db.ExecContext(ctx, `UPDATE users SET name = ?, admin = ?, disabled = ? WHERE id = ?`, u.Name, u.Admin, u.Disabled, u.ID))
}

// SetPassword replaces a user's password hash.
func (s *Store) SetPassword(ctx context.Context, id, hash string) error {
	return one(s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, id))
}

// DeleteUser removes a user with its memberships and tokens. Orgs it owned
// stay, for an admin to delete: they may hold databases.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := one(tx.Exec(`DELETE FROM users WHERE id = ?`, id)); err != nil {
			return err
		}
		for _, q := range []string{
			`DELETE FROM members WHERE user_id = ?`,
			`DELETE FROM platform_tokens WHERE user_id = ?`,
			`DELETE FROM refresh_tokens WHERE user_id = ?`,
		} {
			if _, err := tx.Exec(q, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "org"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

func freeSlug(tx *sql.Tx, base string) (string, error) {
	slug := base
	for i := 2; ; i++ {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM orgs WHERE slug = ?`, slug).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return slug, nil
		}
		slug = base + "-" + strconv.Itoa(i)
	}
}
