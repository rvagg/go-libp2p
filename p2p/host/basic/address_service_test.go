package basichost

import (
	"testing"

	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
	"github.com/stretchr/testify/require"
)

func TestAppendNATAddrs(t *testing.T) {
	if1, if2 := ma.StringCast("/ip4/192.168.0.100"), ma.StringCast("/ip4/1.1.1.1")
	ifaceAddrs := []ma.Multiaddr{if1, if2}
	tcpListenAddr, udpListenAddr := ma.StringCast("/ip4/0.0.0.0/tcp/1"), ma.StringCast("/ip4/0.0.0.0/udp/2/quic-v1")
	cases := []struct {
		Name        string
		Listen      ma.Multiaddr
		Nat         ma.Multiaddr
		ObsAddrFunc func(ma.Multiaddr) []ma.Multiaddr
		Expected    []ma.Multiaddr
	}{
		{
			Name: "nat map success",
			// nat mapping success, obsaddress ignored
			Listen: ma.StringCast("/ip4/0.0.0.0/udp/1/quic-v1"),
			Nat:    ma.StringCast("/ip4/1.1.1.1/udp/10/quic-v1"),
			ObsAddrFunc: func(m ma.Multiaddr) []ma.Multiaddr {
				return []ma.Multiaddr{ma.StringCast("/ip4/2.2.2.2/udp/100/quic-v1")}
			},
			Expected: []ma.Multiaddr{ma.StringCast("/ip4/1.1.1.1/udp/10/quic-v1")},
		},
		{
			Name: "nat map failure",
			//nat mapping fails, obs addresses added
			Listen: ma.StringCast("/ip4/0.0.0.0/tcp/1"),
			Nat:    nil,
			ObsAddrFunc: func(a ma.Multiaddr) []ma.Multiaddr {
				ip, _ := ma.SplitFirst(a)
				if ip == nil {
					return nil
				}
				if ip.Equal(if1) {
					return []ma.Multiaddr{ma.StringCast("/ip4/2.2.2.2/tcp/100")}
				} else {
					return []ma.Multiaddr{ma.StringCast("/ip4/3.3.3.3/tcp/100")}
				}
			},
			Expected: []ma.Multiaddr{ma.StringCast("/ip4/2.2.2.2/tcp/100"), ma.StringCast("/ip4/3.3.3.3/tcp/100")},
		},
		{
			Name: "nat map success but CGNAT",
			//nat addr added, obs address added with nat provided port
			Listen: tcpListenAddr,
			Nat:    ma.StringCast("/ip4/100.100.1.1/tcp/100"),
			ObsAddrFunc: func(a ma.Multiaddr) []ma.Multiaddr {
				ip, _ := ma.SplitFirst(a)
				if ip == nil {
					return nil
				}
				if ip.Equal(if1) {
					return []ma.Multiaddr{ma.StringCast("/ip4/2.2.2.2/tcp/20")}
				} else {
					return []ma.Multiaddr{ma.StringCast("/ip4/3.3.3.3/tcp/30")}
				}
			},
			Expected: []ma.Multiaddr{
				ma.StringCast("/ip4/100.100.1.1/tcp/100"),
				ma.StringCast("/ip4/2.2.2.2/tcp/100"),
				ma.StringCast("/ip4/3.3.3.3/tcp/100"),
			},
		},
		{
			Name: "uses unspecified address for obs address",
			// observed address manager should be queries with both specified and unspecified addresses
			// udp observed addresses are mapped to unspecified addresses
			Listen: udpListenAddr,
			Nat:    nil,
			ObsAddrFunc: func(a ma.Multiaddr) []ma.Multiaddr {
				if manet.IsIPUnspecified(a) {
					return []ma.Multiaddr{ma.StringCast("/ip4/3.3.3.3/udp/20/quic-v1")}
				}
				return []ma.Multiaddr{ma.StringCast("/ip4/2.2.2.2/udp/20/quic-v1")}
			},
			Expected: []ma.Multiaddr{
				ma.StringCast("/ip4/2.2.2.2/udp/20/quic-v1"),
				ma.StringCast("/ip4/3.3.3.3/udp/20/quic-v1"),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			res := appendNATAddrsForListenAddrs(nil,
				tc.Listen, tc.Nat, tc.ObsAddrFunc, ifaceAddrs)
			res = ma.Unique(res)
			require.ElementsMatch(t, tc.Expected, res, "%s\n%s", tc.Expected, res)
		})
	}
}
