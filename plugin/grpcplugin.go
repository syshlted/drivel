package plugin

import (
	"context"
	"log"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/zishmusic/drivel/plugin/internal/pb"
	"github.com/zishmusic/drivel/provider"
)

// maxMessage bounds one non-streamed gRPC message.
//
// It is set explicitly on both ends rather than left to a default, because the
// two calls that can produce a big one — Enumerate and Changes — return a whole
// page at a time, and a page is the backend's choice. Drive's flat sweep pages
// at a thousand objects and its scoped descent can return eight listings at
// once; a third-party backend might reasonably return far more. 64 MiB is enough
// for any of those by a wide margin while still being a bound, and exceeding it
// is a clear error rather than a truncated sweep — which, per M7b row 4, is the
// dangerous shape: an enumeration that quietly comes back short makes every
// missing path look remotely deleted.
const maxMessage = 64 << 20

// grpcPlugin is go-plugin's view of the provider service. One type serves both
// ends: the host reaches GRPCClient, a plugin process reaches GRPCServer, and
// the fields each needs are nil on the other side.
type grpcPlugin struct {
	goplugin.NetRPCUnsupportedPlugin

	// Set in the plugin process only.
	factory provider.Factory
	lg      *log.Logger
}

// dispensed is what the host gets back from Dispense: the RPC stub and the
// broker it must use to serve anything back.
type dispensed struct {
	api    pb.ProviderClient
	broker *goplugin.GRPCBroker
}

// GRPCServer runs in the plugin process.
func (p *grpcPlugin) GRPCServer(broker *goplugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterProviderServer(s, &server{factory: p.factory, lg: p.lg, broker: broker})
	return nil
}

// GRPCClient runs in the host process.
func (p *grpcPlugin) GRPCClient(_ context.Context, broker *goplugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return &dispensed{api: pb.NewProviderClient(c), broker: broker}, nil
}

var _ goplugin.GRPCPlugin = (*grpcPlugin)(nil)
