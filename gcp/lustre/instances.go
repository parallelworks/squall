package lustre

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Instance states, as the API names them.
const (
	StateActive      = "ACTIVE"
	StateCreating    = "CREATING"
	StateDeleting    = "DELETING"
	StateUpgrading   = "UPGRADING"
	StateRepairing   = "REPAIRING"
	StateStopped     = "STOPPED"
	StateUpdating    = "UPDATING"
	StateSuspended   = "SUSPENDED"
	stateUnspecified = "STATE_UNSPECIFIED"
)

// states are indexed by their enum numbers.
var states = []string{stateUnspecified, StateActive, StateCreating, StateDeleting, StateUpgrading, StateRepairing, StateStopped, StateUpdating, StateSuspended}

// Instance is a Managed Lustre instance, in the API's JSON shape.
type Instance struct {
	// Name is the full resource name, projects/{project}/locations/{location}/instances/{id}.
	Name                     string              `json:"name"`
	Filesystem               string              `json:"filesystem"`
	CapacityGib              int64               `json:"capacityGib,string"`
	Network                  string              `json:"network"`
	State                    string              `json:"state"`
	MountPoint               string              `json:"mountPoint,omitzero"`
	CreateTime               time.Time           `json:"createTime"`
	UpdateTime               time.Time           `json:"updateTime"`
	Description              string              `json:"description,omitzero"`
	Labels                   map[string]string   `json:"labels,omitempty"`
	PerUnitStorageThroughput int64               `json:"perUnitStorageThroughput,string,omitzero"`
	GkeSupportEnabled        bool                `json:"gkeSupportEnabled,omitzero"`
	UID                      string              `json:"uid"`
	KmsKey                   string              `json:"kmsKey,omitzero"`
	PlacementPolicy          string              `json:"placementPolicy,omitzero"`
	TargetVersion            string              `json:"targetVersion,omitzero"`
	AccessRulesOptions       *AccessRulesOptions `json:"accessRulesOptions,omitzero"`
	MaintenancePolicy        *MaintenancePolicy  `json:"maintenancePolicy,omitzero"`
	DynamicTierOptions       *DynamicTierOptions `json:"dynamicTierOptions,omitzero"`
}

func (in *Instance) clone() Instance {
	c := *in
	c.Labels = maps.Clone(in.Labels)
	if o := in.AccessRulesOptions; o != nil {
		o := *o
		o.AccessRules = slices.Clone(o.AccessRules)
		for i := range o.AccessRules {
			o.AccessRules[i].IPAddressRanges = slices.Clone(o.AccessRules[i].IPAddressRanges)
		}
		c.AccessRulesOptions = &o
	}
	if p := in.MaintenancePolicy; p != nil {
		p := *p
		p.WeeklyMaintenanceWindows = slices.Clone(p.WeeklyMaintenanceWindows)
		for i := range p.WeeklyMaintenanceWindows {
			p.WeeklyMaintenanceWindows[i].StartTime = clonePtr(p.WeeklyMaintenanceWindows[i].StartTime)
		}
		p.MaintenanceExclusionWindow = slices.Clone(p.MaintenanceExclusionWindow)
		for i := range p.MaintenanceExclusionWindow {
			w := &p.MaintenanceExclusionWindow[i]
			w.StartDate, w.EndDate, w.Time = clonePtr(w.StartDate), clonePtr(w.EndDate), clonePtr(w.Time)
		}
		c.MaintenancePolicy = &p
	}
	c.DynamicTierOptions = clonePtr(in.DynamicTierOptions)
	return c
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	return new(*p)
}

// wire encodes enums as numbers when the caller asks, as the client does with enum-encoding=int.
func (in *Instance) wire(intEnums bool) any {
	if !intEnums {
		return json.RawMessage(encoded(in))
	}
	return in.wireMap(true)
}

func (in *Instance) wireMap(intEnums bool) map[string]any {
	var out map[string]any
	if err := json.Unmarshal(encoded(in), &out); err != nil {
		panic("squall: encoding an instance: " + err.Error())
	}
	if intEnums {
		numberEnums(out)
	}
	return out
}

type instance struct {
	Instance
	op *operation
}

type instanceInput struct {
	Name                     string              `json:"name"`
	Filesystem               string              `json:"filesystem"`
	CapacityGib              int64Value          `json:"capacityGib"`
	Network                  string              `json:"network"`
	Description              string              `json:"description"`
	Labels                   map[string]string   `json:"labels"`
	PerUnitStorageThroughput int64Value          `json:"perUnitStorageThroughput"`
	GkeSupportEnabled        bool                `json:"gkeSupportEnabled"`
	KmsKey                   string              `json:"kmsKey"`
	PlacementPolicy          string              `json:"placementPolicy"`
	TargetVersion            string              `json:"targetVersion"`
	AccessRulesOptions       *AccessRulesOptions `json:"accessRulesOptions"`
	MaintenancePolicy        *MaintenancePolicy  `json:"maintenancePolicy"`
	DynamicTierOptions       *DynamicTierOptions `json:"dynamicTierOptions"`

	State                       stateValue           `json:"state"`
	MountPoint                  string               `json:"mountPoint"`
	CreateTime                  *time.Time           `json:"createTime"`
	UpdateTime                  *time.Time           `json:"updateTime"`
	UID                         string               `json:"uid"`
	StateReason                 string               `json:"stateReason"`
	AvailableVersion            string               `json:"availableVersion"`
	EffectiveVersion            string               `json:"effectiveVersion"`
	UpcomingMaintenanceSchedule *maintenanceSchedule `json:"upcomingMaintenanceSchedule"`
}

var (
	instanceIDPattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	filesystemPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,8}$`)
	networkPattern    = regexp.MustCompile(`^projects/[^/]+/global/networks/[^/]+$`)
	ruleNamePattern   = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)
)

func validInstanceID(id string) *apiError {
	if !instanceIDPattern.MatchString(id) {
		return invalid("Invalid instance_id %q: it must have 1 to 63 lowercase letters, digits and hyphens, start with a letter and end with a letter or digit.", id)
	}
	return nil
}

type tier struct {
	min, max, step int64
	// Above threshold, when set, capacity grows by bigStep.
	threshold, bigStep int64
}

// tiers are from the performance tiers documentation; 0 is the Dynamic tier.
var tiers = map[int64]tier{
	1000: {min: 9_000, max: 10_008_000, step: 9_000, threshold: 2_556_000, bigStep: 72_000},
	500:  {min: 18_000, max: 20_016_000, step: 18_000, threshold: 5_112_000, bigStep: 144_000},
	250:  {min: 36_000, max: 40_032_000, step: 36_000, threshold: 10_224_000, bigStep: 288_000},
	125:  {min: 72_000, max: 20_448_000, step: 72_000},
	0:    {min: 472_000, max: 84_016_000, step: 472_000},
}

func (t tier) check(capacity, throughput int64) *apiError {
	step := t.step
	if t.threshold > 0 && capacity > t.threshold {
		step = t.bigStep
	}
	if capacity < t.min || capacity > t.max || capacity%step != 0 {
		return invalid("Invalid capacity_gib %d for per_unit_storage_throughput %d: it must be from %d to %d GiB in steps of %d GiB.", capacity, throughput, t.min, t.max, step)
	}
	return nil
}

func tierOf(throughput int64, dynamic *DynamicTierOptions) (tier, *apiError) {
	if dynamic != nil && !slices.Contains(tierModes[1:], string(dynamic.Mode)) {
		return tier{}, invalid("dynamic_tier_options.mode is required and must be DISABLED or DEFAULT_CACHE.")
	}
	switch {
	case dynamic.dynamic() && throughput != 0:
		return tier{}, invalid("per_unit_storage_throughput must be 0 or unset for an instance on the Dynamic tier.")
	case dynamic.dynamic():
		return tiers[0], nil
	}
	t, ok := tiers[throughput]
	if !ok || throughput == 0 {
		return tier{}, invalid("Invalid per_unit_storage_throughput %d: it must be 125, 250, 500 or 1000, or 0 with the Dynamic tier.", throughput)
	}
	return t, nil
}

const (
	maxLabels      = 64
	maxLabelLength = 63
)

func validLabel(s string, key bool) bool {
	if utf8.RuneCountInString(s) > maxLabelLength || key && s == "" {
		return false
	}
	for i, c := range s {
		letter := unicode.IsLetter(c) && !unicode.IsUpper(c)
		if !letter && (i == 0 && key || !unicode.IsDigit(c) && c != '_' && c != '-') {
			return false
		}
	}
	return true
}

func checkLabels(labels map[string]string) *apiError {
	if len(labels) > maxLabels {
		return invalid("Too many labels: %d, more than %d.", len(labels), maxLabels)
	}
	for k, v := range labels {
		if !validLabel(k, true) || !validLabel(v, false) {
			return invalid("Invalid label '%s: %s': keys and values have at most 63 lowercase letters, digits, underscores and hyphens, and keys start with a letter.", k, v)
		}
	}
	return nil
}

func (in *instanceInput) check() *apiError {
	switch {
	case in.Filesystem == "":
		return invalid("filesystem is required.")
	case !filesystemPattern.MatchString(in.Filesystem):
		return invalid("Invalid filesystem %q: it must have at most 8 letters and digits.", in.Filesystem)
	case in.Network == "":
		return invalid("network is required.")
	case !networkPattern.MatchString(in.Network):
		return invalid("Invalid network %q: it must be projects/{project}/global/networks/{network}.", in.Network)
	case in.CapacityGib == 0:
		return invalid("capacity_gib is required.")
	}
	t, err := tierOf(int64(in.PerUnitStorageThroughput), in.DynamicTierOptions)
	if err != nil {
		return err
	}
	if err := t.check(int64(in.CapacityGib), int64(in.PerUnitStorageThroughput)); err != nil {
		return err
	}
	if err := in.AccessRulesOptions.check(); err != nil {
		return err
	}
	if err := in.MaintenancePolicy.check(); err != nil {
		return err
	}
	return checkLabels(in.Labels)
}

func instancePath(parent, id string) string { return parent + "/instances/" + id }

// lookup counts as a read of the change in progress, which may finish it.
func (s *Server) lookup(name string) (*instance, *apiError) {
	if inst, ok := s.instances[name]; ok {
		s.observe(inst.op)
	}
	inst, ok := s.instances[name]
	if !ok {
		return nil, notFound(name)
	}
	return inst, nil
}

// current finishes a change in progress first, as a new change on the instance does.
func (s *Server) current(name string) (*instance, *apiError) {
	if inst, ok := s.instances[name]; ok {
		s.finish(inst.op)
	}
	inst, ok := s.instances[name]
	if !ok {
		return nil, notFound(name)
	}
	return inst, nil
}

func (s *Server) change(inst *instance, parent, requestKey, verb, state string, opFault *apiError, apply func() *Instance) *operation {
	var op *operation
	complete := func() *Instance {
		if inst.op == op {
			inst.op = nil
		}
		return apply()
	}
	op = s.startOperation(parent, requestKey, verb, inst.Name, opFault, complete)
	if !op.done {
		inst.op, inst.State = op, state
	}
	return op
}

func (s *Server) createInstance(c *call, opFault *apiError) (any, *apiError) {
	parent := c.parent()
	if c.PathValue("location") == "-" {
		return nil, invalid("Invalid parent %q: an instance is created in one location.", parent)
	}
	id := c.URL.Query().Get("instanceId")
	if id == "" {
		return nil, invalid("instance_id is required.")
	}
	if err := validInstanceID(id); err != nil {
		return nil, err
	}
	key, err := c.requestKey("create")
	if err != nil {
		return nil, err
	}
	body, err := readBody(c.Request)
	if err != nil {
		return nil, err
	}
	var in instanceInput
	if _, err := decodeStrict(body, &in); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(key); ok {
		return op.wire(c.intEnums), nil
	}
	if err := in.check(); err != nil {
		return nil, err
	}
	name := instancePath(parent, id)
	if _, ok := s.instances[name]; ok {
		return nil, alreadyExists(name)
	}
	now := s.now().UTC()
	inst := &instance{
		Name:                     name,
		Filesystem:               in.Filesystem,
		CapacityGib:              int64(in.CapacityGib),
		Network:                  in.Network,
		State:                    StateCreating,
		CreateTime:               now,
		UpdateTime:               now,
		Description:              in.Description,
		Labels:                   maps.Clone(in.Labels),
		PerUnitStorageThroughput: int64(in.PerUnitStorageThroughput),
		GkeSupportEnabled:        in.GkeSupportEnabled,
		UID:                      s.newUID(),
		KmsKey:                   in.KmsKey,
		PlacementPolicy:          in.PlacementPolicy,
		TargetVersion:            in.TargetVersion,
		AccessRulesOptions:       in.AccessRulesOptions,
		MaintenancePolicy:        in.MaintenancePolicy,
		DynamicTierOptions:       in.DynamicTierOptions,
	}
	if len(inst.Labels) == 0 {
		inst.Labels = nil
	}
	if opFault != nil {
		return s.startOperation(parent, key, "create", name, opFault, nil).wire(c.intEnums), nil
	}
	s.instances[name] = inst
	op := s.change(inst, parent, key, "create", StateCreating, nil, func() *Instance {
		s.activate(&inst.Instance)
		return new(inst.clone())
	})
	return op.wire(c.intEnums), nil
}

func (s *Server) activate(in *Instance) {
	in.State, in.UpdateTime = StateActive, s.now().UTC()
	s.mount(in)
}

func (s *Server) mount(in *Instance) {
	if in.MountPoint == "" {
		in.MountPoint = s.nextIP.String() + "@tcp:/" + in.Filesystem
		s.nextIP = s.nextIP.Next()
	}
}

func (s *Server) newUID() string {
	id := s.newID()
	return fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x", id>>32, id>>16&0xffff, id>>4&0xfff, id&0xfff, id)
}

func (s *Server) getInstance(c *call, _ *apiError) (any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, err := s.lookup(c.resource)
	if err != nil {
		return nil, err
	}
	return inst.wire(c.intEnums), nil
}

func (s *Server) listInstances(c *call, _ *apiError) (any, *apiError) {
	q := c.URL.Query()
	filter, ferr := parseFilter(q.Get("filter"))
	if ferr != nil {
		return nil, invalid("Invalid filter: %v", ferr)
	}
	desc := false
	switch strings.Join(strings.Fields(q.Get("orderBy")), " ") {
	case "", "name", "name asc":
	case "name desc":
		desc = true
	default:
		return nil, invalid("squall: orderBy %q is not supported; only name and name desc are", q.Get("orderBy"))
	}
	project, location := c.PathValue("project"), c.PathValue("location")
	prefix := c.parent() + "/instances/"
	if location == "-" {
		prefix = "projects/" + project + "/locations/"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	names := slices.Sorted(maps.Keys(s.instances))
	if desc {
		slices.Reverse(names)
	}
	var matched []*instance
	for _, name := range names {
		if inst := s.instances[name]; strings.HasPrefix(name, prefix) && filter.matches(&inst.Instance) {
			matched = append(matched, inst)
		}
	}
	page, next, err := pageOf(c, matched, func(inst *instance) string { return inst.Name }, desc)
	if err != nil {
		return nil, err
	}
	out := struct {
		Instances     []any  `json:"instances,omitzero"`
		NextPageToken string `json:"nextPageToken,omitzero"`
	}{NextPageToken: next}
	// Only returned instances count as read, and a read that finishes a delete drops one.
	for _, inst := range page {
		if inst, err := s.lookup(inst.Name); err == nil && filter.matches(&inst.Instance) {
			out.Instances = append(out.Instances, inst.wire(c.intEnums))
		}
	}
	return out, nil
}

var updatable = []string{"labels", "description", "capacityGib", "gkeSupportEnabled"}

// outputOnly fields are ignored in a request, as AIP-203 directs; name comes from the path.
var outputOnly = []string{
	"name", "state", "mountPoint", "createTime", "updateTime", "uid", "stateReason",
	"availableVersion", "effectiveVersion", "upcomingMaintenanceSchedule",
}

// immutable fields are documented as Immutable in the Instance reference.
var immutable = []string{"filesystem", "network", "kmsKey", "dynamicTierOptions"}

// instanceFields are what a * update mask names.
var instanceFields = []string{
	"name", "filesystem", "capacityGib", "network", "state", "mountPoint", "createTime", "updateTime",
	"description", "labels", "perUnitStorageThroughput", "gkeSupportEnabled", "uid", "kmsKey",
	"placementPolicy", "targetVersion", "accessRulesOptions", "maintenancePolicy", "dynamicTierOptions",
	"stateReason", "availableVersion", "effectiveVersion", "upcomingMaintenanceSchedule",
}

// camel spells an exact snake_case name as its JSON name; a name with any other underscores names nothing.
func camel(name string) string {
	if !strings.Contains(name, "_") {
		return name
	}
	if !snakeName.MatchString(name) {
		return ""
	}
	parts := strings.Split(name, "_")
	for i := 1; i < len(parts); i++ {
		parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
	}
	return strings.Join(parts, "")
}

var snakeName = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z][a-z0-9]*)+$`)

func patchFields(mask string, body []byte) ([]string, *apiError) {
	switch strings.TrimSpace(mask) {
	case "":
		var present map[string]json.RawMessage
		if err := json.Unmarshal(body, &present); err != nil {
			return nil, invalid("Invalid JSON payload received. %v", err)
		}
		return slices.Sorted(maps.Keys(present)), nil
	case "*":
		return instanceFields, nil
	}
	var fields []string
	for p := range strings.SplitSeq(mask, ",") {
		field := camel(strings.TrimSpace(p))
		if !slices.Contains(instanceFields, field) {
			return nil, invalid("squall: the update mask path %q is not supported; paths name top-level instance fields", p)
		}
		fields = append(fields, field)
	}
	return fields, nil
}

// changes rejects a change to anything not updatable; AIP-203 lets an unchanged immutable field through.
func changes(fields []string, in *instanceInput, stored *Instance) ([]string, *apiError) {
	proposed := map[string]json.RawMessage{}
	_ = json.Unmarshal(encoded(in.instance()), &proposed)
	current := map[string]json.RawMessage{}
	_ = json.Unmarshal(encoded(stored), &current)
	var out []string
	for _, field := range fields {
		switch {
		case slices.Contains(updatable, field):
			out = append(out, field)
		case slices.Contains(outputOnly, field), bytes.Equal(proposed[field], current[field]):
		default:
			if slices.Contains(immutable, field) {
				return nil, invalid("Invalid update: %s is immutable.", field)
			}
			return nil, invalid("squall: changing %s is not supported; the stand-in changes only labels, description, capacityGib and gkeSupportEnabled", field)
		}
	}
	return out, nil
}

func (in *instanceInput) instance() *Instance {
	return &Instance{
		Filesystem:               in.Filesystem,
		CapacityGib:              int64(in.CapacityGib),
		Network:                  in.Network,
		Description:              in.Description,
		Labels:                   in.Labels,
		PerUnitStorageThroughput: int64(in.PerUnitStorageThroughput),
		GkeSupportEnabled:        in.GkeSupportEnabled,
		KmsKey:                   in.KmsKey,
		PlacementPolicy:          in.PlacementPolicy,
		TargetVersion:            in.TargetVersion,
		AccessRulesOptions:       in.AccessRulesOptions,
		MaintenancePolicy:        in.MaintenancePolicy,
		DynamicTierOptions:       in.DynamicTierOptions,
	}
}

func encoded(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("squall: encoding a resource: " + err.Error())
	}
	return b
}

func (s *Server) patchInstance(c *call, opFault *apiError) (any, *apiError) {
	key, err := c.requestKey("patch")
	if err != nil {
		return nil, err
	}
	body, err := readBody(c.Request)
	if err != nil {
		return nil, err
	}
	var in instanceInput
	if body, err = decodeStrict(body, &in); err != nil {
		return nil, err
	}
	fields, err := patchFields(c.URL.Query().Get("updateMask"), body)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(key); ok {
		return op.wire(c.intEnums), nil
	}
	inst, err := s.current(c.resource)
	if err != nil {
		return nil, err
	}
	paths, err := changes(fields, &in, &inst.Instance)
	if err != nil {
		return nil, err
	}
	if slices.Contains(paths, "labels") {
		if err := checkLabels(in.Labels); err != nil {
			return nil, err
		}
	}
	if slices.Contains(paths, "capacityGib") {
		if err := checkGrowth(&inst.Instance, int64(in.CapacityGib)); err != nil {
			return nil, err
		}
	}
	prior := inst.State
	op := s.change(inst, c.parent(), key, "update", prior, opFault, func() *Instance {
		for _, p := range paths {
			switch p {
			case "labels":
				inst.Labels = maps.Clone(in.Labels)
				if len(inst.Labels) == 0 {
					inst.Labels = nil
				}
			case "description":
				inst.Description = in.Description
			case "capacityGib":
				inst.CapacityGib = int64(in.CapacityGib)
			case "gkeSupportEnabled":
				inst.GkeSupportEnabled = in.GkeSupportEnabled
			}
		}
		inst.State, inst.UpdateTime = prior, s.now().UTC()
		if prior != StateCreating {
			s.mount(&inst.Instance)
		}
		return new(inst.clone())
	})
	return op.wire(c.intEnums), nil
}

func checkGrowth(inst *Instance, capacity int64) *apiError {
	t, err := tierOf(inst.PerUnitStorageThroughput, inst.DynamicTierOptions)
	if err != nil {
		return err
	}
	if capacity < inst.CapacityGib {
		return invalid("Invalid capacity_gib %d: the capacity of an instance can only be increased from %d GiB.", capacity, inst.CapacityGib)
	}
	if t.threshold > 0 && inst.CapacityGib <= t.threshold && capacity > t.threshold {
		return invalid("Invalid capacity_gib %d: an instance created at or below %d GiB cannot grow past it.", capacity, t.threshold)
	}
	return t.check(capacity, inst.PerUnitStorageThroughput)
}

func (s *Server) deleteInstance(c *call, opFault *apiError) (any, *apiError) {
	if v := c.URL.Query().Get("force"); v != "" && v != "true" && v != "false" {
		return nil, invalid("Invalid value for force: %q", v)
	}
	key, err := c.requestKey("delete")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(key); ok {
		return op.wire(c.intEnums), nil
	}
	inst, err := s.current(c.resource)
	if err != nil {
		return nil, err
	}
	op := s.change(inst, c.parent(), key, "delete", StateDeleting, opFault, func() *Instance {
		delete(s.instances, inst.Name)
		return nil
	})
	return op.wire(c.intEnums), nil
}

// Instances returns every instance the server holds, ordered by name.
func (s *Server) Instances() []Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Instance, 0, len(s.instances))
	for _, name := range slices.Sorted(maps.Keys(s.instances)) {
		out = append(out, s.instances[name].clone())
	}
	return out
}

// LookupInstance returns the instance id in project and location.
func (s *Server) LookupInstance(project, location, id string) (Instance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[instancePath("projects/"+project+"/locations/"+location, id)]
	if !ok {
		return Instance{}, false
	}
	return inst.clone(), true
}

// PutInstance stores in as if created, checked as a create is, with its State (ACTIVE by default) and the fields the server fills.
func (s *Server) PutInstance(project, location, id string, in Instance) (Instance, error) {
	if err := validInstanceID(id); err != nil {
		return Instance{}, errors.New(err.Message)
	}
	check := instanceInput{
		Filesystem: in.Filesystem, Network: in.Network, Labels: in.Labels,
		CapacityGib: int64Value(in.CapacityGib), PerUnitStorageThroughput: int64Value(in.PerUnitStorageThroughput),
		AccessRulesOptions: in.AccessRulesOptions, MaintenancePolicy: in.MaintenancePolicy, DynamicTierOptions: in.DynamicTierOptions,
	}
	if err := check.check(); err != nil {
		return Instance{}, errors.New(err.Message)
	}
	if in.State == "" {
		in.State = StateActive
	}
	if !slices.Contains(states[1:], in.State) {
		return Instance{}, fmt.Errorf("squall: unknown instance state %q", in.State)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	in = in.clone()
	in.Name = instancePath("projects/"+project+"/locations/"+location, id)
	now := s.now().UTC()
	if in.CreateTime.IsZero() {
		in.CreateTime = now
	}
	if in.UpdateTime.IsZero() {
		in.UpdateTime = in.CreateTime
	}
	if in.UID == "" {
		in.UID = s.newUID()
	}
	if in.State != StateCreating {
		s.mount(&in)
	}
	if old, ok := s.instances[in.Name]; ok {
		s.finish(old.op)
	}
	s.instances[in.Name] = &instance{Instance: in}
	return in.clone(), nil
}

// int64Value reads proto3 JSON, which spells an int64 as a string or a number.
type int64Value int64

func (n *int64Value) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	digits := b
	if len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' {
		digits = b[1 : len(b)-1]
	}
	v, err := strconv.ParseInt(string(digits), 10, 64)
	if err != nil || bytes.ContainsAny(digits, `"\`) {
		return fmt.Errorf("invalid int64 value %s", b)
	}
	*n = int64Value(v)
	return nil
}

// stateValue is an output-only state a request may carry, which must still be a state.
type stateValue string

func (v *stateValue) UnmarshalJSON(b []byte) error { return readEnum(b, states, (*string)(v)) }
