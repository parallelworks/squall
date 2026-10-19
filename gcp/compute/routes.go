package compute

import (
	"net/http"
	"net/netip"
	"strconv"
)

// Route is a static route of a VPC network, in the API's JSON shape. Its
// network and next hop are recorded, not checked.
type Route struct {
	Kind              string   `json:"kind"`
	ID                uint64   `json:"id,string"`
	CreationTimestamp string   `json:"creationTimestamp"`
	Name              string   `json:"name"`
	Description       string   `json:"description,omitzero"`
	Network           string   `json:"network"`
	Tags              []string `json:"tags,omitzero"`
	DestRange         string   `json:"destRange"`
	Priority          *uint32  `json:"priority,omitzero"`
	NextHopInstance   string   `json:"nextHopInstance,omitzero"`
	NextHopIP         string   `json:"nextHopIp,omitzero"`
	NextHopGateway    string   `json:"nextHopGateway,omitzero"`
	RouteType         string   `json:"routeType,omitzero"`
	SelfLink          string   `json:"selfLink"`
}

var routeFields = fieldSet{names: []string{"name", "description", "id", "network", "destRange"}}

func (rt *Route) filterValue(field string) string {
	switch field {
	case "name":
		return rt.Name
	case "description":
		return rt.Description
	case "id":
		return strconv.FormatUint(rt.ID, 10)
	case "network":
		return rt.Network
	case "destRange":
		return rt.DestRange
	}
	return ""
}

func routePath(global, name string) string { return global + "/routes/" + name }

// validateRoute checks rt and fills in what it leaves empty, for project.
func validateRoute(project string, rt *Route) *apiError {
	if !validName(rt.Name) {
		return invalid("Invalid value for field 'resource.name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", rt.Name)
	}
	if p, err := netip.ParsePrefix(rt.DestRange); err != nil || p.Masked() != p {
		return invalid("Invalid value for field 'resource.destRange': '%s'. Must be a valid CIDR range.", rt.DestRange)
	}
	if rt.Network == "" {
		return invalid("Invalid value for field 'resource.network': ''. The network must be specified.")
	}
	hops := 0
	for _, hop := range []string{rt.NextHopInstance, rt.NextHopIP, rt.NextHopGateway} {
		if hop != "" {
			hops++
		}
	}
	if hops != 1 {
		return invalid("Invalid value for field 'resource': a route needs exactly one of nextHopInstance, nextHopIp and nextHopGateway.")
	}
	if rt.NextHopIP != "" {
		if _, err := netip.ParseAddr(rt.NextHopIP); err != nil {
			return invalid("Invalid value for field 'resource.nextHopIp': '%s'.", rt.NextHopIP)
		}
	}
	rt.Network = selfLinkBase + relPath(rt.Network, project)
	if rt.NextHopInstance != "" {
		rt.NextHopInstance = selfLinkBase + relPath(rt.NextHopInstance, project)
	}
	if rt.NextHopGateway != "" {
		rt.NextHopGateway = selfLinkBase + relPath(rt.NextHopGateway, project)
	}
	if rt.Priority == nil {
		rt.Priority = new(uint32(1000))
	}
	return nil
}

func (s *Server) insertRoute(r *http.Request, opFault *apiError) (any, *apiError) {
	var want Route
	if err := decodeStrict(r, &want); err != nil {
		return nil, err
	}
	if err := validateRoute(r.PathValue("project"), &want); err != nil {
		return nil, err
	}
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	global := scopeOf(r)
	path := routePath(global, want.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	if _, ok := lookupIn(s.routes, global, want.Name); ok {
		return nil, alreadyExists(path)
	}
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "insert", path, 0, opFault, nil)), nil
	}
	rt := want
	rt.Kind, rt.ID, rt.CreationTimestamp = "compute#route", s.newID(), timestamp(s.now())
	rt.RouteType, rt.SelfLink = "STATIC", selfLinkBase+path
	scoped(s.routes, global).put(rt.Name, &rt)
	return encoded(s.startOperation(r, reqID, "insert", path, rt.ID, nil, nil)), nil
}

func (s *Server) getRoute(r *http.Request, _ *apiError) (any, *apiError) {
	global, name := scopeOf(r), r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := lookupIn(s.routes, global, name)
	if !ok {
		return nil, notFound(routePath(global, name))
	}
	return encoded(rt), nil
}

func (s *Server) deleteRoute(r *http.Request, opFault *apiError) (any, *apiError) {
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	global, name := scopeOf(r), r.PathValue("name")
	path := routePath(global, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	rt, ok := lookupIn(s.routes, global, name)
	if !ok {
		return nil, notFound(path)
	}
	if opFault == nil {
		s.routes[global].remove(name)
	}
	return encoded(s.startOperation(r, reqID, "delete", path, rt.ID, opFault, nil)), nil
}

func (s *Server) listRoutes(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, routeFields)
	if err != nil {
		return nil, err
	}
	global := scopeOf(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	return listScope(s.routes[global], q, "compute#routeList", global+"/routes", nil), nil
}

// Routes returns every route the server holds, ordered by selfLink.
func (s *Server) Routes() []Route {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Route
	each(s.routes, func(rt *Route) { out = append(out, jsonClone(rt)) })
	return out
}
