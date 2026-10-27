package lustre_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/lustre"
)

// The first create takes effect but its response is lost, so the caller sees
// an error even though the instance now exists. Code under test must find it
// by its labels and adopt or delete it rather than leak it. Real tests point
// the official client library at srv.URL with option.WithEndpoint.
func Example() {
	faults := fault.New().Add(fault.Rule{Method: "lustre.projects.locations.instances.create", Nth: 1, Fault: fault.LoseResponse()})
	s := lustre.New(lustre.WithFaults(faults))
	srv := httptest.NewServer(s)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/projects/my-project/locations/us-central1-a/instances?instanceId=scratch",
		"application/json", strings.NewReader(`{"filesystem":"scratch","capacityGib":"9000",
			"perUnitStorageThroughput":"1000","network":"projects/my-project/global/networks/default",
			"labels":{"owner":"me"}}`))
	if err == nil {
		_, err = resp.Body.Read(make([]byte, 1<<10))
		resp.Body.Close()
	}
	fmt.Println("create failed:", err != nil)

	for _, in := range s.Instances() {
		fmt.Println(in.Name, in.State, in.Labels["owner"], in.MountPoint)
	}
	fmt.Println(faults.Injected()[0])
	// Output:
	// create failed: true
	// projects/my-project/locations/us-central1-a/instances/scratch ACTIVE me 10.0.0.2@tcp:/scratch
	// #1 lustre.projects.locations.instances.create projects/my-project/locations/us-central1-a/instances/scratch: lose response
}
