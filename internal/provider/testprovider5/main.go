// SPDX-License-Identifier: MPL-2.0
//
// The handshake constants in this test fixture are adapted from OpenTofu —
// internal/plugin6/serve.go at tag v1.12.3 — Copyright (c) The OpenTofu
// Authors, licensed under MPL-2.0.

// Package main implements a deliberately protocol-5-only provider process.
// It exists solely to exercise tchori's protocol-negotiation failure path;
// no provider RPC can be reached by tchori's protocol-6-only client.
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

// protocol5Plugin only needs to identify the transport as gRPC. Negotiation
// fails before go-plugin can call either gRPC method because the client offers
// protocol 6 while this process advertises only protocol 5.
type protocol5Plugin struct {
	plugin.NetRPCUnsupportedPlugin
}

var _ plugin.GRPCPlugin = (*protocol5Plugin)(nil)

func (*protocol5Plugin) GRPCServer(*plugin.GRPCBroker, *grpc.Server) error {
	// No services are necessary: protocol negotiation fails first.
	return nil
}

func (*protocol5Plugin) GRPCClient(context.Context, *plugin.GRPCBroker, *grpc.ClientConn) (interface{}, error) {
	return nil, errors.New("protocol-5 test provider has no client implementation")
}

func main() {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: handshake,
		VersionedPlugins: map[int]plugin.PluginSet{
			5: {pluginName: &protocol5Plugin{}},
		},
		GRPCServer: plugin.DefaultGRPCServer,
	})
}
