package compute_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/compute"
)

// The first insert takes effect but its response is lost, so the caller sees
// an error even though the address now exists. Code under test must find and
// adopt it rather than fail or make another. Real tests point the official
// client library at srv.URL with option.WithEndpoint.
func Example() {
	faults := fault.New().Add(fault.Rule{Method: "compute.addresses.insert", Nth: 1, Fault: fault.LoseResponse()})
	s := compute.New(compute.WithFaults(faults))
	srv := httptest.NewServer(s)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/compute/v1/projects/my-project/regions/us-east1/addresses",
		"application/json", strings.NewReader(`{"name":"web"}`))
	if err == nil {
		_, err = resp.Body.Read(make([]byte, 1<<10))
		resp.Body.Close()
	}
	fmt.Println("insert failed:", err != nil)

	a, _ := s.LookupAddress("my-project", "us-east1", "web")
	fmt.Println("address exists:", a.Address)
	fmt.Println(faults.Injected()[0])
	// Output:
	// insert failed: true
	// address exists: 100.64.0.1
	// #1 compute.addresses.insert projects/my-project/regions/us-east1/addresses/web: lose response
}
