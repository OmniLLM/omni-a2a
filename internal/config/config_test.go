package config

import (
	"os"
	"path/filepath"
	"testing"
)

const legacyYAML = `
server:
  host: "0.0.0.0"
  port: 8222
  api_key: "client-key"
  admin_key: "admin-key"
  public_url: "http://localhost:8222"

hub:
  name: "test hub"

agent:
  name: "Legacy Hermes"
  command: "hermes"

upstream:
  - name: hermes
    url: http://localhost:1424
    token: legacy-token
    enabled: true
`

const newYAML = `
server:
  host: "0.0.0.0"
  port: 8222
  api_key: "client-key"
  admin_key: "admin-key"
  public_url: "http://localhost:8222"

hub:
  name: "hub"

upstream:
  - name: research
    base_url: http://localhost:8003
    auth: { scheme: none }
    enabled: true
`

func writeTemp(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

func TestLoad_LegacyMigration(t *testing.T) {
	path := writeTemp(t, legacyYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Upstream) != 1 {
		t.Fatalf("upstream count: got %d want 1", len(cfg.Upstream))
	}
	u := cfg.Upstream[0]
	if u.BaseURL != "http://localhost:1424" {
		t.Errorf("BaseURL not migrated: %q", u.BaseURL)
	}
	if u.Auth.Scheme != "bearer" || u.Auth.Token != "legacy-token" {
		t.Errorf("token not migrated to Auth: %+v", u.Auth)
	}
	if u.LegacyURL != "" || u.LegacyToken != "" {
		t.Errorf("legacy fields not cleared: url=%q token=%q", u.LegacyURL, u.LegacyToken)
	}
}

func TestLoad_NewShape(t *testing.T) {
	path := writeTemp(t, newYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Upstream) != 1 {
		t.Fatalf("upstream count: got %d want 1", len(cfg.Upstream))
	}
	u := cfg.Upstream[0]
	if u.BaseURL != "http://localhost:8003" {
		t.Errorf("BaseURL: %q", u.BaseURL)
	}
	if u.Auth.Scheme != "none" {
		t.Errorf("scheme: %q", u.Auth.Scheme)
	}
}

func TestValidate(t *testing.T) {
	good := &Config{
		Server: ServerConfig{
			Host: "0.0.0.0", Port: 8222,
			PublicURL: "http://x", APIKey: "a", AdminKey: "b",
		},
		Upstream: []UpstreamCfg{
			{Name: "u1", BaseURL: "http://u1", Auth: AuthConfig{Scheme: "bearer", Token: "t"}},
		},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := []struct {
		name  string
		mut   func(c *Config)
		wants string
	}{
		{"port", func(c *Config) { c.Server.Port = 0 }, "port"},
		{"public_url", func(c *Config) { c.Server.PublicURL = "" }, "public_url"},
		{"api_key", func(c *Config) { c.Server.APIKey = "" }, "api_key"},
		{"dup", func(c *Config) {
			c.Upstream = append(c.Upstream, UpstreamCfg{Name: "u1", BaseURL: "http://x"})
		}, "duplicate"},
		{"no url", func(c *Config) {
			c.Upstream[0].BaseURL = ""
		}, "base_url"},
		{"bad scheme", func(c *Config) {
			c.Upstream[0].Auth.Scheme = "digest"
		}, "auth.scheme"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := *good
			cfg.Upstream = append([]UpstreamCfg(nil), good.Upstream...)
			tc.mut(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q; got nil", tc.wants)
			}
		})
	}
}

func TestExpandPath(t *testing.T) {
	got := ExpandPath("~/foo/bar")
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "foo/bar")
	if got != want {
		t.Errorf("ExpandPath(~/foo/bar) = %q, want %q", got, want)
	}
	if ExpandPath("/abs/path") != "/abs/path" {
		t.Errorf("absolute path unchanged")
	}
}
