package compute_test

import (
	"context"
	"fmt"
	"net/http/httptest"

	gcompute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/option"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/compute"
)

// The first insert takes effect but its response is lost, so the caller sees
// an error even though the address now exists. Code under test must find and
// adopt it rather than fail or make another.
func Example() {
	ctx := context.Background()
	faults := fault.New().Add(fault.Rule{Method: "compute.addresses.insert", Nth: 1, Fault: fault.LoseResponse()})
	s := compute.New(compute.WithFaults(faults))
	srv := httptest.NewServer(s)
	defer srv.Close()

	client, err := gcompute.NewAddressesRESTClient(ctx, option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		panic(err)
	}
	defer client.Close()

	_, err = client.Insert(ctx, &computepb.InsertAddressRequest{
		Project: "my-project", Region: "us-east1",
		AddressResource: &computepb.Address{Name: new("web")},
	})
	fmt.Println("insert failed:", err != nil)

	a, _ := s.LookupAddress("my-project", "us-east1", "web")
	fmt.Println("address exists:", a.Address)
	fmt.Println(faults.Injected()[0])
	// Output:
	// insert failed: true
	// address exists: 100.64.0.1
	// #1 compute.addresses.insert projects/my-project/regions/us-east1/addresses/web: lose response
}
