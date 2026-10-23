package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validConfig() Config {
	return Config{
		Listen:         "127.0.0.1:8080",
		Database:       "/var/lib/gamefleet/gamefleet.db",
		Registry:       Registry{URL: "http://registry:5000", Host: "fleet.example.com"},
		DrainRetention: Duration{24 * time.Hour},
		Users: []User{{
			Name:         "admin",
			PasswordHash: "pbkdf2-sha256$600000$c2FsdHNhbHRzYWx0c2FsdA$" + strings.Repeat("A", 43),
			TOTPSecret:   strings.Repeat("A", 32),
		}},
	}
}

func TestValidConfig(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsMistakes(t *testing.T) {
	cases := map[string]func(*Config){
		"missing listen":     func(c *Config) { c.Listen = "" },
		"relative database":  func(c *Config) { c.Database = "gamefleet.db" },
		"registry with path": func(c *Config) { c.Registry.URL = "http://registry:5000/v2" },
		"registry host url":  func(c *Config) { c.Registry.Host = "https://fleet.example.com" },
		"tiny retention":     func(c *Config) { c.DrainRetention.Duration = time.Second },
		"duplicate user":     func(c *Config) { c.Users = append(c.Users, c.Users[0]) },
		"plain password":     func(c *Config) { c.Users[0].PasswordHash = "hunter2" },
		"short totp secret":  func(c *Config) { c.Users[0].TOTPSecret = "AAAA" },
		"padded totp secret": func(c *Config) { c.Users[0].TOTPSecret = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listen": ":8080", "lisen": ":9090"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "lisen") {
		t.Fatalf("expected an unknown field error, got %v", err)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	if _, err := Load(filepath.Join("..", "..", "deploy", "config.example.json")); err != nil {
		t.Fatal(err)
	}
}

func TestDrainRetentionDefaultsToADay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"listen": ":8080", "database": "/data/gamefleet.db",
		"registry": {"url": "http://registry:5000", "host": "fleet.example.com"}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.DrainRetention.Duration != 24*time.Hour {
		t.Fatalf("drainRetention = %v", c.DrainRetention)
	}
}
