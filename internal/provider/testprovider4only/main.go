// SPDX-License-Identifier: MPL-2.0
//
// The handshake constants in this test fixture are adapted from OpenTofu —
// internal/plugin6/serve.go at tag v1.12.3 — Copyright (c) The OpenTofu
// Authors, licensed under MPL-2.0.

// Package main implements a deliberately protocol-4-only provider process.
// It exists solely to exercise tchori's protocol-negotiation failure path
// for a provider that offers neither of tchori's supported protocols (6 or
// 5): no provider RPC can be reached by tchori's client, so Launch must
// fail fast with the structured "provider protocol unsupported" diagnostic.
package main

import (
	"context"
	"errors"

	plugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

const pluginName = "provider"

var handshake = plugin.HandshakeConfig{
	ProtocolVersion:  4,
	MagicCookieKey:   "TF_PLUGIN_MAGIC_COOKIE",
	MagicCookieValue: "d602bf8f470bc67ca7faa0386276bbdd4330efaf76d1a219cb4d6991ca9872b2",
}

// protocol4Plugin only needs to identify the transport as gRPC. Negotiation
// fails before go-plugin can call either gRPC method because the client
// offers protocols 6 and 5 while this process advertises only protocol 4.
type protocol4Plugin struct {
	plugin.NetRPCUnsupportedPlugin
}

var _ plugin.GRPCPlugin = (*protocol4Plugin)(nil)

func (*protocol4Plugin) GRPCServer(*plugin.GRPCBroker, *grpc.Server) error {
	// No services are necessary: protocol negotiation fails first.
	return nil
}

func (*protocol4Plugin) GRPCClient(context.Context, *plugin.GRPCBroker, *grpc.ClientConn) (interface{}, error) {
	return nil, errors.New("protocol-4 test provider has no client implementation")
}

func main() {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: handshake,
		VersionedPlugins: map[int]plugin.PluginSet{
			4: {pluginName: &protocol4Plugin{}},
		},
		GRPCServer: plugin.DefaultGRPCServer,
	})
}
