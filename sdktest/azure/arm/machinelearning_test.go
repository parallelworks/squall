package arm_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/applicationinsights/armapplicationinsights"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v2"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/keyvault/armkeyvault"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/machinelearning/armmachinelearning/v4"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
)

const tenant = "00000000-0000-0000-0000-0000000000aa"

func (c *clients) beginVault(t *testing.T, name string) (*runtime.Poller[armkeyvault.VaultsClientCreateOrUpdateResponse], error) {
	t.Helper()
	return c.vaults.BeginCreateOrUpdate(t.Context(), group, name, armkeyvault.VaultCreateOrUpdateParameters{
		Location: new(location),
		Properties: &armkeyvault.VaultProperties{
			TenantID: new(tenant), SKU: &armkeyvault.SKU{Name: new(armkeyvault.SKUNameStandard), Family: new(armkeyvault.SKUFamilyA)},
			AccessPolicies: []*armkeyvault.AccessPolicyEntry{},
		},
	}, nil)
}

func TestKeyVault(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(-1))
	c.createGroup(t, group, nil)
	p, err := c.beginVault(t, "kv1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Done() {
		t.Fatal("a vault being created is done")
	}
	creating, err := c.vaults.Get(t.Context(), group, "kv1", nil)
	if err != nil || *creating.Properties.ProvisioningState != armkeyvault.VaultProvisioningStateRegisteringDNS {
		t.Fatalf("a vault being created = %+v, %v", creating.Properties, err)
	}
	c.server.Settle()
	res, err := p.PollUntilDone(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if *res.Properties.ProvisioningState != armkeyvault.VaultProvisioningStateSucceeded || *res.Properties.VaultURI != "https://kv1.vault.azure.net/" {
		t.Fatalf("vault = %+v", res.Properties)
	}
	page, err := c.vaults.NewListByResourceGroupPager(group, nil).NextPage(t.Context())
	if err != nil || len(page.Value) != 1 || *page.Value[0].ID != *res.ID {
		t.Fatalf("vaults = %+v, %v", page.Value, err)
	}
}

func TestKeyVaultNeedsTenantAndSKU(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	_, err := c.vaults.BeginCreateOrUpdate(t.Context(), group, "kv1", armkeyvault.VaultCreateOrUpdateParameters{
		Location: new(location), Properties: &armkeyvault.VaultProperties{TenantID: new(tenant)},
	}, nil)
	if status(err) != 400 {
		t.Fatalf("a vault without a SKU = %v", err)
	}
}

func TestApplicationInsightsComponent(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(-1))
	c.createGroup(t, group, nil)
	res, err := c.components.CreateOrUpdate(t.Context(), group, "ai1", armapplicationinsights.Component{
		Location: new(location), Kind: new("web"),
		Properties: &armapplicationinsights.ComponentProperties{ApplicationType: new(armapplicationinsights.ApplicationTypeWeb)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if *res.Properties.ProvisioningState != "Succeeded" || *res.Type != "microsoft.insights/components" ||
		!strings.Contains(*res.ID, "/providers/microsoft.insights/components/ai1") || *res.Properties.InstrumentationKey == "" ||
		!strings.HasPrefix(*res.Properties.ConnectionString, "InstrumentationKey="+*res.Properties.InstrumentationKey+";") {
		t.Fatalf("component = %s %s %+v", *res.ID, *res.Type, res.Properties)
	}
	page, err := c.components.NewListByResourceGroupPager(group, nil).NextPage(t.Context())
	if err != nil || len(page.Value) != 1 || *page.Value[0].ID != *res.ID {
		t.Fatalf("components = %+v, %v", page.Value, err)
	}
	if _, err := c.components.CreateOrUpdate(t.Context(), group, "ai2", armapplicationinsights.Component{Location: new(location)}, nil); status(err) != 400 {
		t.Fatalf("a component without a kind = %v", err)
	}
}

func (c *clients) beginWorkspace(t *testing.T, name string) (*runtime.Poller[armmachinelearning.WorkspacesClientCreateOrUpdateResponse], error) {
	t.Helper()
	return c.workspaces.BeginCreateOrUpdate(t.Context(), group, name, armmachinelearning.Workspace{
		Location: new(location),
		Identity: &armmachinelearning.ManagedServiceIdentity{Type: new(armmachinelearning.ManagedServiceIdentityTypeSystemAssigned)},
		Properties: &armmachinelearning.WorkspaceProperties{
			FriendlyName:   new(name),
			StorageAccount: new("/subscriptions/" + subscription + "/resourceGroups/" + group + "/providers/Microsoft.Storage/storageAccounts/st1"),
			KeyVault:       new("/subscriptions/" + subscription + "/resourceGroups/" + group + "/providers/Microsoft.KeyVault/vaults/kv1"),
		},
	}, nil)
}

func TestMachineLearningWorkspace(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(1))
	c.createGroup(t, group, nil)
	p, err := c.beginWorkspace(t, "ws1")
	res, err := wait(t, p, err)
	if err != nil {
		t.Fatal(err)
	}
	if *res.Properties.ProvisioningState != armmachinelearning.ProvisioningStateSucceeded || *res.Properties.WorkspaceID == "" ||
		*res.Identity.PrincipalID == "" || !strings.HasSuffix(*res.Properties.KeyVault, "/vaults/kv1") {
		t.Fatalf("workspace = %+v %+v", res.Properties, res.Identity)
	}
	if !slices.ContainsFunc(*c.requests, func(r string) bool {
		return strings.Contains(r, "/providers/Microsoft.MachineLearningServices/locations/"+location+"/workspaceOperationsStatus/")
	}) {
		t.Fatalf("the create was not polled: %v", *c.requests)
	}
	got, err := c.workspaces.Get(t.Context(), group, "ws1", nil)
	if err != nil || *got.Properties.WorkspaceID != *res.Properties.WorkspaceID {
		t.Fatalf("workspace = %+v, %v", got.Properties, err)
	}
	purge := &armmachinelearning.WorkspacesClientBeginDeleteOptions{ForceToPurge: new(true)}
	if d, err := c.workspaces.BeginDelete(t.Context(), group, "ws1", purge); err != nil {
		t.Fatal(err)
	} else if _, err := d.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.workspaces.Get(t.Context(), group, "ws1", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("a deleted workspace = %v", err)
	}
	if d, err := c.workspaces.BeginDelete(t.Context(), group, "ws1", nil); err != nil {
		t.Fatalf("deleting a workspace again = %v", err)
	} else if _, err := d.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatalf("deleting a workspace again = %v", err)
	}
}

// A workspace change is polled under its location, which needs the
// workspaceOperationsStatus action.
func TestMachineLearningWorkspacePollingNeedsOperationStatus(t *testing.T) {
	grants := []string{
		"Microsoft.Resources/subscriptions/resourceGroups/write",
		"Microsoft.MachineLearningServices/workspaces/write", "Microsoft.MachineLearningServices/workspaces/read",
	}
	c := setup(t, arm.WithSettleAfter(1), arm.WithPolicy(access.Allow(grants...)))
	c.createGroup(t, group, nil)
	if p, err := c.beginWorkspace(t, "ws1"); code(err) != "" {
		t.Fatal(err)
	} else if _, err := p.PollUntilDone(t.Context(), nil); code(err) != "AuthorizationFailed" {
		t.Fatalf("polling without workspaceOperationsStatus/read = %v", err)
	}
	c = setup(t, arm.WithSettleAfter(1), arm.WithPolicy(access.Allow(append(grants,
		"Microsoft.MachineLearningServices/locations/workspaceOperationsStatus/read")...)))
	c.createGroup(t, group, nil)
	p, err := c.beginWorkspace(t, "ws1")
	if _, err := wait(t, p, err); err != nil {
		t.Fatal(err)
	}
}

func roleAssignment(principal, role string) armauthorization.RoleAssignmentCreateParameters {
	return armauthorization.RoleAssignmentCreateParameters{Properties: &armauthorization.RoleAssignmentProperties{
		PrincipalID:      new(principal),
		PrincipalType:    new(armauthorization.PrincipalTypeUser),
		RoleDefinitionID: new("/subscriptions/" + subscription + "/providers/Microsoft.Authorization/roleDefinitions/" + role),
	}}
}

func TestRoleAssignment(t *testing.T) {
	const (
		contributor = "b24988ac-6180-42a0-ab88-20f7382dd24c"
		reader      = "acdd72a7-3385-48ef-bd42-f606fba81ae7"
		user        = "11111111-0000-0000-0000-000000000001"
		name        = "22222222-0000-0000-0000-000000000001"
	)
	c := setup(t)
	c.createGroup(t, group, nil)
	scope := "/subscriptions/" + subscription + "/resourceGroups/" + group
	res, err := c.roles.Create(t.Context(), scope, name, roleAssignment(user, contributor), nil)
	if err != nil {
		t.Fatal(err)
	}
	if *res.Properties.Scope != scope || *res.Properties.PrincipalID != user || *res.ID != scope+"/providers/Microsoft.Authorization/roleAssignments/"+name {
		t.Fatalf("role assignment = %s %+v", *res.ID, res.Properties)
	}
	if _, err := c.roles.Create(t.Context(), scope, name, roleAssignment(user, contributor), nil); err != nil {
		t.Fatalf("assigning the same role again = %v", err)
	}
	if _, err := c.roles.Create(t.Context(), scope, name, roleAssignment(user, reader), nil); code(err) != "RoleAssignmentUpdateNotPermitted" {
		t.Fatalf("changing an assignment's role = %v", err)
	}
	if _, err := c.roles.Create(t.Context(), scope, "22222222-0000-0000-0000-000000000002", roleAssignment(user, contributor), nil); code(err) != "RoleAssignmentExists" {
		t.Fatalf("assigning a held role under another name = %v", err)
	}
	if _, err := c.roles.Delete(t.Context(), scope, name, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.roles.Delete(t.Context(), scope, name, nil); err != nil {
		t.Fatalf("deleting an assignment again = %v", err)
	}
	if _, err := c.roles.Create(t.Context(), scope, name, roleAssignment(user, contributor), nil); err != nil {
		t.Fatal(err)
	}
	g, err := c.groups.BeginDelete(t.Context(), group, nil)
	if _, err := wait(t, g, err); err != nil {
		t.Fatal(err)
	}
	for _, r := range c.server.Resources() {
		t.Errorf("deleting the group left %s", r.ID)
	}
}
