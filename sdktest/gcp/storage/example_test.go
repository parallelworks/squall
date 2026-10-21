package storage_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/storage"
)

// The first insert takes effect but its response is lost. The client
// retries it, and the retry fails with 409 because the first one made the
// bucket, so code under test must check the bucket's labels before it takes
// the name for someone else's.
func Example() {
	ctx := context.Background()
	faults := fault.New().Add(fault.Rule{Method: "storage.buckets.insert", Nth: 1, Fault: fault.LoseResponse()})
	s := storage.New(storage.WithFaults(faults))
	srv := httptest.NewServer(s)
	defer srv.Close()

	client, err := gcs.NewClient(ctx, option.WithEndpoint(srv.URL+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		panic(err)
	}
	defer client.Close()

	err = client.Bucket("my-bucket").Create(ctx, "my-project", &gcs.BucketAttrs{Labels: map[string]string{"owner": "me"}})
	if e, ok := errors.AsType[*googleapi.Error](err); ok {
		fmt.Println("create failed:", e.Code)
	}

	attrs, _ := client.Bucket("my-bucket").Attrs(ctx)
	fmt.Println("bucket exists:", attrs.Name, attrs.Labels["owner"])
	fmt.Println(faults.Injected()[0])
	// Output:
	// create failed: 409
	// bucket exists: my-bucket me
	// #1 storage.buckets.insert b/my-bucket: lose response
}
