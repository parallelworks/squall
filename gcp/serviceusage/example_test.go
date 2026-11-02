package serviceusage_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/serviceusage"
)

// The first enable answers with an operation that ends in an error, and the
// service stays disabled, so code under test must check the operation and
// enable it again. Real tests point the official client library at srv.URL
// with option.WithEndpoint.
func Example() {
	faults := fault.New().Add(fault.Rule{Method: "serviceusage.services.enable", Nth: 1, Fault: fault.FailOperation(http.StatusInternalServerError)})
	s := serviceusage.New(serviceusage.WithFaults(faults), serviceusage.WithProject("my-project", 123))
	srv := httptest.NewServer(s)
	defer srv.Close()

	for range 2 {
		resp, err := http.Post(srv.URL+"/v1/projects/my-project/services/bigquery.googleapis.com:enable", "application/json", strings.NewReader(`{}`))
		if err != nil {
			fmt.Println(err)
			return
		}
		var op struct {
			Done  bool
			Error *struct{ Code int }
		}
		_ = json.NewDecoder(resp.Body).Decode(&op)
		resp.Body.Close()
		fmt.Println("done:", op.Done, "failed:", op.Error != nil, s.EnabledServices("123"))
	}

	svc, _ := s.LookupService("my-project", "bigquery.googleapis.com")
	fmt.Println(svc.Name, svc.State)
	// Output:
	// done: true failed: true []
	// done: true failed: false [bigquery.googleapis.com]
	// projects/123/services/bigquery.googleapis.com ENABLED
}
