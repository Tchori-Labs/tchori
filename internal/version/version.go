// Package version holds the tchori engine's build-time version string.
package version

// Version is the tchori engine version. goreleaser overrides it at release
// time via -ldflags "-X github.com/tchori-labs/tchori/internal/version.Version=…".
// It is bumped by the board-approved release process only (see AGENTS.md) —
// no automated bumps.
var Version = "0.1.0-dev"
