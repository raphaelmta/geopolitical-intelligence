package api

// RouteInfo describes one entry of the route table the router registers from.
type RouteInfo struct{ Method, Pattern, Role string }

// Routes returns the route table, for tests.
func Routes() []RouteInfo {
	out := make([]RouteInfo, len(routeTable))
	for i, r := range routeTable {
		out[i] = RouteInfo{Method: r.method, Pattern: r.pattern, Role: r.role}
	}
	return out
}
