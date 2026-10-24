// Package server wires musql cloud together: configuration, the management
// API (ConnectRPC), the OIDC provider, Hrana for each hosted database and the
// console's static files, all on one http.Handler.
package server

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/samyfodil/musql/cloud/state"
)

// Config is config.yaml. The command reads the file; this package only
// parses it.
type Config struct {
	Listen    string     `yaml:"listen"`
	PublicURL string     `yaml:"public_url"`
	DataDir   string     `yaml:"data_dir"`
	Node      NodeConfig `yaml:"node"`
	Peers     []Peer     `yaml:"peers"`
	AI        AIConfig   `yaml:"ai"`
	Snapshots struct {
		Every string `yaml:"every"`
		Keep  string `yaml:"keep"`
	} `yaml:"snapshots"`
	Console ConsoleConfig `yaml:"console"`
	OIDC    OIDCConfig    `yaml:"oidc"`
	Uploads UploadsConfig `yaml:"uploads"`

	// snapshots.every and snapshots.keep, parsed; 0 turns each off.
	snapshotEvery, snapshotKeep time.Duration
	// usageFlush is how often counted usage is written to the state.
	usageFlush time.Duration
	// syncInterval is how often a cluster node runs anti-entropy and
	// placement.
	syncInterval time.Duration
}

// OIDCConfig tunes sign-in. Durations take Go syntax plus "d" for days
// ("30d").
type OIDCConfig struct {
	// KeyRotation is how long a signing key signs before a new one takes
	// over (default 30d); "0" turns rotation off.
	KeyRotation string `yaml:"key_rotation"`
	// LoginPerIP and LoginPerEmail are the failed sign-ins one address and
	// one email may make within LoginWindow before /login answers 429
	// (defaults 20, 5 and 15m); a negative count turns that limit off.
	LoginPerIP    int    `yaml:"login_per_ip"`
	LoginPerEmail int    `yaml:"login_per_email"`
	LoginWindow   string `yaml:"login_window"`

	keyRotation time.Duration
	loginWindow time.Duration
}

// UploadsConfig bounds the files clients send to create a database from.
type UploadsConfig struct {
	// MaxBytes is the largest file one upload may carry (default 1 GiB). The
	// file is kept whole on disk while it is used: converting a C SQLite one
	// needs about as much room again.
	MaxBytes int64 `yaml:"max_bytes"`
	// TTL is how long an upload that nothing writes to or uses is kept before
	// it is removed (default 24h).
	TTL string `yaml:"ttl"`

	ttl time.Duration
}

// parseDuration is time.ParseDuration with a "d" suffix for days.
func parseDuration(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.ParseFloat(n, 64)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return d, nil
}

// NodeConfig names this node and its location; P2PListen is where a cluster
// node listens for its peers.
type NodeConfig struct {
	Name      string   `yaml:"name"`
	Location  string   `yaml:"location"`
	P2PListen []string `yaml:"p2p_listen"`
}

// Peer is another node of a cluster: its libp2p peer ID and the multiaddrs
// to dial it at.
type Peer struct {
	Name     string   `yaml:"name"`
	Location string   `yaml:"location"`
	Addrs    []string `yaml:"addrs"`
	PeerID   string   `yaml:"peer_id"`
}

// Clustered reports a cluster node: one with peers.
func (c *Config) Clustered() bool { return len(c.Peers) > 0 }

// checkPeers checks that every node has its own name and location, and every
// peer an ID and an address.
func (c *Config) checkPeers() error {
	names := map[string]bool{c.Node.Name: true}
	locs := map[string]bool{c.Node.Location: true}
	for _, p := range c.Peers {
		if p.Name == "" || names[p.Name] {
			return fmt.Errorf("config: peer %q: every node needs its own name", p.Name)
		}
		if locs[p.Location] || !state.ValidName.MatchString(p.Location) {
			return fmt.Errorf("config: peer %s: location %q is taken or invalid", p.Name, p.Location)
		}
		if p.PeerID == "" || len(p.Addrs) == 0 {
			return fmt.Errorf("config: peer %s: peer_id and addrs are required", p.Name)
		}
		names[p.Name], locs[p.Location] = true, true
	}
	if c.Clustered() && len(c.Node.P2PListen) == 0 {
		c.Node.P2PListen = []string{"/ip4/0.0.0.0/tcp/4001"}
	}
	return nil
}

// AIConfig configures AI SQL; an empty Provider turns it off.
type AIConfig struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	APIKey   string `yaml:"api_key"`
	BaseURL  string `yaml:"base_url"`
	JITLLM   struct {
		ModelsDir string `yaml:"models_dir"`
		Embedded  *bool  `yaml:"embedded"`
	} `yaml:"jitllm"`
}

// ConsoleConfig is where the console's build is and which other origins may
// call the API and Hrana from a browser.
type ConsoleConfig struct {
	Dir         string   `yaml:"dir"`
	CORSOrigins []string `yaml:"cors_origins"`
}

// ParseConfig parses config.yaml, fills defaults and checks it.
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if strings.Contains(err.Error(), "field admin not found") {
			return nil, errors.New("config: the admin block is no longer read: remove it and run musql-cloud init --admin-email <email>")
		}
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, c.normalize()
}

func (c *Config) normalize() error {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.Node.Name == "" {
		c.Node.Name = "node-1"
	}
	if c.Node.Location == "" {
		c.Node.Location = "local"
	}
	if !state.ValidName.MatchString(c.Node.Location) {
		return fmt.Errorf("config: node.location %q: lower-case letters, digits and dashes", c.Node.Location)
	}
	if err := c.checkPeers(); err != nil {
		return err
	}
	if c.PublicURL == "" {
		host := c.Listen
		if strings.HasPrefix(host, ":") {
			host = "localhost" + host
		}
		c.PublicURL = "http://" + host
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	u, err := url.Parse(c.PublicURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" {
		return fmt.Errorf("config: public_url %q is not an http(s) URL", c.PublicURL)
	}
	for _, d := range []struct {
		name, val, def string
		out            *time.Duration
	}{{"snapshots.every", c.Snapshots.Every, "1h", &c.snapshotEvery}, {"snapshots.keep", c.Snapshots.Keep, "168h", &c.snapshotKeep}} {
		v := d.val
		if v == "" {
			v = d.def
		}
		dur, err := time.ParseDuration(v)
		if err != nil || dur < 0 {
			return fmt.Errorf("config: %s %q is not a duration", d.name, d.val)
		}
		*d.out = dur
	}
	c.usageFlush = time.Minute
	c.syncInterval = 2 * time.Second
	if c.OIDC.keyRotation, err = durationOr(c.OIDC.KeyRotation, 30*24*time.Hour); err != nil {
		return fmt.Errorf("config: oidc.key_rotation: %w", err)
	}
	if c.OIDC.loginWindow, err = durationOr(c.OIDC.LoginWindow, 0); err != nil {
		return fmt.Errorf("config: oidc.login_window: %w", err)
	}
	switch {
	case c.Uploads.MaxBytes < 0:
		return fmt.Errorf("config: uploads.max_bytes %d is negative", c.Uploads.MaxBytes)
	case c.Uploads.MaxBytes == 0:
		c.Uploads.MaxBytes = 1 << 30
	}
	if c.Uploads.ttl, err = durationOr(c.Uploads.TTL, 24*time.Hour); err != nil || c.Uploads.ttl == 0 {
		return fmt.Errorf("config: uploads.ttl %q is not a positive duration", c.Uploads.TTL)
	}
	return nil
}

func durationOr(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	return parseDuration(s)
}
