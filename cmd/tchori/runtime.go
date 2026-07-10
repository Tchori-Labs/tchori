package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/zclconf/go-cty/cty"

	"github.com/tchori-labs/tchori/internal/config"
	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/provider"
	"github.com/tchori-labs/tchori/internal/registry"
)

// stateFileName is the state file in the working directory (after -chdir).
const stateFileName = "state.json"

// providerCacheDir returns the default provider cache, ~/.tchori/providers.
func providerCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".tchori", "providers"), nil
}

// runtime bundles everything a provider-touching command needs: the merged
// config, one live client per provider, and the providers' schemas.
type runtime struct {
	Config    *config.Config
	Providers map[string]*provider.Client // key = provider local name
	Schemas   map[string]*provider.ProviderSchemas
}

// buildRuntime is the provider lifecycle helper shared by validate, plan,
// apply, and destroy (Task 14's MCP server mirrors it). It loads the config
// from the working directory (-chdir has already happened in the root
// PersistentPreRunE), discovers each provider binary via registry.Discover
// (pluginDir searched first when non-empty), launches it, fetches its
// schemas, composes its provider config (references are forbidden there in
// MVP: the resolver always errors), and calls Configure. The returned
// cleanup func closes every launched provider; callers defer it. On error
// diagnostics the runtime is nil and cleanup has already run.
func buildRuntime(ctx context.Context, pluginDir string) (*runtime, func(), diag.Diagnostics) {
	noop := func() {}

	cfg, ds := config.Load(".")
	if ds.HasErrors() {
		return nil, noop, ds
	}

	cacheDir, err := providerCacheDir()
	if err != nil {
		return nil, noop, append(ds, diag.Errorf("", "cannot locate provider cache", err.Error()))
	}

	providers := map[string]*provider.Client{}
	schemas := map[string]*provider.ProviderSchemas{}
	cleanup := func() {
		for _, c := range providers {
			_ = c.Close()
		}
	}

	for _, name := range slices.Sorted(maps.Keys(cfg.Providers)) {
		p := cfg.Providers[name]

		bin, err := registry.Discover(cacheDir, pluginDir, p.Source, p.Version)
		if err != nil {
			cleanup()
			return nil, noop, append(ds, diag.Errorf("", fmt.Sprintf("provider %q is not installed", name),
				fmt.Sprintf("%s\nrun: tchori providers install %s %s", err, p.Source, p.Version)))
		}

		client, err := provider.Launch(ctx, bin)
		if err != nil {
			cleanup()
			return nil, noop, append(ds, diag.Errorf("", fmt.Sprintf("launching provider %q failed", name), err.Error()))
		}
		providers[name] = client

		ps, sds := client.Schemas(ctx)
		ds = append(ds, sds...)
		if sds.HasErrors() {
			cleanup()
			return nil, noop, ds
		}
		schemas[name] = ps

		// Provider config cannot reference resources in MVP.
		refsForbidden := func(ref config.Ref) (cty.Value, diag.Diagnostics) {
			return cty.NilVal, diag.Diagnostics{diag.Errorf(ref.Address, "reference in provider config",
				fmt.Sprintf("provider %q configuration cannot reference resources (found ${%s.%s})", name, ref.Address, ref.Attr))}
		}
		composed, cds := provider.Compose(p.Config, ps.Provider.Block.ImpliedType(), true, refsForbidden)
		ds = append(ds, cds...)
		if cds.HasErrors() {
			cleanup()
			return nil, noop, ds
		}

		confDs := client.Configure(ctx, composed)
		ds = append(ds, confDs...)
		if confDs.HasErrors() {
			cleanup()
			return nil, noop, ds
		}
	}

	return &runtime{Config: cfg, Providers: providers, Schemas: schemas}, cleanup, ds
}
