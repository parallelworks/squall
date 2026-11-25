package compute

import (
	"cmp"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/parallelworks/squall/internal/reason"
)

const (
	actionNone     = "NONE"
	actionCreating = "CREATING"
	actionDeleting = "DELETING"

	maxGroupInstances = 1000
	maxBaseNameLength = 58
)

// InstanceGroupManager is a zonal managed instance group, in the API's JSON
// shape. Policies the server only records, such as instanceLifecyclePolicy,
// keep the JSON the caller sent.
type InstanceGroupManager struct {
	Kind                        string          `json:"kind"`
	ID                          uint64          `json:"id,string"`
	CreationTimestamp           string          `json:"creationTimestamp"`
	Name                        string          `json:"name"`
	Description                 string          `json:"description,omitzero"`
	Zone                        string          `json:"zone"`
	InstanceTemplate            string          `json:"instanceTemplate"`
	InstanceGroup               string          `json:"instanceGroup"`
	BaseInstanceName            string          `json:"baseInstanceName"`
	Fingerprint                 string          `json:"fingerprint"`
	CurrentActions              CurrentActions  `json:"currentActions"`
	Status                      GroupStatus     `json:"status"`
	TargetSize                  int             `json:"targetSize"`
	ListManagedInstancesResults string          `json:"listManagedInstancesResults"`
	SelfLink                    string          `json:"selfLink"`
	InstanceLifecyclePolicy     json.RawMessage `json:"instanceLifecyclePolicy,omitzero"`
	ResourcePolicies            json.RawMessage `json:"resourcePolicies,omitzero"`
	UpdatePolicy                json.RawMessage `json:"updatePolicy,omitzero"`
	TargetSizePolicy            json.RawMessage `json:"targetSizePolicy,omitzero"`
	NamedPorts                  json.RawMessage `json:"namedPorts,omitzero"`

	zone     string
	members  *collection[*member]
	requests *collection[*ResizeRequest]
	op       *operation
}

// CurrentActions counts a group's instances by what the group is doing to them.
type CurrentActions struct {
	Abandoning             int `json:"abandoning"`
	Creating               int `json:"creating"`
	CreatingWithoutRetries int `json:"creatingWithoutRetries"`
	Deleting               int `json:"deleting"`
	None                   int `json:"none"`
	Recreating             int `json:"recreating"`
	Refreshing             int `json:"refreshing"`
	Restarting             int `json:"restarting"`
	Resuming               int `json:"resuming"`
	Starting               int `json:"starting"`
	Stopping               int `json:"stopping"`
	Suspending             int `json:"suspending"`
	Verifying              int `json:"verifying"`
}

// GroupStatus is a group's status.
type GroupStatus struct {
	IsStable bool `json:"isStable"`
}

// ManagedInstance is one of a group's instances, as listManagedInstances
// reports it. Instance and InstanceStatus are empty while the group cannot
// create the instance; LastAttempt then says why.
type ManagedInstance struct {
	Name           string                 `json:"name"`
	Instance       string                 `json:"instance,omitzero"`
	ID             uint64                 `json:"id,string,omitzero"`
	InstanceStatus string                 `json:"instanceStatus,omitzero"`
	CurrentAction  string                 `json:"currentAction"`
	Version        ManagedInstanceVersion `json:"version"`
	LastAttempt    *ManagedInstanceErrors `json:"lastAttempt,omitzero"`
}

// ManagedInstanceVersion is the template a managed instance was created from.
type ManagedInstanceVersion struct {
	InstanceTemplate string `json:"instanceTemplate"`
}

// ManagedInstanceErrors are the errors of the group's last attempt at an instance.
type ManagedInstanceErrors struct {
	Errors OperationErrors `json:"errors"`
}

// member is one of a group's instances: the instance, once the group has
// created it, or the error that keeps it from doing so.
type member struct {
	name     string
	template string
	inst     *Instance
	failed   *apiError
	deleting bool
}

func (m *member) filterValue(field string) string {
	if field == "name" {
		return m.name
	}
	return ""
}

var groupFields = fieldSet{names: []string{"name", "description", "id"}}

func (g *InstanceGroupManager) filterValue(field string) string {
	switch field {
	case "name":
		return g.Name
	case "description":
		return g.Description
	case "id":
		return strconv.FormatUint(g.ID, 10)
	}
	return ""
}

func groupPath(zone, name string) string { return zone + "/instanceGroupManagers/" + name }

// templateLink resolves ref, a template's name or URL, to the selfLink of a
// template project holds. field names ref in errors. The caller holds s.mu.
func (s *Server) templateLink(project, field, ref string) (string, *apiError) {
	path := ref
	if !strings.Contains(ref, "/") {
		path = "global/instanceTemplates/" + ref
	}
	global, name, ok := strings.Cut(relPath(path, project), "/instanceTemplates/")
	if !ok || !strings.HasSuffix(global, "/global") || !validName(name) {
		return "", invalid("Invalid value for field '%s': '%s'. The URL is malformed.", field, ref)
	}
	if _, ok := lookupIn(s.templates, global, name); !ok {
		return "", missingRef(field, ref, "instanceTemplate")
	}
	return selfLinkBase + templatePath(global, name), nil
}

// groupUsing returns the path of a group whose template is link, or "". The
// caller holds s.mu.
func (s *Server) groupUsing(link string) string {
	var user string
	each(s.groups, func(g *InstanceGroupManager) {
		if user == "" && g.InstanceTemplate == link {
			user = strings.TrimPrefix(g.SelfLink, selfLinkBase)
		}
	})
	return user
}

// regionOf is the region path of zone, a path such as "projects/p/zones/z".
func (s *Server) regionOf(zone string) string {
	return "projects/" + projectOf(zone) + "/regions/" + s.zoneRegion[zone[strings.LastIndexByte(zone, '/')+1:]]
}

// buildMember builds the instance name in g's zone from g's template, with
// the disks and addresses it takes. The caller holds s.mu.
func (s *Server) buildMember(g *InstanceGroupManager, name string) (*Instance, []*Disk, []claim, *apiError) {
	global, tmplName, _ := strings.Cut(strings.TrimPrefix(g.InstanceTemplate, selfLinkBase), "/instanceTemplates/")
	t, ok := lookupIn(s.templates, global, tmplName)
	if !ok {
		return nil, nil, nil, missingRef("instanceTemplate", g.InstanceTemplate, "instanceTemplate")
	}
	var props Instance
	if err := unmarshalStrict(t.Properties, &props); err != nil {
		return nil, nil, nil, err
	}
	items := slices.Clone(props.Metadata.Items)
	for _, item := range []MetadataItem{
		{"instance-template", strings.TrimPrefix(g.InstanceTemplate, selfLinkBase)},
		{"created-by", strings.TrimPrefix(g.SelfLink, selfLinkBase)},
	} {
		if !slices.ContainsFunc(items, func(m MetadataItem) bool { return m.Key == item.Key }) {
			items = append(items, item)
		}
	}
	props.Metadata.Items = items
	inst, disks, claims, err := s.newInstance(g.zone, s.regionOf(g.zone), name, "instanceTemplate.properties", props, true)
	if err != nil {
		return nil, nil, nil, err
	}
	if _, ok := lookupIn(s.instances, g.zone, name); ok {
		return nil, nil, nil, alreadyExists(instancePath(g.zone, name))
	}
	for _, d := range disks {
		if _, ok := lookupIn(s.disks, g.zone, d.Name); ok {
			return nil, nil, nil, alreadyExists(diskPath(g.zone, d.Name))
		}
	}
	return inst, disks, claims, nil
}

// addMembers adds a member to g for each name and creates its instance. A
// name whose instance cannot be created stays a member whose last attempt
// failed, as the group keeps trying. The instances run once op, a change
// made in the background, finishes. The caller holds s.mu.
func (s *Server) addMembers(g *InstanceGroupManager, names []string) {
	var insts []*Instance
	var disks []*Disk
	for _, name := range names {
		m := &member{name: name, template: g.InstanceTemplate}
		inst, d, c, err := s.buildMember(g, name)
		if err != nil {
			m.failed = err
		} else {
			s.storeInstance(g.zone, s.regionOf(g.zone), inst, d, c)
			m.inst = inst
			insts, disks = append(insts, inst), append(disks, d...)
		}
		g.members.put(name, m)
	}
	if len(insts) > 0 {
		track(s.inBackground(s.provisioned(insts, disks)), insts, disks, true)
	}
}

// inBackground applies complete as a change the API makes after the call
// that asked for it is done, such as a group creating an instance: at once,
// or after the reads [WithSettleAfter] sets. The operation it returns is
// never listed. The caller holds s.mu.
func (s *Server) inBackground(complete func()) *operation {
	op := &operation{}
	if s.settleAfter == 0 {
		complete()
		return op
	}
	op.complete, op.wait = complete, max(s.settleAfter, 0)
	s.pending[op] = struct{}{}
	return op
}

// holds reports whether inst, which may be nil, is still in zone. The caller holds s.mu.
func (s *Server) holds(zone string, inst *Instance) bool {
	if inst == nil {
		return false
	}
	cur, ok := lookupIn(s.instances, zone, inst.Name)
	return ok && cur == inst
}

// releaseMember deletes m's instance, and then m. The caller holds s.mu.
func (s *Server) releaseMember(g *InstanceGroupManager, m *member) {
	if m.inst != nil {
		s.finish(m.inst.op)
	}
	if !s.holds(g.zone, m.inst) {
		g.members.remove(m.name)
		return
	}
	m.deleting = true
	inst := m.inst
	inst.Status = statusStopping
	track(s.inBackground(func() {
		s.removeInstance(g.zone, inst)
		if cur, ok := g.members.byName[m.name]; ok && cur == m {
			g.members.remove(m.name)
		}
	}), []*Instance{inst}, nil, false)
}

// refresh drops the members whose instance went away other than through g,
// and counts what g is doing. The caller holds s.mu.
func (s *Server) refresh(g *InstanceGroupManager) {
	for _, name := range slices.Clone(g.members.names) {
		if m := g.members.byName[name]; m.inst != nil && !s.holds(g.zone, m.inst) {
			g.members.remove(name)
		}
	}
	g.TargetSize = len(g.members.names)
	g.CurrentActions = CurrentActions{}
	for _, name := range g.members.names {
		switch memberAction(g.members.byName[name]) {
		case actionCreating:
			g.CurrentActions.Creating++
		case actionDeleting:
			g.CurrentActions.Deleting++
		default:
			g.CurrentActions.None++
		}
	}
	g.Status.IsStable = g.CurrentActions.None == g.TargetSize && g.op == nil
}

func memberAction(m *member) string {
	switch {
	case m.deleting:
		return actionDeleting
	case m.inst == nil, m.inst.op != nil && (m.inst.Status == statusProvisioning || m.inst.Status == statusStaging):
		return actionCreating
	}
	return actionNone
}

func (m *member) view() ManagedInstance {
	v := ManagedInstance{Name: m.name, CurrentAction: memberAction(m), Version: ManagedInstanceVersion{m.template}}
	if m.inst != nil {
		v.Instance, v.ID, v.InstanceStatus = m.inst.SelfLink, m.inst.ID, m.inst.Status
	}
	if m.failed != nil {
		v.LastAttempt = &ManagedInstanceErrors{OperationErrors{[]OperationError{{reason.UpperSnake(m.failed.Reason), m.failed.Message}}}}
	}
	return v
}

// memberName returns a name for a new instance of g that no instance or
// member has: its base instance name and four characters. The caller holds s.mu.
func (s *Server) memberName(g *InstanceGroupManager, taken []string) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	for {
		h := fnv.New32a()
		fmt.Fprint(h, s.newID())
		n := h.Sum32()
		suffix := make([]byte, 4)
		for i := range suffix {
			suffix[i], n = alphabet[n%uint32(len(alphabet))], n/uint32(len(alphabet))
		}
		name := g.BaseInstanceName + "-" + string(suffix)
		if !g.members.has(name) && !slices.Contains(taken, name) {
			if _, ok := lookupIn(s.instances, g.zone, name); !ok {
				return name
			}
		}
	}
}

// lookupGroup returns the group r names, by its path value key. The caller holds s.mu.
func (s *Server) lookupGroup(r *http.Request, key string) (*InstanceGroupManager, *apiError) {
	zone, name := scopeOf(r), r.PathValue(key)
	g, ok := lookupIn(s.groups, zone, name)
	if !ok {
		return nil, notFound(groupPath(zone, name))
	}
	return g, nil
}

func (s *Server) insertGroupManager(r *http.Request, opFault *apiError) (any, *apiError) {
	var want InstanceGroupManager
	if err := decodeStrict(r, &want); err != nil {
		return nil, err
	}
	if !validName(want.Name) {
		return nil, invalid("Invalid value for field 'resource.name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", want.Name)
	}
	want.BaseInstanceName = cmp.Or(want.BaseInstanceName, want.Name)
	if !validName(want.BaseInstanceName) || len(want.BaseInstanceName) > maxBaseNameLength {
		return nil, invalid("Invalid value for field 'resource.baseInstanceName': '%s'. Must be a match of regex '[a-z](([-a-z0-9]{0,57})|([-a-z0-9]{0,51}[0-9]{0,6}))'", want.BaseInstanceName)
	}
	if want.TargetSize < 0 || want.TargetSize > maxGroupInstances {
		return nil, invalid("Invalid value for field 'resource.targetSize': '%d'. Must be between 0 and %d.", want.TargetSize, maxGroupInstances)
	}
	mode := cmp.Or(want.ListManagedInstancesResults, "PAGELESS")
	if mode != "PAGELESS" && mode != "PAGINATED" {
		return nil, invalid("Invalid value for field 'resource.listManagedInstancesResults': '%s'.", mode)
	}
	if want.InstanceTemplate == "" {
		return nil, invalid("Invalid value for field 'resource.instanceTemplate': ''. An instance template is required.")
	}
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	zone := scopeOf(r)
	path := groupPath(zone, want.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	template, err := s.templateLink(projectOf(zone), "resource.instanceTemplate", want.InstanceTemplate)
	if err != nil {
		return nil, err
	}
	if _, ok := lookupIn(s.groups, zone, want.Name); ok {
		return nil, alreadyExists(path)
	}
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "insert", path, 0, opFault, nil)), nil
	}
	g := &InstanceGroupManager{
		Kind: "compute#instanceGroupManager", ID: s.newID(), CreationTimestamp: timestamp(s.now()),
		Name: want.Name, Description: want.Description, Zone: selfLinkBase + zone, InstanceTemplate: template,
		InstanceGroup: selfLinkBase + zone + "/instanceGroups/" + want.Name, BaseInstanceName: want.BaseInstanceName,
		ListManagedInstancesResults: mode, SelfLink: selfLinkBase + path,
		InstanceLifecyclePolicy: want.InstanceLifecyclePolicy, ResourcePolicies: want.ResourcePolicies,
		UpdatePolicy: want.UpdatePolicy, TargetSizePolicy: want.TargetSizePolicy, NamedPorts: want.NamedPorts,
		zone:     zone,
		members:  &collection[*member]{byName: map[string]*member{}},
		requests: &collection[*ResizeRequest]{byName: map[string]*ResizeRequest{}},
	}
	g.Fingerprint = hash([]any{g.ID, g.InstanceTemplate})
	scoped(s.groups, zone).put(g.Name, g)
	var names []string
	for range want.TargetSize {
		names = append(names, s.memberName(g, names))
	}
	s.addMembers(g, names)
	s.refresh(g)
	return encoded(s.startOperation(r, reqID, "insert", path, g.ID, nil, nil)), nil
}

func (s *Server) getGroupManager(r *http.Request, _ *apiError) (any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.lookupGroup(r, "name")
	if err != nil {
		return nil, err
	}
	if s.observe(g.op); !s.groups[g.zone].has(g.Name) {
		return nil, notFound(groupPath(g.zone, g.Name))
	}
	s.refresh(g)
	return encoded(g), nil
}

func (s *Server) listGroupManagers(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, groupFields)
	if err != nil {
		return nil, err
	}
	zone := scopeOf(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	return listScope(s.groups[zone], q, "compute#instanceGroupManagerList", zone+"/instanceGroupManagers", s.groupSeen()), nil
}

func (s *Server) aggregatedGroupManagers(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, groupFields)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return listAggregated(s.groups, r.PathValue("project"), q,
		"compute#instanceGroupManagerAggregatedList", "instanceGroupManagers", s.groupSeen()), nil
}

// groupSeen observes each listed group's change in progress and brings its
// counts up to date. The caller holds s.mu.
func (s *Server) groupSeen() func(*InstanceGroupManager) {
	seen := s.observer()
	return func(g *InstanceGroupManager) {
		seen(g.op)
		if s.groups[g.zone].has(g.Name) {
			s.refresh(g)
		}
	}
}

func (s *Server) deleteGroupManager(r *http.Request, opFault *apiError) (any, *apiError) {
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	g, err := s.lookupGroup(r, "name")
	if err != nil {
		return nil, err
	}
	if g.op != nil {
		s.finish(g.op)
		if g, err = s.lookupGroup(r, "name"); err != nil {
			return nil, err
		}
	}
	path := groupPath(g.zone, g.Name)
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "delete", path, g.ID, opFault, nil)), nil
	}
	for _, name := range g.requests.names {
		s.stopRequest(g.requests.byName[name])
	}
	var insts []*Instance
	for _, name := range slices.Clone(g.members.names) {
		m := g.members.byName[name]
		if m.inst != nil && m.inst.op != nil {
			s.finish(m.inst.op)
		}
		if !s.holds(g.zone, m.inst) {
			continue
		}
		m.deleting, m.inst.Status = true, statusStopping
		insts = append(insts, m.inst)
	}
	op := s.startOperation(r, reqID, "delete", path, g.ID, nil, func() {
		for _, inst := range insts {
			s.removeInstance(g.zone, inst)
		}
		s.groups[g.zone].remove(g.Name)
		g.op = nil
	})
	if op.complete != nil {
		g.op = op
		track(op, insts, nil, false)
	}
	return encoded(op), nil
}

// PerInstanceConfig names an instance a group or resize request creates.
type PerInstanceConfig struct {
	Name string `json:"name"`
}

// checkNewNames checks that names may be new instances of g: valid, distinct,
// and not taken by an instance or member. field names them in errors. A
// conflicting instance with a change in progress counts a read of it. The
// caller holds s.mu.
func (s *Server) checkNewNames(g *InstanceGroupManager, field string, names []string) (conflict string, err *apiError) {
	if len(names) == 0 || len(names) > maxGroupInstances {
		return "", invalid("Invalid value for field '%s': between 1 and %d instances are required.", field, maxGroupInstances)
	}
	for i, name := range names {
		if !validName(name) {
			return "", invalid("Invalid value for field '%s[%d].name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", field, i, name)
		}
		if slices.Contains(names[:i], name) {
			return "", invalid("Invalid value for field '%s[%d].name': '%s'. Instance names must be unique.", field, i, name)
		}
	}
	for _, name := range names {
		if inst, ok := lookupIn(s.instances, g.zone, name); ok {
			s.observe(inst.op)
		}
		if _, ok := lookupIn(s.instances, g.zone, name); ok || g.members.has(name) {
			return name, nil
		}
	}
	return "", nil
}

func (s *Server) createGroupInstances(r *http.Request, opFault *apiError) (any, *apiError) {
	var in struct {
		Instances []PerInstanceConfig `json:"instances"`
	}
	if err := decodeStrict(r, &in); err != nil {
		return nil, err
	}
	names := make([]string, len(in.Instances))
	for i, c := range in.Instances {
		names[i] = c.Name
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
	g, err := s.lookupGroup(r, "name")
	if err != nil {
		return nil, err
	}
	s.refresh(g)
	conflict, err := s.checkNewNames(g, "resource.instances", names)
	if err != nil {
		return nil, err
	}
	if conflict != "" {
		return nil, alreadyExists(instancePath(g.zone, conflict))
	}
	path := groupPath(g.zone, g.Name)
	if opFault == nil {
		s.addMembers(g, names)
	}
	return encoded(s.startOperation(r, reqID, "createInstances", path, g.ID, opFault, nil)), nil
}

func (s *Server) deleteGroupInstances(r *http.Request, opFault *apiError) (any, *apiError) {
	var in struct {
		Instances                      []string `json:"instances"`
		SkipInstancesOnValidationError bool     `json:"skipInstancesOnValidationError"`
	}
	if err := decodeStrict(r, &in); err != nil {
		return nil, err
	}
	refs := in.Instances
	if len(refs) == 0 || len(refs) > maxGroupInstances {
		return nil, invalid("Invalid value for field 'resource.instances': between 1 and %d instances are required.", maxGroupInstances)
	}
	byURL := slices.ContainsFunc(refs, func(ref string) bool { return strings.Contains(ref, "/") })
	if byURL && slices.ContainsFunc(refs, func(ref string) bool { return !strings.Contains(ref, "/") }) {
		return nil, invalid("Invalid value for field 'resource.instances': instances must be named all by URL or all by name.")
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
	g, err := s.lookupGroup(r, "name")
	if err != nil {
		return nil, err
	}
	s.refresh(g)
	path := groupPath(g.zone, g.Name)
	var release []*member
	for i, ref := range refs {
		name := ref
		if byURL {
			inZone, n, ok := strings.Cut(relPath(ref, projectOf(g.zone)), "/instances/")
			if !ok || !validName(n) || inZone != g.zone {
				opFault = cmp.Or(opFault, invalid("Invalid value for field 'resource.instances[%d]': '%s'. The instance must be in zone '%s'.", i, ref, g.zone))
				break
			}
			name = n
		}
		m, ok := g.members.byName[name]
		if ok && !m.deleting && !slices.Contains(release, m) {
			release = append(release, m)
			continue
		}
		if !in.SkipInstancesOnValidationError {
			opFault = cmp.Or(opFault, invalid("Invalid value for field 'resource.instances[%d]': '%s'. The instance is not a member of the group or is already being deleted.", i, ref))
			break
		}
	}
	if opFault == nil {
		for _, m := range release {
			s.releaseMember(g, m)
		}
	}
	return encoded(s.startOperation(r, reqID, "deleteInstances", path, g.ID, opFault, nil)), nil
}

func (s *Server) setGroupTemplate(r *http.Request, opFault *apiError) (any, *apiError) {
	var in struct {
		InstanceTemplate string `json:"instanceTemplate"`
	}
	if err := decodeStrict(r, &in); err != nil {
		return nil, err
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
	g, err := s.lookupGroup(r, "name")
	if err != nil {
		return nil, err
	}
	link, err := s.templateLink(projectOf(g.zone), "instanceTemplate", in.InstanceTemplate)
	if err != nil {
		return nil, err
	}
	if opFault == nil {
		g.InstanceTemplate = link
		g.Fingerprint = hash([]any{g.ID, g.InstanceTemplate})
	}
	return encoded(s.startOperation(r, reqID, "setInstanceTemplate", groupPath(g.zone, g.Name), g.ID, opFault, nil)), nil
}

// managedInstanceList is a listManagedInstances response.
type managedInstanceList struct {
	ManagedInstances []ManagedInstance `json:"managedInstances,omitzero"`
	NextPageToken    string            `json:"nextPageToken,omitzero"`
}

func (s *Server) listManagedInstances(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, fieldSet{names: []string{"name"}})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.lookupGroup(r, "name")
	if err != nil {
		return nil, err
	}
	s.refresh(g)
	start := 0
	if g.ListManagedInstancesResults == "PAGINATED" {
		start = afterName(g.members.names, q.after)
	} else {
		q.limit = maxGroupInstances
	}
	var list managedInstanceList
	for _, m := range scan(g.members, start, q.filter, nil) {
		if len(list.ManagedInstances) == q.limit {
			list.NextPageToken = list.ManagedInstances[len(list.ManagedInstances)-1].Name
			break
		}
		list.ManagedInstances = append(list.ManagedInstances, m.view())
	}
	return encoded(list), nil
}

// InstanceGroupManagers returns every instance group manager the server
// holds, ordered by selfLink.
func (s *Server) InstanceGroupManagers() []InstanceGroupManager {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []InstanceGroupManager
	each(s.groups, func(g *InstanceGroupManager) {
		s.refresh(g)
		out = append(out, jsonClone(g))
	})
	return out
}

// ManagedInstances returns the instances of the named group in project and
// zone, as listManagedInstances reports them, ordered by name.
func (s *Server) ManagedInstances(project, zone, group string) ([]ManagedInstance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := lookupIn(s.groups, "projects/"+project+"/zones/"+zone, group)
	if !ok {
		return nil, false
	}
	s.refresh(g)
	out := []ManagedInstance{}
	for _, name := range g.members.names {
		out = append(out, g.members.byName[name].view())
	}
	return out, true
}
