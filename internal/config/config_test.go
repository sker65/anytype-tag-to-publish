package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadWithEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(dir, "anypub.yaml")
	yaml := `
anytype:
  api_key_file: ` + keyFile + `
spaces:
  - id: space1
publish:
  base_url: https://pub.example.com
`
	if err := os.WriteFile(cfgFile, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANYPUB_PUBLISH_DEBOUNCE", "2s")
	t.Setenv("ANYPUB_PUBLISH_WORKERS", "7")
	t.Setenv("ANYPUB_PUBLISH_NOINDEX", "false")

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Anytype.APIKey != "secret" {
		t.Errorf("api key = %q", cfg.Anytype.APIKey)
	}
	if cfg.Publish.Debounce != 2*time.Second || cfg.Publish.Workers != 7 || cfg.Publish.NoIndex {
		t.Errorf("env overrides not applied: %+v", cfg.Publish)
	}
	if cfg.Spaces[0].TagName() != "publish" {
		t.Errorf("default tag = %q", cfg.Spaces[0].TagName())
	}
}

func TestValidateRejectsEmptySpacesAndZK(t *testing.T) {
	cfg := Default()
	cfg.Anytype.APIKey = "k"
	cfg.Publish.BaseURL = "https://pub.example.com"
	cfg.Publish.DefaultMode = "zk"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for empty spaces and zk mode")
	}
}

func TestSpacesEnv(t *testing.T) {
	cfg := Default()
	env := map[string]string{"ANYPUB_SPACES": "a, b:share"}
	if err := applyEnv(&cfg, func(k string) (string, bool) { v, ok := env[k]; return v, ok }); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Spaces) != 2 || cfg.Spaces[1].ID != "b" || cfg.Spaces[1].Tag != "share" {
		t.Fatalf("spaces = %+v", cfg.Spaces)
	}
}
