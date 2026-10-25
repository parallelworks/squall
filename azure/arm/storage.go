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
	fileServiceKind = &kind{
		typ: "Microsoft.Storage/storageAccounts/fileServices", plural: "fileServices", typeName: "FileServiceProperties",
		topReadOnly: []string{"sku"},
		props:       []string{"cors", "protocolSettings", "shareDeleteRetentionPolicy"},
	}
	shareKind = &kind{
		typ: "Microsoft.Storage/storageAccounts/fileServices/shares", plural: "shares", typeName: "FileShare",
		props: []string{
			"accessTier", "enabledProtocols", "fileSharePaidBursting", "metadata", "provisionedBandwidthMibps",
			"provisionedIops", "rootSquash", "shareQuota", "signedIdentifiers",
		},
		readOnly: []string{
			"accessTierChangeTime", "accessTierStatus", "deleted", "deletedTime", "includedBurstIops", "lastModifiedTime",
			"leaseDuration", "leaseState", "leaseStatus", "maxBurstCreditsForIops", "nextAllowedProvisionedBandwidthDowngradeTime",
			"nextAllowedProvisionedIopsDowngradeTime", "nextAllowedQuotaDowngradeTime", "remainingRetentionDays",
			"shareUsageBytes", "snapshotTime", "version",
		},
		etag: quotedETag,
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
	// services are the properties of the account's services, by their path.
	services map[string]map[string]any
}

// accountName is the form Azure requires of a storage account name.
var accountName = regexp.MustCompile(`^[a-z0-9]{3,24}$`)

const (
	storage = rg + "/providers/Microsoft.Storage"
	account = storage + "/storageAccounts/{account}"
)

func accountID(c *call) string { return resourceID(c, accountKind.typ, c.val("account")) }

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
	for _, svc := range []*service{blobService, fileService} {
		path := account + "/" + svc.path + "/default"
		s.add("PUT", path, svc.kind.action("write"), false, s.putService(svc))
		s.add("GET", path, svc.kind.action("read"), false, s.getService(svc))
	}
	for _, ch := range []*child{containers, shares} {
		path := account + "/" + ch.service.path + "/default/" + ch.kind.plural
		s.add("GET", path, ch.kind.action("read"), false, s.listChildren(ch))
		s.add("PUT", path+"/{name}", ch.kind.action("write"), false, s.putChild(ch))
		s.add("GET", path+"/{name}", ch.kind.action("read"), false, s.getChild(ch))
		s.add("DELETE", path+"/{name}", ch.kind.action("delete"), false, s.deleteChild(ch))
	}
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
					res.account = &accountState{services: map[string]map[string]any{}}
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

// service is a storage account service, such as Blob, whose properties the
// account keeps.
type service struct {
	kind *kind
	// path is the service's segment of its ID, such as blobServices.
	path string
	// replace is whether setting the properties replaces them all, rather
	// than keeping the ones the call leaves out.
	replace bool
}

var (
	blobService = &service{kind: blobServiceKind, path: "blobServices"}
	fileService = &service{kind: fileServiceKind, path: "fileServices", replace: true}
)

func (svc *service) id(c *call) string { return accountID(c) + "/" + svc.path + "/default" }

func (s *Server) putService(svc *service) handler {
	return func(c *call) (*reply, *apiError) {
		p, err := parseBody(c, svc.kind)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		res, err := s.writableAccount(c, svc.kind)
		if err != nil {
			return nil, err
		}
		if svc.replace || res.account.services[svc.path] == nil {
			res.account.services[svc.path] = map[string]any{}
		}
		maps.Copy(res.account.services[svc.path], p.props)
		return ok200(svc.body(c, res)), nil
	}
}

func (s *Server) getService(svc *service) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		res, err := s.account(c)
		if err != nil {
			return nil, err
		}
		return ok200(svc.body(c, res)), nil
	}
}

func (svc *service) body(c *call, res *resource) map[string]any {
	props := maps.Clone(res.account.services[svc.path])
	if props == nil {
		props = map[string]any{}
	}
	if _, ok := props["cors"]; !ok {
		props["cors"] = map[string]any{"corsRules": []any{}}
	}
	m := map[string]any{"id": svc.id(c), "name": "default", "type": svc.kind.typ, "properties": props}
	if sku, _ := res.top["sku"].(map[string]any); sku != nil {
		m["sku"] = renderSKU(sku)
	}
	return m
}

// child is a resource of a storage account service, such as a blob container.
type child struct {
	kind    *kind
	service *service
	// exists and notFound are the errors the service answers for a create of
	// one that exists and a read of one that does not.
	exists, notFound func() *apiError
	render           func(*resource) map[string]any
}

var (
	containers = &child{
		kind: containerKind, service: blobService, render: renderContainer,
		exists: func() *apiError {
			return newError(http.StatusConflict, "ContainerAlreadyExists", "The specified container already exists.")
		},
		notFound: func() *apiError {
			return newError(http.StatusNotFound, "ContainerNotFound", "The specified container does not exist.")
		},
	}
	shares = &child{
		kind: shareKind, service: fileService, render: renderShare,
		exists: func() *apiError {
			return newError(http.StatusConflict, "ShareAlreadyExists", "The specified share already exists.")
		},
		notFound: func() *apiError {
			return newError(http.StatusNotFound, "ShareNotFound", "The specified share does not exist.")
		},
	}
)

func (ch *child) prefix(c *call) string { return ch.service.id(c) + "/" + ch.kind.plural + "/" }

func (ch *child) id(c *call) string { return ch.prefix(c) + c.val("name") }

func (s *Server) putChild(ch *child) handler {
	return func(c *call) (*reply, *apiError) {
		p, err := parseBody(c, ch.kind)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, err := s.writableAccount(c, ch.kind); err != nil {
			return nil, err
		}
		id := ch.id(c)
		if s.resources[key(id)] != nil {
			return nil, ch.exists()
		}
		res := s.newResource(c, ch.kind, id, p)
		s.resources[key(id)] = res
		return &reply{status: http.StatusCreated, body: encode(ch.render(res))}, nil
	}
}

func (s *Server) getChild(ch *child) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, err := s.account(c); err != nil {
			return nil, err
		}
		res := s.resources[key(ch.id(c))]
		if res == nil {
			return nil, ch.notFound()
		}
		return ok200(ch.render(res)), nil
	}
}

func (s *Server) listChildren(ch *child) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, err := s.account(c); err != nil {
			return nil, err
		}
		prefix := key(ch.prefix(c))
		var items []*resource
		for k, res := range s.resources {
			if res.kind == ch.kind && strings.HasPrefix(k, prefix) {
				items = append(items, res)
			}
		}
		slices.SortFunc(items, func(a, b *resource) int { return cmp.Compare(key(a.name), key(b.name)) })
		return ok(listBody(items, func(res *resource) any { return ch.render(res) })), nil
	}
}

func (s *Server) deleteChild(ch *child) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, err := s.writableAccount(c, ch.kind); err != nil {
			return nil, err
		}
		res := s.resources[key(ch.id(c))]
		if res == nil {
			return &reply{status: http.StatusNoContent}, nil
		}
		s.remove(res)
		return &reply{status: http.StatusOK}, nil
	}
}

// renderLeased renders a container or share with the lease and deletion state of one in use.
func renderLeased(res *resource) (map[string]any, map[string]any) {
	m, props := envelope(res)
	delete(props, "provisioningState")
	props["deleted"] = false
	props["lastModifiedTime"] = timestamp(res.created)
	props["leaseState"] = "Available"
	props["leaseStatus"] = "Unlocked"
	props["remainingRetentionDays"] = 0
	return m, props
}

func renderContainer(res *resource) map[string]any {
	m, props := renderLeased(res)
	if _, ok := props["publicAccess"]; !ok {
		props["publicAccess"] = "None"
	}
	props["hasImmutabilityPolicy"] = false
	props["hasLegalHold"] = false
	return m
}

func renderShare(res *resource) map[string]any {
	m, props := renderLeased(res)
	if _, ok := props["enabledProtocols"]; !ok {
		props["enabledProtocols"] = "SMB"
	}
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
