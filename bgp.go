package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	apb "google.golang.org/protobuf/types/known/anypb"

	api "github.com/osrg/gobgp/v3/api"
	gobgp "github.com/osrg/gobgp/v3/pkg/server"
)

type bgpCfg struct {
	AS          uint32
	RouterID    string
	NextHop     string
	NextHopIPv6 string
	SourceIP    string
	SourceIF    string

	Peers []string
	IPv6  bool
}

type bgpServer struct {
	s *gobgp.BgpServer
	c *bgpCfg
}

func newBgp(c *bgpCfg) (b *bgpServer, err error) {
	if c.AS == 0 {
		return nil, fmt.Errorf("You need to provide AS")
	}

	if c.SourceIP != "" && c.SourceIF != "" {
		return nil, fmt.Errorf("SourceIP and SourceIF are mutually exclusive")
	}

	if len(c.Peers) == 0 {
		return nil, fmt.Errorf("You need to provide at least one peer")
	}

	b = &bgpServer{
		s: gobgp.NewBgpServer(),
		c: c,
	}
	go b.s.Serve()

	if err = b.s.StartBgp(context.Background(), &api.StartBgpRequest{
		Global: &api.Global{
			Asn:        c.AS,
			RouterId:   c.RouterID,
			ListenPort: -1,
		},
	}); err != nil {
		return
	}

	if err = b.s.WatchEvent(context.Background(), &api.WatchEventRequest{Peer: &api.WatchEventRequest_Peer{}}, func(r *api.WatchEventResponse) {
		if p := r.GetPeer(); p != nil && p.Type == api.WatchEventResponse_PeerEvent_STATE {
			log.Println(p)
		}
	}); err != nil {
		return
	}

	for _, p := range c.Peers {
		if err = b.addPeer(p); err != nil {
			return
		}
	}

	return
}

func (b *bgpServer) addPeer(addr string) (err error) {
	port := 179

	if t := strings.SplitN(addr, ":", 2); len(t) == 2 {
		addr = t[0]

		if port, err = strconv.Atoi(t[1]); err != nil {
			return fmt.Errorf("Unable to parse port '%s' as int: %s", t[1], err)
		}
	}

	p := &api.Peer{
		Conf: &api.PeerConf{
			NeighborAddress: addr,
			PeerAsn:         b.c.AS,
		},

		AfiSafis: []*api.AfiSafi{
			{
				Config: &api.AfiSafiConfig{
					Family: &api.Family{
						Afi:  api.Family_AFI_IP,
						Safi: api.Family_SAFI_UNICAST,
					},
					Enabled: true,
				},
			},
			{
				Config: &api.AfiSafiConfig{
					Family: &api.Family{
						Afi:  api.Family_AFI_IP6,
						Safi: api.Family_SAFI_UNICAST,
					},
					Enabled: true,
				},
			},
		},

		Timers: &api.Timers{
			Config: &api.TimersConfig{
				ConnectRetry: 10,
			},
		},

		Transport: &api.Transport{
			MtuDiscovery:  true,
			RemoteAddress: addr,
			RemotePort:    uint32(port),
		},
	}

	if b.c.SourceIP != "" {
		p.Transport.LocalAddress = b.c.SourceIP
	}

	if b.c.SourceIF != "" {
		p.Transport.BindInterface = b.c.SourceIF
	}

	return b.s.AddPeer(context.Background(), &api.AddPeerRequest{
		Peer: p,
	})
}

func (b *bgpServer) getPath(ip net.IP) *api.Path {

	var nh string
	var pfxLen uint32 = 32
	if ip.To4() == nil {
		if !b.c.IPv6 {
			return nil
		}
		pfxLen = 128
	}

	nlri, _ := apb.New(&api.IPAddressPrefix{
		Prefix:    ip.String(),
		PrefixLen: pfxLen,
	})

	a1, _ := apb.New(&api.OriginAttribute{
		Origin: 0,
	})

	if ip.To4() == nil {

		v6Family := &api.Family{
			Afi:  api.Family_AFI_IP6,
			Safi: api.Family_SAFI_UNICAST,
		}

		if b.c.NextHopIPv6 != "" {
			nh = b.c.NextHopIPv6
		} else {
			nh = "fd00::1"
		}

		v6Attrs, _ := apb.New(&api.MpReachNLRIAttribute{
			Family:   v6Family,
			NextHops: []string{nh},
			Nlris:    []*apb.Any{nlri},
		})

		return &api.Path{
			Family: v6Family,
			Nlri:   nlri,
			Pattrs: []*apb.Any{a1, v6Attrs},
		}
	} else {

		if b.c.NextHop != "" {
			nh = b.c.NextHop
		} else if b.c.SourceIP != "" {
			nh = b.c.SourceIP
		} else {
			nh = b.c.RouterID
		}

		a2, _ := apb.New(&api.NextHopAttribute{
			NextHop: nh,
		})

		return &api.Path{
			Family: &api.Family{
				Afi:  api.Family_AFI_IP,
				Safi: api.Family_SAFI_UNICAST,
			},
			Nlri:   nlri,
			Pattrs: []*apb.Any{a1, a2},
		}
	}
}

func (b *bgpServer) addHost(ip net.IP) (err error) {
	p := b.getPath(ip)
	if p == nil {
		return
	}

	_, err = b.s.AddPath(context.Background(), &api.AddPathRequest{
		Path: p,
	})

	return
}

func (b *bgpServer) delHost(ip net.IP) (err error) {
	p := b.getPath(ip)
	if p == nil {
		return
	}

	return b.s.DeletePath(context.Background(), &api.DeletePathRequest{
		Path: p,
	})
}

func (b *bgpServer) close() error {
	ctx, cf := context.WithTimeout(context.Background(), 5*time.Second)
	defer cf()
	return b.s.StopBgp(ctx, &api.StopBgpRequest{})
}
