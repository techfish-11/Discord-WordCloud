package bgpmonitor

import (
	"fmt"
	"net/netip"
	"testing"

	api "github.com/osrg/gobgp/v3/api"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestInitialRIBIsBaselineAndSubsequentChangesEmit(t *testing.T) {
	var events []Event
	m := &Monitor{onEvent: func(event Event) { events = append(events, event) }, rib: make(map[netip.Prefix]Route)}

	m.handlePath(testPath(t, "2001:db8::/32", []uint32{64500, 65001}, false))
	if len(events) != 0 {
		t.Fatalf("initial route emitted %d events", len(events))
	}
	m.handleResponse(&api.WatchEventResponse{Event: &api.WatchEventResponse_Table{Table: &api.WatchEventResponse_TableEvent{Paths: []*api.Path{{Family: ipv6Family()}}}}})

	m.handlePath(testPath(t, "2001:db8:1::/48", []uint32{64500, 65001}, false))
	m.handlePath(testPath(t, "2001:db8::/32", []uint32{64500, 64496, 65001}, false))
	m.handlePath(testPath(t, "2001:db8::/32", nil, true))

	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	if events[0].Type != Announce || events[1].Type != PathChange || events[2].Type != Withdraw {
		t.Fatalf("unexpected event sequence: %#v", events)
	}
	if events[1].Old.ASPath != "64500 65001" || events[1].New.ASPath != "64500 64496 65001" {
		t.Fatalf("unexpected paths: %#v", events[1])
	}
}

func TestResetSuppressesReconnectTable(t *testing.T) {
	var events []Event
	m := &Monitor{onEvent: func(event Event) { events = append(events, event) }, rib: make(map[netip.Prefix]Route), readyIPv6: true}
	m.handlePath(testPath(t, "2001:db8::/32", []uint32{65001}, false))
	m.resetBaseline()
	m.handlePath(testPath(t, "2001:db8::/32", []uint32{65001}, false))
	if len(events) != 1 {
		t.Fatalf("reconnect baseline emitted an event: %#v", events)
	}
}

func TestIPv4BaselineAndChanges(t *testing.T) {
	var events []Event
	m := &Monitor{onEvent: func(event Event) { events = append(events, event) }, rib: make(map[netip.Prefix]Route)}
	m.handlePath(testPath(t, "192.0.2.0/24", []uint32{64500, 65001}, false))
	if len(events) != 0 {
		t.Fatalf("initial IPv4 route emitted %d events", len(events))
	}
	m.handleResponse(&api.WatchEventResponse{Event: &api.WatchEventResponse_Table{Table: &api.WatchEventResponse_TableEvent{Paths: []*api.Path{{Family: ipv4Family()}}}}})
	m.handlePath(testPath(t, "198.51.100.0/24", []uint32{64500, 65002}, false))
	if len(events) != 1 || events[0].Type != Announce || !events[0].New.Prefix.Addr().Is4() {
		t.Fatalf("unexpected IPv4 events: %#v", events)
	}
}

func TestAddressFamilyBaselinesAreIndependent(t *testing.T) {
	var events []Event
	m := &Monitor{onEvent: func(event Event) { events = append(events, event) }, rib: make(map[netip.Prefix]Route)}
	m.handleResponse(&api.WatchEventResponse{Event: &api.WatchEventResponse_Table{Table: &api.WatchEventResponse_TableEvent{Paths: []*api.Path{{Family: ipv4Family()}}}}})
	m.handlePath(testPath(t, "198.51.100.0/24", []uint32{65001}, false))
	m.handlePath(testPath(t, "2001:db8::/32", []uint32{65001}, false))
	if len(events) != 1 || !events[0].New.Prefix.Addr().Is4() {
		t.Fatalf("IPv6 emitted before its EOR: %#v", events)
	}
}

func TestRouteRejectsAmbiguousOriginASSet(t *testing.T) {
	nlri, err := anypb.New(&api.IPAddressPrefix{Prefix: "2001:db8::", PrefixLen: 32})
	if err != nil {
		t.Fatal(err)
	}
	attr, err := anypb.New(&api.AsPathAttribute{Segments: []*api.AsSegment{{Type: api.AsSegment_AS_SET, Numbers: []uint32{65001, 65002}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := routeFromAPI(&api.Path{Family: ipv6Family(), Nlri: nlri, Pattrs: []*anypb.Any{attr}}, 65010); err == nil {
		t.Fatal("expected ambiguous AS_SET origin to be rejected")
	}
}

func testPath(t *testing.T, prefix string, asns []uint32, withdraw bool) *api.Path {
	t.Helper()
	var address string
	var length uint32
	for i, r := range prefix {
		if r == '/' {
			address = prefix[:i]
			_, err := fmt.Sscanf(prefix[i+1:], "%d", &length)
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	nlri, err := anypb.New(&api.IPAddressPrefix{Prefix: address, PrefixLen: length})
	if err != nil {
		t.Fatal(err)
	}
	family := ipv6Family()
	if netip.MustParsePrefix(prefix).Addr().Is4() {
		family = ipv4Family()
	}
	path := &api.Path{Family: family, Nlri: nlri, IsWithdraw: withdraw}
	if asns != nil {
		attr, err := anypb.New(&api.AsPathAttribute{Segments: []*api.AsSegment{{Type: api.AsSegment_AS_SEQUENCE, Numbers: asns}}})
		if err != nil {
			t.Fatal(err)
		}
		path.Pattrs = []*anypb.Any{attr}
	}
	return path
}

func ipv6Family() *api.Family {
	return &api.Family{Afi: api.Family_AFI_IP6, Safi: api.Family_SAFI_UNICAST}
}

func ipv4Family() *api.Family {
	return &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}
}
