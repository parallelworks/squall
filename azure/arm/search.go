package arm

import "net/http"

var searchKind = &kind{
	typ: "Microsoft.Search/searchServices", plural: "searchServices", typeName: "SearchService",
	top: []string{"identity", "sku"},
	props: []string{
		"authOptions", "computeType", "dataExfiltrationProtections", "disableLocalAuth", "encryptionWithCmk", "hostingMode",
		"knowledgeRetrieval", "networkRuleSet", "partitionCount", "publicNetworkAccess", "replicaCount", "semanticSearch",
	},
	readOnly: []string{
		"eTag", "endpoint", "privateEndpointConnections", "provisioningState", "serviceUpgradedAt", "sharedPrivateLinkResources",
		"status", "statusDetails", "upgradeAvailable",
	},
	located: true,
}

const searchService = rg + "/providers/Microsoft.Search/searchServices/{name}"

func searchID(c *call) string { return resourceID(c, searchKind.typ, c.val("name")) }

func (s *Server) registerSearchRoutes() {
	s.add("PUT", searchService, searchKind.action("write"), false, s.put(putSpec{
		kind: searchKind, id: searchID, render: renderSearch, prepare: s.prepareSearch, bodyPolled: true,
	}))
	s.add("GET", searchService, searchKind.action("read"), false, s.getSearch)
	s.add("DELETE", searchService, searchKind.action("delete"), false, s.deleteSearch)
	s.add("POST", searchService+"/listAdminKeys", searchKind.action("listAdminKeys/action"), false, s.listAdminKeys)
}

func (s *Server) prepareSearch(res *resource, p *parsed) (func(), *apiError) {
	if sku, _ := p.top["sku"].(map[string]any); str(sku, "name") == "" {
		return nil, unsupported("a search service needs sku.name")
	}
	return func() { s.update(res, p) }, nil
}

func (s *Server) search(c *call) (*resource, *apiError) {
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	res := s.read(searchID(c))
	if res == nil || res.kind != searchKind {
		return nil, resourceNotFound(searchKind.typ, c.val("name"), c.val("rg"))
	}
	return res, nil
}

func (s *Server) getSearch(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.search(c)
	if err != nil {
		return nil, err
	}
	return polled(res, ok200(renderSearch(res))), nil
}

// deleteSearch deletes the service at once, as Azure answers 200.
func (s *Server) deleteSearch(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	res, err := s.claim(searchID(c), "")
	if err != nil {
		return nil, err
	}
	if res == nil || res.kind != searchKind {
		return &reply{status: http.StatusNoContent}, nil
	}
	s.remove(res)
	return &reply{status: http.StatusOK}, nil
}

func (s *Server) listAdminKeys(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.search(c)
	if err != nil {
		return nil, err
	}
	return ok200(map[string]any{"primaryKey": accessKey(res, "primary", 52), "secondaryKey": accessKey(res, "secondary", 52)}), nil
}

// renderSearch shows the service's endpoint, and its state in lower case as
// Search writes it: provisioning, then succeeded and running.
func renderSearch(res *resource) map[string]any {
	m, props := envelope(res)
	props["endpoint"] = "https://" + res.name + ".search.windows.net"
	props["provisioningState"], props["status"], props["statusDetails"] = "provisioning", "provisioning", ""
	if res.state == stateSucceeded {
		props["provisioningState"], props["status"] = "succeeded", "running"
	}
	return m
}
