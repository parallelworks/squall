package compute

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/parallelworks/squall/internal/reason"
)

const (
	resizeAccepted  = "ACCEPTED"
	resizeCreating  = "CREATING"
	resizeSucceeded = "SUCCEEDED"
	resizeFailed    = "FAILED"
	resizeCancelled = "CANCELLED"
)

// ResizeRequest is a request to add instances to a zonal managed instance
// group, in the API's JSON shape.
type ResizeRequest struct {
	Kind                 string              `json:"kind"`
	ID                   uint64              `json:"id,string"`
	CreationTimestamp    string              `json:"creationTimestamp"`
	Name                 string              `json:"name"`
	Description          string              `json:"description,omitzero"`
	Zone                 string              `json:"zone"`
	ResizeBy             int                 `json:"resizeBy,omitzero"`
	Instances            []PerInstanceConfig `json:"instances,omitzero"`
	RequestedRunDuration *Duration           `json:"requestedRunDuration,omitzero"`
	State                string              `json:"state"`
	Status               ResizeRequestStatus `json:"status"`
	SelfLink             string              `json:"selfLink"`

	group *InstanceGroupManager
	op    *operation
	names []string
}

// Duration is a span of time, in the API's JSON shape.
type Duration struct {
	Seconds Int64 `json:"seconds"`
	Nanos   int   `json:"nanos,omitzero"`
}

// ResizeRequestStatus is what went wrong with a resize request: Error once
// it has FAILED, or LastAttempt while it waits to try again.
type ResizeRequestStatus struct {
	Error       *OperationErrors      `json:"error,omitzero"`
	LastAttempt *ResizeRequestAttempt `json:"lastAttempt,omitzero"`
}

// ResizeRequestAttempt is a resize request's last failed attempt at its instances.
type ResizeRequestAttempt struct {
	Error OperationErrors `json:"error"`
}

var resizeFields = fieldSet{names: []string{"name", "description", "id", "state"}}

func (req *ResizeRequest) filterValue(field string) string {
	switch field {
	case "name":
		return req.Name
	case "description":
		return req.Description
	case "id":
		return strconv.FormatUint(req.ID, 10)
	case "state":
		return req.State
	}
	return ""
}

func resizePath(g *InstanceGroupManager, name string) string {
	return groupPath(g.zone, g.Name) + "/resizeRequests/" + name
}

func (req *ResizeRequest) active() bool {
	return req.State == resizeAccepted || req.State == resizeCreating
}

func errorsOf(code, message string) *OperationErrors {
	if code == "" && message == "" {
		return nil
	}
	return &OperationErrors{[]OperationError{{code, message}}}
}

// lookupRequest returns the resize request r names in the group it names.
// The caller holds s.mu.
func (s *Server) lookupRequest(r *http.Request) (*ResizeRequest, *apiError) {
	g, err := s.lookupGroup(r, "group")
	if err != nil {
		return nil, err
	}
	name := r.PathValue("name")
	req, ok := g.requests.byName[name]
	if !ok {
		return nil, notFound(resizePath(g, name))
	}
	return req, nil
}

func (s *Server) insertResizeRequest(r *http.Request, opFault *apiError) (any, *apiError) {
	var want ResizeRequest
	if err := decodeStrict(r, &want); err != nil {
		return nil, err
	}
	if !validName(want.Name) {
		return nil, invalid("Invalid value for field 'resource.name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", want.Name)
	}
	if (want.ResizeBy != 0) == (len(want.Instances) != 0) {
		return nil, invalid("Invalid value for field 'resource': exactly one of resizeBy and instances must be set.")
	}
	if want.ResizeBy < 0 || want.ResizeBy > maxGroupInstances {
		return nil, invalid("Invalid value for field 'resource.resizeBy': '%d'. Must be between 1 and %d.", want.ResizeBy, maxGroupInstances)
	}
	if d := want.RequestedRunDuration; d != nil && (d.Seconds < 0 || d.Nanos < 0 || d.Nanos > 999_999_999) {
		return nil, invalid("Invalid value for field 'resource.requestedRunDuration': seconds must not be negative and nanos must be from 0 to 999999999.")
	}
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	g, err := s.lookupGroup(r, "group")
	if err != nil {
		return nil, err
	}
	s.refresh(g)
	path := resizePath(g, want.Name)
	if g.requests.has(want.Name) {
		return nil, alreadyExists(path)
	}
	var names []string
	for _, c := range want.Instances {
		names = append(names, c.Name)
	}
	if len(names) > 0 && opFault == nil {
		conflict, err := s.checkNewNames(g, "resource.instances", names)
		if err != nil {
			return nil, err
		}
		if conflict == "" {
			conflict = s.queuedName(g, names)
		}
		if conflict != "" {
			i := slices.Index(names, conflict)
			msg := fmt.Sprintf("Invalid value for field 'resource.instances[%d].name': '%s'. Instance '%s' already exists", i, conflict, conflict)
			opFault = &apiError{Status: http.StatusBadRequest, Reason: "invalid", Message: msg, opErrors: []OperationError{{"INVALID_FIELD_VALUE", msg}}}
		}
	}
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "insert", path, 0, opFault, nil)), nil
	}
	req := &ResizeRequest{
		Kind: "compute#instanceGroupManagerResizeRequest", ID: s.newID(), CreationTimestamp: timestamp(s.now()),
		Name: want.Name, Description: want.Description, Zone: selfLinkBase + g.zone, ResizeBy: want.ResizeBy,
		Instances: want.Instances, RequestedRunDuration: want.RequestedRunDuration, State: resizeAccepted,
		SelfLink: selfLinkBase + path, group: g, names: names,
	}
	g.requests.put(req.Name, req)
	op := s.startOperation(r, reqID, "insert", path, req.ID, nil, nil)
	s.provision(req)
	return encoded(op), nil
}

// queuedName returns the first of names that another active resize request
// of g asks for, or "". The caller holds s.mu.
func (s *Server) queuedName(g *InstanceGroupManager, names []string) string {
	for _, name := range names {
		for _, other := range g.requests.byName {
			if other.State == resizeAccepted && slices.Contains(other.names, name) {
				return name
			}
		}
	}
	return ""
}

// provision fulfills req in the background: it is CREATING its instances on
// the last read [WithSettleAfter] allows, and SUCCEEDED once they run. The
// caller holds s.mu.
func (s *Server) provision(req *ResizeRequest) {
	var insts []*Instance
	var disks []*Disk
	create := func() bool {
		if req.State != resizeAccepted {
			return req.State == resizeCreating
		}
		g := req.group
		if len(req.names) == 0 {
			for range req.ResizeBy {
				req.names = append(req.names, s.memberName(g, req.names))
			}
		}
		type built struct {
			inst   *Instance
			disks  []*Disk
			claims []claim
		}
		var batch []built
		for _, name := range req.names {
			if g.members.has(name) {
				s.failRequest(req, "ALREADY_EXISTS", alreadyExists(instancePath(g.zone, name)).Message)
				return false
			}
			inst, d, c, err := s.buildMember(g, name)
			if err != nil {
				s.failRequest(req, reason.UpperSnake(err.Reason), err.Message)
				return false
			}
			batch = append(batch, built{inst, d, c})
		}
		for _, b := range batch {
			s.storeInstance(g.zone, s.regionOf(g.zone), b.inst, b.disks, b.claims)
			g.members.put(b.inst.Name, &member{name: b.inst.Name, template: g.InstanceTemplate, inst: b.inst})
			insts, disks = append(insts, b.inst), append(disks, b.disks...)
		}
		req.State, req.Status.LastAttempt = resizeCreating, nil
		if req.op != nil {
			track(req.op, insts, disks, true)
		}
		return true
	}
	op := s.inBackground(func() {
		if create() {
			s.provisioned(insts, disks)()
			req.State = resizeSucceeded
		}
		req.op = nil
	})
	if op.complete == nil {
		return
	}
	req.op = op
	op.progress = func() { create() }
}

// stopRequest stops req's provisioning, if it is in progress, leaving the
// instances it has created as they are. The caller holds s.mu.
func (s *Server) stopRequest(req *ResizeRequest) {
	op := req.op
	if op == nil {
		return
	}
	for _, inst := range op.staging {
		if inst.op == op {
			inst.op = nil
		}
	}
	op.complete, op.progress, op.staging = nil, nil, nil
	delete(s.pending, op)
	s.done(op)
	req.op = nil
}

// failRequest ends req in FAILED with an error of code and message, deleting
// the instances it created, as the API rolls them back. The caller holds s.mu.
func (s *Server) failRequest(req *ResizeRequest, code, message string) {
	s.stopRequest(req)
	g := req.group
	if req.State == resizeCreating {
		for _, name := range req.names {
			if m, ok := g.members.byName[name]; ok {
				if s.holds(g.zone, m.inst) {
					s.removeInstance(g.zone, m.inst)
				}
				g.members.remove(name)
			}
		}
	}
	req.State = resizeFailed
	req.Status = ResizeRequestStatus{Error: errorsOf(code, message)}
}

func (s *Server) getResizeRequest(r *http.Request, _ *apiError) (any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, err := s.lookupRequest(r)
	if err != nil {
		return nil, err
	}
	s.observe(req.op)
	return encoded(req), nil
}

func (s *Server) listResizeRequests(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, resizeFields)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.lookupGroup(r, "group")
	if err != nil {
		return nil, err
	}
	seen := s.observer()
	return listScope(g.requests, q, "compute#instanceGroupManagerResizeRequestList", groupPath(g.zone, g.Name)+"/resizeRequests",
		func(req *ResizeRequest) { seen(req.op) }), nil
}

func finalState(verb string) *apiError {
	return &apiError{Status: http.StatusPreconditionFailed, Reason: "conditionNotMet", Message: verb + " resize request that has reached a final state is not possible."}
}

func (s *Server) cancelResizeRequest(r *http.Request, opFault *apiError) (any, *apiError) {
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	req, err := s.lookupRequest(r)
	if err != nil {
		return nil, err
	}
	switch req.State {
	case resizeAccepted:
	case resizeCreating:
		return nil, &apiError{Status: http.StatusPreconditionFailed, Reason: "conditionNotMet", Message: "Cancelling resize request in state CREATING is not possible."}
	default:
		return nil, finalState("Cancelling")
	}
	if opFault == nil {
		s.stopRequest(req)
		req.State, req.Status = resizeCancelled, ResizeRequestStatus{}
	}
	return encoded(s.startOperation(r, reqID, "cancel", resizePath(req.group, req.Name), req.ID, opFault, nil)), nil
}

func (s *Server) deleteResizeRequest(r *http.Request, opFault *apiError) (any, *apiError) {
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	req, err := s.lookupRequest(r)
	if err != nil {
		return nil, err
	}
	if req.active() {
		return nil, &apiError{Status: http.StatusPreconditionFailed, Reason: "conditionNotMet", Message: fmt.Sprintf("Deleting resize request in state %s is not possible. Only inactive resize requests can be deleted.", req.State)}
	}
	if opFault == nil {
		req.group.requests.remove(req.Name)
	}
	return encoded(s.startOperation(r, reqID, "delete", resizePath(req.group, req.Name), req.ID, opFault, nil)), nil
}

// resizeRequest returns the named resize request of a group. The caller holds s.mu.
func (s *Server) resizeRequest(project, zone, group, name string) (*ResizeRequest, *apiError) {
	scope := "projects/" + project + "/zones/" + zone
	g, ok := lookupIn(s.groups, scope, group)
	if !ok {
		return nil, notFound(groupPath(scope, group))
	}
	req, ok := g.requests.byName[name]
	if !ok {
		return nil, notFound(resizePath(g, name))
	}
	return req, nil
}

// LookupResizeRequest returns the named resize request of the named group in
// project and zone.
func (s *Server) LookupResizeRequest(project, zone, group, name string) (ResizeRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, err := s.resizeRequest(project, zone, group, name)
	if err != nil {
		return ResizeRequest{}, false
	}
	return jsonClone(req), true
}

// FailResizeRequest ends the named resize request, which must be ACCEPTED or
// CREATING, in FAILED with an error of code and message in its status, as
// the API ends one it cannot fulfill, such as for want of quota. The
// instances it created are deleted. With no code and no message the status
// carries no error.
func (s *Server) FailResizeRequest(project, zone, group, name, code, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, err := s.resizeRequest(project, zone, group, name)
	if err != nil {
		return err
	}
	if !req.active() {
		return finalState("Failing")
	}
	s.failRequest(req, code, message)
	return nil
}

// FailResizeAttempt records a failed attempt at the named resize request,
// which must be ACCEPTED, in its status's lastAttempt, as a stockout does,
// and leaves it ACCEPTED to try again.
func (s *Server) FailResizeAttempt(project, zone, group, name, code, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, err := s.resizeRequest(project, zone, group, name)
	if err != nil {
		return err
	}
	if req.State != resizeAccepted {
		return &apiError{Status: http.StatusPreconditionFailed, Reason: "conditionNotMet", Message: fmt.Sprintf("The resize request is %s, not ACCEPTED.", req.State)}
	}
	req.Status.LastAttempt = &ResizeRequestAttempt{Error: OperationErrors{[]OperationError{{code, message}}}}
	return nil
}
