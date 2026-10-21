package storage_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/storage"
)

// The first insert takes effect but its response is lost, so the caller sees
// an error even though the bucket now exists. Code under test must find and
// claim it rather than take it for someone else's. Real tests point the
// official client library at srv.URL + "/storage/v1/" with
// option.WithEndpoint.
func Example() {
	faults := fault.New().Add(fault.Rule{Method: "storage.buckets.insert", Nth: 1, Fault: fault.LoseResponse()})
	s := storage.New(storage.WithFaults(faults))
	srv := httptest.NewServer(s)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/storage/v1/b?project=my-project",
		"application/json", strings.NewReader(`{"name":"my-bucket","labels":{"owner":"me"}}`))
	if err == nil {
		_, err = resp.Body.Read(make([]byte, 1<<10))
		resp.Body.Close()
	}
	fmt.Println("insert failed:", err != nil)

	b, _ := s.LookupBucket("my-bucket")
	fmt.Println("bucket exists:", b.Name, b.Project, b.Labels)
	fmt.Println(faults.Injected()[0])
	// Output:
	// insert failed: true
	// bucket exists: my-bucket my-project map[owner:me]
	// #1 storage.buckets.insert b/my-bucket: lose response
}
