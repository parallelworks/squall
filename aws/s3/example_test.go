package s3_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/parallelworks/squall/aws/s3"
	"github.com/parallelworks/squall/fault"
)

// The first create takes effect but its response is lost, so the caller sees
// an error even though the bucket now exists. Code under test must find and
// claim it rather than take it for someone else's. Real tests point the
// official SDK at srv.URL with its base endpoint setting.
func Example() {
	faults := fault.New().Add(fault.Rule{Method: "s3.CreateBucket", Nth: 1, Fault: fault.LoseResponse()})
	s := s3.New(s3.WithFaults(faults))
	srv := httptest.NewServer(s)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/my-bucket", http.NoBody)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKID/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=0")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_, err = resp.Body.Read(make([]byte, 1<<10))
		resp.Body.Close()
	}
	fmt.Println("create failed:", err != nil)

	b, _ := s.LookupBucket("my-bucket")
	fmt.Println("bucket exists:", b.Name, b.Region)
	fmt.Println(faults.Injected()[0])
	// Output:
	// create failed: true
	// bucket exists: my-bucket us-east-1
	// #1 s3.CreateBucket my-bucket: lose response
}
