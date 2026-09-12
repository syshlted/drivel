package plugin

import (
	"net"
	"strings"
	"testing"
)

// A backend is reached over a unix socket and nothing else.
//
// The check exists because the *plugin* names the control address — go-plugin
// reads the network and address off the child's handshake line and dials what it
// is told — so "local only" is a policy the host has to enforce rather than a
// property of the transport. See checkLocalTransport for what a TCP address
// costs.
func TestControlTransportMustBeAUnixSocket(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr net.Addr
		want string // substring of the refusal, or "" to accept
	}{
		{
			name: "unix socket is what a plugin is supposed to announce",
			addr: &net.UnixAddr{Name: "/tmp/plugin1234", Net: "unix"},
		},
		{
			name: "loopback tcp is reachable by every local user",
			addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 41234},
			want: "unix socket only",
		},
		{
			name: "a routable address would dial off this machine",
			addr: &net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 443},
			want: "unix socket only",
		},
		{
			name: "no address at all",
			want: "no control address",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A typed nil in an interface is not a nil interface, so the zero case
			// has to pass an actually-nil net.Addr rather than (*net.TCPAddr)(nil).
			var addr net.Addr
			if tc.addr != nil {
				addr = tc.addr
			}
			err := checkLocalTransport(addr)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("checkLocalTransport(%v) = %v, want accepted", addr, err)
			case tc.want == "":
			case err == nil:
				t.Fatalf("checkLocalTransport(%v) = nil, want a refusal mentioning %q", addr, tc.want)
			case !strings.Contains(err.Error(), tc.want):
				t.Fatalf("checkLocalTransport(%v) = %q, want it to mention %q", addr, err, tc.want)
			}
		})
	}
}
