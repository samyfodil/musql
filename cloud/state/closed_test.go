package state

import (
	"context"
	"testing"
	"time"
)

// TestAClosedStoreFails: every read and write of a store whose database is
// closed fails, rather than answering empty.
func TestAClosedStoreFails(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u := &User{ID: "u", Email: "u@example.com"}
	s.Close()
	err2 := func(_ any, err error) error { return err }
	err3 := func(_, _ any, err error) error { return err }
	for name, call := range map[string]func() error{
		"AllDatabases":            func() error { return err2(s.AllDatabases(ctx)) },
		"CreateOrg":               func() error { return err2(s.CreateOrg(ctx, "o", "O", "u", "")) },
		"OrgBySlug":               func() error { return err2(s.OrgBySlug(ctx, "o")) },
		"OrgByID":                 func() error { return err2(s.OrgByID(ctx, "o")) },
		"UserOrgs":                func() error { return err2(s.UserOrgs(ctx, "u")) },
		"AllOrgs":                 func() error { return err2(s.AllOrgs(ctx)) },
		"UpdateOrgName":           func() error { return s.UpdateOrgName(ctx, "o", "O") },
		"DeleteOrg":               func() error { return s.DeleteOrg(ctx, "o") },
		"MemberRole":              func() error { return err2(s.MemberRole(ctx, "o", "u")) },
		"Members":                 func() error { return err2(s.Members(ctx, "o")) },
		"SetMember":               func() error { return s.SetMember(ctx, "o", "u", RoleMember) },
		"RemoveMember":            func() error { return s.RemoveMember(ctx, "o", "u") },
		"CountOwners":             func() error { return err2(s.CountOwners(ctx, "o")) },
		"CreateInvite":            func() error { return err2(s.CreateInvite(ctx, "o", "a@example.com", RoleMember)) },
		"OrgInvites":              func() error { return err2(s.OrgInvites(ctx, "o")) },
		"EmailInvites":            func() error { return err2(s.EmailInvites(ctx, "a@example.com")) },
		"AcceptInvite":            func() error { return err2(s.AcceptInvite(ctx, "i", u)) },
		"CreateGroup":             func() error { return err2(s.CreateGroup(ctx, "o", "g", "", "")) },
		"EnsureGroupLocation":     func() error { return s.EnsureGroupLocation(ctx, "fra") },
		"Group":                   func() error { return err2(s.Group(ctx, "o", "g")) },
		"GroupByID":               func() error { return err2(s.GroupByID(ctx, "g")) },
		"Groups":                  func() error { return err2(s.Groups(ctx, "o")) },
		"DeleteGroup":             func() error { return s.DeleteGroup(ctx, "g") },
		"AddLocation":             func() error { return s.AddLocation(ctx, "g", "fra") },
		"RemoveLocation":          func() error { return s.RemoveLocation(ctx, "g", "fra") },
		"BumpGroupTokens":         func() error { return s.BumpGroupTokens(ctx, "g") },
		"CreateDatabase":          func() error { return s.CreateDatabase(ctx, &Database{OrgID: "o", Name: "d", GroupID: "g"}) },
		"Database":                func() error { return err2(s.Database(ctx, "o", "d")) },
		"DatabaseByID":            func() error { return err2(s.DatabaseByID(ctx, "d")) },
		"Databases":               func() error { return err2(s.Databases(ctx, "o", "")) },
		"RenameDatabase":          func() error { return s.RenameDatabase(ctx, "d", "e") },
		"DeleteDatabase":          func() error { return s.DeleteDatabase(ctx, "d") },
		"SetDatabaseFile":         func() error { return s.SetDatabaseFile(ctx, "d", "f") },
		"BumpDatabaseTokens":      func() error { return s.BumpDatabaseTokens(ctx, "d") },
		"CreateUser":              func() error { return err2(s.CreateUser(ctx, "a@example.com", "A", "", false)) },
		"UserByID":                func() error { return err2(s.UserByID(ctx, "u")) },
		"UserByEmail":             func() error { return err2(s.UserByEmail(ctx, "u@example.com")) },
		"ListUsers":               func() error { return err2(s.ListUsers(ctx)) },
		"UpdateUser":              func() error { return s.UpdateUser(ctx, u) },
		"SetPassword":             func() error { return s.SetPassword(ctx, "u", "h") },
		"DeleteUser":              func() error { return s.DeleteUser(ctx, "u") },
		"CreatePlatformToken":     func() error { return err3(s.CreatePlatformToken(ctx, "u", "t")) },
		"PlatformTokenUser":       func() error { return err2(s.PlatformTokenUser(ctx, PlatformTokenPrefix+"x")) },
		"PlatformTokens":          func() error { return err2(s.PlatformTokens(ctx, "u")) },
		"RevokePlatformToken":     func() error { return s.RevokePlatformToken(ctx, "u", "t") },
		"CreateRefreshToken":      func() error { return err2(s.CreateRefreshToken(ctx, RefreshToken{UserID: "u"}, time.Hour)) },
		"UseRefreshToken":         func() error { return err2(s.UseRefreshToken(ctx, "x")) },
		"RevokeRefreshToken":      func() error { return s.RevokeRefreshToken(ctx, "x", "cli") },
		"RevokeUserRefreshTokens": func() error { return s.RevokeUserRefreshTokens(ctx, "u") },
		"SigningKeys":             func() error { return err2(s.SigningKeys(ctx)) },
		"AddSigningKey":           func() error { return s.AddSigningKey(ctx, &SigningKey{KID: "k"}) },
		"RetireSigningKey":        func() error { return s.RetireSigningKey(ctx, "k", time.Now()) },
		"SetSigningKeyMaxExp":     func() error { return s.SetSigningKeyMaxExp(ctx, "k", 1) },
		"DeleteSigningKey":        func() error { return s.DeleteSigningKey(ctx, "k") },
		"PutClient":               func() error { return s.PutClient(ctx, &Client{ID: "c"}) },
		"Client":                  func() error { return err2(s.Client(ctx, "c")) },
		"CreateAuthCode":          func() error { return err2(s.CreateAuthCode(ctx, AuthCode{ClientID: "c"}, time.Hour)) },
		"TakeAuthCode":            func() error { return err2(s.TakeAuthCode(ctx, "x")) },
		"CreateDeviceCode":        func() error { return err2(s.CreateDeviceCode(ctx, "BCDF-GHJK", "cli", "", time.Hour)) },
		"DeviceByUserCode":        func() error { return err2(s.DeviceByUserCode(ctx, "BCDF-GHJK")) },
		"DecideDevice":            func() error { return s.DecideDevice(ctx, "BCDF-GHJK", "u", true) },
		"PollDevice":              func() error { return err2(s.PollDevice(ctx, "x")) },
		"AddUsage":                func() error { return s.AddUsage(ctx, []*UsageHour{{OrgID: "o", DBID: "d", Hour: time.Now()}}, nil) },
		"UsageHours":              func() error { return err2(s.UsageHours(ctx, "o", "", time.Now().Add(-time.Hour), time.Now())) },
		"TopQueries":              func() error { return err2(s.TopQueries(ctx, "d", time.Now().Add(-time.Hour), time.Now(), 10)) },
	} {
		if call() == nil {
			t.Errorf("%s answered on a closed store", name)
		}
	}
}
