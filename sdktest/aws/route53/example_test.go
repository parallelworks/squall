package route53_test

import (
	"context"
	"fmt"
	"net/http/httptest"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awsroute53 "github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"

	"github.com/parallelworks/squall/aws/route53"
	"github.com/parallelworks/squall/fault"
)

// The first create takes effect but its response is lost. With retries off
// the caller sees an error, and creating it again with the same caller
// reference fails with HostedZoneAlreadyExists, so code under test must find
// the zone it made by name rather than leak it.
func Example() {
	ctx := context.Background()
	faults := fault.New().Add(fault.Rule{Method: "route53.CreateHostedZone", Nth: 1, Fault: fault.LoseResponse()})
	s := route53.New(route53.WithFaults(faults))
	srv := httptest.NewServer(s)
	defer srv.Close()

	client := awsroute53.New(awsroute53.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(srv.URL),
		Credentials:      credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		RetryMaxAttempts: 1,
	})
	in := &awsroute53.CreateHostedZoneInput{
		Name:             aws.String("cluster.internal"),
		CallerReference:  aws.String("create-1"),
		VPC:              &types.VPC{VPCId: aws.String("vpc-0123456789abcdef0"), VPCRegion: types.VPCRegionUsEast1},
		HostedZoneConfig: &types.HostedZoneConfig{PrivateZone: true},
	}
	_, err := client.CreateHostedZone(ctx, in)
	fmt.Println("create failed:", err != nil)
	_, err = client.CreateHostedZone(ctx, in)
	fmt.Println("create again failed:", err != nil)

	for _, z := range s.HostedZones() {
		fmt.Println("zone exists:", z.ID, z.Name, z.VPCs)
	}
	fmt.Println(faults.Injected()[0])
	// Output:
	// create failed: true
	// create again failed: true
	// zone exists: Z00000000000000000001 cluster.internal. [{us-east-1 vpc-0123456789abcdef0}]
	// #1 route53.CreateHostedZone cluster.internal.: lose response
}
