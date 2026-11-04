package arm

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"io"
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
	queueServiceKind = &kind{
		typ: "Microsoft.Storage/storageAccounts/queueServices", plural: "queueServices", typeName: "QueueServiceProperties",
		props: []string{"cors"},
	}
	tableServiceKind = &kind{
		typ: "Microsoft.Storage/storageAccounts/tableServices", plural: "tableServices", typeName: "TableServiceProperties",
		props: []string{"cors"},
	}
	queueKind = &kind{
		typ: "Microsoft.Storage/storageAccounts/queueServices/queues", plural: "queues", typeName: "StorageQueue",
		props: []string{"metadata"}, readOnly: []string{"approximateMessageCount"},
	}
	tableKind = &kind{
		typ: "Microsoft.Storage/storageAccounts/tableServices/tables", plural: "tables", typeName: "Table",
		props: []string{"signedIdentifiers"}, readOnly: []string{"tableName"},
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
	// deleted are the account's soft-deleted blob containers.
	deleted []*deletedContainer
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
	for _, svc := range []*service{blobService, fileService, queueService, tableService} {
		path := account + "/" + svc.path + "/default"
		s.add("PUT", path, svc.kind.action("write"), false, s.putService(svc))
		s.add("GET", path, svc.kind.action("read"), false, s.getService(svc))
	}
	s.registerWormRoutes()
	for _, ch := range []*child{containers, shares, queues, tables} {
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
			const vlw = "immutableStorageWithVersioning"
			created := res.account == nil
			m, _ := p.props[vlw].(map[string]any)
			if pol := m["immutabilityPolicy"]; pol != nil {
				if pm, ok := pol.(map[string]any); !ok || len(pm) > 0 {
					return nil, unsupported("a default version-level immutability policy is not supported")
				}
			}
			if m != nil {
				m = maps.Clone(m)
				delete(m, "immutabilityPolicy")
				p.props[vlw] = m
			}
			_, given := boolean(m, "enabled")
			was := res.props[vlw]
			if hns, _ := boolean(p.props, "isHnsEnabled"); hns && (versionLevelWORM(p.props) || !created && s.versionLevelAnywhere(res)) {
				return nil, unsupported("version-level immutability on an account with a hierarchical namespace is not supported")
			}
			if !created && given && versionLevelWORM(p.props) != versionLevelWORM(res.props) {
				return nil, newError(http.StatusBadRequest, "PropertyIsImmutable",
					"Version-level immutability can be enabled only when a storage account is created, and cannot be disabled.")
			}
			return func() {
				if created {
					res.account = &accountState{services: map[string]map[string]any{}}
					if versionLevelWORM(p.props) {
						res.account.services[blobService.path] = map[string]any{"isVersioningEnabled": true}
					}
				}
				s.update(res, p)
				if !created && !given {
					if res.props == nil {
						res.props = map[string]any{}
					}
					if was != nil {
						res.props[vlw] = was
					} else {
						delete(res.props, vlw)
					}
				}
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
	s.deletedContainers(res)
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
	s.deletedContainers(res)
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
			items = s.listedInSubscription(c, accountKind)
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
	if err := s.accountProtected(res); err != nil {
		return nil, err
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
	blobService  = &service{kind: blobServiceKind, path: "blobServices"}
	fileService  = &service{kind: fileServiceKind, path: "fileServices", replace: true}
	queueService = &service{kind: queueServiceKind, path: "queueServices"}
	tableService = &service{kind: tableServiceKind, path: "tableServices"}
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
		if on, ok := boolean(p.props, "isVersioningEnabled"); ok && !on && svc == blobService && s.versionLevelAnywhere(res) {
			return nil, newError(http.StatusBadRequest, "InvalidRequestPropertyValue",
				"Conflicting feature 'VersionLevelWorm' is enabled. Please disable it and retry.")
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
	// upsert children take a PUT of one that exists as an update, and
	// answer 200 to every PUT and 204 to every DELETE.
	upsert bool
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
	queues = &child{
		kind: queueKind, service: queueService, render: renderQueue, upsert: true,
		notFound: func() *apiError {
			return newError(http.StatusNotFound, "QueueNotFound", "The specified queue does not exist.")
		},
	}
	tables = &child{
		kind: tableKind, service: tableService, render: renderTable, upsert: true,
		notFound: func() *apiError {
			return newError(http.StatusNotFound, "TableNotFound", "The table specified does not exist.")
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
		if ch == tables {
			// A table create may come with no body.
			body, _ := io.ReadAll(io.LimitReader(c.r.Body, maxBody))
			if len(bytes.TrimSpace(body)) == 0 {
				body = []byte("{}")
			}
			c.r.Body = io.NopCloser(bytes.NewReader(body))
		}
		p, err := parseBody(c, ch.kind)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		acct, err := s.writableAccount(c, ch.kind)
		if err != nil {
			return nil, err
		}
		if ch == containers {
			if err := containerNameError(c.val("name")); err != nil {
				return nil, err
			}
			if hns, _ := boolean(acct.props, "isHnsEnabled"); hns && versionLevelWORM(p.props) {
				return nil, unsupported("version-level immutability on an account with a hierarchical namespace is not supported")
			}
			if versionLevelWORM(p.props) && !settingsOf(acct).versioning {
				return nil, newError(http.StatusBadRequest, "InvalidRequestPropertyValue",
					"Version-level immutability needs blob versioning enabled on the storage account.")
			}
		}
		id := ch.id(c)
		if existing := s.resources[key(id)]; existing != nil {
			if !ch.upsert {
				return nil, ch.exists()
			}
			s.update(existing, p)
			return ok200(s.renderChild(ch, existing)), nil
		}
		res := s.newResource(c, ch.kind, id, p)
		s.resources[key(id)] = res
		if ch.upsert {
			return ok200(s.renderChild(ch, res)), nil
		}
		return &reply{status: http.StatusCreated, body: encode(s.renderChild(ch, res))}, nil
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
		m := s.renderChild(ch, res)
		expand := c.query("$expand")
		switch {
		case ch == shares && slices.Contains(strings.Split(expand, ","), "stats"):
			m["properties"].(map[string]any)["shareUsageBytes"] = res.usage
		case ch.upsert && expand != "":
			return nil, unsupported("$expand=%s is not supported", expand)
		}
		return ok200(m), nil
	}
}

func (s *Server) listChildren(ch *child) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		acct, err := s.account(c)
		if err != nil {
			return nil, err
		}
		// Containers and shares ignore the parameters they do not model, as they always have; queues and tables refuse them.
		include, filter := "", ""
		switch {
		case ch == containers:
			include, filter = c.query("$include"), c.query("$filter")
		case ch.upsert && c.query("$filter") != "":
			return nil, unsupported("$filter is not supported")
		}
		prefix := key(ch.prefix(c))
		var items []map[string]any
		for k, res := range s.resources {
			if res.kind == ch.kind && strings.HasPrefix(k, prefix) && strings.HasPrefix(res.name, filter) {
				items = append(items, s.renderChild(ch, res))
			}
		}
		if include == "deleted" {
			now := s.now()
			for _, d := range s.deletedContainers(acct) {
				if !strings.HasPrefix(d.res.name, filter) {
					continue
				}
				m := s.renderChild(ch, d.res)
				props := m["properties"].(map[string]any)
				props["deleted"], props["deletedTime"], props["version"] = true, timestamp(d.deleted), d.version
				props["remainingRetentionDays"] = daysLeft(d.deleted, now, d.keep)
				items = append(items, m)
			}
		}
		slices.SortStableFunc(items, func(a, b map[string]any) int {
			return cmp.Or(cmp.Compare(key(a["name"].(string)), key(b["name"].(string))), compareDeleted(a, b))
		})
		return ok(listBody(items, func(m map[string]any) any { return m })), nil
	}
}

func (s *Server) deleteChild(ch *child) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		acct, err := s.writableAccount(c, ch.kind)
		if err != nil {
			return nil, err
		}
		res := s.resources[key(ch.id(c))]
		if ch.upsert && res != nil {
			s.remove(res)
		}
		if res == nil || ch.upsert {
			return &reply{status: http.StatusNoContent}, nil
		}
		if ch == containers {
			if err := s.containerProtected(res, false); err != nil {
				return nil, err
			}
			s.deleteContainer(acct, res)
		} else {
			s.remove(res)
		}
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

// renderChild renders res, a child ch holds; a container shows the
// version-level immutability it takes from its account, which is inferred.
// The caller holds s.mu.
func (s *Server) renderChild(ch *child, res *resource) map[string]any {
	m := ch.render(res)
	if ch == containers && s.containerVersionLevel(res) {
		props, _ := m["properties"].(map[string]any)
		vlw, _ := props["immutableStorageWithVersioning"].(map[string]any)
		vlw = maps.Clone(vlw)
		if vlw == nil {
			vlw = map[string]any{}
		}
		vlw["enabled"] = true
		props["immutableStorageWithVersioning"] = vlw
	}
	return m
}

func renderContainer(res *resource) map[string]any {
	m, props := renderLeased(res)
	if _, ok := props["publicAccess"]; !ok {
		props["publicAccess"] = "None"
	}
	renderWorm(res, props)
	return m
}

// compareDeleted orders a live container before deleted ones of its name.
func compareDeleted(a, b map[string]any) int {
	deleted := func(m map[string]any) bool { v, _ := m["properties"].(map[string]any)["deleted"].(bool); return v }
	switch {
	case deleted(a) == deleted(b):
		return 0
	case deleted(a):
		return 1
	}
	return -1
}

func renderQueue(res *resource) map[string]any {
	m, props := envelope(res)
	delete(props, "provisioningState")
	props["approximateMessageCount"] = 0
	return m
}

func renderTable(res *resource) map[string]any {
	m, props := envelope(res)
	delete(props, "provisioningState")
	props["tableName"] = res.name
	return m
}

// SetShareUsage sets how many bytes the file share with ID id holds, which
// its shareUsageBytes shows when it is read with $expand=stats, as the server
// holds no file data. It reports whether the share exists.
func (s *Server) SetShareUsage(id string, bytes int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := s.resources[key(id)]
	if res == nil || res.kind != shareKind {
		return false
	}
	res.usage = bytes
	return true
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
