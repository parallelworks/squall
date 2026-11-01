package route53_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/parallelworks/squall/aws/route53"
	"github.com/parallelworks/squall/fault"
)

// The first create takes effect but its response is lost, so the caller sees
// an error even though the zone now exists. Code under test must find it
// rather than make another. Real tests point the official SDK at srv.URL with
// its base endpoint setting.
func Example() {
	faults := fault.New().Add(fault.Rule{Method: "route53.CreateHostedZone", Nth: 1, Fault: fault.LoseResponse()})
	s := route53.New(route53.WithFaults(faults))
	srv := httptest.NewServer(s)
	defer srv.Close()

	body := `<CreateHostedZoneRequest><Name>cluster.internal</Name><CallerReference>create-1</CallerReference>
<VPC><VPCRegion>us-east-1</VPCRegion><VPCId>vpc-0123456789abcdef0</VPCId></VPC></CreateHostedZoneRequest>`
	resp, err := http.Post(srv.URL+"/2013-04-01/hostedzone", "text/xml", strings.NewReader(body))
	if err == nil {
		_, err = resp.Body.Read(make([]byte, 1<<10))
		resp.Body.Close()
	}
	fmt.Println("create failed:", err != nil)

	for _, z := range s.HostedZones() {
		fmt.Println("zone exists:", z.Name, len(s.RecordSets(z.ID)), "record sets")
	}
	fmt.Println(faults.Injected()[0])
	// Output:
	// create failed: true
	// zone exists: cluster.internal. 2 record sets
	// #1 route53.CreateHostedZone cluster.internal.: lose response
}
