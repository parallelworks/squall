package lustre

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/netip"
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

// states are the instance states in the order of their enum numbers.
var states = []string{stateUnspecified, StateActive, StateCreating, StateDeleting, StateUpgrading, StateRepairing, StateStopped, StateUpdating, StateSuspended}

// Instance is a Managed Lustre instance, in the API's JSON shape.
type Instance struct {
	// Name is the full resource name, projects/{project}/locations/{location}/instances/{id}.
	Name                     string            `json:"name"`
	Filesystem               string            `json:"filesystem"`
	CapacityGib              int64             `json:"capacityGib,string"`
	Network                  string            `json:"network"`
	State                    string            `json:"state"`
	MountPoint               string            `json:"mountPoint,omitzero"`
	CreateTime               time.Time         `json:"createTime"`
	UpdateTime               time.Time         `json:"updateTime"`
	Description              string            `json:"description,omitzero"`
	Labels                   map[string]string `json:"labels,omitzero"`
	PerUnitStorageThroughput int64             `json:"perUnitStorageThroughput,string,omitzero"`
	GkeSupportEnabled        bool              `json:"gkeSupportEnabled,omitzero"`
	UID                      string            `json:"uid"`
	KmsKey                   string            `json:"kmsKey,omitzero"`
	PlacementPolicy          string            `json:"placementPolicy,omitzero"`
	TargetVersion            string            `json:"targetVersion,omitzero"`
	// The options below keep the JSON the caller sent.
	AccessRulesOptions json.RawMessage `json:"accessRulesOptions,omitzero"`
	MaintenancePolicy  json.RawMessage `json:"maintenancePolicy,omitzero"`
	DynamicTierOptions json.RawMessage `json:"dynamicTierOptions,omitzero"`
}

func (in *Instance) clone() Instance {
	c := *in
	c.Labels = maps.Clone(in.Labels)
	c.AccessRulesOptions = bytes.Clone(in.AccessRulesOptions)
	c.MaintenancePolicy = bytes.Clone(in.MaintenancePolicy)
	c.DynamicTierOptions = bytes.Clone(in.DynamicTierOptions)
	return c
}

// instanceJSON is an instance as a response carries it, its state a name or,
// when the caller asked for enums as numbers, a number.
type instanceJSON struct {
	*Instance
	State any `json:"state"`
}

func (in *Instance) wire(intEnums bool) instanceJSON {
	c := in.clone()
	out := instanceJSON{Instance: &c, State: c.State}
	if intEnums {
		out.State = max(slices.Index(states, c.State), 0)
	}
	return out
}

// instance is an instance the server holds, with its change in progress.
type instance struct {
	Instance
	op *operation
}

// instanceInput is an instance as a create or patch sends it. Output-only
// fields are read and ignored, as the API ignores them.
type instanceInput struct {
	Name                     string            `json:"name"`
	Filesystem               string            `json:"filesystem"`
	CapacityGib              int64Value        `json:"capacityGib"`
	Network                  string            `json:"network"`
	Description              string            `json:"description"`
	Labels                   map[string]string `json:"labels"`
	PerUnitStorageThroughput int64Value        `json:"perUnitStorageThroughput"`
	GkeSupportEnabled        bool              `json:"gkeSupportEnabled"`
	KmsKey                   string            `json:"kmsKey"`
	PlacementPolicy          string            `json:"placementPolicy"`
	TargetVersion            string            `json:"targetVersion"`
	AccessRulesOptions       json.RawMessage   `json:"accessRulesOptions"`
	MaintenancePolicy        json.RawMessage   `json:"maintenancePolicy"`
	DynamicTierOptions       json.RawMessage   `json:"dynamicTierOptions"`

	State                       json.RawMessage `json:"state"`
	MountPoint                  string          `json:"mountPoint"`
	CreateTime                  json.RawMessage `json:"createTime"`
	UpdateTime                  json.RawMessage `json:"updateTime"`
	UID                         string          `json:"uid"`
	StateReason                 string          `json:"stateReason"`
	AvailableVersion            string          `json:"availableVersion"`
	EffectiveVersion            string          `json:"effectiveVersion"`
	UpcomingMaintenanceSchedule json.RawMessage `json:"upcomingMaintenanceSchedule"`
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

// tier is a performance tier's capacity rules, in GiB.
type tier struct {
	min, max, step int64
	// Above threshold, when set, capacity grows by bigStep.
	threshold, bigStep int64
}

// tiers are the performance tiers by perUnitStorageThroughput, from the
// Managed Lustre performance tiers documentation. 0 is the Dynamic tier.
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

// tierOf is the tier an instance's throughput and dynamic tier options choose.
func tierOf(throughput int64, dynamic json.RawMessage) (tier, *apiError) {
	isDynamic := false
	if len(dynamic) > 0 && string(dynamic) != "null" {
		var opts struct {
			Mode json.RawMessage `json:"mode"`
		}
		if err := strictly(dynamic, &opts); err != nil {
			return tier{}, invalid("Invalid dynamic_tier_options: %v", err)
		}
		mode, ok := enumName(opts.Mode, "MODE_UNSPECIFIED", "DISABLED", "DEFAULT_CACHE")
		if !ok {
			return tier{}, invalid("Invalid dynamic_tier_options.mode %s", opts.Mode)
		}
		isDynamic = mode == "DEFAULT_CACHE"
	}
	switch {
	case isDynamic && throughput != 0:
		return tier{}, invalid("per_unit_storage_throughput must be 0 or unset for an instance on the Dynamic tier.")
	case isDynamic:
		return tiers[0], nil
	}
	t, ok := tiers[throughput]
	if !ok || throughput == 0 {
		return tier{}, invalid("Invalid per_unit_storage_throughput %d: it must be 125, 250, 500 or 1000, or 0 with the Dynamic tier.", throughput)
	}
	return t, nil
}

// enumName reads an enum sent as its name or number, reporting whether it is one of names.
func enumName(raw json.RawMessage, names ...string) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return names[0], true
	}
	var name string
	if json.Unmarshal(raw, &name) == nil {
		return name, slices.Contains(names, name)
	}
	var n int
	if json.Unmarshal(raw, &n) == nil && n >= 0 && n < len(names) {
		return names[n], true
	}
	return "", false
}

// checkAccessRules applies the rules the API documents for root squash options.
func checkAccessRules(raw json.RawMessage) *apiError {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var opts struct {
		AccessRules []struct {
			Name            string          `json:"name"`
			IPAddressRanges []string        `json:"ipAddressRanges"`
			SquashMode      json.RawMessage `json:"squashMode"`
		} `json:"accessRules"`
		DefaultSquashMode json.RawMessage `json:"defaultSquashMode"`
		DefaultSquashUID  int64Value      `json:"defaultSquashUid"`
		DefaultSquashGID  int64Value      `json:"defaultSquashGid"`
	}
	if err := strictly(raw, &opts); err != nil {
		return invalid("Invalid access_rules_options: %v", err)
	}
	for _, id := range []int64Value{opts.DefaultSquashUID, opts.DefaultSquashGID} {
		if id < math.MinInt32 || id > math.MaxInt32 {
			return invalid("Invalid access_rules_options: squash ID %d is out of range.", id)
		}
	}
	modes := []string{"SQUASH_MODE_UNSPECIFIED", "NO_SQUASH", "ROOT_SQUASH"}
	mode, ok := enumName(opts.DefaultSquashMode, modes...)
	if !ok || mode == modes[0] {
		return invalid("access_rules_options.default_squash_mode is required and must be NO_SQUASH or ROOT_SQUASH.")
	}
	if mode == "NO_SQUASH" && (opts.DefaultSquashUID != 0 || opts.DefaultSquashGID != 0) {
		return invalid("access_rules_options.default_squash_uid and default_squash_gid must not be set when default_squash_mode is NO_SQUASH.")
	}
	for _, rule := range opts.AccessRules {
		if !ruleNamePattern.MatchString(rule.Name) {
			return invalid("Invalid access rule name %q: it must have at most 16 letters, digits and underscores.", rule.Name)
		}
		if err := checkRanges(rule.Name, rule.IPAddressRanges); err != nil {
			return err
		}
		if mode, ok := enumName(rule.SquashMode, modes...); !ok || mode != "NO_SQUASH" {
			return invalid("Access rule %q: squash_mode must be NO_SQUASH; ROOT_SQUASH is not supported inside an access rule.", rule.Name)
		}
	}
	return nil
}

// checkRanges reads an access rule's ranges, CIDR ranges or single
// addresses, which must not overlap.
func checkRanges(rule string, ranges []string) *apiError {
	if len(ranges) == 0 {
		return invalid("Access rule %q needs ip_address_ranges.", rule)
	}
	var seen []netip.Prefix
	for _, r := range ranges {
		prefix, err := netip.ParsePrefix(r)
		if err != nil {
			addr, aerr := netip.ParseAddr(r)
			if aerr != nil {
				return invalid("Access rule %q: %q is neither a CIDR range nor an IP address.", rule, r)
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		prefix = prefix.Masked()
		for _, other := range seen {
			if prefix.Overlaps(other) {
				return invalid("Access rule %q: the ranges %s and %s overlap.", rule, other, prefix)
			}
		}
		seen = append(seen, prefix)
	}
	return nil
}

// checkMaintenance checks that a maintenance policy has only the fields the API knows.
func checkMaintenance(raw json.RawMessage) *apiError {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	type timeOfDay struct {
		Hours   int32 `json:"hours"`
		Minutes int32 `json:"minutes"`
		Seconds int32 `json:"seconds"`
		Nanos   int32 `json:"nanos"`
	}
	type date struct {
		Year  int32 `json:"year"`
		Month int32 `json:"month"`
		Day   int32 `json:"day"`
	}
	var policy struct {
		WeeklyMaintenanceWindows []struct {
			DayOfWeek json.RawMessage `json:"dayOfWeek"`
			StartTime *timeOfDay      `json:"startTime"`
		} `json:"weeklyMaintenanceWindows"`
		MaintenanceExclusionWindow []struct {
			StartDate *date      `json:"startDate"`
			EndDate   *date      `json:"endDate"`
			Time      *timeOfDay `json:"time"`
		} `json:"maintenanceExclusionWindow"`
	}
	if err := strictly(raw, &policy); err != nil {
		return invalid("Invalid maintenance_policy: %v", err)
	}
	return nil
}

// strictly decodes raw into v, rejecting fields v does not have.
func strictly(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

const (
	maxLabels      = 64
	maxLabelLength = 63
)

// validLabel reports whether s is a valid label key or, with key false, value.
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

// check validates a new instance as the API documents.
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
	if err := checkAccessRules(in.AccessRulesOptions); err != nil {
		return err
	}
	if err := checkMaintenance(in.MaintenancePolicy); err != nil {
		return err
	}
	return checkLabels(in.Labels)
}

func instancePath(parent, id string) string { return parent + "/instances/" + id }

// lookup returns the instance at name, after counting a read of its change
// in progress, which may finish it. The caller holds s.mu.
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

// current returns the instance at name once any change in progress on it
// finishes. The caller holds s.mu.
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

// change starts an operation of verb on inst, holding it in state while the
// operation is in progress; apply makes the change and returns the result.
// The caller holds s.mu.
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
	var in instanceInput
	if err := decodeStrict(c.Request, &in); err != nil {
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
		AccessRulesOptions:       rawOrNil(in.AccessRulesOptions),
		MaintenancePolicy:        rawOrNil(in.MaintenancePolicy),
		DynamicTierOptions:       rawOrNil(in.DynamicTierOptions),
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

// activate makes in ACTIVE, giving it a mount point if it has none. The caller holds s.mu.
func (s *Server) activate(in *Instance) {
	in.State, in.UpdateTime = StateActive, s.now().UTC()
	s.mount(in)
}

// mount gives in a mount point if it has none. The caller holds s.mu.
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

func rawOrNil(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return bytes.Clone(raw)
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
		Instances     []instanceJSON `json:"instances,omitzero"`
		NextPageToken string         `json:"nextPageToken,omitzero"`
	}{NextPageToken: next}
	// Only the instances a page returns count as read, and one that the read finishes may no longer belong.
	for _, inst := range page {
		if inst, err := s.lookup(inst.Name); err == nil && filter.matches(&inst.Instance) {
			out.Instances = append(out.Instances, inst.wire(c.intEnums))
		}
	}
	return out, nil
}

// updatable are the update mask paths a patch takes, in both spellings.
var updatable = map[string]string{
	"labels":              "labels",
	"description":         "description",
	"capacity_gib":        "capacityGib",
	"capacityGib":         "capacityGib",
	"gke_support_enabled": "gkeSupportEnabled",
	"gkeSupportEnabled":   "gkeSupportEnabled",
}

// outputOnly are the fields the API sets, which a request may carry and the
// server ignores, as AIP-203 directs, along with the name the path gives.
var outputOnly = []string{
	"name", "state", "mountPoint", "createTime", "updateTime", "uid", "stateReason",
	"availableVersion", "effectiveVersion", "upcomingMaintenanceSchedule",
}

// unmaskedPaths are the fields a patch without an update mask changes: the
// updatable ones present in body. Any other field present must equal what
// inst holds, since the server cannot change it.
func unmaskedPaths(body []byte, inst *Instance) ([]string, *apiError) {
	var present map[string]json.RawMessage
	if err := json.Unmarshal(body, &present); err != nil {
		return nil, invalid("Invalid JSON payload received. %v", err)
	}
	stored := map[string]json.RawMessage{}
	_ = json.Unmarshal(encoded(inst), &stored)
	var paths []string
	for _, field := range slices.Sorted(maps.Keys(present)) {
		if path, ok := updatable[field]; ok {
			paths = append(paths, path)
			continue
		}
		if slices.Contains(outputOnly, field) || sameJSON(present[field], stored[field]) {
			continue
		}
		return nil, invalid("squall: a patch cannot change %s; only labels, description, capacityGib and gkeSupportEnabled can change", field)
	}
	return paths, nil
}

// sameJSON reports whether a and b hold the same value, reading an int64
// sent as a string or a number alike.
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return fmt.Sprint(x) == fmt.Sprint(y) || isZero(x) && b == nil
}

func isZero(v any) bool {
	switch v := v.(type) {
	case nil:
		return true
	case string:
		return v == "" || v == "0"
	case float64:
		return v == 0
	case bool:
		return !v
	}
	return false
}

func encoded(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("squall: encoding a resource: " + err.Error())
	}
	return b
}

func (s *Server) patchInstance(c *call, opFault *apiError) (any, *apiError) {
	mask := c.URL.Query().Get("updateMask")
	var paths []string
	if mask != "" {
		for p := range strings.SplitSeq(mask, ",") {
			field, ok := updatable[strings.TrimSpace(p)]
			if !ok {
				return nil, invalid("squall: the update mask path %q is not supported; only labels, description, capacityGib and gkeSupportEnabled are", p)
			}
			paths = append(paths, field)
		}
	}
	key, err := c.requestKey("patch")
	if err != nil {
		return nil, err
	}
	body, rerr := io.ReadAll(io.LimitReader(c.Body, maxBody+1))
	if rerr != nil || len(body) > maxBody {
		return nil, invalid("squall: reading the request body: bodies are limited to %d bytes", maxBody)
	}
	c.Body = io.NopCloser(bytes.NewReader(body))
	var in instanceInput
	if err := decodeStrict(c.Request, &in); err != nil {
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
	if mask == "" {
		if paths, err = unmaskedPaths(body, &inst.Instance); err != nil {
			return nil, err
		}
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

// checkGrowth applies the documented limits on changing an instance's capacity.
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

// PutInstance stores in as instance id in project and location, as if it had
// been created, replacing any instance there, and returns it as stored. It
// checks what a create checks, keeps in's State if it is one of the API's,
// ACTIVE by default, and fills in the name, times, UID and mount point. Use it
// to set up state, such as an instance left behind by an earlier run.
func (s *Server) PutInstance(project, location, id string, in Instance) (Instance, error) {
	if err := validInstanceID(id); err != nil {
		return Instance{}, errors.New(err.Message)
	}
	check := instanceInput{
		Filesystem: in.Filesystem, Network: in.Network, Labels: in.Labels,
		CapacityGib: int64Value(in.CapacityGib), PerUnitStorageThroughput: int64Value(in.PerUnitStorageThroughput),
		AccessRulesOptions: in.AccessRulesOptions, DynamicTierOptions: in.DynamicTierOptions,
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

// int64Value is a 64-bit integer the API reads from a JSON string or number.
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
