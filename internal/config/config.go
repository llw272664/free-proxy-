// Package config loads runtime configuration from FREE_PROXY_-prefixed
// environment variables. It mirrors the semantics of the former Python
// pydantic-settings module so the external env contract is unchanged.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/caarlos0/env/v11"

	"github.com/masteralanlab/free-proxy/internal/naming"
)

// Config holds the values that are not fixed at build time: where this install
// keeps its data, who administers it, and where it listens. Everything else the
// program needs to decide is a constant in tuning.go.
//
// Field env tags omit the FREE_PROXY_ prefix; it is applied globally in Load via
// env.Options.Prefix.
type Config struct {
	Environment string `env:"ENVIRONMENT" envDefault:"development"`
	DataDir     string `env:"DATA_DIR" envDefault:"free_proxy_data"`
	DatabaseURL string `env:"DATABASE_URL"`

	// Identity and listener fields keep their env tags for install-time seeding
	// and for the one-time upgrade import. SQLite is authoritative afterwards,
	// and the web console is where they are changed.
	WebHost             string `env:"WEB_HOST" envDefault:"0.0.0.0"`
	WebPort             int    `env:"WEB_PORT" envDefault:"39527"`
	AdminUsername       string `env:"ADMIN_USERNAME"`
	AdminPassword       string `env:"ADMIN_PASSWORD"`
	AdminSecretPath     string `env:"ADMIN_SECRET_PATH"`
	AllowProcessRestart bool   `env:"ALLOW_PROCESS_RESTART" envDefault:"true"`

	ProxyHost     string `env:"PROXY_HOST" envDefault:"0.0.0.0"`
	ProxyPort     int    `env:"PROXY_PORT" envDefault:"9527"`
	ProxyEnabled  bool   `env:"PROXY_ENABLED" envDefault:"true"`
	ProxyUsername string `env:"PROXY_USERNAME"`
	ProxyPassword string `env:"PROXY_PASSWORD"`

	// Machine-level values intentionally remain environment-backed.
	//
	// TunnelInterface, ProbeDevicePrefix and PolicyRoutingTable all name things
	// in namespaces shared with every other program on the host, and
	// OpenVPNCommand names a binary whose path is the host's business. Their
	// defaults come from internal/naming rather than literals here, so the
	// project has exactly one place that decides what it claims. Leave them
	// unset to accept those defaults; set them to move out of a neighbour's way.
	OpenVPNCommand    string `env:"OPENVPN_COMMAND" envDefault:"openvpn"`
	TunnelInterface   string `env:"TUNNEL_INTERFACE"`
	ProbeDevicePrefix string `env:"PROBE_DEVICE_PREFIX"`

	// UpdateRepo is the GitHub repository the console offers updates from. It
	// is the same variable install.sh reads, so a fork installed from its own
	// repository keeps updating from it. Left empty it falls back to the
	// project default in internal/updater.
	UpdateRepo string `env:"REPO"`

	// PolicyRoutingTable belongs to that same family. TestTunStart/TestTunEnd do
	// not come from the environment — they start at the constants in tuning.go —
	// but finalizeNaming may narrow the range to keep it clear of
	// TunnelInterface, so they are fields rather than constants at the use site.
	PolicyRoutingTable int `env:"POLICY_ROUTING_TABLE"`
	TestTunStart       int
	TestTunEnd         int
}

// Load parses the environment into a Config, applies derived defaults, and
// validates cross-field invariants.
func Load() (*Config, error) {
	loadEnvFile()
	cfg := &Config{}
	if err := env.ParseWithOptions(cfg, env.Options{Prefix: "FREE_PROXY_"}); err != nil {
		return nil, err
	}
	if err := cfg.finalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loadEnvFile merges FREE_PROXY_* keys from the system env file (written by
// `free-proxy install`) into the process environment, so CLI invocations see
// the same configuration as the service. Variables already set win; the file
// path can be overridden with FREE_PROXY_ENV_FILE.
func loadEnvFile() {
	path := os.Getenv("FREE_PROXY_ENV_FILE")
	if path == "" {
		path = "/etc/free-proxy/free-proxy.env"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if !strings.HasPrefix(key, "FREE_PROXY_") {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, strings.Trim(strings.TrimSpace(value), `"'`))
		}
	}
}

func (c *Config) finalize() error {
	if err := c.finalizeNaming(); err != nil {
		return err
	}
	if (c.ProxyUsername == "") != (c.ProxyPassword == "") {
		return errors.New("FREE_PROXY_PROXY_USERNAME and FREE_PROXY_PROXY_PASSWORD must be configured together")
	}
	abs, err := filepath.Abs(expandUser(c.DataDir))
	if err != nil {
		return fmt.Errorf("resolve data dir: %w", err)
	}
	c.DataDir = abs
	if c.DatabaseURL == "" {
		dbPath := filepath.Join(c.DataDir, "free-proxy.db")
		c.DatabaseURL = fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)", dbPath)
	}
	return nil
}

// finalizeNaming resolves and validates every identifier this process places
// into a host-global namespace. Anything left empty/zero takes the project
// default from internal/naming; anything set by the operator is validated here
// so a bad value fails at startup instead of inside an OpenVPN log line.
func (c *Config) finalizeNaming() error {
	c.TestTunStart, c.TestTunEnd = ProbeDeviceRangeStart, ProbeDeviceRangeEnd
	if c.ProbeDevicePrefix == "" {
		c.ProbeDevicePrefix = naming.DevicePrefix
	}
	if err := naming.ValidateDevicePrefix(c.ProbeDevicePrefix); err != nil {
		return fmt.Errorf("FREE_PROXY_PROBE_DEVICE_PREFIX: %w", err)
	}
	if c.TunnelInterface == "" {
		c.TunnelInterface = naming.ActiveDevice()
	}
	if err := naming.ValidateDeviceName(c.TunnelInterface); err != nil {
		return fmt.Errorf("FREE_PROXY_TUNNEL_INTERFACE: %w", err)
	}
	if c.PolicyRoutingTable == 0 {
		c.PolicyRoutingTable = naming.DefaultRoutingTable
	}
	if err := naming.ValidateRoutingTable(c.PolicyRoutingTable); err != nil {
		return fmt.Errorf("FREE_PROXY_POLICY_ROUTING_TABLE: %w", err)
	}
	// The active tunnel's device must never fall inside the probe pool, or a
	// probe would be handed the name the live exit is already using. The range
	// is ours, so it yields: an operator who moved TunnelInterface onto a probe
	// index gets the pool narrowed rather than a startup failure.
	if idx, ok := probeIndex(c.TunnelInterface, c.ProbeDevicePrefix); ok && idx >= c.TestTunStart && idx <= c.TestTunEnd {
		c.TestTunStart = idx + 1
		if c.TestTunStart > c.TestTunEnd {
			return fmt.Errorf("FREE_PROXY_TUNNEL_INTERFACE %q leaves no probe device below %s%d",
				c.TunnelInterface, c.ProbeDevicePrefix, ProbeDeviceRangeEnd)
		}
	}
	return nil
}

// probeIndex reports the pool index of device when it belongs to prefix.
func probeIndex(device, prefix string) (int, bool) {
	if !naming.HasDevicePrefix(device, prefix) {
		return 0, false
	}
	idx, err := strconv.Atoi(device[len(prefix):])
	if err != nil {
		return 0, false
	}
	return idx, true
}

// EnsureDirectories creates the data directory tree used at runtime.
func (c *Config) EnsureDirectories() error {
	for _, dir := range []string{c.DataDir, c.ConfigsDir(), c.LogsDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) ConfigsDir() string { return filepath.Join(c.DataDir, "configs") }
func (c *Config) LogsDir() string    { return filepath.Join(c.DataDir, "logs") }

func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
