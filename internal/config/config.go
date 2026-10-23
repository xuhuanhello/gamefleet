// Package config loads and validates the GameFleet configuration file.
package config

import (
	"bytes"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Config is the whole configuration file. Clusters, API keys, builds,
// deployments, routes and game server settings are not here: they live in
// the database and change from the console.
type Config struct {
	Listen string `json:"listen"`
	// Database is the path of GameFleet's SQLite file.
	Database string   `json:"database"`
	Registry Registry `json:"registry"`
	// DrainRetention is how long a deployment that no route uses any more is
	// kept, scaled to zero, so that a rollback is instant. Default 24h.
	DrainRetention Duration `json:"drainRetention"`
	Users          []User   `json:"users"`
}

// Registry is the image registry behind GameFleet.
type Registry struct {
	// URL is where GameFleet reaches the registry itself, for example
	// http://registry:5000. GameFleet checks API keys in front of it.
	URL string `json:"url"`
	// Host is the public name clusters pull images from, for example
	// fleet.example.com.
	Host string `json:"host"`
}

// Duration is a time.Duration written like "24h" or "90m".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// User is a console account. It signs in with a password and a TOTP code.
// Create one with "gamefleet user NAME", which prints this entry.
type User struct {
	Name         string `json:"name"`
	PasswordHash string `json:"passwordHash"`
	TOTPSecret   string `json:"totpSecret"`
}

// Validate checks one user entry.
func (u User) Validate() error {
	var errs []error
	if !dnsLabel.MatchString(u.Name) {
		errs = append(errs, errors.New("name must be lowercase letters, digits and dashes"))
	}
	if !passwordHash.MatchString(u.PasswordHash) {
		errs = append(errs, errors.New("passwordHash is not a hash made by \"gamefleet user\""))
	}
	if secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(u.TOTPSecret); err != nil || len(secret) < 16 {
		errs = append(errs, errors.New("totpSecret must be base32 without padding, at least 16 bytes"))
	}
	return errors.Join(errs...)
}

var (
	// dnsLabel matches Kubernetes-style names: lowercase letters, digits and dashes.
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// passwordHash matches what "gamefleet user" writes: PBKDF2-SHA256 with
	// the iteration count, salt and key.
	hostName     = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?$`)
	passwordHash = regexp.MustCompile(`^pbkdf2-sha256\$[1-9][0-9]{4,}\$[A-Za-z0-9+/]{16,}\$[A-Za-z0-9+/]{43}$`)
)

// Load reads, parses and validates the configuration file. Unknown fields are
// rejected so that a typo cannot silently disable a setting.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.DrainRetention.Duration == 0 {
		c.DrainRetention.Duration = 24 * time.Hour
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid %s: %w", path, err)
	}
	return c, nil
}

// Validate reports every problem in the configuration at once.
func (c Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Listen == "" {
		fail("listen is required")
	}

	if !strings.HasPrefix(c.Database, "/") {
		fail("database must be an absolute path")
	}
	if u, err := url.Parse(c.Registry.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
		fail("registry.url must be an http:// or https:// URL without a path")
	}
	if !hostName.MatchString(c.Registry.Host) {
		fail("registry.host must be a host name such as fleet.example.com")
	}
	if c.DrainRetention.Duration < time.Minute {
		fail("drainRetention must be at least 1m")
	}

	userNames := map[string]bool{}
	for i, u := range c.Users {
		if userNames[u.Name] {
			fail("users[%d]: name %q is used twice", i, u.Name)
		}
		userNames[u.Name] = true
		if err := u.Validate(); err != nil {
			fail("users[%d]: %w", i, err)
		}
	}

	return errors.Join(errs...)
}
