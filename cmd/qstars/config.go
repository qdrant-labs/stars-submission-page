package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// config is what `qstars login` saves: the server and the session token for it.
type config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

var errNotLoggedIn = errors.New("not logged in: run `qstars login --server <url>`")

// configPath honors QSTARS_CONFIG, then falls back to the user config directory.
func configPath() (string, error) {
	if p := os.Getenv("QSTARS_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "qstars", "config.json"), nil
}

func loadConfig() (*config, error) {
	p, err := configPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNotLoggedIn
	}
	if err != nil {
		return nil, err
	}
	var c config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("could not read %s: %w", p, err)
	}
	if c.Server == "" || c.Token == "" {
		return nil, errNotLoggedIn
	}
	return &c, nil
}

// saveConfig writes the file readable by the owner only, since it holds a credential.
func saveConfig(c *config) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func deleteConfig() error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
