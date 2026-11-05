package arm

import (
	"cmp"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

var (
	netappAccountKind = &kind{
		typ: "Microsoft.NetApp/netAppAccounts", plural: "netAppAccounts", typeName: "NetAppAccount",
		top:      []string{"identity"},
		props:    []string{"activeDirectories", "encryption"},
		readOnly: []string{"disableShowmount", "provisioningState"},
		located:  true, etag: weakETag,
	}
	poolKind = &kind{
		typ: "Microsoft.NetApp/netAppAccounts/capacityPools", plural: "capacityPools", typeName: "CapacityPool",
		props:    []string{"coolAccess", "encryptionType", "qosType", "serviceLevel", "size"},
		readOnly: []string{"poolId", "provisioningState", "totalThroughputMibps", "utilizedThroughputMibps"},
		located:  true, etag: weakETag,
	}
	volumeKind = &kind{
		typ: "Microsoft.NetApp/netAppAccounts/capacityPools/volumes", plural: "volumes", typeName: "Volume",
		top: []string{"zones"},
		props: []string{
			"avsDataStore", "backupId", "capacityPoolResourceId", "coolAccess", "coolAccessRetrievalPolicy", "coolnessPeriod",
			"creationToken", "dataProtection", "defaultGroupQuotaInKiBs", "defaultUserQuotaInKiBs", "deleteBaseSnapshot",
			"enableSubvolumes", "encryptionKeySource", "exportPolicy", "isDefaultQuotaEnabled", "isLargeVolume", "isRestoring",
			"kerberosEnabled", "keyVaultPrivateEndpointResourceId", "ldapEnabled", "networkFeatures", "placementRules",
			"protocolTypes", "proximityPlacementGroup", "securityStyle", "serviceLevel", "smbAccessBasedEnumeration",
			"smbContinuouslyAvailable", "smbEncryption", "smbNonBrowsable", "snapshotDirectoryVisible", "snapshotId", "subnetId",
			"throughputMibps", "unixPermissions", "usageThreshold", "volumeSpecName", "volumeType",
		},
		readOnly: []string{
			"actualThroughputMibps", "baremetalTenantId", "cloneProgress", "dataStoreResourceId", "encrypted", "fileAccessLogs",
			"fileSystemId", "maximumNumberOfFiles", "mountTargets", "networkSiblingSetId", "originatingResourceId",
			"provisionedAvailabilityZone", "provisioningState", "storageToNetworkProximity", "t2Network", "volumeGroupName",
		},
		located: true, etag: weakETag,
	}
	netappSnapshotKind = &kind{
		typ: "Microsoft.NetApp/netAppAccounts/capacityPools/volumes/snapshots", plural: "snapshots", typeName: "Snapshot",
		readOnly: []string{"created", "provisioningState", "snapshotId"},
		located:  true,
	}
)

const (
	netappAccount  = rg + "/providers/Microsoft.NetApp/netAppAccounts/{account}"
	netappPool     = netappAccount + "/capacityPools/{pool}"
	netappVolume   = netappPool + "/volumes/{volume}"
	netappSnapshot = netappVolume + "/snapshots/{snapshot}"
)

func netappAccountID(c *call) string {
	return resourceID(c, netappAccountKind.typ, c.val("account"))
}

func poolID(c *call) string { return netappAccountID(c) + "/capacityPools/" + c.val("pool") }

func volumeID(c *call) string { return poolID(c) + "/volumes/" + c.val("volume") }

func netappSnapshotID(c *call) string { return volumeID(c) + "/snapshots/" + c.val("snapshot") }

// netappName is the name NetApp gives the resource id names: its own name
// after those of the resources that hold it, such as account/pool/volume.
func netappName(id string) string {
	_, rest, _ := strings.Cut(strings.ToLower(id), "/providers/microsoft.netapp/netappaccounts/")
	parts := strings.Split(id[len(id)-len(rest):], "/")
	names := make([]string, 0, len(parts)/2+1)
	for i := 0; i < len(parts); i += 2 {
		names = append(names, parts[i])
	}
	return strings.Join(names, "/")
}

// resultLocation is the query the Location URL of a NetApp or Cognitive
// Services operation adds to its path.
const resultLocation = "operationResultResponseType=Location"

// netappOps are the paths of a Microsoft.NetApp operation, whose
// Azure-AsyncOperation and Location URLs share one path.
func netappOps(c *call, location string) opPaths {
	path := "/subscriptions/" + c.val("sub") + "/providers/Microsoft.NetApp/locations/" + location + "/operationResults/"
	return opPaths{status: path, result: path, resultStatus: http.StatusOK}
}

// netappLevel is a NetApp resource type, such as accounts, capacity pools or volumes.
type netappLevel struct {
	kind    *kind
	path    string
	id      func(*call) string
	parent  func(*call) string
	prepare func(s *Server, res *resource, p *parsed) (func(), *apiError)
	// updatedStatus answers a put to an existing one, if not 200.
	updatedStatus int
	// syncUpdate finishes a put to an existing one before answering.
	syncUpdate bool
	// sync finishes a put before answering.
	sync bool
	// readOnly types are not created through the API.
	readOnly bool
}

var netappLevels = []*netappLevel{
	{kind: netappAccountKind, path: netappAccount, id: netappAccountID},
	{kind: poolKind, path: netappPool, id: poolID, parent: netappAccountID, prepare: (*Server).preparePool},
	{kind: volumeKind, path: netappVolume, id: volumeID, parent: poolID, prepare: (*Server).prepareVolume},
	{
		kind: netappSnapshotKind, path: netappSnapshot, id: netappSnapshotID, parent: volumeID,
		prepare: (*Server).prepareNetAppSnapshot, updatedStatus: http.StatusCreated, syncUpdate: true,
	},
	{kind: backupVaultKind, path: backupVaultPath, id: backupVaultID, parent: netappAccountID},
	{kind: netappBackupKind, path: netappBackupPath, id: netappBackupID, parent: backupVaultID, readOnly: true},
	{kind: backupPolicyKind, path: backupPolicyPath, id: backupPolicyID, parent: netappAccountID},
	{kind: snapshotPolicyKind, path: snapshotPolicyPath, id: snapshotPolicyID, parent: netappAccountID, sync: true},
	{
		kind: volumeGroupKind, path: volumeGroupPath, id: volumeGroupID, parent: netappAccountID,
		prepare: (*Server).prepareVolumeGroup, updatedStatus: http.StatusCreated,
	},
	{kind: cacheKind, path: cachePath, id: cacheID, parent: poolID, prepare: (*Server).prepareCache},
}

func (s *Server) registerNetAppRoutes() {
	s.add("GET", sub+"/providers/Microsoft.NetApp/locations/{loc}/operationResults/{op}", "Microsoft.NetApp/locations/operationresults/read", false,
		s.onePathOperation(resultLocation, s.netappOperation))
	for _, l := range netappLevels {
		s.add("GET", l.path[:strings.LastIndex(l.path, "/")], l.kind.action("read"), false, s.listNetApp(l))
		if !l.readOnly {
			s.add("PUT", l.path, l.kind.action("write"), true, s.putNetApp(l))
		}
		s.add("GET", l.path, l.kind.action("read"), false, s.getNetApp(l))
		s.add("DELETE", l.path, l.kind.action("delete"), true, s.deleteNetApp(l))
	}
}

func (s *Server) putNetApp(l *netappLevel) handler {
	spec := putSpec{
		kind: l.kind, id: l.id, parent: l.parent, paths: netappOps, render: s.renderNetApp,
		updatedStatus: l.updatedStatus, syncUpdate: l.syncUpdate, sync: l.sync,
	}
	spec.prepare = func(res *resource, p *parsed) (func(), *apiError) {
		res.name = netappResourceName(res)
		if l.prepare == nil {
			return func() { s.update(res, p) }, nil
		}
		return l.prepare(s, res, p)
	}
	return s.put(spec)
}

func (s *Server) preparePool(res *resource, p *parsed) (func(), *apiError) {
	if str(p.props, "serviceLevel") == "" || p.props["size"] == nil {
		return nil, unsupported("a capacity pool needs serviceLevel and size")
	}
	return func() { s.update(res, p) }, nil
}

// prepareVolume validates p as the new body of the volume res, and returns
// how to apply it. A new volume takes an address in its subnet. The caller
// holds s.mu.
func (s *Server) prepareVolume(res *resource, p *parsed) (func(), *apiError) {
	subnet := str(p.props, "subnetId")
	if subnet == "" || str(p.props, "creationToken") == "" || p.props["usageThreshold"] == nil {
		return nil, unsupported("a volume needs creationToken, subnetId and usageThreshold")
	}
	return s.holdAddress(res, p, subnet, "subnetId")
}

// maxVolumeSnapshots is how many snapshots Azure lets a volume hold.
const maxVolumeSnapshots = 255

// prepareNetAppSnapshot accepts a snapshot, which has nothing to change, so
// a put to an existing one leaves it as it is. The caller holds s.mu.
func (s *Server) prepareNetAppSnapshot(res *resource, p *parsed) (func(), *apiError) {
	if p.tags != nil {
		return nil, unsupported("a NetApp snapshot takes no tags")
	}
	if s.resources[key(res.id)] != res {
		vol, _, _ := strings.Cut(res.id, "/snapshots/")
		if err := s.checkSnapshotRoom(vol); err != nil {
			return nil, err
		}
	}
	return func() {}, nil
}

// checkSnapshotRoom fails a new snapshot of the volume vol once it holds the
// most Azure allows. The caller holds s.mu.
func (s *Server) checkSnapshotRoom(vol string) *apiError {
	if len(s.volumeSnapshots(vol)) >= maxVolumeSnapshots {
		return newError(http.StatusConflict, "SnapshotLimitReached",
			"Volume '%s' already has the maximum of %d snapshots.", netappName(vol), maxVolumeSnapshots)
	}
	return nil
}

// volumeSnapshots returns the snapshots of the volume vol. The caller holds s.mu.
func (s *Server) volumeSnapshots(vol string) []*resource {
	prefix := key(vol) + "/snapshots/"
	var out []*resource
	for k, snap := range s.resources {
		if snap.kind == netappSnapshotKind && strings.HasPrefix(k, prefix) {
			out = append(out, snap)
		}
	}
	return out
}

func (s *Server) getNetApp(l *netappLevel) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.readableGroup(c); err != nil {
			return nil, err
		}
		if err := s.netappParentMissing(c, l); err != nil {
			return nil, err
		}
		id := l.id(c)
		res := s.read(id)
		if res == nil || res.kind != l.kind {
			return nil, resourceNotFound(l.kind.typ, netappIDName(l.kind, id), c.val("rg"))
		}
		return ok200(s.renderNetApp(res)), nil
	}
}

func (s *Server) listNetApp(l *netappLevel) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.readableGroup(c); err != nil {
			return nil, err
		}
		if err := s.netappParentMissing(c, l); err != nil {
			return nil, err
		}
		items := s.listed(c, l.kind)
		if l.parent != nil {
			prefix := key(l.parent(c)) + "/"
			items = slices.DeleteFunc(items, func(res *resource) bool { return !strings.HasPrefix(key(res.id), prefix) })
		}
		if l.kind == netappBackupKind {
			volume, err := backupVolumeFilter(c)
			if err != nil {
				return nil, err
			}
			if volume != "" {
				items = slices.DeleteFunc(items, func(res *resource) bool { return key(str(res.props, "volumeResourceId")) != key(volume) })
			}
		}
		return ok(listBody(items, func(res *resource) any { return s.renderNetAppListed(res) })), nil
	}
}

func (s *Server) deleteNetApp(l *netappLevel) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.writableGroup(c); err != nil {
			return nil, err
		}
		if f := c.query("forceDelete"); l.kind == volumeKind && f != "" && !strings.EqualFold(f, "true") && !strings.EqualFold(f, "false") {
			return nil, unsupported("forceDelete must be true or false, not '%s'", f)
		}
		res, err := s.claim(l.id(c), "")
		if err != nil {
			return nil, err
		}
		if res == nil || res.kind != l.kind {
			return &reply{status: http.StatusNoContent}, nil
		}
		if err := s.checkUnused(res); err != nil {
			return nil, err
		}
		if res.kind == volumeKind {
			for _, snap := range s.volumeSnapshots(res.id) {
				s.finish(snap.pending)
			}
		}
		op := s.change(c, res, netappOps(c, res.loc), 0, stateDeleting, func() { s.remove(res) })
		return acceptedOnePath(c, op, resultLocation), nil
	}
}

// removeVolumeSnapshots deletes the snapshots of the volume res, as deleting
// a volume does, with or without forceDelete. The caller holds s.mu.
func (s *Server) removeVolumeSnapshots(res *resource) {
	for _, snap := range s.volumeSnapshots(res.id) {
		s.finish(snap.pending)
		delete(s.resources, key(snap.id))
	}
}

// nestedInUse fails the deletion of a resource that still holds others, such
// as a NetApp account or capacity pool that holds pools or volumes, or a
// gallery that holds image definitions. The caller holds s.mu.
func (s *Server) nestedInUse(res *resource) *apiError {
	prefix := key(res.id) + "/"
	for k := range s.resources {
		if strings.HasPrefix(k, prefix) {
			return newError(http.StatusConflict, "CannotDeleteResource", "Can not delete resource before nested resources are deleted.")
		}
	}
	return nil
}

func (s *Server) renderNetApp(res *resource) map[string]any {
	m, props := envelope(res)
	m["name"] = netappResourceName(res)
	m["systemData"] = systemData(res)
	switch res.kind {
	case volumeGroupKind:
		vols := s.renderGroupVolumes(res)
		meta, _ := props["groupMetaData"].(map[string]any)
		meta = maps.Clone(meta)
		if meta == nil {
			meta = map[string]any{}
		}
		meta["volumesCount"] = len(vols)
		props["volumes"], props["groupMetaData"] = vols, meta
	case backupPolicyKind:
		props["volumesAssigned"] = len(s.volumesNaming(res.id, "backup", "backupPolicyId"))
	}
	if res.state == stateCreating {
		return m
	}
	switch res.kind {
	case netappSnapshotKind:
		props["snapshotId"], props["created"] = res.guid, timestamp(res.created)
	case poolKind:
		props["poolId"] = res.guid
	case volumeKind:
		props["fileSystemId"] = res.guid
		if res.held != nil {
			props["mountTargets"] = []any{map[string]any{
				"mountTargetId": res.guid, "fileSystemId": res.guid, "ipAddress": res.held.address.String(),
			}}
		}
		if g := s.resources[key(res.volumeGroup)]; g != nil && g.kind == volumeGroupKind {
			props["volumeGroupName"] = g.name
		}
	case backupPolicyKind:
		props["backupPolicyId"] = res.guid
	case netappBackupKind:
		props["backupId"], props["creationDate"], props["completionDate"] = res.guid, timestamp(res.created), timestamp(res.created)
	case cacheKind:
		if res.held != nil {
			props["mountTargets"] = []any{map[string]any{"mountTargetId": res.guid, "ipAddress": res.held.address.String()}}
		}
	}
	return m
}

type netappOpBody struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Status          string           `json:"status"`
	StartTime       string           `json:"startTime"`
	EndTime         string           `json:"endTime"`
	PercentComplete float64          `json:"percentComplete"`
	Properties      netappOpResource `json:"properties"`
	Error           *errorDetail     `json:"error,omitzero"`
}

var netappActions = map[string]string{stateCreating: "CREATE", stateUpdating: "UPDATE", stateDeleting: "DELETE"}

type netappOpResource struct {
	ResourceName string `json:"resourceName"`
	Action       string `json:"action"`
}

// netappOperation answers a poll of a NetApp operation's Azure-AsyncOperation
// URL: 200 with the operation's status, which reads as the state the resource
// shows, such as Creating, until it ends.
func (s *Server) netappOperation(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, err := s.lookupOperation(c, false)
	if err != nil {
		return nil, err
	}
	s.observe(op)
	body := netappOpBody{
		ID: op.statusPath, Name: op.id, Status: op.status, StartTime: timestamp(op.start), EndTime: "0001-01-01T00:00:00Z",
		Properties: netappOpResource{ResourceName: op.target, Action: netappActions[op.state]},
	}
	switch op.status {
	case opInProgress:
		body.Status = op.state
	case opSucceeded:
		body.EndTime, body.PercentComplete = timestamp(op.end), 100
	default:
		body.EndTime = timestamp(op.end)
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

// Volume is a snapshot of a NetApp volume.
type Volume struct {
	ID string
	// MountAddress is the address its mount target has in its subnet.
	MountAddress string
}

// Volumes returns the NetApp volumes the server holds, ordered by ID.
func (s *Server) Volumes() []Volume {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Volume
	for _, res := range s.resources {
		if res.kind == volumeKind && res.held != nil {
			out = append(out, Volume{ID: res.id, MountAddress: res.held.address.String()})
		}
	}
	slices.SortFunc(out, func(a, b Volume) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// SnapshotVolume adds the snapshot name, created at at, to the NetApp volume
// with ID id, as a finished create of it does. It returns the snapshot's ID
// and reports whether the volume exists, once any create of it in progress
// finishes, has not failed and is not being deleted, has no snapshot of that
// name and has room for another.
func (s *Server) SnapshotVolume(id, name string, at time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if vol := s.resources[key(id)]; vol == nil || vol.kind != volumeKind || vol.state == stateDeleting {
		return "", false
	}
	vol := s.settled(id)
	if vol == nil || vol.state == stateFailed ||
		name == "" || strings.Contains(name, "/") || at.IsZero() {
		return "", false
	}
	snapID := vol.id + "/snapshots/" + name
	if s.resources[key(snapID)] != nil || s.checkSnapshotRoom(vol.id) != nil {
		return "", false
	}
	at = at.UTC()
	s.resources[key(snapID)] = &resource{
		kind: netappSnapshotKind, id: snapID, name: netappName(snapID), sub: vol.sub, group: vol.group, loc: vol.loc,
		top: map[string]any{}, props: map[string]any{}, state: stateSucceeded, created: at, modified: at, guid: s.newUUID(),
	}
	return snapID, true
}
