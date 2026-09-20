// Package config resolves boot configuration from flags and environment variables.
// Precedence: explicit flag > environment variable (LLM_SWITCH_*) > default.
package config

import (
	"flag"
	"os"
	"path/filepath"
	"strconv"
)

// Config holds everything resolved at boot. All remaining runtime settings
// (retention, failover attempts, defaults) live in the SQLite settings table
// and are editable through the admin API without restarts.
type Config struct {
	// Addr is the HTTP listen address (host:port).
	Addr string
	// DataDir holds the SQLite database file. Permissions are tightened to 0700.
	DataDir string
	// DBPath is the resolved SQLite file path (DataDir/llm-switch.db).
	DBPath string
	// AdminPassword seeds the admin password on first boot only. Empty means
	// "generate a random password and log it once".
	AdminPassword string
	// LogFormat is "text" or "json" for the structured logger.
	LogFormat string
	// WebDev serves the UI from the Vite dev server instead of the embedded
	// assets (frontend development mode).
	WebDev bool
	// Daemon re-executes the process detached in the background, logging to
	// <DataDir>/llm-switch.log.
	Daemon bool
	// Version is stamped at build time.
	Version string
}

// Load parses flags and environment variables into a Config.
func Load(version string) *Config {
	cfg := &Config{Version: version}

	flag.StringVar(&cfg.Addr, "addr", envOr("LLM_SWITCH_ADDR", ":8901"), "HTTP listen address")
	flag.StringVar(&cfg.DataDir, "data-dir", envOr("LLM_SWITCH_DATA_DIR", defaultDataDir()), "data directory for the SQLite database")
	flag.StringVar(&cfg.AdminPassword, "admin-password", os.Getenv("LLM_SWITCH_ADMIN_PASSWORD"), "seed admin password on first boot (default: random, logged once)")
	flag.StringVar(&cfg.LogFormat, "log-format", envOr("LLM_SWITCH_LOG_FORMAT", "text"), "log format: text|json")
	flag.BoolVar(&cfg.WebDev, "web-dev", boolEnv("LLM_SWITCH_WEB_DEV", false), "serve UI from the Vite dev server (frontend development)")
	flag.BoolVar(&cfg.Daemon, "daemon", boolEnv("LLM_SWITCH_DAEMON", false), "run detached in the background (logs to <data-dir>/llm-switch.log)")
	flag.Parse()

	cfg.DBPath = filepath.Join(cfg.DataDir, "llm-switch.db")
	return cfg
}

// defaultDataDir resolves the per-user data directory (~/.llm-switch),
// falling back to ./data when the home directory is undeterminable.
func defaultDataDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".llm-switch")
	}
	return "./data"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func boolEnv(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return fallback
}
