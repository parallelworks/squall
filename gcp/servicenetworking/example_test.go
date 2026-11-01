package servicenetworking_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/servicenetworking"
)

// The first create takes effect but its response is lost, so the caller sees
// an error even though the connection now exists. Code under test must find
// it with a list and reuse it, since a second create fails. Real tests point
// the official client library at srv.URL with option.WithEndpoint, and pass
// WithCompute so networks and ranges are checked.
func Example() {
	faults := fault.New().Add(fault.Rule{Method: "servicenetworking.services.connections.create", Nth: 1, Fault: fault.LoseResponse()})
	s := servicenetworking.New(servicenetworking.WithFaults(faults), servicenetworking.WithProjectNumber("my-project", 123))
	srv := httptest.NewServer(s)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/services/servicenetworking.googleapis.com/connections",
		"application/json", strings.NewReader(`{"network":"projects/my-project/global/networks/vpc","reservedPeeringRanges":["psa"]}`))
	if err == nil {
		_, err = resp.Body.Read(make([]byte, 1<<10))
		resp.Body.Close()
	}
	fmt.Println("create failed:", err != nil)

	for _, c := range s.Connections() {
		fmt.Println(c.Network, c.Peering, c.ReservedPeeringRanges)
	}
	fmt.Println(faults.Injected()[0])
	// Output:
	// create failed: true
	// projects/123/global/networks/vpc servicenetworking-googleapis-com [psa]
	// #1 servicenetworking.services.connections.create services/servicenetworking.googleapis.com/connections: lose response
}
