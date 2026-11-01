package compute

import (
	"cmp"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

const maxFirewallPriority = 65535

// Firewall is a VPC firewall rule, in the API's JSON shape.
type Firewall struct {
	Kind                  string             `json:"kind"`
	ID                    uint64             `json:"id,string"`
	CreationTimestamp     string             `json:"creationTimestamp"`
	Name                  string             `json:"name"`
	Description           string             `json:"description,omitzero"`
	Network               string             `json:"network"`
	Priority              *int               `json:"priority,omitzero"`
	Direction             string             `json:"direction"`
	Allowed               []FirewallProtocol `json:"allowed,omitzero"`
	Denied                []FirewallProtocol `json:"denied,omitzero"`
	SourceRanges          []string           `json:"sourceRanges,omitzero"`
	DestinationRanges     []string           `json:"destinationRanges,omitzero"`
	SourceTags            []string           `json:"sourceTags,omitzero"`
	TargetTags            []string           `json:"targetTags,omitzero"`
	SourceServiceAccounts []string           `json:"sourceServiceAccounts,omitzero"`
	TargetServiceAccounts []string           `json:"targetServiceAccounts,omitzero"`
	Disabled              bool               `json:"disabled"`
	LogConfig             *FirewallLogConfig `json:"logConfig,omitzero"`
	SelfLink              string             `json:"selfLink"`
}

// FirewallProtocol is a protocol, and optionally ports, a rule allows or denies.
type FirewallProtocol struct {
	IPProtocol string   `json:"IPProtocol"`
	Ports      []string `json:"ports,omitzero"`
}

// FirewallLogConfig is whether and what a rule logs.
type FirewallLogConfig struct {
	Enable   bool   `json:"enable"`
	Metadata string `json:"metadata,omitzero"`
}

func (f *Firewall) clone() Firewall { return jsonClone(f) }

var firewallFields = fieldSet{names: []string{"name", "description", "id", "network", "direction"}}

func (f *Firewall) filterValue(field string) string {
	switch field {
	case "name":
		return f.Name
	case "description":
		return f.Description
	case "id":
		return strconv.FormatUint(f.ID, 10)
	case "network":
		return f.Network
	case "direction":
		return f.Direction
	}
	return ""
}

func firewallPath(global, name string) string { return global + "/firewalls/" + name }

var firewallProtocols = []string{"tcp", "udp", "icmp", "esp", "ah", "sctp", "ipip", "all"}

// validateFirewall checks f and fills in its defaults.
func validateFirewall(f *Firewall) *apiError {
	if !validName(f.Name) {
		return invalid("Invalid value for field 'resource.name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", f.Name)
	}
	f.Direction = cmp.Or(f.Direction, "INGRESS")
	if f.Direction != "INGRESS" && f.Direction != "EGRESS" {
		return invalid("Invalid value for field 'resource.direction': '%s'.", f.Direction)
	}
	if f.Priority == nil {
		f.Priority = new(1000)
	}
	if p := *f.Priority; p < 0 || p > maxFirewallPriority {
		return invalid("Invalid value for field 'resource.priority': '%d'. Must be between 0 and %d.", p, maxFirewallPriority)
	}
	if (len(f.Allowed) == 0) == (len(f.Denied) == 0) {
		return invalid("Invalid value for field 'resource': a firewall rule needs either allowed or denied protocols, not both.")
	}
	for _, list := range []struct {
		kind      string
		protocols []FirewallProtocol
	}{{"allowed", f.Allowed}, {"denied", f.Denied}} {
		for i, p := range list.protocols {
			field := fmt.Sprintf("resource.%s[%d]", list.kind, i)
			if _, err := strconv.ParseUint(p.IPProtocol, 10, 8); err != nil && !slices.Contains(firewallProtocols, strings.ToLower(p.IPProtocol)) {
				return invalid("Invalid value for field '%s.IPProtocol': '%s'.", field, p.IPProtocol)
			}
			proto := strings.ToLower(p.IPProtocol)
			if len(p.Ports) > 0 && proto != "tcp" && proto != "udp" && proto != "sctp" && proto != "all" {
				return invalid("Invalid value for field '%s.ports': ports are only allowed for tcp, udp, sctp and all.", field)
			}
			for _, port := range p.Ports {
				if !validPortRange(port) {
					return invalid("Invalid value for field '%s.ports': '%s'.", field, port)
				}
			}
		}
	}
	for _, list := range []struct {
		field  string
		ranges []string
	}{{"sourceRanges", f.SourceRanges}, {"destinationRanges", f.DestinationRanges}} {
		for i, r := range list.ranges {
			if _, err := netip.ParsePrefix(r); err != nil {
				if _, err := netip.ParseAddr(r); err != nil {
					return invalid("Invalid value for field 'resource.%s[%d]': '%s'. Must be a CIDR address range that is contained in the network.", list.field, i, r)
				}
			}
		}
	}
	if f.Direction == "EGRESS" && (len(f.SourceRanges) > 0 || len(f.SourceTags) > 0 || len(f.SourceServiceAccounts) > 0) {
		return invalid("Invalid value for field 'resource': an EGRESS rule cannot have sources.")
	}
	if f.Direction == "INGRESS" && len(f.DestinationRanges) > 0 {
		return invalid("Invalid value for field 'resource.destinationRanges': an INGRESS rule cannot have destination ranges.")
	}
	if len(f.TargetTags) > 0 && len(f.TargetServiceAccounts) > 0 || len(f.SourceTags) > 0 && len(f.SourceServiceAccounts) > 0 {
		return invalid("Invalid value for field 'resource': a rule cannot mix network tags and service accounts.")
	}
	if lc := f.LogConfig; lc != nil && lc.Metadata != "" && lc.Metadata != "INCLUDE_ALL_METADATA" && lc.Metadata != "EXCLUDE_ALL_METADATA" {
		return invalid("Invalid value for field 'resource.logConfig.metadata': '%s'.", lc.Metadata)
	}
	return nil
}

// validPortRange reports whether s is a port, such as "22", or a range, such as "8000-8080".
func validPortRange(s string) bool {
	lo, hi, isRange := strings.Cut(s, "-")
	a, err := strconv.ParseUint(lo, 10, 16)
	if err != nil {
		return false
	}
	if !isRange {
		return true
	}
	b, err := strconv.ParseUint(hi, 10, 16)
	return err == nil && a <= b
}

func (s *Server) insertFirewall(r *http.Request, opFault *apiError) (any, *apiError) {
	var want Firewall
	if err := decodeStrict(r, &want); err != nil {
		return nil, err
	}
	if err := validateFirewall(&want); err != nil {
		return nil, err
	}
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	project, global := r.PathValue("project"), scopeOf(r)
	path := firewallPath(global, want.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	if _, ok := lookupIn(s.firewalls, global, want.Name); ok {
		return nil, alreadyExists(path)
	}
	n, err := s.resolveNetwork(project, cmp.Or(want.Network, "default"))
	if err != nil {
		return nil, err
	}
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "insert", path, 0, opFault, nil)), nil
	}
	f := want.clone()
	f.Kind, f.ID, f.CreationTimestamp = "compute#firewall", s.newID(), timestamp(s.now())
	f.Network, f.SelfLink = n.SelfLink, selfLinkBase+path
	scoped(s.firewalls, global).put(f.Name, &f)
	return encoded(s.startOperation(r, reqID, "insert", path, f.ID, nil, nil)), nil
}

func (s *Server) getFirewall(r *http.Request, _ *apiError) (any, *apiError) {
	global, name := scopeOf(r), r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := lookupIn(s.firewalls, global, name)
	if !ok {
		return nil, notFound(firewallPath(global, name))
	}
	return encoded(f), nil
}

func (s *Server) deleteFirewall(r *http.Request, opFault *apiError) (any, *apiError) {
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	global, name := scopeOf(r), r.PathValue("name")
	path := firewallPath(global, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	f, ok := lookupIn(s.firewalls, global, name)
	if !ok {
		return nil, notFound(path)
	}
	if opFault == nil {
		s.firewalls[global].remove(name)
	}
	return encoded(s.startOperation(r, reqID, "delete", path, f.ID, opFault, nil)), nil
}

func (s *Server) listFirewalls(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, firewallFields)
	if err != nil {
		return nil, err
	}
	global := scopeOf(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	return listScope(s.firewalls[global], q, "compute#firewallList", global+"/firewalls", nil), nil
}

// Firewalls returns every firewall rule the server holds, ordered by selfLink.
func (s *Server) Firewalls() []Firewall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Firewall
	each(s.firewalls, func(f *Firewall) { out = append(out, f.clone()) })
	return out
}
