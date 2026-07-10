// Package config loads, merges, and validates *.tchori.json configuration
// files into the engine's typed configuration model.
package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/tchori-labs/tchori/internal/diag"
)

//go:embed schema.json
var schemaBytes []byte

// versionPattern pins provider versions to exact semver ("3.2.4"). The
// embedded schema carries the same pattern; Load pre-checks with it so
// constraint syntax like "~> 0.1" gets a clearer diagnostic than the
// schema's raw pattern-mismatch error.
var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// Config is the merged configuration across all *.tchori.json files.
type Config struct {
	Providers map[string]*ProviderConfig // key = provider local name
	Resources map[string]*Resource       // key = address "type.name"
}

// ProviderConfig is one entry under the top-level "providers" object.
type ProviderConfig struct {
	Name    string         // local name, map key repeated
	Source  string         // e.g. "opentofu/null" or "tchori-labs/metaads"
	Version string         // exact version string, e.g. "3.2.4"
	Config  map[string]any // raw JSON, env-wrappers unresolved
}

// Resource is one entry under the top-level "resources" object.
type Resource struct {
	Address  string         // "null_resource.demo"
	Type     string         // "null_resource"
	Name     string         // "demo"
	Provider string         // resolved provider local name
	Config   map[string]any // raw JSON, refs and env-wrappers unresolved
}

// fileDoc mirrors the JSON shape of a single *.tchori.json file.
type fileDoc struct {
	Providers map[string]*fileProvider `json:"providers"`
	Resources map[string]*fileResource `json:"resources"`
}

type fileProvider struct {
	Source  string         `json:"source"`
	Version string         `json:"version"`
	Config  map[string]any `json:"config"`
}

type fileResource struct {
	Provider string         `json:"provider"`
	Config   map[string]any `json:"config"`
}

// Load reads and merges all *.tchori.json files in dir (lexical filename
// order), validates against the embedded JSON Schema, resolves each
// resource's Provider, and errors on duplicate provider names or resource
// addresses across files. When any error diagnostic is produced the
// returned *Config is nil.
func Load(dir string) (*Config, diag.Diagnostics) {
	var ds diag.Diagnostics

	paths, err := filepath.Glob(filepath.Join(dir, "*.tchori.json"))
	if err != nil {
		return nil, diag.Diagnostics{diag.Errorf("", "config directory scan failed", err.Error())}
	}
	slices.Sort(paths)
	if len(paths) == 0 {
		return nil, diag.Diagnostics{diag.Errorf("", "no configuration files",
			fmt.Sprintf("no *.tchori.json files found in %s", dir))}
	}

	schema, err := compileSchema()
	if err != nil {
		return nil, diag.Diagnostics{diag.Errorf("", "internal error compiling embedded config schema", err.Error())}
	}

	cfg := &Config{
		Providers: map[string]*ProviderConfig{},
		Resources: map[string]*Resource{},
	}
	providerFile := map[string]string{} // provider local name -> declaring file
	resourceFile := map[string]string{} // resource address -> declaring file

	for _, path := range paths {
		base := filepath.Base(path)

		rawBytes, err := os.ReadFile(path) //nolint:gosec // G304: path comes from filepath.Glob(filepath.Join(dir, "*.tchori.json")), scoped to the operator-provided config directory
		if err != nil {
			ds = append(ds, diag.Errorf("", "cannot read config file", fmt.Sprintf("%s: %s", base, err)))
			continue
		}

		instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawBytes))
		if err != nil {
			ds = append(ds, diag.Errorf("", "invalid JSON in config file", fmt.Sprintf("%s: %s", base, err)))
			continue
		}

		var doc fileDoc
		if err := json.Unmarshal(rawBytes, &doc); err != nil {
			ds = append(ds, diag.Errorf("", "cannot decode config file", fmt.Sprintf("%s: %s", base, err)))
			continue
		}

		// Exact-version pre-check, before schema validation, so constraint
		// syntax like "~> 0.1" gets a diagnostic that says what to do
		// instead of the schema's raw pattern-mismatch error. Missing or
		// empty versions fall through to the schema's own errors.
		badVersion := false
		for _, name := range slices.Sorted(maps.Keys(doc.Providers)) {
			p := doc.Providers[name]
			if p == nil || p.Version == "" || versionPattern.MatchString(p.Version) {
				continue
			}
			ds = append(ds, diag.Errorf("", fmt.Sprintf("invalid provider version %q", p.Version),
				fmt.Sprintf("%s: provider %q: exact version required (e.g. \"3.2.4\"); constraint syntax is not supported in MVP", base, name)))
			badVersion = true
		}
		if badVersion {
			continue
		}

		if err := schema.Validate(instance); err != nil {
			ds = append(ds, diag.Errorf("", "config file violates schema", fmt.Sprintf("%s: %s", base, err)))
			continue
		}

		for _, name := range slices.Sorted(maps.Keys(doc.Providers)) {
			if prev, dup := providerFile[name]; dup {
				ds = append(ds, diag.Errorf("", fmt.Sprintf("duplicate provider %q", name),
					fmt.Sprintf("provider %q is declared in both %s and %s", name, prev, base)))
				continue
			}
			providerFile[name] = base
			p := doc.Providers[name]
			cfg.Providers[name] = &ProviderConfig{
				Name:    name,
				Source:  p.Source,
				Version: p.Version,
				Config:  p.Config,
			}
		}

		for _, addr := range slices.Sorted(maps.Keys(doc.Resources)) {
			if prev, dup := resourceFile[addr]; dup {
				ds = append(ds, diag.Errorf(addr, fmt.Sprintf("duplicate resource address %q", addr),
					fmt.Sprintf("resource %q is declared in both %s and %s", addr, prev, base)))
				continue
			}
			resourceFile[addr] = base
			r := doc.Resources[addr]
			// The schema guarantees exactly one "." in the address.
			typ, rname, _ := strings.Cut(addr, ".")
			cfg.Resources[addr] = &Resource{
				Address:  addr,
				Type:     typ,
				Name:     rname,
				Provider: r.Provider, // resolved below, after all files merge
				Config:   r.Config,
			}
		}
	}

	// Provider resolution runs after the merge so a resource in one file may
	// use a provider declared in another file.
	for _, addr := range slices.Sorted(maps.Keys(cfg.Resources)) {
		r := cfg.Resources[addr]
		name := r.Provider
		if name == "" {
			name = r.Type
			if i := strings.Index(name, "_"); i >= 0 {
				name = name[:i]
			}
		}
		if _, ok := cfg.Providers[name]; !ok {
			ds = append(ds, diag.Errorf(addr, fmt.Sprintf("unknown provider %q", name),
				fmt.Sprintf("resource %s requires provider %q, but no such entry exists under \"providers\"", addr, name)))
			continue
		}
		r.Provider = name
	}

	if ds.HasErrors() {
		return nil, ds
	}
	return cfg, ds
}

// compileSchema compiles the embedded schema.json. The bytes are registered
// under a synthetic URL because go:embed provides no filesystem path for the
// compiler's default loader.
func compileSchema() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	const url = "embed://schema.json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	return c.Compile(url)
}
