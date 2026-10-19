package compute

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// ResourcePolicy is a regional resource policy, in the API's JSON shape. The
// server keeps group placement policies, the only kind it accepts, as the
// caller sent them.
type ResourcePolicy struct {
	Kind                 string          `json:"kind"`
	ID                   uint64          `json:"id,string"`
	CreationTimestamp    string          `json:"creationTimestamp"`
	SelfLink             string          `json:"selfLink"`
	Region               string          `json:"region"`
	Name                 string          `json:"name"`
	Description          string          `json:"description,omitzero"`
	Status               string          `json:"status"`
	GroupPlacementPolicy json.RawMessage `json:"groupPlacementPolicy,omitzero"`
}

// InstanceTemplate is a global instance template, in the API's JSON shape.
// Its properties are kept as the caller sent them.
type InstanceTemplate struct {
	Kind              string          `json:"kind"`
	ID                uint64          `json:"id,string"`
	CreationTimestamp string          `json:"creationTimestamp"`
	Name              string          `json:"name"`
	Description       string          `json:"description,omitzero"`
	Properties        json.RawMessage `json:"properties"`
	SelfLink          string          `json:"selfLink"`
}

var namedFields = fieldSet{names: []string{"name", "description", "id"}}

var policyFields = fieldSet{names: []string{"name", "description", "status", "id"}}

func (p *ResourcePolicy) filterValue(field string) string {
	switch field {
	case "name":
		return p.Name
	case "description":
		return p.Description
	case "status":
		return p.Status
	case "id":
		return strconv.FormatUint(p.ID, 10)
	}
	return ""
}

func (t *InstanceTemplate) filterValue(field string) string {
	switch field {
	case "name":
		return t.Name
	case "description":
		return t.Description
	case "id":
		return strconv.FormatUint(t.ID, 10)
	}
	return ""
}

func policyPath(region, name string) string { return region + "/resourcePolicies/" + name }

// resolvePolicy returns the link of the resource policy ref names in region:
// a bare name, as bulk insertions take, or a URL. field names ref in errors.
// The caller holds s.mu.
func (s *Server) resolvePolicy(region, field, ref string) (string, *apiError) {
	path := ref
	if !strings.Contains(ref, "/") {
		path = policyPath(region, ref)
	}
	inRegion, name, ok := strings.Cut(relPath(path, projectOf(region)), "/resourcePolicies/")
	if !ok || inRegion != region {
		return "", invalid("Invalid value for field '%s': '%s'. The resource policy must be in region '%s'.", field, ref, region)
	}
	if _, ok := lookupIn(s.policies, region, name); !ok {
		return "", missingRef(field, ref, "resourcePolicy")
	}
	return selfLinkBase + policyPath(region, name), nil
}

func (s *Server) insertPolicy(r *http.Request, opFault *apiError) (any, *apiError) {
	var want ResourcePolicy
	if err := decodeStrict(r, &want); err != nil {
		return nil, err
	}
	if !validName(want.Name) {
		return nil, invalid("Invalid value for field 'resource.name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", want.Name)
	}
	if len(want.GroupPlacementPolicy) == 0 {
		return nil, invalid("Invalid value for field 'resource': a resource policy needs a groupPlacementPolicy.")
	}
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	region := scopeOf(r)
	path := policyPath(region, want.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	if _, ok := lookupIn(s.policies, region, want.Name); ok {
		return nil, alreadyExists(path)
	}
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "insert", path, 0, opFault, nil)), nil
	}
	p := &ResourcePolicy{
		Kind: "compute#resourcePolicy", ID: s.newID(), CreationTimestamp: timestamp(s.now()),
		SelfLink: betaSelfLinkBase + path, Region: betaSelfLinkBase + region, Name: want.Name,
		Description: want.Description, Status: "READY", GroupPlacementPolicy: want.GroupPlacementPolicy,
	}
	scoped(s.policies, region).put(p.Name, p)
	return encoded(s.startOperation(r, reqID, "insert", path, p.ID, nil, nil)), nil
}

func (s *Server) getPolicy(r *http.Request, _ *apiError) (any, *apiError) {
	region, name := scopeOf(r), r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := lookupIn(s.policies, region, name)
	if !ok {
		return nil, notFound(policyPath(region, name))
	}
	return encoded(p), nil
}

func (s *Server) deletePolicy(r *http.Request, opFault *apiError) (any, *apiError) {
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	region, name := scopeOf(r), r.PathValue("name")
	path := policyPath(region, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	p, ok := lookupIn(s.policies, region, name)
	if !ok {
		return nil, notFound(path)
	}
	var user string
	each(s.instances, func(inst *Instance) {
		for _, link := range inst.ResourcePolicies {
			if user == "" && link == selfLinkBase+path {
				user = strings.TrimPrefix(inst.SelfLink, selfLinkBase)
			}
		}
	})
	if user != "" {
		return nil, inUse("resource_policy", path, user)
	}
	if opFault == nil {
		s.policies[region].remove(name)
	}
	return encoded(s.startOperation(r, reqID, "delete", path, p.ID, opFault, nil)), nil
}

func (s *Server) listPolicies(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, policyFields)
	if err != nil {
		return nil, err
	}
	region := scopeOf(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	list := listScope(s.policies[region], q, "compute#resourcePolicyList", region+"/resourcePolicies", nil)
	list.SelfLink = betaSelfLinkBase + list.ID
	return list, nil
}

// ResourcePolicies returns every resource policy the server holds, ordered by selfLink.
func (s *Server) ResourcePolicies() []ResourcePolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ResourcePolicy
	each(s.policies, func(p *ResourcePolicy) { out = append(out, jsonClone(p)) })
	return out
}

func templatePath(global, name string) string { return global + "/instanceTemplates/" + name }

// templateProperties returns the properties of the template ref names, for
// a bulk insertion in project. The caller holds s.mu.
func (s *Server) templateProperties(project, ref string) (json.RawMessage, *apiError) {
	path := ref
	if !strings.Contains(ref, "/") {
		path = "global/instanceTemplates/" + ref
	}
	global, name, ok := strings.Cut(relPath(path, project), "/instanceTemplates/")
	if !ok || !strings.HasSuffix(global, "/global") {
		return nil, invalid("Invalid value for field 'resource.sourceInstanceTemplate': '%s'.", ref)
	}
	t, ok := lookupIn(s.templates, global, name)
	if !ok {
		return nil, missingRef("resource.sourceInstanceTemplate", ref, "instanceTemplate")
	}
	return t.Properties, nil
}

func (s *Server) insertTemplate(r *http.Request, opFault *apiError) (any, *apiError) {
	var want InstanceTemplate
	if err := decodeStrict(r, &want); err != nil {
		return nil, err
	}
	if !validName(want.Name) {
		return nil, invalid("Invalid value for field 'resource.name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", want.Name)
	}
	var props Instance
	if err := unmarshalStrict(want.Properties, &props); err != nil || props.MachineType == "" || props.Name != "" {
		return nil, invalid("Invalid value for field 'resource.properties': the properties need a machineType and take the fields of an instance's properties.")
	}
	if err := validateLabels("resource.properties.labels", props.Labels); err != nil {
		return nil, err
	}
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	global := scopeOf(r)
	path := templatePath(global, want.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	if _, ok := lookupIn(s.templates, global, want.Name); ok {
		return nil, alreadyExists(path)
	}
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "insert", path, 0, opFault, nil)), nil
	}
	t := &InstanceTemplate{
		Kind: "compute#instanceTemplate", ID: s.newID(), CreationTimestamp: timestamp(s.now()),
		Name: want.Name, Description: want.Description, Properties: want.Properties, SelfLink: selfLinkBase + path,
	}
	scoped(s.templates, global).put(t.Name, t)
	return encoded(s.startOperation(r, reqID, "insert", path, t.ID, nil, nil)), nil
}

func (s *Server) getTemplate(r *http.Request, _ *apiError) (any, *apiError) {
	global, name := scopeOf(r), r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := lookupIn(s.templates, global, name)
	if !ok {
		return nil, notFound(templatePath(global, name))
	}
	return encoded(t), nil
}

func (s *Server) deleteTemplate(r *http.Request, opFault *apiError) (any, *apiError) {
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	global, name := scopeOf(r), r.PathValue("name")
	path := templatePath(global, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	t, ok := lookupIn(s.templates, global, name)
	if !ok {
		return nil, notFound(path)
	}
	if opFault == nil {
		s.templates[global].remove(name)
	}
	return encoded(s.startOperation(r, reqID, "delete", path, t.ID, opFault, nil)), nil
}

func (s *Server) listTemplates(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, namedFields)
	if err != nil {
		return nil, err
	}
	global := scopeOf(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	return listScope(s.templates[global], q, "compute#instanceTemplateList", global+"/instanceTemplates", nil), nil
}

// InstanceTemplates returns every instance template the server holds, ordered by selfLink.
func (s *Server) InstanceTemplates() []InstanceTemplate {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []InstanceTemplate
	each(s.templates, func(t *InstanceTemplate) { out = append(out, jsonClone(t)) })
	return out
}

// groupManager stands in for instance group managers, which the server
// never holds: their aggregated list is always empty.
type groupManager struct{}

func (groupManager) filterValue(string) string { return "" }

func (s *Server) aggregatedGroupManagers(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, namedFields)
	if err != nil {
		return nil, err
	}
	return listAggregated(map[string]*collection[groupManager]{}, r.PathValue("project"), q,
		"compute#instanceGroupManagerAggregatedList", "instanceGroupManagers", nil), nil
}
