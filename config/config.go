// Package config handles configuration loading from YAML files and connection strings.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Mode    string        `yaml:"mode"`
	TeamKey string        `yaml:"teamKey"`
	Client  *ClientConfig `yaml:"client,omitempty"`
	Server  *ServerConfig `yaml:"server,omitempty"`
}

type ClientConfig struct {
	ServerAddr    string `yaml:"serverAddr"`
	BindInterface string `yaml:"bindInterface,omitempty"` // single-NIC mode (Windows)
	DedupWindow   int    `yaml:"dedupWindow,omitempty"`
	WebUI         string `yaml:"webUI,omitempty"` // unused in v2 client, kept for YAML compat
}

type ServerConfig struct {
	ListenAddr    string `yaml:"listenAddr"`
	VirtualIP     string `yaml:"virtualIP"`
	Subnet        string `yaml:"subnet"`
	ClientTimeout int    `yaml:"clientTimeout"`
	AddrTimeout   int    `yaml:"addrTimeout"`
	DedupWindow   int    `yaml:"dedupWindow,omitempty"`
	IPPoolFile    string `yaml:"ipPoolFile,omitempty"`
	WebUI         string `yaml:"webUI,omitempty"`
}

// LoadFile reads a YAML config from disk.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	applyDefaults(&cfg)
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// FromConnString parses "mpfpv://teamKey@host:port" into a client Config.
func FromConnString(s string) (*Config, error) {
	teamKey, serverAddr, err := parseConnString(s)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		Mode:    "client",
		TeamKey: teamKey,
		Client: &ClientConfig{
			ServerAddr:  serverAddr,
			DedupWindow: 4096,
		},
	}
	return cfg, nil
}

func parseConnString(s string) (teamKey, serverAddr string, err error) {
	// mpfpv://teamKey@host:port
	if strings.HasPrefix(s, "mpfpv://") {
		u, e := url.Parse(s)
		if e != nil {
			return "", "", fmt.Errorf("config: invalid connection string: %w", e)
		}
		teamKey = u.User.Username()
		serverAddr = u.Host
		if serverAddr == "" {
			return "", "", fmt.Errorf("config: missing host in connection string")
		}
		return teamKey, serverAddr, nil
	}
	// host:port or host:port/teamKey
	if idx := strings.LastIndex(s, "/"); idx > 0 {
		serverAddr = s[:idx]
		teamKey = s[idx+1:]
	} else {
		serverAddr = s
	}
	if _, _, e := net.SplitHostPort(serverAddr); e != nil {
		return "", "", fmt.Errorf("config: invalid server address %q: %w", serverAddr, e)
	}
	return teamKey, serverAddr, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Client != nil {
		if cfg.Client.DedupWindow <= 0 {
			cfg.Client.DedupWindow = 4096
		}
	}
	if cfg.Server != nil {
		if cfg.Server.ClientTimeout <= 0 {
			cfg.Server.ClientTimeout = 30
		}
		if cfg.Server.AddrTimeout <= 0 {
			cfg.Server.AddrTimeout = 15
		}
		if cfg.Server.DedupWindow <= 0 {
			cfg.Server.DedupWindow = 4096
		}
		if cfg.Server.IPPoolFile == "" {
			cfg.Server.IPPoolFile = "ip_pool.json"
		}
	}
}

func validate(cfg *Config) error {
	if cfg.Mode != "client" && cfg.Mode != "server" {
		return fmt.Errorf("config: mode must be client or server")
	}
	switch cfg.Mode {
	case "client":
		if cfg.Client == nil {
			return fmt.Errorf("config: client section required")
		}
		if cfg.Client.ServerAddr == "" {
			return fmt.Errorf("config: client.serverAddr required")
		}
	case "server":
		if cfg.Server == nil {
			return fmt.Errorf("config: server section required")
		}
		if cfg.Server.ListenAddr == "" {
			return fmt.Errorf("config: server.listenAddr required")
		}
		if cfg.Server.VirtualIP == "" {
			return fmt.Errorf("config: server.virtualIP required")
		}
		if cfg.Server.Subnet == "" {
			return fmt.Errorf("config: server.subnet required")
		}
	}
	return nil
}

// SaveFile writes config to YAML.
func SaveFile(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
