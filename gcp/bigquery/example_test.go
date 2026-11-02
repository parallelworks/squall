package bigquery_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/bigquery"
)

// The first insert takes effect but its response is lost, so the caller sees
// an error even though the dataset now exists. Code under test must find it
// with a get, since a second insert fails with 409. Real tests point the
// official client library at srv.URL + "/bigquery/v2/" with
// option.WithEndpoint.
func Example() {
	faults := fault.New().Add(fault.Rule{Method: "bigquery.datasets.insert", Nth: 1, Fault: fault.LoseResponse()})
	s := bigquery.New(bigquery.WithFaults(faults))
	srv := httptest.NewServer(s)
	defer srv.Close()

	const datasets = "/bigquery/v2/projects/my-project/datasets"
	for range 2 {
		resp, err := http.Post(srv.URL+datasets, "application/json", strings.NewReader(`{"datasetReference":{"datasetId":"analytics"}}`))
		if err != nil {
			fmt.Println("insert failed:", err)
			continue
		}
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Println("insert:", resp.StatusCode, "lost:", err != nil)
	}

	d, _ := s.LookupDataset("my-project", "analytics")
	fmt.Println(d.ID, d.Location)
	fmt.Println(faults.Injected()[0])
	// Output:
	// insert: 200 lost: true
	// insert: 409 lost: false
	// my-project:analytics US
	// #1 bigquery.datasets.insert projects/my-project/datasets/analytics: lose response
}
