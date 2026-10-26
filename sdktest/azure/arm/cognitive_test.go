package arm_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/cognitiveservices/armcognitiveservices"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
)

func (c *clients) beginAccount(t *testing.T, name, domain string) (*runtime.Poller[armcognitiveservices.AccountsClientCreateResponse], error) {
	t.Helper()
	return c.cognitive.BeginCreate(t.Context(), group, name, armcognitiveservices.Account{
		Location:   new(location),
		Kind:       new("AIServices"),
		SKU:        &armcognitiveservices.SKU{Name: new("S0")},
		Properties: &armcognitiveservices.AccountProperties{CustomSubDomainName: new(domain)},
	}, nil)
}

func (c *clients) beginDeployment(t *testing.T, account, name string) (*runtime.Poller[armcognitiveservices.DeploymentsClientCreateOrUpdateResponse], error) {
	t.Helper()
	return c.deployments.BeginCreateOrUpdate(t.Context(), group, account, name, armcognitiveservices.Deployment{
		SKU: &armcognitiveservices.SKU{Name: new("GlobalStandard"), Capacity: new(int32(50))},
		Properties: &armcognitiveservices.DeploymentProperties{
			Model: &armcognitiveservices.DeploymentModel{Format: new("OpenAI"), Name: new(name), Version: new("2024-11-20")},
		},
	}, nil)
}

func (c *clients) deploymentNames(t *testing.T, account string) []string {
	t.Helper()
	var names []string
	pager := c.deployments.NewListPager(group, account, nil)
	for pager.More() {
		page, err := pager.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range page.Value {
			names = append(names, *d.Name)
		}
	}
	return names
}

func TestCognitiveServicesAccount(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(-1))
	c.createGroup(t, group, nil)
	p, err := c.beginAccount(t, "ai", "ai-sub")
	if err != nil {
		t.Fatal(err)
	}
	pollOnce(t, p)
	creating, err := c.cognitive.Get(t.Context(), group, "ai", nil)
	if err != nil || *creating.Properties.ProvisioningState != armcognitiveservices.ProvisioningStateCreating {
		t.Fatalf("an account being created = %+v, %v", creating.Properties, err)
	}
	c.server.Settle()
	res, err := p.PollUntilDone(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if *res.Properties.ProvisioningState != armcognitiveservices.ProvisioningStateSucceeded ||
		*res.Properties.Endpoint != "https://ai-sub.cognitiveservices.azure.com/" || *res.Kind != "AIServices" {
		t.Fatalf("account = %+v", res.Properties)
	}
	polled := slices.ContainsFunc(*c.requests, func(r string) bool {
		return strings.Contains(r, "/providers/Microsoft.CognitiveServices/locations/"+location+"/operationResults/")
	})
	if !polled {
		t.Fatalf("the create was not polled through Cognitive Services' operations: %v", *c.requests)
	}
	keys, err := c.cognitive.ListKeys(t.Context(), group, "ai", nil)
	if err != nil || len(*keys.Key1) != 84 || *keys.Key1 == *keys.Key2 {
		t.Fatalf("keys = %+v, %v", keys, err)
	}
	again, _ := c.cognitive.ListKeys(t.Context(), group, "ai", nil)
	if *again.Key2 != *keys.Key2 {
		t.Fatal("an account's keys changed")
	}
}

func TestCognitiveServicesDeployments(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(-1))
	c.createGroup(t, group, nil)
	c.createCognitiveAccount(t, "ai", "ai-sub")
	p, err := c.beginDeployment(t, "ai", "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if p.Done() {
		t.Fatal("a deployment being created is done")
	}
	creating, err := c.deployments.Get(t.Context(), group, "ai", "gpt-4o", nil)
	if err != nil || *creating.Properties.ProvisioningState != armcognitiveservices.DeploymentProvisioningStateAccepted {
		t.Fatalf("a deployment being created = %+v, %v", creating.Properties, err)
	}
	c.server.Settle()
	res, err := p.PollUntilDone(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if *res.Properties.ProvisioningState != armcognitiveservices.DeploymentProvisioningStateSucceeded || *res.Properties.CurrentCapacity != 50 {
		t.Fatalf("deployment = %+v", res.Properties)
	}
	if names := c.deploymentNames(t, "ai"); !slices.Equal(names, []string{"gpt-4o"}) {
		t.Fatalf("deployments = %v", names)
	}
	dp, err := c.deployments.BeginDelete(t.Context(), group, "ai", "gpt-4o", nil)
	if err != nil || !dp.Done() {
		t.Fatalf("deleting a deployment = done %v, %v", dp != nil && dp.Done(), err)
	}
	if names := c.deploymentNames(t, "ai"); len(names) != 0 {
		t.Fatalf("deployments after the delete = %v", names)
	}
	if _, err := c.beginDeployment(t, "none", "gpt-4o"); code(err) != "ParentResourceNotFound" {
		t.Fatalf("a deployment on no account = %v", err)
	}
}

func TestCognitiveServicesSoftDelete(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(-1))
	c.createGroup(t, group, nil)
	c.createCognitiveAccount(t, "ai", "ai-sub")
	c.createDeployment(t, "ai", "gpt-4o")
	dp, err := c.cognitive.BeginDelete(t.Context(), group, "ai", nil)
	if err != nil || !dp.Done() {
		t.Fatalf("deleting an account = done %v, %v", dp != nil && dp.Done(), err)
	}
	if _, err := c.cognitive.Get(t.Context(), group, "ai", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("a deleted account = %v", err)
	}
	deleted, err := c.deleted.Get(t.Context(), location, group, "ai", nil)
	if err != nil || *deleted.Properties.Endpoint != "https://ai-sub.cognitiveservices.azure.com/" {
		t.Fatalf("the deleted account = %+v, %v", deleted, err)
	}
	if _, err := c.beginAccount(t, "other", "AI-SUB"); code(err) != "CustomDomainInUse" || status(err) != 409 {
		t.Fatalf("taking a deleted account's subdomain = %v", err)
	}

	pp, err := c.deleted.BeginPurge(t.Context(), location, group, "ai", nil)
	if err != nil {
		t.Fatal(err)
	}
	pollOnce(t, pp)
	c.server.Settle()
	if _, err := pp.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if left := c.server.DeletedAccounts(); len(left) != 0 {
		t.Fatalf("deleted accounts after the purge = %v", left)
	}
	c.createCognitiveAccount(t, "other", "ai-sub")
	if names := c.deploymentNames(t, "other"); len(names) != 0 {
		t.Fatalf("a new account has the purged one's deployments: %v", names)
	}
}

func TestCognitiveServicesGroupDeletionSoftDeletes(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.createCognitiveAccount(t, "ai", "ai-sub")
	gp, err := c.groups.BeginDelete(t.Context(), group, nil)
	if _, err = wait(t, gp, err); err != nil {
		t.Fatal(err)
	}
	want := "/subscriptions/" + subscription + "/providers/Microsoft.CognitiveServices/locations/" + location + "/resourceGroups/" + group + "/deletedAccounts/ai"
	if left := c.server.DeletedAccounts(); !slices.Equal(left, []string{want}) {
		t.Fatalf("deleted accounts after the group's deletion = %v", left)
	}
}

func TestModelCapacities(t *testing.T) {
	c := setup(t, arm.WithModelCapacities(
		arm.ModelCapacity{Location: "westus", Format: "OpenAI", Name: "gpt-4o", Version: "2024-11-20", SKU: "GlobalStandard", Available: 30},
		arm.ModelCapacity{Location: location, Format: "OpenAI", Name: "gpt-4o", Version: "2024-11-20", SKU: "GlobalStandard", Available: 450},
		arm.ModelCapacity{Location: location, Format: "OpenAI", Name: "gpt-4o", Version: "2024-08-06", SKU: "Standard", Available: 10},
	))
	var got []string
	pager := c.capacities.NewListPager("OpenAI", "gpt-4o", "2024-11-20", nil)
	for pager.More() {
		page, err := pager.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, mc := range page.Value {
			got = append(got, fmt.Sprintf("%s/%s/%v", *mc.Location, *mc.Properties.SKUName, *mc.Properties.AvailableCapacity))
		}
	}
	if want := []string{"eastus/GlobalStandard/450", "westus/GlobalStandard/30"}; !slices.Equal(got, want) {
		t.Fatalf("capacities = %v, want %v", got, want)
	}
}

func TestCognitiveServicesPollingNeedsOperationResults(t *testing.T) {
	writes := []string{
		"Microsoft.Resources/subscriptions/resourceGroups/write", "Microsoft.CognitiveServices/accounts/write",
		"Microsoft.CognitiveServices/accounts/read",
	}
	c := setup(t, arm.WithSettleAfter(1), arm.WithPolicy(access.Allow(writes...)))
	c.createGroup(t, group, nil)
	p, err := c.beginAccount(t, "ai", "ai-sub")
	const poll = "Microsoft.CognitiveServices/locations/operationresults/read"
	if _, err = wait(t, p, err); code(err) != "AuthorizationFailed" || !strings.Contains(err.Error(), poll) {
		t.Fatalf("polling without %s = %v", poll, err)
	}
}

// createCognitiveAccount creates an account, settling its create.
func (c *clients) createCognitiveAccount(t *testing.T, name, domain string) {
	t.Helper()
	p, err := c.beginAccount(t, name, domain)
	c.server.Settle()
	if _, err := wait(t, p, err); err != nil {
		t.Fatal(err)
	}
}

// createDeployment creates a deployment, settling its create.
func (c *clients) createDeployment(t *testing.T, account, name string) {
	t.Helper()
	p, err := c.beginDeployment(t, account, name)
	c.server.Settle()
	if _, err := wait(t, p, err); err != nil {
		t.Fatal(err)
	}
}
