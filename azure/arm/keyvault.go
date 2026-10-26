package arm

import "net/http"

var vaultKind = &kind{
	typ: "Microsoft.KeyVault/vaults", plural: "vaults", typeName: "Vault",
	props: []string{
		"accessPolicies", "createMode", "enablePurgeProtection", "enableRbacAuthorization", "enableSoftDelete", "enabledForDeployment",
		"enabledForDiskEncryption", "enabledForTemplateDeployment", "networkAcls", "publicNetworkAccess", "sku", "softDeleteRetentionInDays",
		"tenantId",
	},
	readOnly: []string{"hsmPoolResourceId", "privateEndpointConnections", "provisioningState", "vaultUri"},
	located:  true,
}

const vaults = rg + "/providers/Microsoft.KeyVault/vaults"

func vaultID(c *call) string { return resourceID(c, vaultKind.typ, c.val("name")) }

func (s *Server) registerKeyVaultRoutes() {
	s.add("GET", vaults, vaultKind.action("read"), false, s.listVaults)
	s.add("PUT", vaults+"/{name}", vaultKind.action("write"), false, s.put(putSpec{
		kind: vaultKind, id: vaultID, render: renderVault, prepare: s.prepareVault, bodyPolled: true, createdStatus: http.StatusOK,
	}))
	s.add("GET", vaults+"/{name}", vaultKind.action("read"), false, s.getVault)
}

func (s *Server) prepareVault(res *resource, p *parsed) (func(), *apiError) {
	if sku, _ := p.props["sku"].(map[string]any); str(p.props, "tenantId") == "" || str(sku, "name") == "" {
		return nil, unsupported("a key vault needs properties.tenantId and properties.sku.name")
	}
	return func() { s.update(res, p) }, nil
}

func (s *Server) getVault(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	res := s.read(vaultID(c))
	if res == nil || res.kind != vaultKind {
		return nil, resourceNotFound(vaultKind.typ, c.val("name"), c.val("rg"))
	}
	return polled(res, ok200(renderVault(res))), nil
}

func (s *Server) listVaults(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	return ok(listBody(s.listed(c, vaultKind), func(res *resource) any { return renderVault(res) })), nil
}

// renderVault shows a vault being created or updated as RegisteringDns, as
// Key Vault answers its create, and its URI.
func renderVault(res *resource) map[string]any {
	m, props := envelope(res)
	if res.state != stateSucceeded {
		props["provisioningState"] = "RegisteringDns"
	}
	props["vaultUri"] = "https://" + res.name + ".vault.azure.net/"
	return m
}
