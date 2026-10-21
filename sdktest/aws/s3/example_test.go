package s3_test

import (
	"context"
	"fmt"
	"net/http/httptest"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/parallelworks/squall/aws/s3"
	"github.com/parallelworks/squall/fault"
)

// The first create takes effect but its response is lost. With retries off
// the caller sees an error, and creating it again outside us-east-1 fails
// with BucketAlreadyOwnedByYou, so code under test must recognize its own
// bucket, by its tags, rather than give up on a taken name.
func Example() {
	ctx := context.Background()
	faults := fault.New().Add(fault.Rule{Method: "s3.CreateBucket", Nth: 1, Fault: fault.LoseResponse()})
	s := s3.New(s3.WithFaults(faults))
	srv := httptest.NewServer(s)
	defer srv.Close()

	client := awss3.New(awss3.Options{
		Region:           "us-west-2",
		BaseEndpoint:     aws.String(srv.URL),
		Credentials:      credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		RetryMaxAttempts: 1,
	})
	in := &awss3.CreateBucketInput{
		Bucket:                    aws.String("my-bucket"),
		CreateBucketConfiguration: &types.CreateBucketConfiguration{LocationConstraint: "us-west-2"},
	}
	_, err := client.CreateBucket(ctx, in)
	fmt.Println("create failed:", err != nil)
	_, err = client.CreateBucket(ctx, in)
	fmt.Println("create again:", err != nil)

	for _, b := range s.Buckets() {
		fmt.Println("bucket exists:", b.Name, b.Region)
	}
	fmt.Println(faults.Injected()[0])
	// Output:
	// create failed: true
	// create again: true
	// bucket exists: my-bucket us-west-2
	// #1 s3.CreateBucket my-bucket: lose response
}
