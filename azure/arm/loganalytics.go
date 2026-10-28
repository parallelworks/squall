package arm

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

var logWorkspaceKind = &kind{
	typ: "Microsoft.OperationalInsights/workspaces", plural: "workspaces", typeName: "Workspace",
	top: []string{"identity"},
	props: []string{
		"defaultDataCollectionRuleResourceId", "failover", "features", "forceCmkForQuery", "publicNetworkAccessForIngestion",
		"publicNetworkAccessForQuery", "replication", "retentionInDays", "sku", "workspaceCapping",
	},
	readOnly: []string{"createdDate", "customerId", "modifiedDate", "privateLinkScopedResources", "provisioningState"},
	located:  true, etag: quotedETag,
}

const (
	operationalInsights = "/providers/Microsoft.OperationalInsights"
	logWorkspaces       = rg + operationalInsights + "/workspaces"
	// logWorkspaceRetention is how long a deleted workspace can be recovered.
	logWorkspaceRetention = 14 * 24 * time.Hour
)

func logWorkspaceID(c *call) string { return resourceID(c, logWorkspaceKind.typ, c.val("name")) }

// logWorkspaceOps are the paths of a workspace operation: its status under
// the provider's location, and its result under the workspace.
func logWorkspaceOps(c *call, location string) opPaths {
	return opPaths{
		status: "/subscriptions/" + c.val("sub") + operationalInsights + "/locations/" + location + "/operationstatuses/",
		result: logWorkspaceID(c) + "/operationresults/",
	}
}

func (s *Server) registerLogAnalyticsRoutes() {
	s.add("GET", sub+operationalInsights+"/locations/{loc}/operationstatuses/{op}",
		"Microsoft.OperationalInsights/locations/operationstatuses/read", false, s.operationStatus)
	s.add("GET", logWorkspaces+"/{name}/operationresults/{op}", logWorkspaceKind.action("operationresults/read"), false, s.operationResult)
	s.add("GET", logWorkspaces, logWorkspaceKind.action("read"), false, s.listLogWorkspaces)
	s.add("PUT", logWorkspaces+"/{name}", logWorkspaceKind.action("write"), true, s.put(putSpec{
		kind: logWorkspaceKind, id: logWorkspaceID, paths: logWorkspaceOps, render: renderLogWorkspace, prepare: s.prepareLogWorkspace,
	}))
	s.add("GET", logWorkspaces+"/{name}", logWorkspaceKind.action("read"), false, s.getLogWorkspace)
	s.add("DELETE", logWorkspaces+"/{name}", logWorkspaceKind.action("delete"), true, s.deleteLogWorkspace)
}

// prepareLogWorkspace recovers a deleted workspace that a create names in
// the same group and location, keeping its customer ID and the properties
// the create leaves out. The caller holds s.mu.
func (s *Server) prepareLogWorkspace(res *resource, p *parsed) (func(), *apiError) {
	d := s.deletedLogWorkspace(res.id)
	if s.resources[key(res.id)] != nil || d == nil {
		return func() { s.update(res, p) }, nil
	}
	if d.account.loc != p.location {
		return nil, newError(http.StatusConflict, "Conflict",
			"This workspace name is already in use by a deleted workspace in location '%s'. Recover it there or delete it permanently.", d.account.loc)
	}
	return func() {
		delete(s.logWorkspacesDeleted, key(res.id))
		props := maps.Clone(d.account.props)
		maps.Copy(props, p.props)
		p.props = props
		res.guid, res.created, res.restoring = d.account.guid, d.account.created, true
		s.update(res, p)
	}, nil
}

// deletedLogWorkspace returns the deleted workspace id names, if it can
// still be recovered, forgetting it once it cannot. The caller holds s.mu.
func (s *Server) deletedLogWorkspace(id string) *softDeleted {
	d := s.logWorkspacesDeleted[key(id)]
	if d != nil && s.now().Sub(d.deleted) >= logWorkspaceRetention {
		delete(s.logWorkspacesDeleted, key(id))
		return nil
	}
	return d
}

// softDeleteLogWorkspace keeps the workspace res, which is being removed, as
// deleted. The caller holds s.mu.
func (s *Server) softDeleteLogWorkspace(res *resource) {
	s.logWorkspacesDeleted[key(res.id)] = &softDeleted{account: res, id: res.id, deleted: s.now()}
}

func (s *Server) getLogWorkspace(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	res := s.read(logWorkspaceID(c))
	if res == nil || res.kind != logWorkspaceKind {
		return nil, resourceNotFound(logWorkspaceKind.typ, c.val("name"), c.val("rg"))
	}
	return ok200(renderLogWorkspace(res)), nil
}

func (s *Server) listLogWorkspaces(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	return ok(listBody(s.listed(c, logWorkspaceKind), func(res *resource) any { return renderLogWorkspace(res) })), nil
}

// deleteLogWorkspace keeps the workspace as deleted, or with force=true
// deletes it for good, answering 200 when done and 204 when there is none.
func (s *Server) deleteLogWorkspace(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	res, err := s.claim(logWorkspaceID(c), "")
	if err != nil {
		return nil, err
	}
	if res == nil || res.kind != logWorkspaceKind {
		return &reply{status: http.StatusNoContent}, nil
	}
	force := strings.EqualFold(c.query("force"), "true")
	op := s.change(c, res, logWorkspaceOps(c, res.loc), 0, stateDeleting, func() {
		s.remove(res)
		if force {
			delete(s.logWorkspacesDeleted, key(res.id))
		}
	})
	return accepted(c, op, http.StatusOK, http.StatusAccepted, true, noBody), nil
}

// renderLogWorkspace shows the workspace's customer ID, the PerGB2018 SKU,
// 30 days of retention and the features and capping Azure gives a new
// workspace unless set, and a recovered workspace as Restoring until it is.
func renderLogWorkspace(res *resource) map[string]any {
	m, props := envelope(res)
	props["customerId"] = res.guid
	props["createdDate"], props["modifiedDate"] = timestamp(res.created), timestamp(res.created)
	if res.restoring && res.state == stateCreating {
		props["provisioningState"] = "Restoring"
	}
	sku, _ := props["sku"].(map[string]any)
	sku = maps.Clone(sku)
	if sku == nil {
		sku = map[string]any{"name": "PerGB2018"}
	}
	sku["lastSkuUpdate"] = timestamp(res.created)
	props["sku"] = sku
	defaults := map[string]any{
		"retentionInDays": 30, "publicNetworkAccessForIngestion": "Enabled", "publicNetworkAccessForQuery": "Enabled",
		"features":         map[string]any{"legacy": 0, "searchVersion": 1, "enableLogAccessUsingOnlyResourcePermissions": true},
		"workspaceCapping": map[string]any{"dailyQuotaGb": -1.0, "dataIngestionStatus": "RespectQuota"},
	}
	for k, v := range defaults {
		if props[k] == nil {
			props[k] = v
		}
	}
	return m
}

// laRegionCodes are the codes the Application Insights portal's region
// mapping gives Log Analytics in each location, which name the workspace and
// group a component made without one gets.
var laRegionCodes = map[string]string{
	"australiacentral": "CAU", "australiacentral2": "CBR2", "australiaeast": "EAU", "australiasoutheast": "SEAU", "austriaeast": "ATE",
	"belgiumcentral": "BEC", "brazilsouth": "CQ", "canadacentral": "CCAN", "centralindia": "CID", "centralus": "CUS", "chilecentral": "CLC",
	"chinaeast2": "CNE2", "chinaeast3": "CNE3", "chinanorth3": "CNN3", "denmarkeast": "DKE", "eastasia": "EA", "eastus": "EUS", "eastus2": "EUS2",
	"francecentral": "PAR", "germanynorth": "DEN", "germanywestcentral": "DEWC", "indiasouthcentral": "INSC", "indonesiacentral": "IDC",
	"israelcentral": "ILC", "israelnorthwest": "ILNW", "italynorth": "ITN", "japaneast": "EJP", "japanwest": "OS", "jioindiacentral": "JINC",
	"jioindiawest": "JINW", "koreacentral": "SE", "malaysiawest": "MYW", "mexicocentral": "MXC", "newzealandnorth": "NZN",
	"northcentralus": "NCUS", "northeurope": "NEU", "polandcentral": "PLC", "qatarcentral": "QAC", "southafricanorth": "JNB",
	"southafricawest": "CPT", "southcentralus": "SCUS", "southeastasia": "SEA", "spaincentral": "ESC", "swedencentral": "SEC",
	"swedensouth": "SES", "switzerlandnorth": "CHN", "switzerlandwest": "CHW", "taiwannorth": "TWN", "taiwannorthwest": "TWNW",
	"uaecentral": "AUH", "uksouth": "SUK", "ukwest": "WUK", "usgovarizona": "PHX", "usgovtexas": "SN", "usgovvirginia": "USBN1",
	"usnateast": "EXE", "usnatwest": "EXW", "usseceast": "RXE", "ussecwest": "RXW", "ussecwestcentral": "USSWC", "westcentralus": "WCUS",
	"westeurope": "WEU", "westus": "WUS", "westus2": "WUS2", "westus3": "USW3",
}

// defaultLogWorkspace returns the ID of the workspace Application Insights
// connects a component in sub and location to when it names none, making
// it, and its group, when they do not exist. The caller holds s.mu.
func (s *Server) defaultLogWorkspace(sub, location string) string {
	code, ok := laRegionCodes[location]
	if !ok {
		code = location
	}
	name := "DefaultResourceGroup-" + code
	gk := key(sub + "/" + name)
	g := s.groups[gk]
	if g == nil {
		g = &group{id: "/subscriptions/" + sub + "/resourceGroups/" + name, name: name, sub: sub, location: location, state: stateSucceeded}
		s.groups[gk] = g
	}
	id := g.id + "/providers/" + logWorkspaceKind.typ + "/DefaultWorkspace-" + sub + "-" + code
	if s.resources[key(id)] != nil {
		return id
	}
	res := &resource{
		kind: logWorkspaceKind, id: id, name: id[strings.LastIndex(id, "/")+1:], sub: sub, group: g.name, loc: location,
		top: map[string]any{}, props: map[string]any{}, state: stateSucceeded, created: s.now(), guid: s.newUUID(),
	}
	if d := s.deletedLogWorkspace(id); d != nil && d.account.loc == location {
		delete(s.logWorkspacesDeleted, key(id))
		res.guid, res.created, res.props = d.account.guid, d.account.created, d.account.props
	}
	s.touch(res)
	s.resources[key(id)] = res
	return id
}

// DeletedLogAnalyticsWorkspaces returns the IDs of the deleted Log Analytics
// workspaces that can still be recovered, ordered.
func (s *Server) DeletedLogAnalyticsWorkspaces() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, d := range s.logWorkspacesDeleted {
		if s.deletedLogWorkspace(d.id) != nil {
			out = append(out, d.id)
		}
	}
	slices.Sort(out)
	return out
}
