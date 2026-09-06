package bgpmonitor

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	api "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/apiutil"
	"github.com/osrg/gobgp/v3/pkg/packet/bgp"
)

// Route is the subset of a BGP path needed by the monitor.
type Route struct {
	Prefix    netip.Prefix
	ASPath    string
	OriginASN uint32
}

func routeFromAPI(path *api.Path, localOrigin uint32) (Route, error) {
	prefix, err := prefixFromAPI(path)
	if err != nil {
		return Route{}, err
	}

	attrs, err := apiutil.UnmarshalPathAttributes(path.Pattrs)
	if err != nil {
		return Route{}, fmt.Errorf("decode path attributes: %w", err)
	}
	for _, attr := range attrs {
		asPath, ok := attr.(*bgp.PathAttributeAsPath)
		if !ok {
			continue
		}
		formatted, origin, ok := formatASPath(asPath.Value)
		if !ok {
			if len(asPath.Value) == 0 && localOrigin != 0 {
				return Route{Prefix: prefix.Masked(), ASPath: "(empty)", OriginASN: localOrigin}, nil
			}
			return Route{}, fmt.Errorf("AS_PATH has no unambiguous origin ASN")
		}
		return Route{Prefix: prefix.Masked(), ASPath: formatted, OriginASN: origin}, nil
	}
	return Route{}, fmt.Errorf("path has no AS_PATH")
}

func prefixFromAPI(path *api.Path) (netip.Prefix, error) {
	if path == nil || path.Nlri == nil {
		return netip.Prefix{}, fmt.Errorf("path has no NLRI")
	}
	if path.Family == nil || path.Family.Afi != api.Family_AFI_IP6 || path.Family.Safi != api.Family_SAFI_UNICAST {
		return netip.Prefix{}, fmt.Errorf("path is not IPv6 unicast")
	}
	nlri, err := apiutil.UnmarshalNLRI(bgp.RF_IPv6_UC, path.Nlri)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("decode NLRI: %w", err)
	}
	prefix, err := netip.ParsePrefix(nlri.String())
	if err != nil || !prefix.Addr().Is6() {
		return netip.Prefix{}, fmt.Errorf("invalid IPv6 prefix %q", nlri.String())
	}
	return prefix.Masked(), nil
}

func formatASPath(segments []bgp.AsPathParamInterface) (string, uint32, bool) {
	parts := make([]string, 0, len(segments))
	var origin uint32
	found := false
	for _, segment := range segments {
		asns := segment.GetAS()
		if len(asns) == 0 {
			continue
		}
		values := make([]string, len(asns))
		for i, asn := range asns {
			values[i] = strconv.FormatUint(uint64(asn), 10)
		}
		switch segment.GetType() {
		case bgp.BGP_ASPATH_ATTR_TYPE_SEQ:
			parts = append(parts, strings.Join(values, " "))
			origin, found = asns[len(asns)-1], true
		case bgp.BGP_ASPATH_ATTR_TYPE_SET:
			parts = append(parts, "{"+strings.Join(values, ",")+"}")
			// An AS_SET has no single, well-defined origin. Do not attribute the
			// route to an arbitrary member when it is the rightmost segment.
			origin, found = 0, false
		default:
			parts = append(parts, "("+strings.Join(values, " ")+")")
		}
	}
	return strings.Join(parts, " "), origin, found
}
