// Package runtime builds the shared execution environment used by the CLI
// (validate/plan/apply/destroy) and the MCP server: parsed config, loaded
// state, and launched + configured provider clients with their schemas.
//
// This logic was moved out of cmd/tchori in Task 14 so internal/mcpserv can
// reuse it — a main package cannot be imported. cmd/tchori keeps a thin
// wrapper (cmd/tchori/runtime.go) that delegates here.
package runtime

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
	"github.com/tchori-labs/tchori/internal/state"
)

// Options configures Build.
type Options struct {
	// Workdir is the directory containing *.tchori.json config files and
	// state.json. The CLI passes "." — its -chdir flag has already changed
	// the process working directory.
	Workdir string
	// PluginDir, when non-empty, is searched for provider binaries before
	// the cache (local development override).
	PluginDir string
	// CacheDir is the provider cache root; empty means ~/.tchori/providers.
	CacheDir string
}

// Runtime bundles everything plan/apply/destroy and the MCP tools need.
type Runtime struct {
	Config    *config.Config
	State     *state.State
	StatePath string
	Providers map[string]*provider.Client          // key = provider local name
	Schemas   map[string]*provider.ProviderSchemas // key = provider local name
}

// Build loads config and state from opts.Workdir, then for every provider
// declared in the config — iterated in sorted order for deterministic
// launches and diagnostics, exactly like Task 13's helper —
// registry.Discover -> provider.Launch -> Schemas -> Configure (provider
// config composed against the provider schema's implied type; references
// are forbidden in provider configuration in MVP). On error diagnostics the
// Runtime is nil and every already-launched provider has been closed. On
// success the accumulated non-error diagnostics are returned alongside the
// Runtime.
func Build(ctx context.Context, opts Options) (*Runtime, diag.Diagnostics) {
	cfg, ds := config.Load(opts.Workdir)
	if ds.HasErrors() {
		return nil, ds
	}

	statePath := filepath.Join(opts.Workdir, "state.json")
	st, err := state.Load(statePath)
	if err != nil {
		return nil, append(ds, diag.Errorf("", "failed to load state", err.Error()))
	}

	cacheDir := opts.CacheDir
	if cacheDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, append(ds, diag.Errorf("", "cannot locate provider cache", err.Error()))
		}
		cacheDir = filepath.Join(home, ".tchori", "providers")
	}

	rt := &Runtime{
		Config:    cfg,
		State:     st,
		StatePath: statePath,
		Providers: map[string]*provider.Client{},
		Schemas:   map[string]*provider.ProviderSchemas{},
	}

	for _, name := range slices.Sorted(maps.Keys(cfg.Providers)) {
		p := cfg.Providers[name]

		bin, err := registry.Discover(cacheDir, opts.PluginDir, p.Source, p.Version)
		if err != nil {
			rt.Close()
			return nil, append(ds, diag.Errorf("", fmt.Sprintf("provider %q is not installed", name),
				fmt.Sprintf("%s\nrun: tchori providers install %s %s", err, p.Source, p.Version)))
		}

		client, err := provider.Launch(ctx, bin)
		if err != nil {
			rt.Close()
			return nil, append(ds, diag.Errorf("", fmt.Sprintf("launching provider %q failed", name), err.Error()))
		}
		rt.Providers[name] = client

		ps, sds := client.Schemas(ctx)
		ds = append(ds, sds...)
		if sds.HasErrors() {
			rt.Close()
			return nil, ds
		}
		rt.Schemas[name] = ps

		// Provider config cannot reference resources in MVP.
		refsForbidden := func(ref config.Ref) (cty.Value, diag.Diagnostics) {
			return cty.NilVal, diag.Diagnostics{diag.Errorf(ref.Address, "reference in provider config",
				fmt.Sprintf("provider %q configuration cannot reference resources (found ${%s.%s})", name, ref.Address, ref.Attr))}
		}
		composed, cds := provider.Compose(p.Config, ps.Provider.Block.ImpliedType(), true, refsForbidden)
		ds = append(ds, cds...)
		if cds.HasErrors() {
			rt.Close()
			return nil, ds
		}

		confDs := client.Configure(ctx, composed)
		ds = append(ds, confDs...)
		if confDs.HasErrors() {
			rt.Close()
			return nil, ds
		}
	}

	return rt, ds
}

// Close shuts down every launched provider subprocess. Safe to call on a
// partially built Runtime.
func (r *Runtime) Close() {
	for _, c := range r.Providers {
		_ = c.Close()
	}
}
