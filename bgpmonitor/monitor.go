// Package bgpmonitor embeds GoBGP and emits changes to received IPv4- and
// IPv6-unicast routes after each family's initial RIB reaches End-of-RIB.
package bgpmonitor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"sync"
	"time"

	api "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/server"
)

type Config struct {
	LocalASN        uint32
	RouterID        string
	LocalAddress    string
	NeighborAddress string
	NeighborASN     uint32
}

type ChangeType string

const (
	Announce   ChangeType = "announce"
	Withdraw   ChangeType = "withdraw"
	PathChange ChangeType = "as_path_change"
)

type Event struct {
	Type ChangeType
	New  *Route
	Old  *Route
}

type Monitor struct {
	config   Config
	onEvent  func(Event)
	server   *server.BgpServer
	stopOnce sync.Once

	mu        sync.Mutex
	rib       map[netip.Prefix]Route
	readyIPv4 bool
	readyIPv6 bool
}

func New(config Config, onEvent func(Event)) (*Monitor, error) {
	if config.LocalASN == 0 || config.NeighborASN == 0 {
		return nil, errors.New("local and neighbor ASN must be non-zero")
	}
	if addr, err := netip.ParseAddr(config.LocalAddress); err != nil || !addr.Is4() {
		return nil, fmt.Errorf("invalid IPv4 local address %q", config.LocalAddress)
	}
	if addr, err := netip.ParseAddr(config.NeighborAddress); err != nil || !addr.Is4() {
		return nil, fmt.Errorf("invalid IPv4 neighbor address %q", config.NeighborAddress)
	}
	if addr, err := netip.ParseAddr(config.RouterID); err != nil || !addr.Is4() {
		return nil, fmt.Errorf("invalid BGP router ID %q", config.RouterID)
	}
	if onEvent == nil {
		return nil, errors.New("event handler is required")
	}
	return &Monitor{config: config, onEvent: onEvent, rib: make(map[netip.Prefix]Route)}, nil
}

func (m *Monitor) Start(ctx context.Context) error {
	m.server = server.NewBgpServer()
	go m.server.Serve()
	if err := m.server.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn: m.config.LocalASN, RouterId: m.config.RouterID, ListenPort: -1,
	}}); err != nil {
		m.server.Stop()
		return fmt.Errorf("start BGP: %w", err)
	}

	watch := &api.WatchEventRequest{
		Peer: &api.WatchEventRequest_Peer{},
		Table: &api.WatchEventRequest_Table{Filters: []*api.WatchEventRequest_Table_Filter{
			{Type: api.WatchEventRequest_Table_Filter_ADJIN, PeerAddress: m.config.NeighborAddress},
			{Type: api.WatchEventRequest_Table_Filter_EOR},
		}},
		BatchSize: 512,
	}
	if err := m.server.WatchEvent(ctx, watch, m.handleResponse); err != nil {
		m.stop()
		return fmt.Errorf("watch BGP events: %w", err)
	}

	// An explicit default-reject export policy is a safety invariant: this
	// process must never advertise a route, even if paths are added later.
	if err := m.server.SetPolicyAssignment(ctx, &api.SetPolicyAssignmentRequest{Assignment: &api.PolicyAssignment{
		Name: "global", Direction: api.PolicyDirection_EXPORT, DefaultAction: api.RouteAction_REJECT,
	}}); err != nil {
		m.stop()
		return fmt.Errorf("install export reject policy: %w", err)
	}

	peer := &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: m.config.NeighborAddress, PeerAsn: m.config.NeighborASN, LocalAsn: m.config.LocalASN},
		Transport: &api.Transport{LocalAddress: m.config.LocalAddress},
		AfiSafis: []*api.AfiSafi{
			{Config: &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}, Enabled: true}},
			{Config: &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_IP6, Safi: api.Family_SAFI_UNICAST}, Enabled: true}},
		},
	}
	if err := m.server.AddPeer(ctx, &api.AddPeerRequest{Peer: peer}); err != nil {
		m.stop()
		return fmt.Errorf("add BGP peer: %w", err)
	}
	go func() {
		<-ctx.Done()
		m.stop()
	}()
	return nil
}

func (m *Monitor) stop() {
	m.stopOnce.Do(func() {
		if m.server == nil {
			return
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.server.StopBgp(stopCtx, &api.StopBgpRequest{}); err != nil {
			log.Printf("stop BGP: %v", err)
		}
		m.server.Stop()
	})
}

// Shutdown synchronously stops the peer and the embedded BGP server. It is
// safe to call more than once.
func (m *Monitor) Shutdown() {
	m.stop()
}

func (m *Monitor) handleResponse(response *api.WatchEventResponse) {
	if peerEvent := response.GetPeer(); peerEvent != nil {
		if peerEvent.Type == api.WatchEventResponse_PeerEvent_STATE &&
			peerEvent.Peer.GetState().GetSessionState() != api.PeerState_ESTABLISHED {
			m.resetBaseline()
		}
		return
	}
	tableEvent := response.GetTable()
	if tableEvent == nil {
		return
	}
	for _, path := range tableEvent.Paths {
		if isUnicastEOR(path) {
			m.mu.Lock()
			if path.Family.Afi == api.Family_AFI_IP {
				m.readyIPv4 = true
			} else {
				m.readyIPv6 = true
			}
			count := len(m.rib)
			m.mu.Unlock()
			log.Printf("BGP %s baseline ready (%d total routes in RIB)", path.Family.Afi, count)
			continue
		}
		m.handlePath(path)
	}
}

func isUnicastEOR(path *api.Path) bool {
	if path == nil || path.Nlri != nil {
		return false
	}
	_, err := apiRouteFamily(path.Family)
	return err == nil
}

func (m *Monitor) resetBaseline() {
	m.mu.Lock()
	m.rib = make(map[netip.Prefix]Route)
	m.readyIPv4 = false
	m.readyIPv6 = false
	m.mu.Unlock()
}

func (m *Monitor) handlePath(path *api.Path) {
	route, err := routeFromAPI(path, m.config.NeighborASN)
	if err != nil {
		// Withdrawals normally retain path attributes in GoBGP's Adj-RIB-In.
		// If a peer supplies an attribute-less withdrawal, resolve it by NLRI.
		if path != nil && path.IsWithdraw {
			m.handleBareWithdraw(path)
		}
		return
	}

	m.mu.Lock()
	old, existed := m.rib[route.Prefix]
	active := m.readyIPv6
	if route.Prefix.Addr().Is4() {
		active = m.readyIPv4
	}
	if path.IsWithdraw {
		delete(m.rib, route.Prefix)
	} else {
		m.rib[route.Prefix] = route
	}
	m.mu.Unlock()

	if !active {
		return
	}
	if path.IsWithdraw {
		if existed {
			m.onEvent(Event{Type: Withdraw, Old: &old})
		}
		return
	}
	if !existed {
		m.onEvent(Event{Type: Announce, New: &route})
	} else if old.ASPath != route.ASPath {
		m.onEvent(Event{Type: PathChange, Old: &old, New: &route})
	}
}

func (m *Monitor) handleBareWithdraw(path *api.Path) {
	prefix, err := prefixFromAPI(path)
	if err != nil {
		return
	}
	m.mu.Lock()
	old, existed := m.rib[prefix]
	active := m.readyIPv6
	if prefix.Addr().Is4() {
		active = m.readyIPv4
	}
	delete(m.rib, prefix)
	m.mu.Unlock()
	if active && existed {
		m.onEvent(Event{Type: Withdraw, Old: &old})
	}
}
