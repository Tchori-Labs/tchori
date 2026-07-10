// SPDX-License-Identifier: MPL-2.0
//
// The handshake constants and go-plugin client wiring in this file are
// adapted from OpenTofu — internal/plugin6/serve.go, internal/plugin/plugin.go
// and internal/command/meta_providers.go at tag v1.12.3 — Copyright (c) The
// OpenTofu Authors, licensed under MPL-2.0.

// Package provider launches Terraform plugin-protocol-6 provider binaries
// over hashicorp/go-plugin and exposes their gRPC API to the rest of tchori.
package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	hclog "github.com/hashicorp/go-hclog"
	plugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/tchori-labs/tchori/internal/provider/proto/tfplugin6"
)

// pluginName is the fixed plugin name every Terraform/OpenTofu provider
// registers itself under. It is not configurable.
const pluginName = "provider"

// handshake must exactly match Terraform/OpenTofu's handshake constants —
// every real provider binary rejects clients that present anything else.
// The magic cookie values should NEVER be changed.
//
// Adapted from OpenTofu internal/plugin6/serve.go (v1.12.3), MPL-2.0.
var handshake = plugin.HandshakeConfig{
	// Fallback for legacy (non-VersionedPlugins) negotiation only; the real
	// protocol negotiation happens through VersionedPlugins in Launch.
	ProtocolVersion:  4,
	MagicCookieKey:   "TF_PLUGIN_MAGIC_COOKIE",
	MagicCookieValue: "d602bf8f470bc67ca7faa0386276bbdd4330efaf76d1a219cb4d6991ca9872b2",
}

// grpcProviderPlugin is the client-side plugin.GRPCPlugin implementation
// for protocol 6. tchori is only ever a client of provider plugins, so
// GRPCServer is never called.
type grpcProviderPlugin struct {
	plugin.Plugin
}

var _ plugin.GRPCPlugin = (*grpcProviderPlugin)(nil)

// GRPCClient is invoked by go-plugin once the gRPC connection to the
// provider subprocess is established.
func (p *grpcProviderPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, conn *grpc.ClientConn) (interface{}, error) {
	return tfplugin6.NewProviderClient(conn), nil
}

// GRPCServer would only be called if tchori served a provider itself.
func (p *grpcProviderPlugin) GRPCServer(*plugin.GRPCBroker, *grpc.Server) error {
	return errors.New("provider: tchori is a plugin client, not a plugin server")
}

// Client wraps a live provider subprocess speaking plugin protocol 6.
type Client struct {
	grpc    tfplugin6.ProviderClient
	plugin  *plugin.Client
	schemas *ProviderSchemas // cached by Schemas
}

// Launch starts the provider binary as a go-plugin subprocess, performs
// the handshake (AutoMTLS, gRPC only, protocol 6 only), and dispenses the
// provider's gRPC client. ctx is accepted for interface symmetry; process
// lifetime is managed by Close, not by ctx.
func Launch(ctx context.Context, binary string) (*Client, error) {
	_ = ctx

	pc := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig: handshake,
		VersionedPlugins: map[int]plugin.PluginSet{
			6: {pluginName: &grpcProviderPlugin{}},
		},
		Cmd:              exec.Command(binary), //nolint:gosec // no CLI args; binary path comes from tchori's own registry/discovery
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		AutoMTLS:         true,
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:   "provider",
			Level:  hclog.Warn, // keep go-plugin's trace noise out of tchori's stderr
			Output: os.Stderr,
		}),
	})

	rpcClient, err := pc.Client()
	if err != nil {
		pc.Kill()
		// go-plugin reports a failed protocol negotiation as
		// "incompatible API version with plugin. Plugin version: 5,
		// Client versions: [6]" (capitalization varies across go-plugin
		// releases, hence the case-insensitive match). Name the mismatch
		// in tchori's own words: this is the engine's documented graceful
		// failure for protocol-5-only providers (the classic
		// null/random/time/local); a tfplugin5 adapter is a recorded
		// post-MVP roadmap item.
		if strings.Contains(strings.ToLower(err.Error()), "incompatible api version with plugin") {
			return nil, fmt.Errorf("provider protocol unsupported: tchori speaks plugin protocol 6 (tfplugin6) only, and %q does not offer it: %w", binary, err)
		}
		return nil, fmt.Errorf("provider: connecting to %q: %w", binary, err)
	}

	raw, err := rpcClient.Dispense(pluginName)
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("provider: dispensing %q: %w", pluginName, err)
	}

	grpcClient, ok := raw.(tfplugin6.ProviderClient)
	if !ok {
		pc.Kill()
		return nil, fmt.Errorf("provider: dispensed plugin is %T, not tfplugin6.ProviderClient", raw)
	}

	if v := pc.NegotiatedVersion(); v != 6 {
		pc.Kill()
		return nil, fmt.Errorf("provider: %q negotiated protocol %d, want 6 (protocol-5-only provider?)", binary, v)
	}

	return &Client{grpc: grpcClient, plugin: pc}, nil
}

// Close asks the provider to stop via the StopProvider RPC (a chance to
// flush and clean up), then kills the subprocess. Kill blocks until the
// process has exited. Pattern adapted from OpenTofu
// internal/plugin6/grpc_provider.go (Stop + Close) at v1.12.3, MPL-2.0.
func (c *Client) Close() error {
	_, stopErr := c.grpc.StopProvider(context.Background(), &tfplugin6.StopProvider_Request{})
	c.plugin.Kill()
	return stopErr
}
