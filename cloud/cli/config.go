// Package cli is the musql command line: the cloud's API, sign-in and a SQL
// shell over Hrana. The command in cmd/musql supplies everything that comes
// from the environment (the config directory, standard streams, a browser).
package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Config is what the CLI keeps between runs, in <dir>/config.json.
type Config struct {
	URL          string    `json:"url,omitempty"`
	Org          string    `json:"org,omitempty"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitzero"`
}

const configFile = "config.json"

// LoadConfig reads dir's config; a missing file is an empty config.
func LoadConfig(dir string) (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(filepath.Join(dir, configFile))
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	return c, nil
}

// Save writes the config readable by its owner only: it holds credentials.
func (c *Config) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, configFile+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, configFile))
}
