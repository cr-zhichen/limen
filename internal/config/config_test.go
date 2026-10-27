package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cr-zhichen/limen"
)

func TestConfigurationModesAndTimeouts(t *testing.T) {
	tests := []struct {
		name, body string
		valid      bool
	}{
		{"full", limen.ConfigExample, true},
		{"zero disables", strings.ReplaceAll(limen.ConfigExample, "connect_timeout = 15", "connect_timeout = 0"), true},
		{"negative", strings.ReplaceAll(limen.ConfigExample, "command_timeout = 300", "command_timeout = -1"), false},
		{"unknown mode", strings.ReplaceAll(limen.ConfigExample, `mode = "hybrid"`, `mode = "auto"`), false},
		{"unknown field", strings.ReplaceAll(limen.ConfigExample, `ssh_port = 2222`, `ssh_port = 2222`+"\norg_id = 'old'"), false},
		{"unconfigured model", strings.Split(limen.ConfigExample, "[review]")[0], false},
		{"unrestricted only", strings.ReplaceAll(strings.Split(limen.ConfigExample, "[review]")[0], `"read-only"`, `"unrestricted"`), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.toml")
			os.WriteFile(p, []byte(tt.body), 0600)
			c, e := Load(p)
			if (e == nil) != tt.valid {
				t.Fatalf("valid=%v err=%v", tt.valid, e)
			}
			if tt.name == "zero disables" && c.Session.ConnectTimeout != 0 {
				t.Fatal("zero overridden")
			}
		})
	}
}
func TestSecretSourcesAndCaching(t *testing.T) {
	t.Setenv("LIMEN_TEST_KEY", "first")
	s := Secret{Env: "LIMEN_TEST_KEY"}
	first, e := s.Resolve(context.Background())
	if e != nil || first != "first" {
		t.Fatal(first, e)
	}
	t.Setenv("LIMEN_TEST_KEY", "second")
	again, e := s.Resolve(context.Background())
	if e != nil || again != "first" {
		t.Fatal("credential was not cached")
	}
	for _, s := range []*Secret{{}, {Value: "one", Env: "TWO"}, {Env: "LIMEN_TEST_MISSING"}} {
		if _, e = s.Resolve(context.Background()); e == nil {
			t.Fatal("invalid source accepted")
		}
	}
}
func TestSecretCommand(t *testing.T) {
	if os.Getenv("LIMEN_CREDENTIAL_HELPER") == "1" {
		os.Stdout.WriteString("credential\r\n")
		os.Exit(0)
	}
	t.Setenv("LIMEN_CREDENTIAL_HELPER", "1")
	exe, _ := os.Executable()
	s := Secret{Command: []string{exe, "-test.run=^TestSecretCommand$"}}
	value, e := s.Resolve(context.Background())
	if e != nil || value != "credential" {
		t.Fatal("command source failed", e)
	}
}
