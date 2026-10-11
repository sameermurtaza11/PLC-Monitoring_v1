// Package config reads runtime settings from environment variables.
//
// Only infrastructure settings live here (DB URL, HTTP port).
// PLC and PV configuration lives in PostgreSQL — never here.
package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL    string        // DATABASE_URL
	HTTPAddr       string        // HTTP_ADDR
	ConfigReload   time.Duration // CONFIG_RELOAD — how often acquisition re-reads plc/pv metadata
	AcquireEnabled bool          // ACQUISITION — "off" runs the web UI only

	// Login session cookie. Secure must be true when served over HTTPS.
	CookieSecure   bool          // COOKIE_SECURE   — "true" for HTTPS deployments (default false: local HTTP works)
	CookieSameSite string        // COOKIE_SAMESITE — "lax" (default) or "strict"
	SessionHours   time.Duration // SESSION_HOURS   — how long a login lasts (default 12)
}

func Load() Config {
	// A .env file next to the executable is convenient on Windows.
	// Real environment variables always win over .env values.
	loadDotEnv(".env")

	return Config{
		DatabaseURL:    env("DATABASE_URL", "postgres://postgres@localhost:5432/plc_monitoring"),
		HTTPAddr:       env("HTTP_ADDR", ":8080"),
		ConfigReload:   envDuration("CONFIG_RELOAD", 10*time.Second),
		AcquireEnabled: env("ACQUISITION", "on") != "off",
		CookieSecure:   strings.EqualFold(env("COOKIE_SECURE", "false"), "true"),
		CookieSameSite: strings.ToLower(env("COOKIE_SAMESITE", "lax")),
		SessionHours:   time.Duration(envInt("SESSION_HOURS", 12)) * time.Hour,
	}
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	n, err := strconv.Atoi(env(key, ""))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(env(key, ""))
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// loadDotEnv sets KEY=VALUE lines from path unless the variable is already set.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // no .env is fine
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}
