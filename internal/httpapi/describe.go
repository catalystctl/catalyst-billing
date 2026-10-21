package httpapi

// The route table's neutral projection.
//
// The API document is generated from the same table that builds the router, which is what
// makes "documented" and "served" the same set. The projection exists so the generator does
// not import this package: it describes routes, it does not depend on how they are served.

// RouteDescription is one route, described without any handler or server state.
type RouteDescription struct {
	Method  string
	Pattern string
	// Access is "public", "user" or "permission".
	Access     string
	Permission string
	StaffOnly  bool
	CSRF       bool
	Idempotent bool
	// Rate is "read", "mutation", "export" or "".
	Rate string
}

// DescribeRoutes returns the route table as served by a Server built with the standard
// configuration.
//
// It builds a Server with no dependencies, because building the table does not touch the
// database: the handlers are values, not calls. That is what lets the document be generated
// in CI without a PostgreSQL instance.
func DescribeRoutes() []RouteDescription {
	s := New(Config{}, nil, nil, "", "", nil)
	s.Handler()
	out := make([]RouteDescription, 0, len(s.Routes()))
	for _, rt := range s.Routes() {
		out = append(out, RouteDescription{
			Method:     rt.Method,
			Pattern:    rt.Pattern,
			Access:     rt.Access.String(),
			Permission: string(rt.Permission),
			StaffOnly:  rt.StaffOnly,
			CSRF:       rt.CSRF,
			Idempotent: rt.Idempotent,
			Rate:       string(rt.Rate),
		})
	}
	return out
}

// String renders an Access for the document.
func (a Access) String() string {
	switch a {
	case AccessPublic:
		return "public"
	case AccessUser:
		return "user"
	case AccessPermission:
		return "permission"
	default:
		return "unknown"
	}
}
