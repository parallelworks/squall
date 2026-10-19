package arm_test

import (
	"context"
	"fmt"
	"net/http/httptest"

	azarm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources/v3"

	"github.com/parallelworks/squall/azure/arm"
	"github.com/parallelworks/squall/fault"
)

// The first create of a public IP address takes effect but its response is
// lost. With retries off the caller sees an error even though the address
// now exists, so code under test must find and adopt it rather than fail or
// make another.
func Example() {
	ctx := context.Background()
	faults := fault.New().Add(fault.Rule{Method: "Microsoft.Network/publicIPAddresses/write", Nth: 1, Fault: fault.LoseResponse()})
	s := arm.New(arm.WithFaults(faults))
	srv := httptest.NewServer(s)
	defer srv.Close()

	options := &azarm.ClientOptions{
		Cloud: cloud.Configuration{Services: map[cloud.ServiceName]cloud.ServiceConfiguration{
			cloud.ResourceManager: {Endpoint: srv.URL, Audience: "https://management.azure.com"},
		}},
		Retry:                           policy.RetryOptions{MaxRetries: -1},
		InsecureAllowCredentialWithHTTP: true,
	}
	groups, _ := armresources.NewResourceGroupsClient("my-subscription", credential{}, options)
	pips, _ := armnetwork.NewPublicIPAddressesClient("my-subscription", credential{}, options)

	_, _ = groups.CreateOrUpdate(ctx, "my-group", armresources.ResourceGroup{Location: new("eastus")}, nil)
	_, err := pips.BeginCreateOrUpdate(ctx, "my-group", "web", armnetwork.PublicIPAddress{Location: new("eastus")}, nil)
	fmt.Println("create failed:", err != nil)

	for _, a := range s.PublicIPAddresses() {
		fmt.Println("address exists:", a.IPAddress)
	}
	fmt.Println(faults.Injected()[0])
	// Output:
	// create failed: true
	// address exists: 100.64.0.1
	// #2 Microsoft.Network/publicIPAddresses/write /subscriptions/my-subscription/resourceGroups/my-group/providers/Microsoft.Network/publicIPAddresses/web: lose response
}
