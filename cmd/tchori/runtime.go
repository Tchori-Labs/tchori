package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/runtime"
)

// stateFileName is the state file in the working directory (after -chdir).
const stateFileName = "state.json"

// providerCacheDir returns the default provider cache, ~/.tchori/providers.
// commands.go's providers install/list use it directly; runtime.Build
// computes the same default when Options.CacheDir is empty.
func providerCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".tchori", "providers"), nil
}

// buildRuntime is a thin wrapper over internal/runtime.Build, keeping Task
// 13's signature so every call site in commands.go compiles unchanged. The
// helper's implementation moved to internal/runtime in Task 14 so
// internal/mcpserv can share it (a main package cannot be imported). The
// returned cleanup func closes every launched provider; callers defer it.
// On error diagnostics the runtime is nil and cleanup is a no-op (Build has
// already closed anything it launched).
func buildRuntime(ctx context.Context, pluginDir string) (*runtime.Runtime, func(), diag.Diagnostics) {
	rt, ds := runtime.Build(ctx, runtime.Options{Workdir: ".", PluginDir: pluginDir})
	if rt == nil {
		return nil, func() {}, ds
	}
	return rt, rt.Close, ds
}
