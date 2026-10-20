package ec2_test

import (
	"context"
	"fmt"
	"net/http/httptest"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/parallelworks/squall/aws/ec2"
	"github.com/parallelworks/squall/fault"
)

// The first allocation takes effect but its response is lost. With retries
// off the caller sees an error even though the address now exists, so code
// under test must find and adopt it, by its tags, rather than leak it.
func Example() {
	ctx := context.Background()
	faults := fault.New().Add(fault.Rule{Method: "ec2.AllocateAddress", Nth: 1, Fault: fault.LoseResponse()})
	s := ec2.New(ec2.WithFaults(faults))
	srv := httptest.NewServer(s)
	defer srv.Close()

	client := awsec2.New(awsec2.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(srv.URL),
		Credentials:      credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		RetryMaxAttempts: 1,
	})
	_, err := client.AllocateAddress(ctx, &awsec2.AllocateAddressInput{Domain: "vpc"})
	fmt.Println("allocate failed:", err != nil)

	for _, a := range s.Addresses() {
		fmt.Println("address exists:", a.AllocationID, a.PublicIP)
	}
	fmt.Println(faults.Injected()[0])
	// Output:
	// allocate failed: true
	// address exists: eipalloc-00000000000000001 100.64.0.1
	// #1 ec2.AllocateAddress us-east-1/eipalloc-00000000000000001: lose response
}
