package compute

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// lookupInstance returns the instance r names by name or by ID. The caller holds s.mu.
func (s *Server) lookupInstance(r *http.Request) (*Instance, *apiError) {
	zone, name := scopeOf(r), r.PathValue("name")
	if inst, ok := lookupIn(s.instances, zone, name); ok {
		return inst, nil
	}
	if id, err := strconv.ParseUint(name, 10, 64); err == nil && s.instances[zone] != nil {
		for _, inst := range s.instances[zone].byName {
			if inst.ID == id {
				return inst, nil
			}
		}
	}
	return nil, notFound(instancePath(zone, name))
}

// settled finishes the change in progress on inst, as a change made while
// another is in progress waits for it, and returns the instance r names once
// it has. The caller holds s.mu.
func (s *Server) settled(r *http.Request, inst *Instance) (*Instance, *apiError) {
	if inst.op == nil {
		return inst, nil
	}
	s.finish(inst.op)
	return s.lookupInstance(r)
}

func (s *Server) getInstance(r *http.Request, _ *apiError) (any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, err := s.lookupInstance(r)
	if err != nil {
		return nil, err
	}
	if s.observe(inst.op); !s.instances[scopeOf(r)].has(inst.Name) {
		return nil, notFound(instancePath(scopeOf(r), r.PathValue("name")))
	}
	return encoded(inst), nil
}

func (s *Server) listInstances(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, instanceFields)
	if err != nil {
		return nil, err
	}
	zone := scopeOf(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := s.observer()
	return listScope(s.instances[zone], q, "compute#instanceList", zone+"/instances", func(inst *Instance) { seen(inst.op) }), nil
}

func (s *Server) aggregatedInstances(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, instanceFields)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := s.observer()
	return listAggregated(s.instances, r.PathValue("project"), q, "compute#instanceAggregatedList", "instances", func(inst *Instance) { seen(inst.op) }), nil
}

func (s *Server) deleteInstance(r *http.Request, opFault *apiError) (any, *apiError) {
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	zone := scopeOf(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	inst, err := s.lookupInstance(r)
	if err != nil {
		return nil, err
	}
	if inst, err = s.settled(r, inst); err != nil {
		return nil, err
	}
	path := instancePath(zone, inst.Name)
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "delete", path, inst.ID, opFault, nil)), nil
	}
	inst.Status = statusStopping
	op := s.startOperation(r, reqID, "delete", path, inst.ID, nil, func() { s.removeInstance(zone, inst) })
	track(op, []*Instance{inst}, nil, false)
	return encoded(op), nil
}

// removeInstance deletes inst, the disks it auto-deletes, and its hold on
// the rest and on its addresses. The caller holds s.mu.
func (s *Server) removeInstance(zone string, inst *Instance) {
	now := timestamp(s.now())
	s.releaseIPs(inst, true)
	for _, ad := range inst.Disks {
		d := s.diskAt(ad.Source)
		if d == nil {
			continue
		}
		if d.release(inst.SelfLink, now); ad.AutoDelete && len(d.Users) == 0 {
			s.disks[zone].remove(d.Name)
		}
	}
	s.instances[zone].remove(inst.Name)
	inst.op = nil
}

// change is a validated change to an instance: now applies what takes effect
// at once, and later, when not nil, what takes effect when its operation
// finishes. A change with fail set takes no effect: the API accepts the call
// and its operation ends in that error.
type change struct {
	now, later func()
	fail       *apiError
}

// instanceVerb handles a POST to an instance's custom method, such as stop.
func (s *Server) instanceVerb(verb string) handler {
	return func(r *http.Request, opFault *apiError) (any, *apiError) {
		var (
			labels  setLabelsRequest
			machine struct {
				MachineType string `json:"machineType"`
			}
			access AccessConfig
			attach AttachedDisk
		)
		body := map[string]any{"setLabels": &labels, "setMachineType": &machine, "addAccessConfig": &access, "attachDisk": &attach}[verb]
		if body != nil {
			if err := decodeStrict(r, body); err != nil {
				return nil, err
			}
		}
		reqID, err := requestID(r)
		if err != nil {
			return nil, err
		}
		zone, region := scopeOf(r), s.regionOfZone(r)
		s.mu.Lock()
		defer s.mu.Unlock()
		if op, ok := s.requests[reqID]; ok {
			return encoded(op), nil
		}
		inst, err := s.lookupInstance(r)
		if err != nil {
			return nil, err
		}
		if inst, err = s.settled(r, inst); err != nil {
			return nil, err
		}
		path := instancePath(zone, inst.Name)
		var c change
		switch verb {
		case "stop":
			c = s.stopInstance(inst)
		case "start":
			c = s.startInstance(inst)
		case "setLabels":
			c, err = setInstanceLabels(inst, labels)
		case "setMachineType":
			c, err = setMachineType(inst, zone, path, machine.MachineType)
		case "addAccessConfig":
			c, err = s.addAccessConfig(inst, region, r.URL.Query().Get("networkInterface"), access)
		case "attachDisk":
			c, err = s.attachDisk(inst, zone, attach)
		case "detachDisk":
			c, err = s.detachDisk(inst, r.URL.Query().Get("deviceName"))
		}
		if err != nil {
			return nil, err
		}
		if opFault = cmp.Or(opFault, c.fail); opFault != nil {
			return encoded(s.startOperation(r, reqID, verb, path, inst.ID, opFault, nil)), nil
		}
		if c.now != nil {
			c.now()
		}
		op := s.startOperation(r, reqID, verb, path, inst.ID, nil, c.later)
		track(op, []*Instance{inst}, nil, false)
		return encoded(op), nil
	}
}

// stopInstance stops inst, unless it is already stopped. The caller holds s.mu.
func (s *Server) stopInstance(inst *Instance) change {
	if inst.Status == statusTerminated {
		return change{}
	}
	return change{
		now: func() { inst.Status = statusStopping },
		later: func() {
			inst.Status, inst.LastStopTimestamp, inst.op = statusTerminated, timestamp(s.now()), nil
			s.releaseIPs(inst, false)
		},
	}
}

// startInstance starts inst, unless it is already running. The caller holds s.mu.
func (s *Server) startInstance(inst *Instance) change {
	if inst.Status == statusRunning {
		return change{}
	}
	return change{
		now: func() { inst.Status = statusStaging },
		later: func() {
			inst.Status, inst.LastStartTimestamp, inst.op = statusRunning, timestamp(s.now()), nil
			s.assignEphemeral(inst)
		},
	}
}

func setInstanceLabels(inst *Instance, req setLabelsRequest) (change, *apiError) {
	if req.LabelFingerprint == "" {
		return change{}, invalid("Required field 'labelFingerprint' not specified")
	}
	if err := validateLabels("labels", req.Labels); err != nil {
		return change{}, err
	}
	if req.LabelFingerprint != inst.LabelFingerprint {
		return change{}, fingerprintMismatch()
	}
	return change{now: func() {
		inst.Labels = maps.Clone(req.Labels)
		inst.LabelFingerprint = fingerprint(inst.Labels)
	}}, nil
}

// setMachineType changes a stopped instance's machine type.
func setMachineType(inst *Instance, zone, path, ref string) (change, *apiError) {
	link, err := zonalType(zone, "machineTypes", "machineType", ref)
	if err != nil {
		return change{}, err
	}
	if inst.Status != statusTerminated {
		return change{fail: notReady(path)}, nil
	}
	return change{now: func() { inst.MachineType = link }}, nil
}

func (s *Server) addAccessConfig(inst *Instance, region, nicName string, ac AccessConfig) (change, *apiError) {
	if nicName == "" {
		return change{}, invalid("Required field 'networkInterface' not specified")
	}
	i := slices.IndexFunc(inst.NetworkInterfaces, func(nic NetworkInterface) bool { return nic.Name == nicName })
	if i < 0 {
		return change{}, invalid("Invalid value for field 'networkInterface': '%s'. The instance has no such network interface.", nicName)
	}
	if len(inst.NetworkInterfaces[i].AccessConfigs) > 0 {
		return change{}, &apiError{Status: http.StatusBadRequest, Reason: "badRequest", Message: "At most one access config currently supported."}
	}
	c, err := s.configureAccess(&ac, region, "resource", false)
	if err != nil {
		return change{}, err
	}
	return change{now: func() {
		if c.address != nil {
			c.address.Status, c.address.Users = "IN_USE", []string{inst.SelfLink}
		}
		nic := &inst.NetworkInterfaces[i]
		nic.AccessConfigs = append(nic.AccessConfigs, ac)
		if inst.Status == statusRunning {
			s.assignEphemeral(inst)
		}
	}}, nil
}

func (s *Server) attachDisk(inst *Instance, zone string, ad AttachedDisk) (change, *apiError) {
	if ad.Source == "" || ad.InitializeParams != nil || ad.Boot {
		return change{}, invalid("Invalid value for field 'resource': attachDisk takes the source of an existing data disk.")
	}
	ad.Mode = cmp.Or(ad.Mode, "READ_WRITE")
	d, err := s.attachable(zone, "resource.source", ad.Source, ad.Mode, inst.SelfLink)
	if err != nil && err.Reason == "resourceInUseByAnotherResource" {
		return change{fail: err}, nil
	}
	if err != nil {
		return change{}, err
	}
	index := 0
	for _, a := range inst.Disks {
		index = max(index, a.Index+1)
	}
	ad.DeviceName = cmp.Or(ad.DeviceName, fmt.Sprintf("persistent-disk-%d", index))
	if slices.ContainsFunc(inst.Disks, func(a AttachedDisk) bool { return a.DeviceName == ad.DeviceName }) {
		return change{}, invalid("Invalid value for field 'resource.deviceName': '%s'. Device names must be unique.", ad.DeviceName)
	}
	ad.Kind, ad.Type, ad.Index, ad.Source, ad.DiskSizeGB = "compute#attachedDisk", "PERSISTENT", index, d.SelfLink, d.SizeGB
	ad.Interface = cmp.Or(ad.Interface, "SCSI")
	return change{now: func() {
		inst.Disks = append(inst.Disks, ad)
		d.Users, d.LastAttachTimestamp = append(d.Users, inst.SelfLink), timestamp(s.now())
	}}, nil
}

func (s *Server) detachDisk(inst *Instance, device string) (change, *apiError) {
	i := slices.IndexFunc(inst.Disks, func(a AttachedDisk) bool { return a.DeviceName == device })
	if i < 0 {
		return change{fail: &apiError{Status: http.StatusBadRequest, Reason: "invalidUsage", Message: fmt.Sprintf("No attached disk found with device name '%s'", device)}}, nil
	}
	if inst.Disks[i].Boot && inst.Status != statusTerminated {
		return change{}, notReady(strings.TrimPrefix(inst.SelfLink, selfLinkBase))
	}
	return change{now: func() {
		if d := s.diskAt(inst.Disks[i].Source); d != nil {
			d.release(inst.SelfLink, timestamp(s.now()))
		}
		inst.Disks = slices.Delete(inst.Disks, i, i+1)
	}}, nil
}

// Instances returns every instance the server holds, ordered by selfLink.
func (s *Server) Instances() []Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Instance
	each(s.instances, func(inst *Instance) { out = append(out, jsonClone(inst)) })
	return out
}

// LookupInstance returns the named instance in project and zone.
func (s *Server) LookupInstance(project, zone, name string) (Instance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := lookupIn(s.instances, "projects/"+project+"/zones/"+zone, name)
	if !ok {
		return Instance{}, false
	}
	return jsonClone(inst), true
}
