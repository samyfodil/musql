package state

import (
	"context"
	"database/sql"
	"regexp"
	"time"
)

// Role is a member's role in an org.
type Role string

const (
	RoleViewer Role = "viewer"
	RoleMember Role = "member"
	RoleAdmin  Role = "admin"
	RoleOwner  Role = "owner"
)

// Rank orders roles; 0 is no role.
func (r Role) Rank() int {
	switch r {
	case RoleViewer:
		return 1
	case RoleMember:
		return 2
	case RoleAdmin:
		return 3
	case RoleOwner:
		return 4
	}
	return 0
}

// AtLeast reports whether r grants min.
func (r Role) AtLeast(min Role) bool { return r.Rank() > 0 && r.Rank() >= min.Rank() }

// Org is an organization.
type Org struct {
	ID        string
	Slug      string
	Name      string
	Personal  bool
	CreatedAt time.Time
}

// ValidName is the rule for slugs, group and database names: they appear in
// URLs and file names.
var ValidName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

const orgCols = `id, slug, name, personal, created_at`

func scanOrg(r scanner) (*Org, error) {
	var o Org
	var created int64
	if err := r.Scan(&o.ID, &o.Slug, &o.Name, &o.Personal, &created); err != nil {
		return nil, notFound(err)
	}
	o.CreatedAt = unix(created)
	return &o, nil
}

// CreateOrg creates an org with owner as its owner and a default group.
func (s *Store) CreateOrg(ctx context.Context, slug, name, owner, location string) (*Org, error) {
	o := &Org{ID: NewID(), Slug: slug, Name: name, CreatedAt: unix(now())}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO orgs (`+orgCols+`) VALUES (?, ?, ?, 0, ?)`, o.ID, o.Slug, o.Name, o.CreatedAt.Unix()); err != nil {
			if isUnique(err) {
				return ErrExists
			}
			return err
		}
		if _, err := tx.Exec(`INSERT INTO members (org_id, user_id, role) VALUES (?, ?, ?)`, o.ID, owner, RoleOwner); err != nil {
			return err
		}
		return createGroupTx(tx, NewID(), o.ID, "default", location, s.dbType)
	})
	if err != nil {
		return nil, err
	}
	return o, nil
}

// OrgBySlug returns an org.
func (s *Store) OrgBySlug(ctx context.Context, slug string) (*Org, error) {
	return scanOrg(s.db.QueryRowContext(ctx, `SELECT `+orgCols+` FROM orgs WHERE slug = ?`, slug))
}

// OrgByID returns an org.
func (s *Store) OrgByID(ctx context.Context, id string) (*Org, error) {
	return scanOrg(s.db.QueryRowContext(ctx, `SELECT `+orgCols+` FROM orgs WHERE id = ?`, id))
}

// Membership is an org and the user's role in it.
type Membership struct {
	Org  *Org
	Role Role
}

// UserOrgs returns the orgs user belongs to.
func (s *Store) UserOrgs(ctx context.Context, userID string) ([]Membership, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT o.id, o.slug, o.name, o.personal, o.created_at, m.role
		FROM orgs o JOIN members m ON m.org_id = o.id WHERE m.user_id = ? ORDER BY o.slug`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Membership
	for rows.Next() {
		var o Org
		var created int64
		var role string
		if err := rows.Scan(&o.ID, &o.Slug, &o.Name, &o.Personal, &created, &role); err != nil {
			return nil, err
		}
		o.CreatedAt = unix(created)
		out = append(out, Membership{Org: &o, Role: Role(role)})
	}
	return out, rows.Err()
}

// AllOrgs returns every org.
func (s *Store) AllOrgs(ctx context.Context) ([]*Org, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+orgCols+` FROM orgs ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Org
	for rows.Next() {
		o, err := scanOrg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// UpdateOrgName renames an org's display name.
func (s *Store) UpdateOrgName(ctx context.Context, id, name string) error {
	return one(s.db.ExecContext(ctx, `UPDATE orgs SET name = ? WHERE id = ?`, name, id))
}

// DeleteOrg removes an org and its records; the caller removes database
// files first.
func (s *Store) DeleteOrg(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := one(tx.Exec(`DELETE FROM orgs WHERE id = ?`, id)); err != nil {
			return err
		}
		for _, q := range []string{
			`DELETE FROM members WHERE org_id = ?`,
			`DELETE FROM invites WHERE org_id = ?`,
			`DELETE FROM group_locations WHERE group_id IN (SELECT id FROM groups WHERE org_id = ?)`,
			`DELETE FROM groups WHERE org_id = ?`,
			`DELETE FROM databases WHERE org_id = ?`,
		} {
			if _, err := tx.Exec(q, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// MemberRole is user's role in org, or "" when not a member.
func (s *Store) MemberRole(ctx context.Context, orgID, userID string) (Role, error) {
	var role string
	err := s.db.QueryRowContext(ctx, `SELECT role FROM members WHERE org_id = ? AND user_id = ?`, orgID, userID).Scan(&role)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return Role(role), err
}

// Member is a user and its role.
type Member struct {
	User *User
	Role Role
}

// Members lists an org's members.
func (s *Store) Members(ctx context.Context, orgID string) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.id, u.email, u.name, u.password_hash, u.admin, u.disabled, u.created_at, m.role
		FROM users u JOIN members m ON m.user_id = u.id WHERE m.org_id = ? ORDER BY u.email`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var u User
		var created int64
		var role string
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Admin, &u.Disabled, &created, &role); err != nil {
			return nil, err
		}
		u.CreatedAt = unix(created)
		out = append(out, Member{User: &u, Role: Role(role)})
	}
	return out, rows.Err()
}

// SetMember adds user to org with role, or changes its role.
func (s *Store) SetMember(ctx context.Context, orgID, userID string, role Role) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE members SET role = ? WHERE org_id = ? AND user_id = ?`, role, orgID, userID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n > 0 {
			return err
		}
		_, err = tx.Exec(`INSERT INTO members (org_id, user_id, role) VALUES (?, ?, ?)`, orgID, userID, role)
		return err
	})
}

// RemoveMember removes user from org.
func (s *Store) RemoveMember(ctx context.Context, orgID, userID string) error {
	return one(s.db.ExecContext(ctx, `DELETE FROM members WHERE org_id = ? AND user_id = ?`, orgID, userID))
}

// CountOwners counts an org's owners.
func (s *Store) CountOwners(ctx context.Context, orgID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM members WHERE org_id = ? AND role = ?`, orgID, RoleOwner).Scan(&n)
	return n, err
}

// Invite is a pending membership for an email.
type Invite struct {
	ID        string
	OrgID     string
	OrgSlug   string
	Email     string
	Role      Role
	CreatedAt time.Time
}

// CreateInvite invites email to org with role.
func (s *Store) CreateInvite(ctx context.Context, orgID, email string, role Role) (*Invite, error) {
	inv := &Invite{ID: NewID(), OrgID: orgID, Email: NormalizeEmail(email), Role: role, CreatedAt: unix(now())}
	_, err := s.db.ExecContext(ctx, `INSERT INTO invites (id, org_id, email, role, created_at) VALUES (?, ?, ?, ?, ?)`,
		inv.ID, inv.OrgID, inv.Email, inv.Role, inv.CreatedAt.Unix())
	if isUnique(err) {
		return nil, ErrExists
	}
	return inv, err
}

func (s *Store) invites(ctx context.Context, where string, arg string) ([]*Invite, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.id, i.org_id, o.slug, i.email, i.role, i.created_at
		FROM invites i JOIN orgs o ON o.id = i.org_id WHERE `+where+` ORDER BY i.created_at`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Invite
	for rows.Next() {
		var inv Invite
		var created int64
		var role string
		if err := rows.Scan(&inv.ID, &inv.OrgID, &inv.OrgSlug, &inv.Email, &role, &created); err != nil {
			return nil, err
		}
		inv.Role, inv.CreatedAt = Role(role), unix(created)
		out = append(out, &inv)
	}
	return out, rows.Err()
}

// OrgInvites lists an org's pending invites.
func (s *Store) OrgInvites(ctx context.Context, orgID string) ([]*Invite, error) {
	return s.invites(ctx, "i.org_id = ?", orgID)
}

// EmailInvites lists the invites pending for an email.
func (s *Store) EmailInvites(ctx context.Context, email string) ([]*Invite, error) {
	return s.invites(ctx, "i.email = ?", NormalizeEmail(email))
}

// AcceptInvite turns the invite into a membership for user, whose email must
// be the invited one.
func (s *Store) AcceptInvite(ctx context.Context, id string, u *User) (*Invite, error) {
	var inv Invite
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var role string
		if err := tx.QueryRow(`SELECT org_id, email, role FROM invites WHERE id = ?`, id).Scan(&inv.OrgID, &inv.Email, &role); err != nil {
			return notFound(err)
		}
		if inv.Email != u.Email {
			return ErrNotFound
		}
		inv.ID, inv.Role = id, Role(role)
		if _, err := tx.Exec(`DELETE FROM invites WHERE id = ?`, id); err != nil {
			return err
		}
		var have string
		err := tx.QueryRow(`SELECT role FROM members WHERE org_id = ? AND user_id = ?`, inv.OrgID, u.ID).Scan(&have)
		switch {
		case err == sql.ErrNoRows:
			_, err = tx.Exec(`INSERT INTO members (org_id, user_id, role) VALUES (?, ?, ?)`, inv.OrgID, u.ID, inv.Role)
			return err
		case err != nil:
			return err
		case inv.Role.Rank() > Role(have).Rank():
			_, err = tx.Exec(`UPDATE members SET role = ? WHERE org_id = ? AND user_id = ?`, inv.Role, inv.OrgID, u.ID)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &inv, nil
}
