package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfigFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestLoadHappyPath(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "main.tchori.json", `{
  "providers": {
    "null": {
      "source": "opentofu/null",
      "version": "3.2.4",
      "config": {"region": {"env": "TCHORI_REGION"}}
    }
  },
  "resources": {
    "null_resource.demo": {
      "config": {"triggers": {"greeting": "hello"}}
    },
    "null_resource.pinned": {
      "provider": "null",
      "config": {}
    }
  }
}`)

	cfg, ds := Load(dir)
	if ds.HasErrors() {
		t.Fatalf("unexpected error diagnostics: %+v", ds)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}

	p, ok := cfg.Providers["null"]
	if !ok {
		t.Fatalf("provider %q missing; got %+v", "null", cfg.Providers)
	}
	if p.Name != "null" || p.Source != "opentofu/null" || p.Version != "3.2.4" {
		t.Errorf("provider fields wrong: %+v", p)
	}
	if _, ok := p.Config["region"]; !ok {
		t.Errorf("provider config not carried through raw: %+v", p.Config)
	}

	r, ok := cfg.Resources["null_resource.demo"]
	if !ok {
		t.Fatalf("resource null_resource.demo missing; got %+v", cfg.Resources)
	}
	if r.Address != "null_resource.demo" || r.Type != "null_resource" || r.Name != "demo" {
		t.Errorf("resource identity fields wrong: %+v", r)
	}
	if r.Provider != "null" {
		t.Errorf("prefix-based provider resolution: got %q, want %q", r.Provider, "null")
	}
	if got := cfg.Resources["null_resource.pinned"].Provider; got != "null" {
		t.Errorf("explicit provider resolution: got %q, want %q", got, "null")
	}
	triggers, ok := r.Config["triggers"].(map[string]any)
	if !ok || triggers["greeting"] != "hello" {
		t.Errorf("resource config not carried through raw: %+v", r.Config)
	}
}

func TestLoadDuplicateResourceAddressAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "a.tchori.json", `{
  "providers": {"null": {"source": "opentofu/null", "version": "3.2.4"}},
  "resources": {"null_resource.demo": {"config": {}}}
}`)
	writeConfigFile(t, dir, "b.tchori.json", `{
  "resources": {"null_resource.demo": {"config": {}}}
}`)

	cfg, ds := Load(dir)
	if cfg != nil {
		t.Fatalf("expected nil config on duplicate address, got %+v", cfg)
	}
	if !ds.HasErrors() {
		t.Fatal("expected error diagnostics for duplicate resource address")
	}
	namesBothFiles := false
	for _, d := range ds {
		if strings.Contains(d.Detail, "a.tchori.json") && strings.Contains(d.Detail, "b.tchori.json") {
			namesBothFiles = true
		}
	}
	if !namesBothFiles {
		t.Fatalf("no diagnostic names both declaring files: %+v", ds)
	}
}

func TestLoadUnknownTopLevelKey(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "bad.tchori.json", `{
  "providers": {"null": {"source": "opentofu/null", "version": "3.2.4"}},
  "outputs": {}
}`)

	cfg, ds := Load(dir)
	if cfg != nil {
		t.Fatalf("expected nil config on schema violation, got %+v", cfg)
	}
	if !ds.HasErrors() {
		t.Fatal("expected error diagnostics for unknown top-level key")
	}
	namesFile := false
	for _, d := range ds {
		if strings.Contains(d.Detail, "bad.tchori.json") {
			namesFile = true
		}
	}
	if !namesFile {
		t.Fatalf("no diagnostic names the offending file: %+v", ds)
	}
}

func TestLoadMissingProvider(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "main.tchori.json", `{
  "resources": {"aws_instance.web": {"config": {}}}
}`)

	cfg, ds := Load(dir)
	if cfg != nil {
		t.Fatalf("expected nil config when provider is missing, got %+v", cfg)
	}
	if !ds.HasErrors() {
		t.Fatal("expected error diagnostics for missing provider")
	}
	found := false
	for _, d := range ds {
		if d.Address == "aws_instance.web" && strings.Contains(d.Summary, `"aws"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no diagnostic for aws_instance.web naming provider %q: %+v", "aws", ds)
	}
}

func TestLoadConstraintVersionRejected(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "main.tchori.json", `{
  "providers": {"null": {"source": "opentofu/null", "version": "~> 0.1"}},
  "resources": {"null_resource.demo": {"config": {}}}
}`)

	cfg, ds := Load(dir)
	if cfg != nil {
		t.Fatalf("expected nil config for constraint version syntax, got %+v", cfg)
	}
	if !ds.HasErrors() {
		t.Fatal("expected error diagnostics for constraint version syntax")
	}
	found := false
	for _, d := range ds {
		if strings.Contains(d.Summary, `"~> 0.1"`) && strings.Contains(d.Detail, "exact version required") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no diagnostic rejects version %q with detail mentioning %q: %+v", "~> 0.1", "exact version required", ds)
	}
}
