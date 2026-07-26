package goalrun

import (
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/routecontract"
)

// ParsedRouteRequirement re-exports the shared Gate-1 strict contract type.
type ParsedRouteRequirement = routecontract.ParsedRouteRequirement

// ParseRouteRequirement is the shared strict parser (routecontract).
func ParseRouteRequirement(routeReq string) (ParsedRouteRequirement, error) {
	return routecontract.ParseRouteRequirement(routeReq)
}

// TaskClassFromRoute returns only the class field via ParseRouteRequirement.
func TaskClassFromRoute(routeReq string) (capclass.Class, error) {
	p, err := routecontract.ParseRouteRequirement(routeReq)
	if err != nil {
		return "", err
	}
	return p.Class, nil
}
