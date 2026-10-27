package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cr-zhichen/limen"
)

func runCLI(args ...string) (string, error) {
	cmd := New()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}
func TestConfigCreationPreservesExistingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if _, e := runCLI("--config", p, "config", "init"); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(p, []byte("existing config"), 0600)
	if _, e := runCLI("--config", p, "config", "init"); e == nil {
		t.Fatal("overwrote config")
	}
	got, _ := os.ReadFile(p)
	if string(got) != "existing config" {
		t.Fatal("config lost")
	}
}
func TestPermissionRejectedBeforeCredentialsOrNetwork(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(p, []byte(limen.ConfigExample), 0600)
	output, e := runCLI("--config", p, "exec", "--asset", "test", "--account", "test", "--permission", "unrestricted", "--command", "uname -a")
	if e == nil || !strings.Contains(e.Error(), "permissions.allowed") || strings.Contains(output, "connected") {
		t.Fatal("permission was not rejected first", e, output)
	}
}
func TestSkillInstallIsExplicitAndDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	if _, e := runCLI("skill", "install", "--dir", dir); e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(dir, "limen", "SKILL.md")
	b, e := os.ReadFile(p)
	if e != nil || !strings.Contains(string(b), "limen skill") {
		t.Fatal("missing entrypoint")
	}
	os.WriteFile(p, []byte("custom"), 0644)
	if _, e = runCLI("skill", "install", "--dir", dir); e == nil {
		t.Fatal("overwrote custom skill")
	}
	if _, e = runCLI("skill", "install", "--dir", dir, "--force"); e != nil {
		t.Fatal(e)
	}
	b, _ = os.ReadFile(p)
	if !strings.Contains(string(b), "limen skill") {
		t.Fatal("explicit replacement failed")
	}
}
