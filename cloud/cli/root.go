package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
)

// Env is everything the CLI takes from outside: the command fills it from
// the process, a test from its fixtures.
type Env struct {
	Dir    string // config directory, e.g. ~/.config/musql
	Token  string // a platform or access token that overrides the stored sign-in
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Interactive reports whether Stdin is a terminal.
	Interactive bool
	// StderrIsTerminal reports whether Stderr is a terminal, where a transfer's
	// progress redraws one line instead of printing a line per step.
	StderrIsTerminal bool
	// StdinFd is Stdin's descriptor when Interactive, for raw-mode line editing.
	StdinFd int
	// OpenBrowser opens a URL for the user; nil or an error means print only.
	OpenBrowser func(string) error
	HTTP        *http.Client
}

var errNotSignedIn = errors.New("not signed in: run musql auth login")

type app struct {
	env      Env
	cfg      *Config
	url      string // --url
	org      string // --org
	json     bool
	cached   string // resolved org
	services struct {
		auth  cloudv1connect.AuthServiceClient
		orgs  cloudv1connect.OrgServiceClient
		group cloudv1connect.GroupServiceClient
		db    cloudv1connect.DatabaseServiceClient
	}
}

// Run executes the CLI with args (without the program name).
func Run(ctx context.Context, env Env, args []string) error {
	if env.HTTP == nil {
		// No overall timeout: a download streams for as long as the file takes,
		// and creating a database from a large upload can run for minutes.
		env.HTTP = &http.Client{}
	}
	a := &app{env: env}
	root := a.root()
	root.SetArgs(args)
	root.SetIn(env.Stdin)
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	return explain(root.ExecuteContext(ctx))
}

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:           "musql",
		Short:         "Manage musql cloud databases",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig(a.env.Dir)
			if err != nil {
				return err
			}
			a.cfg = cfg
			return nil
		},
	}
	root.PersistentFlags().StringVar(&a.url, "url", "", "server URL (default: the configured one)")
	root.PersistentFlags().StringVar(&a.org, "org", "", "organization slug (default: the current one)")
	root.PersistentFlags().BoolVar(&a.json, "json", false, "print JSON")
	root.AddCommand(a.authCmd(), a.configCmd(), a.orgCmd(), a.groupCmd(), a.dbCmd())
	return root
}

// explain turns API errors into a line a person can act on.
func explain(err error) error {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return err
	}
	switch ce.Code() {
	case connect.CodeUnimplemented:
		return errors.New("this server does not support that yet")
	case connect.CodeUnauthenticated:
		return errNotSignedIn
	}
	return errors.New(ce.Message())
}

func (a *app) server() (string, error) {
	u := a.url
	if u == "" {
		u = a.cfg.URL
	}
	if u == "" {
		return "", errors.New("no server URL: pass --url or run musql config set url <url>")
	}
	return strings.TrimRight(u, "/"), nil
}

// token is the bearer for API calls, refreshed (and saved) when it is about
// to expire.
func (a *app) token(ctx context.Context) (string, error) {
	if a.env.Token != "" {
		return a.env.Token, nil
	}
	c := a.cfg
	if c.AccessToken != "" && time.Until(c.Expiry) > 30*time.Second {
		return c.AccessToken, nil
	}
	if c.RefreshToken == "" {
		return "", errNotSignedIn
	}
	srv, err := a.server()
	if err != nil {
		return "", err
	}
	t, err := refresh(ctx, a.env.HTTP, srv, c.RefreshToken)
	if err != nil {
		return "", err
	}
	c.setTokens(t)
	if err := c.Save(a.env.Dir); err != nil {
		return "", err
	}
	return c.AccessToken, nil
}

func (a *app) clients() error {
	if a.services.db != nil {
		return nil
	}
	srv, err := a.server()
	if err != nil {
		return err
	}
	auth := connect.WithInterceptors(bearer{a})
	hc := a.env.HTTP
	a.services.auth = cloudv1connect.NewAuthServiceClient(hc, srv, auth)
	a.services.orgs = cloudv1connect.NewOrgServiceClient(hc, srv, auth)
	a.services.group = cloudv1connect.NewGroupServiceClient(hc, srv, auth)
	a.services.db = cloudv1connect.NewDatabaseServiceClient(hc, srv, auth)
	return nil
}

// bearer puts the signed-in token on every call, unary or streaming.
type bearer struct{ a *app }

func (b bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		tok, err := b.a.token(ctx)
		if err != nil {
			return nil, err
		}
		req.Header().Set("Authorization", "Bearer "+tok)
		return next(ctx, req)
	}
}

func (b bearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		tok, err := b.a.token(ctx)
		if err != nil {
			return failedConn{conn, err}
		}
		conn.RequestHeader().Set("Authorization", "Bearer "+tok)
		return conn
	}
}

func (bearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// failedConn is a call that could not be authorized: it fails when it is used.
type failedConn struct {
	connect.StreamingClientConn
	err error
}

func (c failedConn) Send(any) error    { return c.err }
func (c failedConn) Receive(any) error { return c.err }

// orgSlug is --org, else the switched-to org, else the user's personal org.
func (a *app) orgSlug(ctx context.Context) (string, error) {
	switch {
	case a.org != "":
		return a.org, nil
	case a.cfg.Org != "":
		return a.cfg.Org, nil
	case a.cached != "":
		return a.cached, nil
	}
	who, err := a.services.auth.WhoAmI(ctx, connect.NewRequest(&v1.WhoAmIRequest{}))
	if err != nil {
		return "", err
	}
	for _, m := range who.Msg.Memberships {
		if m.Org.GetPersonal() {
			a.cached = m.Org.Slug
			return a.cached, nil
		}
	}
	if len(who.Msg.Memberships) > 0 {
		a.cached = who.Msg.Memberships[0].Org.Slug
		return a.cached, nil
	}
	return "", errors.New("you are in no organization: pass --org")
}

// cmd builds a subcommand whose run gets the API clients and a context.
func (a *app) cmd(use, short string, args cobra.PositionalArgs, run func(ctx context.Context, args []string) error) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  args,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.clients(); err != nil {
				return err
			}
			return run(cmd.Context(), args)
		},
	}
}

func group(use, short string, subs ...*cobra.Command) *cobra.Command {
	c := &cobra.Command{Use: use, Short: short}
	c.AddCommand(subs...)
	return c
}

func (a *app) printf(format string, args ...any) {
	fmt.Fprintf(a.env.Stdout, format, args...)
}

// confirm asks a yes/no question unless yes is already set.
func (a *app) confirm(yes bool, question string) bool {
	if yes {
		return true
	}
	fmt.Fprintf(a.env.Stderr, "%s [y/N] ", question)
	var line string
	fmt.Fscanln(a.env.Stdin, &line)
	return strings.EqualFold(line, "y") || strings.EqualFold(line, "yes")
}
