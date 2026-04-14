package config

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

const (
	DefaultAPIURL = "https://app.opencomputer.dev"
	configDir     = ".oc"
	configFile    = "config.json"
)

// Config holds the resolved CLI configuration.
type Config struct {
	APIURL         string `json:"api_url,omitempty"`
	APIKey         string `json:"api_key,omitempty"`
	SessionsAPIURL string `json:"sessions_api_url,omitempty"`
}

// Load resolves configuration from flags > env > config file > defaults.
func Load(cmd *cobra.Command) Config {
	cfg := loadFile()

	// Apply defaults
	if cfg.APIURL == "" {
		cfg.APIURL = DefaultAPIURL
	}

	// Env overrides file
	if v := os.Getenv("OPENCOMPUTER_API_URL"); v != "" {
		cfg.APIURL = v
	}
	if v := os.Getenv("OPENCOMPUTER_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("SESSIONS_API_URL"); v != "" {
		cfg.SessionsAPIURL = v
	}

	// Flags override env
	if cmd != nil {
		if v, _ := cmd.Flags().GetString("api-url"); v != "" {
			cfg.APIURL = v
		}
		if v, _ := cmd.Flags().GetString("api-key"); v != "" {
			cfg.APIKey = v
		}
		if v, _ := cmd.Flags().GetString("sessions-api-url"); v != "" {
			cfg.SessionsAPIURL = v
		}
	}

	return cfg
}

// ConfigPath returns the path to the config file.
func ConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, configDir, configFile)
}

func loadFile() Config {
	var cfg Config
	data, err := os.ReadFile(ConfigPath())
	if err != nil {
		return cfg
	}
	json.Unmarshal(data, &cfg)
	return cfg
}

// Save writes the config to disk.
func Save(cfg Config) error {
	dir := filepath.Dir(ConfigPath())
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ConfigPath(), data, 0600)
}
