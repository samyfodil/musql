// Package server wires musql cloud together: configuration, the management
// API (ConnectRPC), the OIDC provider, Hrana for each hosted database and the
// console's static files, all on one http.Handler.
package server

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/samyfodil/musql/cloud/state"
)

// Config is config.yaml. The command reads the file; this package only
// parses it.
type Config struct {
	Listen    string      `yaml:"listen"`
	PublicURL string      `yaml:"public_url"`
	DataDir   string      `yaml:"data_dir"`
	Admin     AdminConfig `yaml:"admin"`
	Node      NodeConfig  `yaml:"node"`
	Peers     []Peer      `yaml:"peers"`
	AI        AIConfig    `yaml:"ai"`
	Snapshots struct {
		Every string `yaml:"every"`
		Keep  string `yaml:"keep"`
	} `yaml:"snapshots"`
	Console ConsoleConfig `yaml:"console"`
}

// AdminConfig is the admin created, or whose password is reset, at start.
type AdminConfig struct {
	Email        string `yaml:"email"`
	Password     string `yaml:"password"`
	PasswordHash string `yaml:"password_hash"`
}

// NodeConfig names this node and its location.
type NodeConfig struct {
	Name     string `yaml:"name"`
	Location string `yaml:"location"`
}

// Peer is another node of the deployment.
type Peer struct {
	Name     string `yaml:"name"`
	Location string `yaml:"location"`
	URL      string `yaml:"url"`
	Secret   string `yaml:"secret"`
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
	if c.Admin.Email == "" {
		return errors.New("config: admin.email is required")
	}
	if c.Admin.Password == "" && c.Admin.PasswordHash == "" {
		return errors.New("config: admin.password or admin.password_hash is required")
	}
	if c.Admin.PasswordHash != "" {
		if err := state.CheckPasswordHash(c.Admin.PasswordHash); err != nil {
			return errors.New("config: admin.password_hash is not an argon2id hash")
		}
	}
	return nil
}
