package arm_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/parallelworks/squall/azure/arm"
	"github.com/parallelworks/squall/fault"
)

// The first create of a public IP address takes effect but its response is
// lost, so the caller sees an error even though the address now exists. Code
// under test must find and adopt it rather than fail or make another. Real
// tests point the Azure SDK's clients at srv.URL through their cloud
// configuration.
func Example() {
	faults := fault.New().Add(fault.Rule{Method: "Microsoft.Network/publicIPAddresses/write", Nth: 1, Fault: fault.LoseResponse()})
	s := arm.New(arm.WithFaults(faults))
	srv := httptest.NewServer(s)
	defer srv.Close()

	put := func(path, body string) error {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+path+"?api-version=2024-05-01", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer token")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, err = resp.Body.Read(make([]byte, 1<<10))
			resp.Body.Close()
		}
		return err
	}
	_ = put("/subscriptions/s/resourceGroups/rg", `{"location":"eastus"}`)
	err := put("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/web", `{"location":"eastus"}`)
	fmt.Println("create failed:", err != nil)

	for _, a := range s.PublicIPAddresses() {
		fmt.Println("address exists:", a.IPAddress)
	}
	fmt.Println(faults.Injected()[0])
	// Output:
	// create failed: true
	// address exists: 100.64.0.1
	// #2 Microsoft.Network/publicIPAddresses/write /subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/web: lose response
}
