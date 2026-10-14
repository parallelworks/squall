package ec2_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/parallelworks/squall/aws/ec2"
	"github.com/parallelworks/squall/fault"
)

// The first allocation takes effect but its response is lost, so the caller
// sees an error even though the address now exists. Code under test must find
// and adopt it rather than leak it. Real tests point the official SDK at
// srv.URL with its base endpoint setting.
func Example() {
	faults := fault.New().Add(fault.Rule{Method: "ec2.AllocateAddress", Nth: 1, Fault: fault.LoseResponse()})
	s := ec2.New(ec2.WithFaults(faults))
	srv := httptest.NewServer(s)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL,
		strings.NewReader("Action=AllocateAddress&Version=2016-11-15&Domain=vpc"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKID/20260101/us-east-1/ec2/aws4_request, SignedHeaders=host, Signature=0")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_, err = resp.Body.Read(make([]byte, 1<<10))
		resp.Body.Close()
	}
	fmt.Println("allocate failed:", err != nil)

	a, _ := s.LookupAddress("us-east-1", "eipalloc-00000000000000001")
	fmt.Println("address exists:", a.PublicIP)
	fmt.Println(faults.Injected()[0])
	// Output:
	// allocate failed: true
	// address exists: 100.64.0.1
	// #1 ec2.AllocateAddress us-east-1/eipalloc-00000000000000001: lose response
}
