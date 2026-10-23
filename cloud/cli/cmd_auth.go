package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

func (a *app) authCmd() *cobra.Command {
	var headless bool
	login := &cobra.Command{
		Use:   "login",
		Short: "Sign in with the browser (device flow)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			srv, err := a.server()
			if err != nil {
				return err
			}
			d, err := StartDevice(ctx, a.env.HTTP, srv)
			if err != nil {
				return err
			}
			link := d.VerificationURIComplete
			if link == "" {
				link = d.VerificationURI
			}
			fmt.Fprintf(a.env.Stderr, "Open %s\nand confirm the code %s\n", link, d.UserCode)
			if !headless && a.env.OpenBrowser != nil {
				a.env.OpenBrowser(link) // best effort: the link is printed above
			}
			t, err := PollDevice(ctx, a.env.HTTP, srv, d)
			if err != nil {
				return err
			}
			a.cfg.URL = srv
			a.cfg.setTokens(t)
			if err := a.cfg.Save(a.env.Dir); err != nil {
				return err
			}
			if err := a.clients(); err != nil {
				return err
			}
			who, err := a.services.auth.WhoAmI(ctx, connect.NewRequest(&v1.WhoAmIRequest{}))
			if err != nil {
				return err
			}
			fmt.Fprintf(a.env.Stderr, "Signed in as %s\n", who.Msg.User.Email)
			return nil
		},
	}
	login.Flags().BoolVar(&headless, "headless", false, "do not open a browser")

	logout := &cobra.Command{
		Use:   "logout",
		Short: "Forget the stored sign-in",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a.cfg.AccessToken, a.cfg.RefreshToken = "", ""
			a.cfg.Expiry = time.Time{}
			return a.cfg.Save(a.env.Dir)
		},
	}

	whoami := a.cmd("whoami", "Show the signed-in user and their organizations", cobra.NoArgs, func(ctx context.Context, args []string) error {
		who, err := a.services.auth.WhoAmI(ctx, connect.NewRequest(&v1.WhoAmIRequest{}))
		if err != nil {
			return err
		}
		if a.json {
			return writeJSON(a.env.Stdout, who.Msg)
		}
		u := who.Msg.User
		admin := ""
		if u.Admin {
			admin = " (server admin)"
		}
		a.printf("%s%s\n", u.Email, admin)
		var orgs []string
		for _, m := range who.Msg.Memberships {
			orgs = append(orgs, m.Org.Slug+" ("+roleName(m.Role)+")")
		}
		if len(orgs) > 0 {
			a.printf("orgs: %s\n", strings.Join(orgs, ", "))
		}
		return nil
	})

	token := &cobra.Command{
		Use:   "token",
		Short: "Print the current access token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.token(cmd.Context())
			if err != nil {
				return err
			}
			a.printf("%s\n", t)
			return nil
		},
	}

	listTokens := a.cmd("list", "List API tokens", cobra.NoArgs, func(ctx context.Context, args []string) error {
		r, err := a.services.auth.ListPlatformTokens(ctx, connect.NewRequest(&v1.ListPlatformTokensRequest{}))
		if err != nil {
			return err
		}
		var rows [][]string
		for _, t := range r.Msg.Tokens {
			rows = append(rows, []string{t.Id, t.Name, when(t.CreatedAt), when(t.LastUsedAt)})
		}
		return a.list(r.Msg, []string{"ID", "NAME", "CREATED", "LAST USED"}, rows)
	})
	mint := a.cmd("mint <name>", "Create an API token (printed once)", cobra.ExactArgs(1), func(ctx context.Context, args []string) error {
		r, err := a.services.auth.CreatePlatformToken(ctx, connect.NewRequest(&v1.CreatePlatformTokenRequest{Name: args[0]}))
		if err != nil {
			return err
		}
		if a.json {
			return writeJSON(a.env.Stdout, r.Msg)
		}
		a.printf("%s\n", r.Msg.Secret)
		return nil
	})
	revoke := a.cmd("revoke <id>", "Revoke an API token", cobra.ExactArgs(1), func(ctx context.Context, args []string) error {
		_, err := a.services.auth.RevokePlatformToken(ctx, connect.NewRequest(&v1.RevokePlatformTokenRequest{Id: args[0]}))
		return err
	})

	return group("auth", "Sign in and manage tokens", login, logout, whoami, token,
		group("api-tokens", "Manage API tokens for CI and scripts", listTokens, mint, revoke))
}

func (a *app) configCmd() *cobra.Command {
	set := &cobra.Command{
		Use:   "set <url|org> <value>",
		Short: "Set the server URL or the current organization",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "url":
				v := strings.TrimRight(args[1], "/")
				if v != a.cfg.URL {
					// Tokens belong to the server that issued them.
					a.cfg.AccessToken, a.cfg.RefreshToken, a.cfg.Org = "", "", ""
				}
				a.cfg.URL = v
			case "org":
				a.cfg.Org = args[1]
			default:
				return fmt.Errorf("unknown setting %q (url or org)", args[0])
			}
			return a.cfg.Save(a.env.Dir)
		},
	}
	show := &cobra.Command{
		Use:   "show",
		Short: "Show the configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			signedIn := "no"
			if a.cfg.RefreshToken != "" {
				signedIn = "yes"
			}
			a.printf("dir:       %s\nurl:       %s\norg:       %s\nsigned in: %s\n", a.env.Dir, a.cfg.URL, a.cfg.Org, signedIn)
			return nil
		},
	}
	return group("config", "Show or change the CLI's settings", set, show)
}

func (a *app) orgCmd() *cobra.Command {
	list := a.cmd("list", "List your organizations", cobra.NoArgs, func(ctx context.Context, args []string) error {
		r, err := a.services.orgs.List(ctx, connect.NewRequest(&v1.ListOrgsRequest{}))
		if err != nil {
			return err
		}
		current, _ := a.orgSlug(ctx) // no current org is not an error for a listing
		var rows [][]string
		for _, o := range r.Msg.Orgs {
			mark := ""
			if o.Slug == current {
				mark = "*"
			}
			rows = append(rows, []string{o.Slug + mark, o.Name, fmt.Sprint(o.Personal)})
		}
		return a.list(r.Msg, []string{"SLUG", "NAME", "PERSONAL"}, rows)
	})
	var name string
	create := a.cmd("create <slug>", "Create an organization", cobra.ExactArgs(1), func(ctx context.Context, args []string) error {
		if name == "" {
			name = args[0]
		}
		r, err := a.services.orgs.Create(ctx, connect.NewRequest(&v1.CreateOrgRequest{Slug: args[0], Name: name}))
		if err != nil {
			return err
		}
		a.printf("Created organization %s\n", r.Msg.Org.Slug)
		return nil
	})
	create.Flags().StringVar(&name, "name", "", "display name (default: the slug)")
	sw := a.cmd("switch <slug>", "Make an organization the current one", cobra.ExactArgs(1), func(ctx context.Context, args []string) error {
		r, err := a.services.orgs.List(ctx, connect.NewRequest(&v1.ListOrgsRequest{}))
		if err != nil {
			return err
		}
		for _, o := range r.Msg.Orgs {
			if o.Slug == args[0] {
				a.cfg.Org = o.Slug
				if err := a.cfg.Save(a.env.Dir); err != nil {
					return err
				}
				a.printf("Current organization: %s\n", o.Slug)
				return nil
			}
		}
		return fmt.Errorf("no organization %q", args[0])
	})

	members := a.cmd("list", "List members", cobra.NoArgs, func(ctx context.Context, args []string) error {
		org, err := a.orgSlug(ctx)
		if err != nil {
			return err
		}
		r, err := a.services.orgs.ListMembers(ctx, connect.NewRequest(&v1.ListMembersRequest{Org: org}))
		if err != nil {
			return err
		}
		var rows [][]string
		for _, m := range r.Msg.Members {
			rows = append(rows, []string{m.User.Email, m.User.Name, roleName(m.Role)})
		}
		return a.list(r.Msg, []string{"EMAIL", "NAME", "ROLE"}, rows)
	})
	var role string
	invite := a.cmd("invite <email>", "Invite someone by email", cobra.ExactArgs(1), func(ctx context.Context, args []string) error {
		org, err := a.orgSlug(ctx)
		if err != nil {
			return err
		}
		rl, err := parseRole(role)
		if err != nil {
			return err
		}
		if _, err := a.services.orgs.Invite(ctx, connect.NewRequest(&v1.InviteRequest{Org: org, Email: args[0], Role: rl})); err != nil {
			return err
		}
		a.printf("Invited %s to %s as %s\n", args[0], org, role)
		return nil
	})
	invite.Flags().StringVar(&role, "role", "member", "viewer, member, admin or owner")
	rm := a.cmd("rm <email>", "Remove a member", cobra.ExactArgs(1), func(ctx context.Context, args []string) error {
		org, id, err := a.member(ctx, args[0])
		if err != nil {
			return err
		}
		_, err = a.services.orgs.RemoveMember(ctx, connect.NewRequest(&v1.RemoveMemberRequest{Org: org, UserId: id}))
		return err
	})
	setRole := a.cmd("role <email> <role>", "Change a member's role", cobra.ExactArgs(2), func(ctx context.Context, args []string) error {
		rl, err := parseRole(args[1])
		if err != nil {
			return err
		}
		org, id, err := a.member(ctx, args[0])
		if err != nil {
			return err
		}
		_, err = a.services.orgs.UpdateMember(ctx, connect.NewRequest(&v1.UpdateMemberRequest{Org: org, UserId: id, Role: rl}))
		return err
	})
	return group("org", "Manage organizations", list, create, sw,
		group("members", "Manage an organization's members", members, invite, rm, setRole))
}

// member finds a member of the current org by email.
func (a *app) member(ctx context.Context, email string) (org, id string, err error) {
	if org, err = a.orgSlug(ctx); err != nil {
		return "", "", err
	}
	r, err := a.services.orgs.ListMembers(ctx, connect.NewRequest(&v1.ListMembersRequest{Org: org}))
	if err != nil {
		return "", "", err
	}
	for _, m := range r.Msg.Members {
		if strings.EqualFold(m.User.Email, email) || m.User.Id == email {
			return org, m.User.Id, nil
		}
	}
	return "", "", fmt.Errorf("%s is not a member of %s", email, org)
}

func (a *app) groupCmd() *cobra.Command {
	list := a.cmd("list", "List groups", cobra.NoArgs, func(ctx context.Context, args []string) error {
		org, err := a.orgSlug(ctx)
		if err != nil {
			return err
		}
		r, err := a.services.group.List(ctx, connect.NewRequest(&v1.ListGroupsRequest{Org: org}))
		if err != nil {
			return err
		}
		var rows [][]string
		for _, g := range r.Msg.Groups {
			rows = append(rows, []string{g.Name, g.Primary, strings.Join(g.Locations, ", ")})
		}
		return a.list(r.Msg, []string{"NAME", "PRIMARY", "LOCATIONS"}, rows)
	})
	var location string
	create := a.cmd("create <name>", "Create a group", cobra.ExactArgs(1), func(ctx context.Context, args []string) error {
		org, err := a.orgSlug(ctx)
		if err != nil {
			return err
		}
		r, err := a.services.group.Create(ctx, connect.NewRequest(&v1.CreateGroupRequest{Org: org, Name: args[0], Location: location}))
		if err != nil {
			return err
		}
		a.printf("Created group %s in %s\n", r.Msg.Group.Name, r.Msg.Group.Primary)
		return nil
	})
	create.Flags().StringVar(&location, "location", "", "primary location (default: the server's)")
	var yes bool
	destroy := a.cmd("destroy <name>", "Delete a group", cobra.ExactArgs(1), func(ctx context.Context, args []string) error {
		org, err := a.orgSlug(ctx)
		if err != nil {
			return err
		}
		if !a.confirm(yes, "Delete group "+args[0]+"?") {
			return fmt.Errorf("not deleted")
		}
		_, err = a.services.group.Delete(ctx, connect.NewRequest(&v1.DeleteGroupRequest{Org: org, Name: args[0]}))
		return err
	})
	destroy.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	locs := a.cmd("list", "List the locations this server offers", cobra.NoArgs, func(ctx context.Context, args []string) error {
		r, err := a.services.group.ListLocations(ctx, connect.NewRequest(&v1.ListLocationsRequest{}))
		if err != nil {
			return err
		}
		var rows [][]string
		for _, l := range r.Msg.Locations {
			rows = append(rows, []string{l.Code, l.Node})
		}
		return a.list(r.Msg, []string{"CODE", "NODE"}, rows)
	})
	add := a.cmd("add <group> <location>...", "Add locations to a group", cobra.MinimumNArgs(2), func(ctx context.Context, args []string) error {
		org, err := a.orgSlug(ctx)
		if err != nil {
			return err
		}
		for _, l := range args[1:] {
			if _, err := a.services.group.AddLocation(ctx, connect.NewRequest(&v1.AddLocationRequest{Org: org, Group: args[0], Location: l})); err != nil {
				return err
			}
		}
		return nil
	})
	rm := a.cmd("rm <group> <location>...", "Remove locations from a group", cobra.MinimumNArgs(2), func(ctx context.Context, args []string) error {
		org, err := a.orgSlug(ctx)
		if err != nil {
			return err
		}
		for _, l := range args[1:] {
			if _, err := a.services.group.RemoveLocation(ctx, connect.NewRequest(&v1.RemoveLocationRequest{Org: org, Group: args[0], Location: l})); err != nil {
				return err
			}
		}
		return nil
	})
	return group("group", "Manage groups and their locations", list, create, destroy,
		group("locations", "Manage locations", locs, add, rm))
}
