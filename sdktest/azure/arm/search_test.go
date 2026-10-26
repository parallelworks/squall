package arm_test

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/search/armsearch"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
)

func (c *clients) beginSearch(t *testing.T, name string) (*runtime.Poller[armsearch.ServicesClientCreateOrUpdateResponse], error) {
	t.Helper()
	return c.search.BeginCreateOrUpdate(t.Context(), group, name, armsearch.Service{
		Location: new(location),
		SKU:      &armsearch.SKU{Name: new(armsearch.SKUNameStandard)},
		Properties: &armsearch.ServiceProperties{
			HostingMode: new(armsearch.HostingModeDefault), PartitionCount: new(int32(1)), ReplicaCount: new(int32(1)),
			PublicNetworkAccess: new(armsearch.PublicNetworkAccessEnabled),
		},
	}, nil, nil)
}

func TestSearchService(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(-1))
	c.createGroup(t, group, nil)
	p, err := c.beginSearch(t, "find")
	if err != nil {
		t.Fatal(err)
	}
	if p.Done() {
		t.Fatal("a service being created is done")
	}
	creating, err := c.search.Get(t.Context(), group, "find", nil, nil)
	if err != nil || *creating.Properties.ProvisioningState != armsearch.ProvisioningStateProvisioning ||
		*creating.Properties.Status != armsearch.SearchServiceStatusProvisioning {
		t.Fatalf("a service being created = %+v, %v", creating.Properties, err)
	}
	c.server.Settle()
	res, err := p.PollUntilDone(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if *res.Properties.ProvisioningState != armsearch.ProvisioningStateSucceeded || *res.Properties.Status != armsearch.SearchServiceStatusRunning ||
		*res.Properties.Endpoint != "https://find.search.windows.net" {
		t.Fatalf("service = %+v", res.Properties)
	}
	keys, err := c.searchKeys.Get(t.Context(), group, "find", nil, nil)
	if err != nil || len(*keys.PrimaryKey) != 52 || *keys.PrimaryKey == *keys.SecondaryKey {
		t.Fatalf("admin keys = %+v, %v", keys, err)
	}
	if _, err := c.search.Delete(t.Context(), group, "find", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.search.Get(t.Context(), group, "find", nil, nil); code(err) != "ResourceNotFound" {
		t.Fatalf("a deleted service = %v", err)
	}
	if _, err := c.searchKeys.Get(t.Context(), group, "find", nil, nil); code(err) != "ResourceNotFound" {
		t.Fatalf("a deleted service's keys = %v", err)
	}
}

// Search's create is polled through the service itself, so it needs no
// action beyond reading the service.
func TestSearchServicePollingNeedsRead(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(1), arm.WithPolicy(access.Allow(
		"Microsoft.Resources/subscriptions/resourceGroups/write", "Microsoft.Search/searchServices/write", "Microsoft.Search/searchServices/read",
	)))
	c.createGroup(t, group, nil)
	p, err := c.beginSearch(t, "find")
	if _, err = wait(t, p, err); err != nil {
		t.Fatal(err)
	}
}
