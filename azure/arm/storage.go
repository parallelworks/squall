package arm

import (
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

var (
	accountKind = &kind{
		typ: "Microsoft.Storage/storageAccounts", plural: "storageAccounts", typeName: "StorageAccountCreateParameters",
		top: []string{"sku", "kind", "identity", "extendedLocation", "placement", "zones"},
		props: []string{
			"accessTier", "allowBlobPublicAccess", "allowCrossTenantReplication", "allowSharedKeyAccess", "allowedCopyScope",
			"azureFilesIdentityBasedAuthentication", "customDomain", "defaultToOAuthAuthentication", "dnsEndpointType",
			"dualStackEndpointPreference", "enableExtendedGroups", "encryption", "immutableStorageWithVersioning", "isHnsEnabled",
			"isLocalUserEnabled", "isNfsV3Enabled", "isSftpEnabled", "keyPolicy", "largeFileSharesState", "minimumTlsVersion",
			"networkAcls", "publicNetworkAccess", "routingPreference", "sasPolicy", "supportsHttpsTrafficOnly",
		},
		located: true,
	}
	blobServiceKind = &kind{
		typ: "Microsoft.Storage/storageAccounts/blobServices", plural: "blobServices", typeName: "BlobServiceProperties",
		topReadOnly: []string{"sku"},
		props: []string{
			"automaticSnapshotPolicyEnabled", "changeFeed", "containerDeleteRetentionPolicy", "cors", "defaultServiceVersion",
			"deleteRetentionPolicy", "isVersioningEnabled", "lastAccessTimeTrackingPolicy", "restorePolicy",
		},
	}
	containerKind = &kind{
		typ: "Microsoft.Storage/storageAccounts/blobServices/containers", plural: "containers", typeName: "BlobContainer",
		props: []string{
			"defaultEncryptionScope", "denyEncryptionScopeOverride", "enableNfsV3AllSquash", "enableNfsV3RootSquash",
			"immutableStorageWithVersioning", "metadata", "publicAccess",
		},
		readOnly: []string{
			"deleted", "deletedTime", "hasImmutabilityPolicy", "hasLegalHold", "immutabilityPolicy", "lastModifiedTime",
			"leaseDuration", "leaseState", "leaseStatus", "legalHold", "remainingRetentionDays", "version",
		},
		etag: quotedETag,
	}
)

// accountState is what a storage account holds beyond its body.
type accountState struct {
	// blob is the account's blob service properties.
	blob map[string]any
}

// accountName is the form Azure requires of a storage account name.
var accountName = regexp.MustCompile(`^[a-z0-9]{3,24}$`)

const (
	storage     = rg + "/providers/Microsoft.Storage"
	account     = storage + "/storageAccounts/{account}"
	blobService = account + "/blobServices/default"
)

func accountID(c *call) string { return resourceID(c, accountKind.typ, c.val("account")) }

func blobServiceID(c *call) string { return accountID(c) + "/blobServices/default" }

func containerID(c *call) string { return blobServiceID(c) + "/containers/" + c.val("name") }

// storageOps are the paths of a Microsoft.Storage operation, which is polled
// by its Location only and answers 200 with the resource once it succeeds.
func storageOps(c *call, _ string) opPaths {
	return opPaths{result: "/subscriptions/" + c.val("sub") + "/providers/Microsoft.Storage/operations/", resultStatus: http.StatusOK}
}

func (s *Server) registerStorageRoutes() {
	s.add("GET", sub+"/providers/Microsoft.Storage/operations/{op}", "Microsoft.Storage/operations/read", false, s.operationResult)
	s.add("POST", sub+"/providers/Microsoft.Storage/checkNameAvailability", "Microsoft.Storage/checkNameAvailability/read", false, s.checkAccountName)
	s.add("GET", sub+"/providers/Microsoft.Storage/storageAccounts", accountKind.action("read"), false, s.listAccounts(false))
	s.add("GET", storage+"/storageAccounts", accountKind.action("read"), false, s.listAccounts(true))
	s.add("PUT", account, accountKind.action("write"), true, s.putAccount)
	s.add("GET", account, accountKind.action("read"), false, s.getAccount)
	s.add("DELETE", account, accountKind.action("delete"), false, s.deleteAccount)
	s.add("POST", account+"/listKeys", accountKind.action("listKeys/action"), false, s.listKeys)
	s.add("PUT", blobService, blobServiceKind.action("write"), false, s.putBlobService)
	s.add("GET", blobService, blobServiceKind.action("read"), false, s.getBlobService)
	s.add("GET", blobService+"/containers", containerKind.action("read"), false, s.listContainers)
	s.add("PUT", blobService+"/containers/{name}", containerKind.action("write"), false, s.putContainer)
	s.add("GET", blobService+"/containers/{name}", containerKind.action("read"), false, s.getContainer)
	s.add("DELETE", blobService+"/containers/{name}", containerKind.action("delete"), false, s.deleteContainer)
}

func (s *Server) putAccount(c *call) (*reply, *apiError) {
	return s.put(putSpec{
		kind: accountKind, id: accountID, paths: storageOps, render: s.accountBody, acceptedBody: noBody, createdStatus: http.StatusOK,
		prepare: func(res *resource, p *parsed) (func(), *apiError) {
			if !accountName.MatchString(res.name) {
				return nil, newError(http.StatusBadRequest, "AccountNameInvalid", "%s", invalidAccountName(res.name))
			}
			if sku, _ := p.top["sku"].(map[string]any); str(sku, "name") == "" || str(p.top, "kind") == "" {
				return nil, unsupported("a storage account needs sku.name and kind")
			}
			if other := s.accountNamed(res.name); other != nil && other != res {
				if key(other.sub) == key(res.sub) {
					return nil, newError(http.StatusConflict, "StorageAccountInAnotherResourceGroup",
						"The account %s is already in another resource group in this subscription.", res.name)
				}
				return nil, newError(http.StatusConflict, "StorageAccountAlreadyTaken", "The storage account named %s is already taken.", res.name)
			}
			return func() {
				if res.account == nil {
					res.account = &accountState{blob: map[string]any{}}
				}
				s.update(res, p)
			}, nil
		},
	})(c)
}

func invalidAccountName(name string) string {
	return name + " is not a valid storage account name. Storage account name must be between 3 and 24 characters in length and use numbers and lower-case letters only."
}

// accountNamed returns the storage account with name in any group, as names
// are unique across Azure. The caller holds s.mu.
func (s *Server) accountNamed(name string) *resource {
	for _, res := range s.resources {
		if res.kind == accountKind && strings.EqualFold(res.name, name) {
			return res
		}
	}
	return nil
}

func (s *Server) checkAccountName(c *call) (*reply, *apiError) {
	body, err := readObject(c.r)
	if err != nil {
		return nil, err
	}
	if err := checkMembers(body, "StorageAccountCheckNameAvailabilityParameters", "", "name", "type"); err != nil {
		return nil, err
	}
	name := str(body, "name")
	if !strings.EqualFold(str(body, "type"), accountKind.typ) {
		return nil, badRequest("The type '%s' is not supported. It must be '%s'.", str(body, "type"), accountKind.typ)
	}
	if !accountName.MatchString(name) {
		return ok200(map[string]any{"nameAvailable": false, "reason": "AccountNameInvalid", "message": invalidAccountName(name)}), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accountNamed(name) != nil {
		return ok200(map[string]any{
			"nameAvailable": false, "reason": "AlreadyExists", "message": "The storage account named " + name + " is already taken.",
		}), nil
	}
	return ok200(map[string]any{"nameAvailable": true}), nil
}

// account returns the call's storage account, counting the read toward a
// change in progress, or the error a read of it answers. The caller holds s.mu.
func (s *Server) account(c *call) (*resource, *apiError) {
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	res := s.read(accountID(c))
	if res == nil || res.kind != accountKind {
		return nil, resourceNotFound(accountKind.typ, c.val("account"), c.val("rg"))
	}
	return res, nil
}

// writableAccount returns the call's storage account, after any change to it
// in progress finishes, for a change to what it holds. The caller holds s.mu.
func (s *Server) writableAccount(c *call, k *kind) (*resource, *apiError) {
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	res := s.settled(accountID(c))
	if res == nil || res.kind != accountKind {
		return nil, parentNotFound(k, accountID(c))
	}
	return res, nil
}

func (s *Server) getAccount(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.account(c)
	if err != nil {
		return nil, err
	}
	return ok200(s.accountBody(res)), nil
}

func (s *Server) listAccounts(inGroup bool) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var items []*resource
		if inGroup {
			if err := s.readableGroup(c); err != nil {
				return nil, err
			}
			items = s.listed(c, accountKind)
		} else {
			observe := s.observer()
			for _, res := range s.resources {
				if res.kind == accountKind && key(res.sub) == key(c.val("sub")) {
					observe(res.pending)
				}
			}
			for _, res := range s.resources {
				if res.kind == accountKind && key(res.sub) == key(c.val("sub")) {
					items = append(items, res)
				}
			}
			slices.SortFunc(items, func(a, b *resource) int { return cmp.Compare(key(a.id), key(b.id)) })
		}
		return ok(listBody(items, func(res *resource) any { return s.accountBody(res) })), nil
	}
}

func (s *Server) deleteAccount(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	res := s.settled(accountID(c))
	if res == nil || res.kind != accountKind {
		return &reply{status: http.StatusNoContent}, nil
	}
	s.remove(res)
	return &reply{status: http.StatusOK}, nil
}

func (s *Server) listKeys(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.account(c)
	if err != nil {
		return nil, err
	}
	keys := make([]any, 0, 2)
	for _, name := range []string{"key1", "key2"} {
		sum := sha256.Sum256([]byte(res.guid + "/" + name))
		keys = append(keys, map[string]any{
			"keyName": name, "value": base64.StdEncoding.EncodeToString(append(sum[:], sum[:]...)),
			"permissions": "FULL", "creationTime": timestamp(res.created),
		})
	}
	return ok200(map[string]any{"keys": keys}), nil
}

func (s *Server) accountBody(res *resource) map[string]any {
	m, props := envelope(res)
	if sku, _ := res.top["sku"].(map[string]any); sku != nil {
		m["sku"] = renderSKU(sku)
	}
	props["creationTime"] = timestamp(res.created)
	props["primaryLocation"] = res.loc
	props["statusOfPrimary"] = "available"
	endpoints := map[string]any{}
	for _, svc := range []string{"blob", "dfs", "file", "queue", "table"} {
		endpoints[svc] = "https://" + res.name + "." + svc + ".core.windows.net/"
	}
	props["primaryEndpoints"] = endpoints
	return m
}

// renderSKU adds the tier Azure derives from a storage SKU's name.
func renderSKU(sku map[string]any) map[string]any {
	out := maps.Clone(sku)
	out["tier"] = "Standard"
	if strings.HasPrefix(str(sku, "name"), "Premium") {
		out["tier"] = "Premium"
	}
	return out
}

func (s *Server) putBlobService(c *call) (*reply, *apiError) {
	p, err := parseBody(c, blobServiceKind)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.writableAccount(c, blobServiceKind)
	if err != nil {
		return nil, err
	}
	maps.Copy(res.account.blob, p.props)
	return ok200(blobServiceBody(c, res)), nil
}

func (s *Server) getBlobService(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.account(c)
	if err != nil {
		return nil, err
	}
	return ok200(blobServiceBody(c, res)), nil
}

func blobServiceBody(c *call, res *resource) map[string]any {
	props := maps.Clone(res.account.blob)
	if _, ok := props["cors"]; !ok {
		props["cors"] = map[string]any{"corsRules": []any{}}
	}
	m := map[string]any{"id": blobServiceID(c), "name": "default", "type": blobServiceKind.typ, "properties": props}
	if sku, _ := res.top["sku"].(map[string]any); sku != nil {
		m["sku"] = renderSKU(sku)
	}
	return m
}

func containerNotFound() *apiError {
	return newError(http.StatusNotFound, "ContainerNotFound", "The specified container does not exist.")
}

func (s *Server) putContainer(c *call) (*reply, *apiError) {
	p, err := parseBody(c, containerKind)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.writableAccount(c, containerKind); err != nil {
		return nil, err
	}
	id := containerID(c)
	if s.resources[key(id)] != nil {
		return nil, newError(http.StatusConflict, "ContainerAlreadyExists", "The specified container already exists.")
	}
	res := s.newResource(c, containerKind, id, p)
	s.resources[key(id)] = res
	return &reply{status: http.StatusCreated, body: encode(renderContainer(res))}, nil
}

func (s *Server) getContainer(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.account(c); err != nil {
		return nil, err
	}
	res := s.resources[key(containerID(c))]
	if res == nil {
		return nil, containerNotFound()
	}
	return ok200(renderContainer(res)), nil
}

func (s *Server) listContainers(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.account(c); err != nil {
		return nil, err
	}
	prefix := key(blobServiceID(c)) + "/containers/"
	var items []*resource
	for k, res := range s.resources {
		if res.kind == containerKind && strings.HasPrefix(k, prefix) {
			items = append(items, res)
		}
	}
	slices.SortFunc(items, func(a, b *resource) int { return cmp.Compare(key(a.name), key(b.name)) })
	return ok(listBody(items, func(res *resource) any { return renderContainer(res) })), nil
}

func (s *Server) deleteContainer(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.writableAccount(c, containerKind); err != nil {
		return nil, err
	}
	res := s.resources[key(containerID(c))]
	if res == nil {
		return &reply{status: http.StatusNoContent}, nil
	}
	s.remove(res)
	return &reply{status: http.StatusOK}, nil
}

func renderContainer(res *resource) map[string]any {
	m, props := envelope(res)
	delete(props, "provisioningState")
	if _, ok := props["publicAccess"]; !ok {
		props["publicAccess"] = "None"
	}
	props["deleted"] = false
	props["hasImmutabilityPolicy"] = false
	props["hasLegalHold"] = false
	props["lastModifiedTime"] = timestamp(res.created)
	props["leaseState"] = "Available"
	props["leaseStatus"] = "Unlocked"
	props["remainingRetentionDays"] = 0
	return m
}

// removeAccountChildren deletes what the storage account res holds. The caller holds s.mu.
func (s *Server) removeAccountChildren(res *resource) {
	prefix := key(res.id) + "/"
	for k := range s.resources {
		if strings.HasPrefix(k, prefix) {
			delete(s.resources, k)
		}
	}
}
