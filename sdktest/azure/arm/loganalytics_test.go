package arm_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/applicationinsights/armapplicationinsights"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/operationalinsights/armoperationalinsights/v3"

	"github.com/parallelworks/squall/azure/arm"
)

func (c *clients) createLogWorkspace(t *testing.T, name string) armoperationalinsights.Workspace {
	t.Helper()
	p, err := c.logs.BeginCreateOrUpdate(t.Context(), group, name, armoperationalinsights.Workspace{
		Location: new(location),
		Properties: &armoperationalinsights.WorkspaceProperties{
			SKU: &armoperationalinsights.WorkspaceSKU{Name: new(armoperationalinsights.WorkspaceSKUNameEnumPerGB2018)},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.PollUntilDone(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.Workspace
}

func (c *clients) deleteLogWorkspace(t *testing.T, name string, force bool) {
	t.Helper()
	p, err := c.logs.BeginDelete(t.Context(), group, name, &armoperationalinsights.WorkspacesClientBeginDeleteOptions{Force: new(force)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestLogAnalyticsWorkspace(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(1))
	c.createGroup(t, group, nil)
	created := c.createLogWorkspace(t, "law")
	if c.polls() == 0 || *created.Properties.ProvisioningState != armoperationalinsights.WorkspaceEntityStatusSucceeded ||
		*created.Properties.SKU.Name != armoperationalinsights.WorkspaceSKUNameEnumPerGB2018 || *created.Properties.RetentionInDays != 30 ||
		*created.Properties.CustomerID == "" || *created.Type != "Microsoft.OperationalInsights/workspaces" {
		t.Fatalf("workspace = %d polls, %+v", c.polls(), created.Properties)
	}
	got, err := c.logs.Get(t.Context(), group, "law", nil)
	if err != nil || *got.Properties.CustomerID != *created.Properties.CustomerID {
		t.Fatalf("get = %+v, %v", got.Properties, err)
	}
	page, err := c.logs.NewListByResourceGroupPager(group, nil).NextPage(t.Context())
	if err != nil || len(page.Value) != 1 || *page.Value[0].Name != "law" {
		t.Fatalf("list = %+v, %v", page.Value, err)
	}

	c.deleteLogWorkspace(t, "law", false)
	if _, err := c.logs.Get(t.Context(), group, "law", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("a deleted workspace = %v", err)
	}
	if deleted := c.server.DeletedLogAnalyticsWorkspaces(); len(deleted) != 1 || !strings.HasSuffix(deleted[0], "/workspaces/law") {
		t.Fatalf("deleted workspaces = %v", deleted)
	}
	recovered := c.createLogWorkspace(t, "law")
	if *recovered.Properties.CustomerID != *created.Properties.CustomerID || len(c.server.DeletedLogAnalyticsWorkspaces()) != 0 {
		t.Fatalf("a recovered workspace = %+v", recovered.Properties)
	}

	c.deleteLogWorkspace(t, "law", true)
	if deleted := c.server.DeletedLogAnalyticsWorkspaces(); len(deleted) != 0 {
		t.Fatalf("deleted workspaces after a forced delete = %v", deleted)
	}
	if again := c.createLogWorkspace(t, "law"); *again.Properties.CustomerID == *created.Properties.CustomerID {
		t.Fatal("a workspace made after a forced delete kept the customer ID")
	}
}

func TestDeletingAGroupSoftDeletesItsWorkspaces(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.createLogWorkspace(t, "law")
	p, err := c.groups.BeginDelete(t.Context(), group, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if deleted := c.server.DeletedLogAnalyticsWorkspaces(); len(deleted) != 1 {
		t.Fatalf("deleted workspaces = %v", deleted)
	}
}

func TestComponentLinkedToAWorkspace(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	workspace := c.createLogWorkspace(t, "law")
	res, err := c.components.CreateOrUpdate(t.Context(), group, "ai", armapplicationinsights.Component{
		Location: new(location), Kind: new("web"),
		Properties: &armapplicationinsights.ComponentProperties{
			ApplicationType: new(armapplicationinsights.ApplicationTypeWeb), WorkspaceResourceID: workspace.ID,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if *res.Properties.WorkspaceResourceID != *workspace.ID || *res.Properties.IngestionMode != armapplicationinsights.IngestionModeLogAnalytics {
		t.Fatalf("component = %+v", res.Properties)
	}
	if groups := c.server.ResourceGroups(); len(groups) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
}

func TestComponentWithoutAWorkspaceGetsTheDefault(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	component := armapplicationinsights.Component{
		Location: new(location), Kind: new("web"),
		Properties: &armapplicationinsights.ComponentProperties{ApplicationType: new(armapplicationinsights.ApplicationTypeWeb)},
	}
	res, err := c.components.CreateOrUpdate(t.Context(), group, "ai", component, nil)
	if err != nil {
		t.Fatal(err)
	}
	const defaultGroup = "DefaultResourceGroup-EUS"
	want := "/subscriptions/" + subscription + "/resourceGroups/" + defaultGroup +
		"/providers/Microsoft.OperationalInsights/workspaces/DefaultWorkspace-" + subscription + "-EUS"
	if *res.Properties.WorkspaceResourceID != want || *res.Properties.IngestionMode != armapplicationinsights.IngestionModeLogAnalytics {
		t.Fatalf("component = %v %v", *res.Properties.WorkspaceResourceID, *res.Properties.IngestionMode)
	}
	if ws, err := c.logs.Get(t.Context(), defaultGroup, "DefaultWorkspace-"+subscription+"-EUS", nil); err != nil || *ws.Location != location {
		t.Fatalf("default workspace = %+v, %v", ws, err)
	}
	again, err := c.components.CreateOrUpdate(t.Context(), group, "ai2", component, nil)
	if err != nil || *again.Properties.WorkspaceResourceID != want {
		t.Fatalf("a second component = %+v, %v", again.Properties, err)
	}
	updated, err := c.components.CreateOrUpdate(t.Context(), group, "ai", component, nil)
	if err != nil || *updated.Properties.WorkspaceResourceID != want {
		t.Fatalf("an update that leaves out the workspace = %+v, %v", updated.Properties, err)
	}

	p, err := c.groups.BeginDelete(t.Context(), group, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	groups := c.server.ResourceGroups()
	if len(groups) != 1 || groups[0].Name != defaultGroup || groups[0].Location != location {
		t.Fatalf("groups left = %+v", groups)
	}
	if !slices.ContainsFunc(c.server.Resources(), func(r arm.Resource) bool { return r.ID == want && r.Type == "Microsoft.OperationalInsights/workspaces" }) {
		t.Fatalf("resources left = %+v", c.server.Resources())
	}
}
