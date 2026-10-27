package lustre_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"

	glustre "cloud.google.com/go/lustre/apiv1"
	"cloud.google.com/go/lustre/apiv1/lustrepb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/lustre"
)

// The first create takes effect but its response is lost, so the caller sees
// an error even though the instance now exists. A sweep that lists instances
// by label finds it and deletes it.
func Example() {
	ctx := context.Background()
	faults := fault.New().Add(fault.Rule{Method: "lustre.projects.locations.instances.create", Nth: 1, Fault: fault.LoseResponse()})
	s := lustre.New(lustre.WithFaults(faults))
	srv := httptest.NewServer(s)
	defer srv.Close()

	client, err := glustre.NewRESTClient(ctx, option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		panic(err)
	}
	defer client.Close()

	_, err = client.CreateInstance(ctx, &lustrepb.CreateInstanceRequest{
		Parent:     "projects/my-project/locations/us-central1-a",
		InstanceId: "scratch",
		Instance: &lustrepb.Instance{
			Filesystem:               "scratch",
			CapacityGib:              9000,
			PerUnitStorageThroughput: 1000,
			Network:                  "projects/my-project/global/networks/default",
			Labels:                   map[string]string{"owner": "me"},
		},
	})
	fmt.Println("create failed:", err != nil)

	it := client.ListInstances(ctx, &lustrepb.ListInstancesRequest{
		Parent: "projects/my-project/locations/-",
		Filter: `labels.owner = "me"`,
	})
	for {
		inst, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			panic(err)
		}
		op, err := client.DeleteInstance(ctx, &lustrepb.DeleteInstanceRequest{Name: inst.GetName()})
		if err == nil {
			err = op.Wait(ctx)
		}
		fmt.Println("deleted", inst.GetName(), err == nil)
	}
	fmt.Println("left:", len(s.Instances()))
	// Output:
	// create failed: true
	// deleted projects/my-project/locations/us-central1-a/instances/scratch true
	// left: 0
}
