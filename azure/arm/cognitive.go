package arm

import (
	"cmp"
	"crypto/sha512"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"time"
)

var (
	cognitiveAccountKind = &kind{
		typ: "Microsoft.CognitiveServices/accounts", plural: "accounts", typeName: "Account",
		top: []string{"identity", "kind", "sku"},
		props: []string{
			"allowProjectManagement", "allowedFqdnList", "amlWorkspace", "apiProperties", "associatedProjects", "customSubDomainName",
			"defaultProject", "disableLocalAuth", "dynamicThrottlingEnabled", "encryption", "locations", "migrationToken", "networkAcls",
			"networkInjections", "publicNetworkAccess", "raiMonitorConfig", "restrictOutboundNetworkAccess", "storedCompletionsDisabled",
			"userOwnedStorage",
		},
		readOnly: []string{
			"abusePenalty", "callRateLimit", "capabilities", "commitmentPlanAssociations", "dateCreated", "deletionDate", "endpoint",
			"endpoints", "internalId", "isMigrated", "privateEndpointConnections", "provisioningState", "quotaLimit", "scheduledPurgeDate",
			"skuChangeInfo",
		},
		located: true, etag: quotedETag,
	}
	deploymentKind = &kind{
		typ: "Microsoft.CognitiveServices/accounts/deployments", plural: "deployments", typeName: "Deployment",
		top: []string{"sku"},
		props: []string{
			"capacitySettings", "currentCapacity", "deploymentState", "model", "modelProviderData", "parentDeploymentName", "raiPolicyName", "routing",
			"scaleSettings", "serviceTier", "spilloverDeploymentName", "versionUpgradeOption",
		},
		readOnly: []string{"callRateLimit", "capabilities", "dynamicThrottlingEnabled", "provisioningState", "rateLimits"},
	}
)

const (
	cognitive        = "/providers/Microsoft.CognitiveServices"
	cognitiveAccount = rg + cognitive + "/accounts/{account}"
	deployment       = cognitiveAccount + "/deployments/{name}"
	deletedAccount   = sub + cognitive + "/locations/{loc}/resourceGroups/{rg}/deletedAccounts/{account}"
)

func cognitiveAccountID(c *call) string {
	return resourceID(c, cognitiveAccountKind.typ, c.val("account"))
}

func deploymentID(c *call) string { return cognitiveAccountID(c) + "/deployments/" + c.val("name") }

// cognitiveOps are the paths of a Microsoft.CognitiveServices operation,
// whose Azure-AsyncOperation and Location URLs share one path.
func cognitiveOps(c *call, location string) opPaths {
	path := "/subscriptions/" + c.val("sub") + cognitive + "/locations/" + location + "/operationResults/"
	return opPaths{status: path, result: path, resultStatus: http.StatusOK}
}

// softDeleted is a deleted Cognitive Services account, which keeps its name
// and subdomain until it is purged.
type softDeleted struct {
	account *resource
	id      string
	deleted time.Time
	// purge is the purge in progress, if any.
	purge *operation
}

// ModelCapacity is how much of a model a subscription can deploy in a
// location with a deployment SKU, as Microsoft.CognitiveServices lists it.
type ModelCapacity struct {
	Location, Format, Name, Version, SKU string
	Available                            float64
}

// WithModelCapacities sets the model capacities the server lists. The
// default lists none.
func WithModelCapacities(capacities ...ModelCapacity) Option {
	return func(s *Server) { s.capacities = capacities }
}

func (s *Server) registerCognitiveRoutes() {
	s.add("GET", sub+cognitive+"/locations/{loc}/operationResults/{op}", "Microsoft.CognitiveServices/locations/operationresults/read", false,
		s.onePathOperation(resultLocation, s.stateOperation))
	s.add("GET", sub+cognitive+"/modelCapacities", "Microsoft.CognitiveServices/modelCapacities/read", false, s.listModelCapacities)
	s.add("PUT", cognitiveAccount, cognitiveAccountKind.action("write"), true, s.put(putSpec{
		kind: cognitiveAccountKind, id: cognitiveAccountID, paths: cognitiveOps, render: renderCognitiveAccount, prepare: s.prepareCognitiveAccount,
	}))
	s.add("GET", cognitiveAccount, cognitiveAccountKind.action("read"), false, s.getCognitiveAccount)
	s.add("DELETE", cognitiveAccount, cognitiveAccountKind.action("delete"), false, s.deleteCognitiveAccount)
	s.add("POST", cognitiveAccount+"/listKeys", cognitiveAccountKind.action("listKeys/action"), false, s.listCognitiveKeys)
	s.add("GET", cognitiveAccount+"/deployments", deploymentKind.action("read"), false, s.listDeployments)
	s.add("PUT", deployment, deploymentKind.action("write"), false, s.put(putSpec{
		kind: deploymentKind, id: deploymentID, parent: cognitiveAccountID, render: renderDeployment, prepare: s.prepareDeployment, bodyPolled: true,
	}))
	s.add("GET", deployment, deploymentKind.action("read"), false, s.getDeployment)
	s.add("DELETE", deployment, deploymentKind.action("delete"), false, s.deleteDeployment)
	s.add("GET", deletedAccount, "Microsoft.CognitiveServices/locations/resourceGroups/deletedAccounts/read", false, s.getDeletedAccount)
	s.add("DELETE", deletedAccount, "Microsoft.CognitiveServices/locations/resourceGroups/deletedAccounts/delete", true, s.purgeAccount)
}

// prepareCognitiveAccount validates p as the new body of the account res,
// whose subdomain, which is unique across Azure, no other account may hold,
// deleted or not. The caller holds s.mu.
func (s *Server) prepareCognitiveAccount(res *resource, p *parsed) (func(), *apiError) {
	if sku, _ := p.top["sku"].(map[string]any); str(sku, "name") == "" || str(p.top, "kind") == "" {
		return nil, unsupported("a Cognitive Services account needs sku.name and kind")
	}
	if holder := s.subdomainHolder(str(p.props, "customSubDomainName")); holder != nil && holder != res {
		domain := str(p.props, "customSubDomainName")
		return nil, newError(http.StatusConflict, "CustomDomainInUse",
			"Please pick a different name. The subdomain name '%s' is not available as it's already used by a resource. If the resource using the name was deleted in the last 48 hours, you may need to purge the deleted resource. Learn more at \"https://go.microsoft.com/fwlink/?linkid=2215493\"",
			domain)
	}
	return func() { s.update(res, p) }, nil
}

// subdomainHolder returns the account, deleted or not, whose subdomain is
// domain, or nil. The caller holds s.mu.
func (s *Server) subdomainHolder(domain string) *resource {
	if domain == "" {
		return nil
	}
	for _, res := range s.resources {
		if res.kind == cognitiveAccountKind && strings.EqualFold(str(res.props, "customSubDomainName"), domain) {
			return res
		}
	}
	for _, d := range s.deleted {
		if strings.EqualFold(str(d.account.props, "customSubDomainName"), domain) {
			return d.account
		}
	}
	return nil
}

func (s *Server) cognitiveAccount(c *call) (*resource, *apiError) {
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	res := s.read(cognitiveAccountID(c))
	if res == nil || res.kind != cognitiveAccountKind {
		return nil, resourceNotFound(cognitiveAccountKind.typ, c.val("account"), c.val("rg"))
	}
	return res, nil
}

func (s *Server) getCognitiveAccount(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.cognitiveAccount(c)
	if err != nil {
		return nil, err
	}
	return ok200(renderCognitiveAccount(res)), nil
}

// deleteCognitiveAccount deletes the account at once, as Azure answers 200,
// keeping it as deleted until it is purged.
func (s *Server) deleteCognitiveAccount(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	res, err := s.claim(cognitiveAccountID(c), "")
	if err != nil {
		return nil, err
	}
	if res == nil || res.kind != cognitiveAccountKind {
		return &reply{status: http.StatusNoContent}, nil
	}
	s.remove(res)
	return &reply{status: http.StatusOK}, nil
}

// softDelete keeps the account res, which is being removed, as deleted, with
// none of its deployments. The caller holds s.mu.
func (s *Server) softDelete(res *resource) {
	prefix := key(res.id) + "/"
	for k := range s.resources {
		if strings.HasPrefix(k, prefix) {
			delete(s.resources, k)
		}
	}
	id := "/subscriptions/" + res.sub + cognitive + "/locations/" + res.loc + "/resourceGroups/" + res.group + "/deletedAccounts/" + res.name
	s.deleted[key(id)] = &softDeleted{account: res, id: id, deleted: s.now()}
}

func (s *Server) listCognitiveKeys(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.cognitiveAccount(c)
	if err != nil {
		return nil, err
	}
	return ok200(map[string]any{"key1": accessKey(res, "key1", 84), "key2": accessKey(res, "key2", 84)}), nil
}

// accessKey derives the key called name of res, n hex digits long, so it
// never changes.
func accessKey(res *resource, name string, n int) string {
	sum := sha512.Sum512([]byte(res.guid + "/" + name))
	return strings.ToUpper(hex.EncodeToString(sum[:]))[:n]
}

// derivedUUID derives the UUID called name of res, so it never changes.
func derivedUUID(res *resource, name string) string {
	k := strings.ToLower(accessKey(res, name, 32))
	return k[:8] + "-" + k[8:12] + "-" + k[12:16] + "-" + k[16:20] + "-" + k[20:]
}

func renderCognitiveAccount(res *resource) map[string]any {
	m, props := envelope(res)
	props["endpoint"] = "https://" + res.loc + ".api.cognitive.microsoft.com/"
	if domain := str(res.props, "customSubDomainName"); domain != "" {
		props["endpoint"] = "https://" + domain + ".cognitiveservices.azure.com/"
	}
	props["internalId"] = strings.ReplaceAll(res.guid, "-", "")
	props["dateCreated"] = timestamp(res.created)
	return m
}

func (s *Server) getDeletedAccount(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.deletedAccount(c)
	if err != nil {
		return nil, err
	}
	return ok200(renderDeletedAccount(d)), nil
}

func (s *Server) deletedAccount(c *call) (*softDeleted, *apiError) {
	d := s.deleted[key(c.r.URL.Path)]
	if d == nil {
		return nil, newError(http.StatusNotFound, "ResourceNotFound", "The Resource 'Microsoft.CognitiveServices/deletedAccounts/%s' was not found.", c.val("account"))
	}
	return d, nil
}

func renderDeletedAccount(d *softDeleted) map[string]any {
	m := renderCognitiveAccount(d.account)
	m["id"], m["type"] = d.id, "Microsoft.CognitiveServices/deletedAccounts"
	props := m["properties"].(map[string]any)
	props["deletionDate"] = timestamp(d.deleted)
	props["scheduledPurgeDate"] = timestamp(d.deleted.Add(48 * time.Hour))
	return m
}

// purgeAccount removes a deleted account for good, freeing its name and
// subdomain, polled as [Server.onePathOperation] describes.
func (s *Server) purgeAccount(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.deleted[key(c.r.URL.Path)]
	if d == nil {
		return &reply{status: http.StatusNoContent}, nil
	}
	if d.purge != nil && d.purge.complete != nil {
		return acceptedOnePath(c, d.purge, resultLocation), nil
	}
	op := s.begin(c, cognitiveOps(c, d.account.loc), 0, func() *apiError {
		delete(s.deleted, key(d.id))
		return nil
	})
	op.target, op.state = d.id, stateDeleting
	if op.status == opInProgress {
		d.purge = op
	}
	return acceptedOnePath(c, op, resultLocation), nil
}

// DeletedAccounts returns the IDs of the deleted Cognitive Services accounts
// the server keeps until they are purged, ordered.
func (s *Server) DeletedAccounts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, d := range s.deleted {
		out = append(out, d.id)
	}
	slices.Sort(out)
	return out
}

// prepareDeployment validates p as the new body of the deployment res. The
// caller holds s.mu.
func (s *Server) prepareDeployment(res *resource, p *parsed) (func(), *apiError) {
	sku, _ := p.top["sku"].(map[string]any)
	model, _ := p.props["model"].(map[string]any)
	if str(sku, "name") == "" || str(model, "name") == "" || str(model, "format") == "" {
		return nil, unsupported("a deployment needs sku.name, properties.model.name and properties.model.format")
	}
	data, err := object(p.props, "modelProviderData")
	if err != nil {
		return nil, err
	}
	if err := checkMembers(data, "ModelProviderData", "properties.modelProviderData", "countryCode", "industry", "organizationName"); err != nil {
		return nil, err
	}
	return func() { s.update(res, p) }, nil
}

// Deployment is a snapshot of a Cognitive Services deployment.
type Deployment struct {
	ID string
	// Account is the ID of the account it belongs to.
	Account                              string
	ModelFormat, ModelName, ModelVersion string
	SKU                                  string
	ProvisioningState                    string
	// ModelProviderData is what the deployment told the model's provider about the caller, or nil.
	ModelProviderData *ModelProviderData
}

// ModelProviderData is the attestation a deployment of a partner model gives its provider.
type ModelProviderData struct {
	Industry, OrganizationName, CountryCode string
}

// Deployments returns the Cognitive Services deployments the server holds, ordered by ID.
func (s *Server) Deployments() []Deployment {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Deployment
	for _, res := range s.resources {
		if res.kind != deploymentKind {
			continue
		}
		model, _ := res.props["model"].(map[string]any)
		sku, _ := res.top["sku"].(map[string]any)
		d := Deployment{
			ID: res.id, Account: res.id[:strings.LastIndex(strings.ToLower(res.id), "/deployments/")],
			ModelFormat: str(model, "format"), ModelName: str(model, "name"), ModelVersion: str(model, "version"), SKU: str(sku, "name"),
			ProvisioningState: res.state,
		}
		if data, ok := res.props["modelProviderData"].(map[string]any); ok {
			d.ModelProviderData = &ModelProviderData{
				Industry: str(data, "industry"), OrganizationName: str(data, "organizationName"), CountryCode: str(data, "countryCode"),
			}
		}
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b Deployment) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

func (s *Server) getDeployment(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.cognitiveAccount(c); err != nil {
		return nil, err
	}
	res := s.read(deploymentID(c))
	if res == nil || res.kind != deploymentKind {
		return nil, resourceNotFound(deploymentKind.typ, c.val("account")+"/"+c.val("name"), c.val("rg"))
	}
	return polled(res, ok200(renderDeployment(res))), nil
}

func (s *Server) listDeployments(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, err := s.cognitiveAccount(c)
	if err != nil {
		return nil, err
	}
	prefix := key(account.id) + "/"
	items := slices.DeleteFunc(s.listed(c, deploymentKind), func(res *resource) bool { return !strings.HasPrefix(key(res.id), prefix) })
	return ok(listBody(items, func(res *resource) any { return renderDeployment(res) })), nil
}

// deleteDeployment deletes the deployment at once, as Azure answers 200.
func (s *Server) deleteDeployment(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	res, err := s.claim(deploymentID(c), "")
	if err != nil {
		return nil, err
	}
	if res == nil || res.kind != deploymentKind {
		return &reply{status: http.StatusNoContent}, nil
	}
	s.remove(res)
	return &reply{status: http.StatusOK}, nil
}

// renderDeployment shows a deployment being created as Accepted, as Azure
// answers its create, and its SKU's capacity as its current capacity.
func renderDeployment(res *resource) map[string]any {
	m, props := envelope(res)
	if res.state == stateCreating {
		props["provisioningState"] = "Accepted"
	}
	if sku, _ := res.top["sku"].(map[string]any); sku["capacity"] != nil {
		props["currentCapacity"] = sku["capacity"]
	}
	return m
}

// listModelCapacities lists the capacities [WithModelCapacities] sets for the
// model the call names, any version when it names none.
func (s *Server) listModelCapacities(c *call) (*reply, *apiError) {
	format, name, version := c.query("modelFormat"), c.query("modelName"), c.query("modelVersion")
	if format == "" || name == "" {
		return nil, invalidParameter("modelName", "The model format and name are required.")
	}
	var items []ModelCapacity
	for _, mc := range s.capacities {
		if strings.EqualFold(mc.Format, format) && strings.EqualFold(mc.Name, name) && (version == "" || mc.Version == version) {
			items = append(items, mc)
		}
	}
	slices.SortFunc(items, func(a, b ModelCapacity) int {
		return cmp.Or(cmp.Compare(a.Location, b.Location), cmp.Compare(a.SKU, b.SKU), cmp.Compare(a.Version, b.Version))
	})
	return ok(listBody(items, func(mc ModelCapacity) any {
		model := mc.Format + "." + mc.Name + "." + mc.Version
		return map[string]any{
			"id":       "/subscriptions/" + c.val("sub") + cognitive + "/locations/" + mc.Location + "/models/" + model + "/skuCapacities/" + mc.SKU,
			"name":     mc.SKU,
			"type":     "Microsoft.CognitiveServices/locations/models/skuCapacities",
			"location": mc.Location,
			"properties": map[string]any{
				"availableCapacity": mc.Available, "skuName": mc.SKU, "scopeId": mc.Location, "scopeType": "Regional",
				"model": map[string]any{"format": mc.Format, "name": mc.Name, "version": mc.Version},
			},
		}
	})), nil
}

// stateOperation answers a poll of an Azure-AsyncOperation URL whose status
// reads as the state the resource shows, such as Creating, until it ends, as
// Microsoft.CognitiveServices answers.
func (s *Server) stateOperation(c *call) (*reply, *apiError) {
	return s.pollOperation(c, func(op *operation) string { return op.state })
}
