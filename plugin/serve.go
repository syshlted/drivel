// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"log"
	"os"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/zishmusic/drivel/provider"
)

// Serve runs a provider backend as a drivel plugin. It is what a plugin's main
// calls, and it does not return:
//
//	package main
//
//	import (
//		"github.com/zishmusic/drivel/plugin"
//		"example.com/drivel-provider-thing/thing"
//	)
//
//	func main() {
//		plugin.Serve(thing.Factory)
//	}
//
// The factory is the same provider.Factory the in-process registry takes, and it
// is called exactly once, with the configuration the host was given for this
// mount. A backend therefore has one implementation and one entry point whether
// it is loaded as a plugin or compiled in — which matters because compiled in is
// how its own tests run it.
//
// The kind this plugin provides is NOT named here. It comes from the
// executable's filename (see BinaryPrefix), so that what a config file asks for
// and what the host launches cannot disagree.
//
// # Output
//
// Everything the factory writes to the provider.Params logger, and anything else
// this process writes to stderr, is forwarded to the log of the mount that
// launched it. Standard output is reserved: go-plugin's handshake is written
// there, so a stray fmt.Println in a backend will break the handshake rather
// than appear anywhere. That is why the logger handed to the factory writes to
// stderr, and why a plugin should never build one of its own over os.Stdout.
func Serve(factory provider.Factory) {
	// The provider's logger writes to stderr with no prefix and no timestamp: the
	// host adds both, using the mount's own prefix, so that a plugin's lines read
	// as part of the mount they belong to rather than as a second program's
	// output with its own idea of what time it is.
	lg := log.New(os.Stderr, "", 0)

	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: handshake,
		Plugins: goplugin.PluginSet{
			pluginName: &grpcPlugin{factory: factory, lg: lg},
		},
		GRPCServer: func(opts []grpc.ServerOption) *grpc.Server {
			return grpc.NewServer(append(opts,
				grpc.MaxRecvMsgSize(maxMessage),
				grpc.MaxSendMsgSize(maxMessage),
			)...)
		},
		// go-plugin's own logging is silenced for the same reason it is on the host
		// side: this process's stderr is the provider's log, and hclog's framing in
		// the middle of it would be a second program talking.
		Logger: hclog.New(&hclog.LoggerOptions{Output: os.Stderr, Level: hclog.Off}),
	})
}
