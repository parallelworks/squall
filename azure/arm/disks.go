package arm

import (
	"cmp"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
)

var (
	diskKind = &kind{
		typ: "Microsoft.Compute/disks", plural: "disks", typeName: "Disk",
		top:         []string{"extendedLocation", "sku", "zones"},
		topReadOnly: []string{"managedBy", "managedByExtended"},
		props: []string{
			"availabilityPolicy", "burstingEnabled", "creationData", "dataAccessAuthMode", "diskAccessId", "diskIOPSReadOnly",
			"diskIOPSReadWrite", "diskMBpsReadOnly", "diskMBpsReadWrite", "diskSizeGB", "encryption", "encryptionSettingsCollection",
			"hyperVGeneration", "maxShares", "networkAccessPolicy", "optimizedForFrequentAttach", "osType", "publicNetworkAccess",
			"purchasePlan", "securityProfile", "supportedCapabilities", "supportsHibernation", "tier",
		},
		readOnly: []string{
			"burstingEnabledTime", "completionPercent", "diskSizeBytes", "diskState", "lastOwnershipUpdateTime",
			"propertyUpdatesInProgress", "provisioningState", "shareInfo", "timeCreated", "uniqueId",
		},
		located: true,
	}
	snapshotKind = &kind{
		typ: "Microsoft.Compute/snapshots", plural: "snapshots", typeName: "Snapshot",
		top:         []string{"extendedLocation", "sku"},
		topReadOnly: []string{"managedBy"},
		props: []string{
			"creationData", "dataAccessAuthMode", "diskAccessId", "diskSizeGB", "encryption", "encryptionSettingsCollection",
			"hyperVGeneration", "incremental", "networkAccessPolicy", "osType", "publicNetworkAccess", "purchasePlan",
			"securityProfile", "supportedCapabilities", "supportsHibernation",
		},
		readOnly: []string{
			"completionPercent", "copyCompletionError", "diskSizeBytes", "diskState", "immutabilityPolicy",
			"incrementalSnapshotFamilyId", "provisioningState", "snapshotAccessState", "timeCreated", "uniqueId",
		},
		located: true,
	}
)

var (
	diskSKUs     = []string{"Premium_LRS", "Premium_ZRS", "PremiumV2_LRS", "StandardSSD_LRS", "StandardSSD_ZRS", "Standard_LRS", "UltraSSD_LRS"}
	snapshotSKUs = []string{"Premium_LRS", "Standard_LRS", "Standard_ZRS"}
	// creationDataMembers are the members of a disk's or snapshot's creationData.
	creationDataMembers = []string{
		"createOption", "elasticSanResourceId", "galleryImageReference", "imageReference", "instantAccessDurationMinutes",
		"logicalSectorSize", "performancePlus", "provisionedBandwidthCopySpeed", "securityDataUri", "securityMetadataUri",
		"sourceResourceId", "sourceUniqueId", "sourceUri", "storageAccountId", "uploadSizeBytes",
	}
)

const (
	// osDiskSizeGB is the size of an OS disk a VM creates without diskSizeGB.
	osDiskSizeGB = 30
)

// diskState is what a managed disk holds beyond its body.
type diskState struct {
	// vm is the ID of the VM the disk is attached to, if any.
	vm string
	// source and sourceGUID name the disk or snapshot it was copied from, if any.
	source, sourceGUID string
	// family is the incrementalSnapshotFamilyId of its incremental snapshots.
	family string
}

// snapState is what a snapshot holds beyond its body.
type snapState struct {
	source, sourceGUID     string
	incremental, copyStart bool
	// origin is the disk its data came from, through any copies.
	origin, family string
	// percent is a CopyStart snapshot's completionPercent, and copyErr the
	// message of the error its background copy ended in, if any.
	percent int
	copyErr string
}

func computeID(c *call, k *kind) string { return resourceID(c, k.typ, c.val("name")) }

// diskOps are the paths of a Microsoft.Compute disk or snapshot operation,
// whose Azure-AsyncOperation and Location URLs share one path, the Location
// URL adding monitor=true.
func diskOps(c *call, location string) opPaths {
	path := "/subscriptions/" + c.val("sub") + "/providers/Microsoft.Compute/locations/" + location + "/DiskOperations/"
	return opPaths{status: path, result: path, resultStatus: http.StatusOK, locationQuery: cacheMonitor}
}

func (s *Server) registerDiskRoutes() {
	s.add("GET", sub+"/providers/Microsoft.Compute/locations/{loc}/DiskOperations/{op}", "Microsoft.Compute/locations/diskOperations/read", false,
		s.onePathOperation(cacheMonitor, s.diskOperation))
	for _, k := range []*kind{diskKind, snapshotKind} {
		prepare := s.prepareDisk
		if k == snapshotKind {
			prepare = s.prepareSnapshot
		}
		items := compute + "/" + k.plural
		s.add("GET", items, k.action("read"), false, s.listDisks(k))
		s.add("PUT", items+"/{name}", k.action("write"), true, s.put(putSpec{
			kind: k, id: func(c *call) string { return computeID(c, k) }, paths: diskOps, render: s.renderDisk, prepare: prepare,
			acceptedRender: true, createdStatus: http.StatusOK, createState: stateUpdating,
		}))
		s.add("GET", items+"/{name}", k.action("read"), false, s.getDisk(k))
		s.add("DELETE", items+"/{name}", k.action("delete"), true, s.deleteCompute(k, func(c *call) string { return computeID(c, k) }, diskOps))
	}
}

// checkSKU reads the sku of p, which must name one of names, and sets it to
// the default, the first Standard_LRS, when p gives none.
func checkSKU(p *parsed, names []string) *apiError {
	sku, err := object(p.top, "sku")
	if err != nil {
		return err
	}
	name := str(sku, "name")
	if name == "" {
		p.top["sku"] = map[string]any{"name": "Standard_LRS"}
		return nil
	}
	i := slices.IndexFunc(names, func(n string) bool { return strings.EqualFold(n, name) })
	if i < 0 {
		return invalidParameter("sku.name", "The value '%s' of parameter 'sku.name' is not valid. Allowed values are %s.", name, strings.Join(names, ", "))
	}
	p.top["sku"] = map[string]any{"name": names[i]}
	return nil
}

// creationData reads the createOption and sourceResourceId of p's creationData.
func creationData(p *parsed) (option, source string, err *apiError) {
	cd, err := object(p.props, "creationData")
	if err != nil {
		return "", "", err
	}
	if cd == nil || str(cd, "createOption") == "" {
		return "", "", requiredParameter("creationData.createOption")
	}
	if err := checkMembers(cd, "CreationData", "properties.creationData", creationDataMembers...); err != nil {
		return "", "", err
	}
	delete(cd, "sourceUniqueId")
	return str(cd, "createOption"), str(cd, "sourceResourceId"), nil
}

// sameCreation fails an update of res that changes how it was created.
func sameCreation(res *resource, option, source string) *apiError {
	old, _ := res.props["creationData"].(map[string]any)
	if !strings.EqualFold(option, str(old, "createOption")) || key(source) != key(str(old, "sourceResourceId")) {
		return changeNotAllowed("creationData")
	}
	return nil
}

// copySource returns the disk or snapshot id names, for a copy into res. It
// must be in res's location. The caller holds s.mu.
func (s *Server) copySource(res *resource, id string) (*resource, *apiError) {
	if id == "" {
		return nil, requiredParameter("creationData.sourceResourceId")
	}
	src := s.settled(id)
	if src == nil || src.disk == nil && src.snap == nil || src.loc != res.loc {
		return nil, newError(http.StatusNotFound, "NotFound", "Resource %s is not found.", id[strings.LastIndex(id, "/")+1:])
	}
	return src, nil
}

// copyInProgress fails the use of the snapshot res before its background copy completes.
func copyInProgress(res *resource) *apiError {
	return newError(http.StatusConflict, "Conflict",
		"Source incremental snapshot %s copy is still in progress. Please retry after source snapshot's copy has completed.", res.name)
}

func diskAttached(disk, vm string) *apiError {
	return newError(http.StatusConflict, "OperationNotAllowed", "Disk %s is attached to VM %s.", disk, vm)
}

// inherit gives p what it leaves out of the source src's description.
func inherit(p *parsed, src *resource) {
	for _, k := range []string{"diskSizeGB", "hyperVGeneration", "osType", "supportedCapabilities"} {
		if _, ok := p.props[k]; !ok && src.props[k] != nil {
			p.props[k] = src.props[k]
		}
	}
}

// prepareDisk validates p as the new body of the disk res, and returns how to
// apply it. The caller holds s.mu.
func (s *Server) prepareDisk(res *resource, p *parsed) (func(), *apiError) {
	if err := checkSKU(p, diskSKUs); err != nil {
		return nil, err
	}
	option, source, err := creationData(p)
	if err != nil {
		return nil, err
	}
	if res.disk != nil {
		if err := sameCreation(res, option, source); err != nil {
			return nil, err
		}
		return func() { s.update(res, p) }, nil
	}
	st := &diskState{}
	switch {
	case strings.EqualFold(option, "Empty"):
		if p.props["diskSizeGB"] == nil {
			return nil, requiredParameter("diskSizeGB")
		}
	case strings.EqualFold(option, "Copy"):
		src, err := s.copySource(res, source)
		if err != nil {
			return nil, err
		}
		if !ready(src) {
			return nil, copyInProgress(src)
		}
		st.source, st.sourceGUID = src.id, src.guid
		inherit(p, src)
	case strings.EqualFold(option, "FromImage"):
	default:
		return nil, unsupported("disk createOption %s is not supported; use Empty, Copy or FromImage", option)
	}
	return func() {
		res.disk = st
		s.update(res, p)
	}, nil
}

// prepareSnapshot validates p as the new body of the snapshot res, and returns
// how to apply it. The caller holds s.mu.
func (s *Server) prepareSnapshot(res *resource, p *parsed) (func(), *apiError) {
	if err := checkSKU(p, snapshotSKUs); err != nil {
		return nil, err
	}
	option, source, err := creationData(p)
	if err != nil {
		return nil, err
	}
	if res.snap != nil {
		if err := sameCreation(res, option, source); err != nil {
			return nil, err
		}
		return func() { s.update(res, p) }, nil
	}
	incremental, set := boolean(p.props, "incremental")
	st := &snapState{incremental: incremental}
	var src *resource
	switch {
	case strings.EqualFold(option, "Copy"):
		if src, err = s.copySource(res, source); err != nil {
			return nil, err
		}
		if !ready(src) {
			return nil, copyInProgress(src)
		}
		st.origin = src.id
		if src.snap != nil {
			st.origin = src.snap.origin
		}
	case strings.EqualFold(option, "CopyStart"):
		if set && !incremental {
			return nil, invalidParameter("incremental", "Only incremental snapshots can be copied with createOption CopyStart.")
		}
		if source == "" {
			return nil, requiredParameter("creationData.sourceResourceId")
		}
		if src = s.settled(source); src == nil || src.snap == nil {
			return nil, newError(http.StatusNotFound, "NotFound", "Resource %s is not found.", source[strings.LastIndex(source, "/")+1:])
		}
		if !src.snap.incremental {
			return nil, invalidParameter("creationData.sourceResourceId",
				"Snapshot %s is not an incremental snapshot. Only incremental snapshots can be copied with createOption CopyStart.", src.id)
		}
		if !ready(src) {
			return nil, copyInProgress(src)
		}
		if other := s.copyOf(src.snap.origin); other != nil {
			return nil, newError(http.StatusConflict, "Conflict",
				"Snapshot %s of disk %s is still being copied. Copy one incremental snapshot of a disk at a time.", other.id, src.snap.origin)
		}
		st.incremental, st.copyStart, st.origin = true, true, src.snap.origin
		p.props["incremental"] = true
	default:
		return nil, unsupported("snapshot createOption %s is not supported; use Copy or CopyStart", option)
	}
	st.source, st.sourceGUID = src.id, src.guid
	inherit(p, src)
	return func() {
		if st.incremental {
			st.family = s.newUUID()
			if src.disk != nil && !st.copyStart {
				if src.disk.family == "" {
					src.disk.family = s.newUUID()
				}
				st.family = src.disk.family
			}
		}
		res.snap = st
		s.update(res, p)
	}, nil
}

// inCopy reports whether res is a CopyStart snapshot whose copy has not ended.
func inCopy(res *resource) bool {
	return res != nil && res.snap != nil && res.snap.copyStart && res.snap.percent < 100 && res.snap.copyErr == ""
}

// copying reports whether res is a CopyStart snapshot whose background copy
// runs: its operation is done and its copy has not ended.
func copying(res *resource) bool { return inCopy(res) && res.pending == nil }

func (s *Server) stepCopy(res *resource) {
	res.snap.percent = min(res.snap.percent+(100+s.copySteps-1)/s.copySteps, 100)
}

// ready reports whether the disk or snapshot res can be a source.
func ready(res *resource) bool {
	return res.snap == nil || !res.snap.copyStart || res.snap.percent == 100
}

// copyOf returns a CopyStart snapshot of the disk origin whose copy has not
// ended, or nil. The caller holds s.mu.
func (s *Server) copyOf(origin string) *resource {
	if origin == "" {
		return nil
	}
	for _, res := range s.resources {
		if inCopy(res) && key(res.snap.origin) == key(origin) {
			return res
		}
	}
	return nil
}

// failCopiesOf ends the copies from the snapshot src, which is being deleted,
// as Azure ends them with CopySourceNotFound. The caller holds s.mu.
func (s *Server) failCopiesOf(src *resource) {
	for _, res := range s.resources {
		if inCopy(res) && key(res.snap.source) == key(src.id) {
			res.snap.copyErr = "The source snapshot " + src.id + " was deleted while the copy was in progress."
		}
	}
}

// attachOSDisk attaches the VM vm, which is new, to its OS disk, creating the
// disk when the VM makes it from an image. The caller holds s.mu.
func (s *Server) attachOSDisk(vm *resource) {
	os := osDiskOf(vm)
	id := refID(os, "managedDisk")
	d := s.resources[key(id)]
	if d == nil && strings.EqualFold(str(os, "createOption"), "FromImage") {
		d = s.newOSDisk(vm, os, id)
		s.resources[key(id)] = d
	}
	if d != nil && d.disk != nil {
		d.disk.vm = vm.id
	}
}

// newOSDisk makes the OS disk id the VM vm creates from its image. The caller holds s.mu.
func (s *Server) newOSDisk(vm *resource, os map[string]any, id string) *resource {
	md, _ := os["managedDisk"].(map[string]any)
	cd := map[string]any{"createOption": "FromImage"}
	storage, _ := vm.props["storageProfile"].(map[string]any)
	switch image := refID(storage, "imageReference"); {
	case strings.Contains(strings.ToLower(image), "/providers/microsoft.compute/galleries/"):
		cd["galleryImageReference"] = ref(image)
	case image != "":
		cd["imageReference"] = ref(image)
	}
	res := &resource{
		kind: diskKind, id: id, name: id[strings.LastIndex(id, "/")+1:], sub: vm.sub, group: vm.group, loc: vm.loc,
		top:   map[string]any{"sku": map[string]any{"name": cmp.Or(str(md, "storageAccountType"), "Standard_LRS")}},
		props: map[string]any{"osType": os["osType"], "diskSizeGB": cmp.Or(intOf(os["diskSizeGB"]), osDiskSizeGB), "creationData": cd},
		state: stateSucceeded, created: s.now(), guid: s.newUUID(), disk: &diskState{},
	}
	if zones, ok := vm.top["zones"]; ok {
		res.top["zones"] = zones
	}
	return res
}

// releaseOSDisk detaches the VM vm, which is being deleted, from its OS disk,
// deleting the disk when its deleteOption is Delete. The caller holds s.mu.
func (s *Server) releaseOSDisk(vm *resource) {
	os := osDiskOf(vm)
	d := s.resources[key(refID(os, "managedDisk"))]
	if d == nil || d.disk == nil || key(d.disk.vm) != key(vm.id) {
		return
	}
	d.disk.vm = ""
	if strings.EqualFold(str(os, "deleteOption"), "Delete") {
		s.remove(d)
	}
}

func intOf(v any) int {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// skuTier is the tier Azure shows for a disk or snapshot SKU, such as Premium for Premium_LRS.
func skuTier(name string) string {
	tier, _, _ := strings.Cut(name, "_")
	return tier
}

// diskState is the diskState the disk res shows. The caller holds s.mu.
func (s *Server) diskStateOf(res *resource) string {
	if res.disk == nil || res.disk.vm == "" {
		return "Unattached"
	}
	if vm := s.resources[key(res.disk.vm)]; vm != nil && vm.vm != nil && vm.vm.power == powerDeallocated {
		return "Reserved"
	}
	return "Attached"
}

func (s *Server) renderDisk(res *resource) map[string]any {
	m, props := envelope(res)
	sku, _ := res.top["sku"].(map[string]any)
	m["sku"] = map[string]any{"name": str(sku, "name"), "tier": skuTier(str(sku, "name"))}
	props["timeCreated"] = timestamp(res.created)
	props["uniqueId"] = res.guid
	if size := intOf(props["diskSizeGB"]); size > 0 {
		props["diskSizeBytes"] = int64(size) << 30
	}
	for k, v := range map[string]any{
		"encryption": map[string]any{"type": "EncryptionAtRestWithPlatformKey"}, "networkAccessPolicy": "AllowAll", "publicNetworkAccess": "Enabled",
	} {
		if _, ok := props[k]; !ok {
			props[k] = v
		}
	}
	cd, _ := props["creationData"].(map[string]any)
	cd = maps.Clone(cd)
	props["diskState"] = s.diskStateOf(res)
	switch {
	case res.disk != nil:
		if res.disk.sourceGUID != "" {
			cd["sourceUniqueId"] = res.disk.sourceGUID
		}
		if res.disk.vm != "" {
			m["managedBy"] = res.disk.vm
		}
	case res.snap != nil:
		st := res.snap
		cd["sourceUniqueId"] = st.sourceGUID
		props["incremental"] = st.incremental
		if st.family != "" {
			props["incrementalSnapshotFamilyId"] = st.family
		}
		props["snapshotAccessState"] = "Available"
		if st.copyStart {
			props["completionPercent"] = float64(st.percent)
			if st.percent < 100 {
				props["snapshotAccessState"] = "Pending"
			}
			if st.copyErr != "" {
				props["copyCompletionError"] = map[string]any{"errorCode": "CopySourceNotFound", "errorMessage": st.copyErr}
			}
		}
	}
	if cd != nil {
		props["creationData"] = cd
	}
	return m
}

func (s *Server) getDisk(k *kind) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.readableGroup(c); err != nil {
			return nil, err
		}
		id := computeID(c, k)
		running := copying(s.resources[key(id)])
		res := s.read(id)
		if res == nil || res.kind != k {
			return nil, resourceNotFound(k.typ, c.val("name"), c.val("rg"))
		}
		if running && s.settleAfter >= 0 && copying(res) {
			s.stepCopy(res)
		}
		return ok200(s.renderDisk(res)), nil
	}
}

func (s *Server) listDisks(k *kind) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.readableGroup(c); err != nil {
			return nil, err
		}
		return ok(listBody(s.listed(c, k), func(res *resource) any { return s.renderDisk(res) })), nil
	}
}

// deleteCompute deletes a Microsoft.Compute resource of kind k, polled on paths.
func (s *Server) deleteCompute(k *kind, id func(*call) string, paths func(*call, string) opPaths) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.writableGroup(c); err != nil {
			return nil, err
		}
		res, err := s.claim(id(c), "")
		if err != nil {
			return nil, err
		}
		if res == nil || res.kind != k {
			return &reply{status: http.StatusNoContent}, nil
		}
		if err := s.checkUnused(res); err != nil {
			return nil, err
		}
		op := s.change(c, res, paths(c, res.loc), 0, stateDeleting, func() { s.remove(res) })
		return accepted(c, op, http.StatusOK, http.StatusAccepted, true, noBody), nil
	}
}

type diskOpBody struct {
	Name       string        `json:"name"`
	Status     string        `json:"status"`
	StartTime  string        `json:"startTime"`
	EndTime    string        `json:"endTime,omitzero"`
	Properties *diskOpOutput `json:"properties,omitzero"`
	Error      *errorDetail  `json:"error,omitzero"`
}

type diskOpOutput struct {
	Output json.RawMessage `json:"output"`
}

// diskOperation answers a poll of a disk operation's Azure-AsyncOperation
// URL: once a change succeeds, its status carries the resource as its output.
func (s *Server) diskOperation(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, err := s.lookupOperation(c, false)
	if err != nil {
		return nil, err
	}
	s.observe(op)
	body := diskOpBody{Name: op.id, Status: op.status, StartTime: timestamp(op.start)}
	if op.status != opInProgress {
		body.EndTime = timestamp(op.end)
	}
	if op.status == opSucceeded && op.result != nil {
		body.Properties = &diskOpOutput{Output: op.result()}
	}
	if op.err != nil {
		body.Error = new(op.err.detail())
	}
	rep := ok(encode(body))
	if op.status == opInProgress {
		rep.header = http.Header{"Retry-After-Ms": {retryAfterMS}}
	}
	return rep, nil
}

// Disk is a snapshot of a managed disk.
type Disk struct {
	ID       string
	Location string
	SKU      string
	SizeGB   int
	// State is its diskState, such as Unattached, Attached or Reserved.
	State string
	// VirtualMachine is the ID of the VM it is attached to, if any.
	VirtualMachine string
	// Source is the disk or snapshot it was copied from, if any.
	Source            string
	ProvisioningState string
	Tags              map[string]string
}

// Disks returns the managed disks the server holds, not counting those of
// scale set VMs, ordered by ID.
func (s *Server) Disks() []Disk {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Disk
	for _, res := range s.resources {
		if res.disk == nil {
			continue
		}
		sku, _ := res.top["sku"].(map[string]any)
		out = append(out, Disk{
			ID: res.id, Location: res.loc, SKU: str(sku, "name"), SizeGB: intOf(res.props["diskSizeGB"]), State: s.diskStateOf(res),
			VirtualMachine: res.disk.vm, Source: res.disk.source, ProvisioningState: res.state, Tags: maps.Clone(res.tags),
		})
	}
	slices.SortFunc(out, func(a, b Disk) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// Snapshot is a snapshot of a managed disk snapshot.
type Snapshot struct {
	ID       string
	Location string
	SKU      string
	// Source is the disk or snapshot it was made from.
	Source      string
	Incremental bool
	// CopyStart reports whether it was copied with createOption CopyStart.
	CopyStart bool
	// CompletionPercent is how much of a CopyStart copy is done; 100 for any other snapshot.
	CompletionPercent int
	// CopyError is the message of the error a CopyStart copy ended in, if any.
	CopyError         string
	ProvisioningState string
	Tags              map[string]string
}

// Snapshots returns the snapshots the server holds, ordered by ID.
func (s *Server) Snapshots() []Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Snapshot
	for _, res := range s.resources {
		if res.snap == nil {
			continue
		}
		sku, _ := res.top["sku"].(map[string]any)
		snap := Snapshot{
			ID: res.id, Location: res.loc, SKU: str(sku, "name"), Source: res.snap.source, Incremental: res.snap.incremental,
			CopyStart: res.snap.copyStart, CompletionPercent: 100, CopyError: res.snap.copyErr, ProvisioningState: res.state, Tags: maps.Clone(res.tags),
		}
		if res.snap.copyStart {
			snap.CompletionPercent = res.snap.percent
		}
		out = append(out, snap)
	}
	slices.SortFunc(out, func(a, b Snapshot) int { return cmp.Compare(a.ID, b.ID) })
	return out
}
