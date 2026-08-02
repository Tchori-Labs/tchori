// SPDX-License-Identifier: MPL-2.0
//
// The handshake constants and go-plugin client wiring in this file are
// adapted from OpenTofu — internal/plugin6/serve.go, internal/plugin/plugin.go
// and internal/command/meta_providers.go at tag v1.12.3 — Copyright (c) The
// OpenTofu Authors, licensed under MPL-2.0.

// Package provider launches Terraform plugin-protocol provider binaries
// (protocol 6 natively, and protocol 5 via a translation adapter — see
// tfplugin5_adapter.go) over hashicorp/go-plugin and exposes their gRPC API
// to the rest of tchori.
package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	plugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/tchori-labs/tchori/internal/provider/proto/tfplugin5"
	"github.com/tchori-labs/tchori/internal/provider/proto/tfplugin6"
)

const (
	// pluginName is the fixed plugin name every Terraform/OpenTofu provider
	// registers itself under. It is not configurable.
	pluginName = "provider"

	// providerStartTimeout bounds the provider handshake at 60 seconds so a
	// subprocess that never starts serving cannot block tchori indefinitely.
	providerStartTimeout = 60 * time.Second

	// providerStopGrace gives a provider 5 seconds to flush and stop cleanly;
	// after this finite grace period tchori unconditionally kills the process.
	providerStopGrace = 5 * time.Second
)

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

// providerGRPC is the subset of tfplugin6.ProviderClient the engine
// actually calls: the eight RPCs used by rpc.go and schema.go (Configure,
// ValidateResource, PlanResource, ApplyResource, ReadResource,
// ImportResource, Schemas, and Close/Stop). *tfplugin6.NewProviderClient's
// concrete type satisfies this directly; protocol5Adapter
// (tfplugin5_adapter.go) wraps a tfplugin5.ProviderClient to satisfy it too,
// so Client.grpc can hold either a native protocol-6 connection or an
// adapted protocol-5 one. This is deliberately narrower than the full
// generated tfplugin6.ProviderClient interface (6.10 also has streaming and
// data-source/function/ephemeral-resource RPCs tchori never calls) so the
// adapter never has to implement RPCs it doesn't need.
type providerGRPC interface {
	GetProviderSchema(ctx context.Context, in *tfplugin6.GetProviderSchema_Request, opts ...grpc.CallOption) (*tfplugin6.GetProviderSchema_Response, error)
	ConfigureProvider(ctx context.Context, in *tfplugin6.ConfigureProvider_Request, opts ...grpc.CallOption) (*tfplugin6.ConfigureProvider_Response, error)
	ValidateResourceConfig(ctx context.Context, in *tfplugin6.ValidateResourceConfig_Request, opts ...grpc.CallOption) (*tfplugin6.ValidateResourceConfig_Response, error)
	PlanResourceChange(ctx context.Context, in *tfplugin6.PlanResourceChange_Request, opts ...grpc.CallOption) (*tfplugin6.PlanResourceChange_Response, error)
	ApplyResourceChange(ctx context.Context, in *tfplugin6.ApplyResourceChange_Request, opts ...grpc.CallOption) (*tfplugin6.ApplyResourceChange_Response, error)
	ReadResource(ctx context.Context, in *tfplugin6.ReadResource_Request, opts ...grpc.CallOption) (*tfplugin6.ReadResource_Response, error)
	ImportResourceState(ctx context.Context, in *tfplugin6.ImportResourceState_Request, opts ...grpc.CallOption) (*tfplugin6.ImportResourceState_Response, error)
	StopProvider(ctx context.Context, in *tfplugin6.StopProvider_Request, opts ...grpc.CallOption) (*tfplugin6.StopProvider_Response, error)
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

// grpcProviderPlugin5 is the client-side plugin.GRPCPlugin implementation
// for protocol 5. It mirrors grpcProviderPlugin but dispenses a raw
// tfplugin5.ProviderClient; Launch wraps that in a protocol5Adapter before
// storing it on Client.grpc.
type grpcProviderPlugin5 struct {
	plugin.Plugin
}

var _ plugin.GRPCPlugin = (*grpcProviderPlugin5)(nil)

func (p *grpcProviderPlugin5) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, conn *grpc.ClientConn) (interface{}, error) {
	return tfplugin5.NewProviderClient(conn), nil
}

func (p *grpcProviderPlugin5) GRPCServer(*plugin.GRPCBroker, *grpc.Server) error {
	return errors.New("provider: tchori is a plugin client, not a plugin server")
}

// Client wraps a live provider subprocess speaking plugin protocol 6 or 5
// (protocol-5 connections are transparently wrapped in a protocol5Adapter).
type Client struct {
	grpc          providerGRPC
	plugin        *plugin.Client
	cancelCommand context.CancelFunc
	schemas       *ProviderSchemas // cached by Schemas
}

// Launch starts the provider binary as a go-plugin subprocess, performs
// the handshake (AutoMTLS, gRPC only, protocol 6 or 5), and dispenses the
// provider's gRPC client — negotiating protocol 6 natively and protocol 5
// through protocol5Adapter. Startup honors ctx and is independently bounded
// by providerStartTimeout; cancellation kills and reaps the subprocess.
func Launch(ctx context.Context, binary string) (*Client, error) {
	commandCtx, cancelCommand := context.WithCancel(context.Background())
	stopCallerCancellation := context.AfterFunc(ctx, cancelCommand)
	keepCommandContext := false
	defer func() {
		stopCallerCancellation()
		if !keepCommandContext {
			cancelCommand()
		}
	}()

	pc := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig: handshake,
		VersionedPlugins: map[int]plugin.PluginSet{
			6: {pluginName: &grpcProviderPlugin{}},
			5: {pluginName: &grpcProviderPlugin5{}},
		},
		Cmd:              exec.CommandContext(commandCtx, binary), //nolint:gosec // no CLI args; binary path comes from tchori's own registry/discovery
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		StartTimeout:     providerStartTimeout,
		AutoMTLS:         true,
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:   "provider",
			Level:  hclog.Warn, // keep go-plugin's trace noise out of tchori's stderr
			Output: os.Stderr,
		}),
	})

	type clientResult struct {
		client plugin.ClientProtocol
		err    error
	}
	resultCh := make(chan clientResult, 1)
	go func() {
		client, err := pc.Client()
		resultCh <- clientResult{client: client, err: err}
	}()

	var result clientResult
	select {
	case result = <-resultCh:
		// Detach the live provider from startup cancellation. If cancellation
		// has already won the race, AfterFunc has killed the command and the
		// launch must still be reported as canceled.
		if !stopCallerCancellation() {
			pc.Kill()
			return nil, fmt.Errorf("provider: launching %q canceled: %w", binary, ctx.Err())
		}
	case <-ctx.Done():
		// CommandContext interrupts Start while go-plugin holds its startup
		// lock; Kill then waits for Client to unwind and reaps the process.
		cancelCommand()
		pc.Kill()
		return nil, fmt.Errorf("provider: launching %q canceled: %w", binary, ctx.Err())
	}

	rpcClient, err := result.client, result.err
	if err != nil {
		pc.Kill()
		// go-plugin reports a failed protocol negotiation as
		// "incompatible API version with plugin. Plugin version: 5,
		// Client versions: [6]" (capitalization varies across go-plugin
		// releases, hence the case-insensitive match). This only fires when
		// the provider offers neither protocol tchori negotiates (6 or 5) —
		// name the mismatch in tchori's own words: this is the engine's
		// documented graceful failure for providers that speak neither
		// supported protocol.
		if strings.Contains(strings.ToLower(err.Error()), "incompatible api version with plugin") {
			return nil, fmt.Errorf("provider protocol unsupported: tchori speaks plugin protocols 6 (tfplugin6) and 5 (tfplugin5), and %q does not offer either: %w", binary, err)
		}
		return nil, fmt.Errorf("provider: connecting to %q: %w", binary, err)
	}

	raw, err := rpcClient.Dispense(pluginName)
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("provider: dispensing %q: %w", pluginName, err)
	}

	var grpcClient providerGRPC
	switch v := pc.NegotiatedVersion(); v {
	case 6:
		c, ok := raw.(tfplugin6.ProviderClient)
		if !ok {
			pc.Kill()
			return nil, fmt.Errorf("provider: dispensed plugin is %T, not tfplugin6.ProviderClient", raw)
		}
		grpcClient = c
	case 5:
		c, ok := raw.(tfplugin5.ProviderClient)
		if !ok {
			pc.Kill()
			return nil, fmt.Errorf("provider: dispensed plugin is %T, not tfplugin5.ProviderClient", raw)
		}
		grpcClient = &protocol5Adapter{client: c}
	default:
		pc.Kill()
		return nil, fmt.Errorf("provider: %q negotiated protocol %d, want 6 or 5", binary, v)
	}

	keepCommandContext = true
	return &Client{grpc: grpcClient, plugin: pc, cancelCommand: cancelCommand}, nil
}

// Close gives the provider providerStopGrace to stop via the StopProvider RPC
// (a chance to flush and clean up), then always kills the subprocess. Kill
// blocks until the process has exited; a stop timeout or error is returned
// after the process is reaped. Pattern adapted from OpenTofu
// internal/plugin6/grpc_provider.go (Stop + Close) at v1.12.3, MPL-2.0.
func (c *Client) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), providerStopGrace)
	defer cancel()

	_, stopErr := c.grpc.StopProvider(ctx, &tfplugin6.StopProvider_Request{})
	c.plugin.Kill()
	c.cancelCommand()
	return stopErr
}
