package arm

import (
	"cmp"
	"net/http"
	"slices"
	"strings"
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
)

const (
	netappAccount = rg + "/providers/Microsoft.NetApp/netAppAccounts/{account}"
	netappPool    = netappAccount + "/capacityPools/{pool}"
	netappVolume  = netappPool + "/volumes/{volume}"
)

func netappAccountID(c *call) string {
	return resourceID(c, netappAccountKind.typ, c.val("account"))
}

func poolID(c *call) string { return netappAccountID(c) + "/capacityPools/" + c.val("pool") }

func volumeID(c *call) string { return poolID(c) + "/volumes/" + c.val("volume") }

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

// netappLocation is the query a NetApp operation's Location URL adds to its path.
const netappLocation = "operationResultResponseType=Location"

// netappOps are the paths of a Microsoft.NetApp operation, whose
// Azure-AsyncOperation and Location URLs share one path.
func netappOps(c *call, location string) opPaths {
	path := "/subscriptions/" + c.val("sub") + "/providers/Microsoft.NetApp/locations/" + location + "/operationResults/"
	return opPaths{status: path, result: path, resultStatus: http.StatusOK}
}

// netappLevel is a NetApp resource type: accounts, capacity pools or volumes.
type netappLevel struct {
	kind    *kind
	path    string
	id      func(*call) string
	parent  func(*call) string
	prepare func(s *Server, res *resource, p *parsed) (func(), *apiError)
}

var netappLevels = []*netappLevel{
	{kind: netappAccountKind, path: netappAccount, id: netappAccountID},
	{kind: poolKind, path: netappPool, id: poolID, parent: netappAccountID, prepare: (*Server).preparePool},
	{kind: volumeKind, path: netappVolume, id: volumeID, parent: poolID, prepare: (*Server).prepareVolume},
}

func (s *Server) registerNetAppRoutes() {
	s.add("GET", sub+"/providers/Microsoft.NetApp/locations/{loc}/operationResults/{op}", "Microsoft.NetApp/locations/operationresults/read", false,
		s.onePathOperation(netappLocation, s.netappOperation))
	for _, l := range netappLevels {
		s.add("GET", l.path[:strings.LastIndex(l.path, "/")], l.kind.action("read"), false, s.listNetApp(l))
		s.add("PUT", l.path, l.kind.action("write"), true, s.putNetApp(l))
		s.add("GET", l.path, l.kind.action("read"), false, s.getNetApp(l))
		s.add("DELETE", l.path, l.kind.action("delete"), true, s.deleteNetApp(l))
	}
}

func (s *Server) putNetApp(l *netappLevel) handler {
	spec := putSpec{kind: l.kind, id: l.id, parent: l.parent, paths: netappOps, render: renderNetApp}
	spec.prepare = func(res *resource, p *parsed) (func(), *apiError) {
		res.name = netappName(res.id)
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

func (s *Server) getNetApp(l *netappLevel) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.readableGroup(c); err != nil {
			return nil, err
		}
		id := l.id(c)
		res := s.read(id)
		if res == nil || res.kind != l.kind {
			return nil, resourceNotFound(l.kind.typ, netappName(id), c.val("rg"))
		}
		return ok200(renderNetApp(res)), nil
	}
}

func (s *Server) listNetApp(l *netappLevel) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.readableGroup(c); err != nil {
			return nil, err
		}
		items := s.listed(c, l.kind)
		if l.parent != nil {
			prefix := key(l.parent(c)) + "/"
			items = slices.DeleteFunc(items, func(res *resource) bool { return !strings.HasPrefix(key(res.id), prefix) })
		}
		return ok(listBody(items, func(res *resource) any { return renderNetApp(res) })), nil
	}
}

func (s *Server) deleteNetApp(l *netappLevel) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.writableGroup(c); err != nil {
			return nil, err
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
		op := s.change(c, res, netappOps(c, res.loc), 0, stateDeleting, func() { s.remove(res) })
		return acceptedOnePath(c, op, netappLocation), nil
	}
}

// netappInUse fails the deletion of a NetApp account or capacity pool that
// still holds pools or volumes. The caller holds s.mu.
func (s *Server) netappInUse(res *resource) *apiError {
	prefix := key(res.id) + "/"
	for k := range s.resources {
		if strings.HasPrefix(k, prefix) {
			return newError(http.StatusConflict, "CannotDeleteResource", "Can not delete resource before nested resources are deleted.")
		}
	}
	return nil
}

func renderNetApp(res *resource) map[string]any {
	m, props := envelope(res)
	m["name"] = netappName(res.id)
	if res.state == stateCreating {
		return m
	}
	switch res.kind {
	case poolKind:
		props["poolId"] = res.guid
	case volumeKind:
		props["fileSystemId"] = res.guid
		if res.held != nil {
			props["mountTargets"] = []any{map[string]any{
				"mountTargetId": res.guid, "fileSystemId": res.guid, "ipAddress": res.held.address.String(),
			}}
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
