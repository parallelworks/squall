package cur_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awscur "github.com/aws/aws-sdk-go-v2/service/costandusagereportservice"
	"github.com/aws/aws-sdk-go-v2/service/costandusagereportservice/types"

	"github.com/parallelworks/squall/aws/cur"
	"github.com/parallelworks/squall/fault"
)

// The first put takes effect but its response is lost. With retries off the
// caller sees an error, and putting it again fails with
// DuplicateReportNameException, so code under test must treat the report as
// made rather than give up.
func Example() {
	ctx := context.Background()
	faults := fault.New().Add(fault.Rule{Method: "cur.PutReportDefinition", Nth: 1, Fault: fault.LoseResponse()})
	s := cur.New(cur.WithFaults(faults))
	srv := httptest.NewServer(s)
	defer srv.Close()

	client := awscur.New(awscur.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(srv.URL),
		Credentials:      credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		RetryMaxAttempts: 1,
	})
	in := &awscur.PutReportDefinitionInput{ReportDefinition: &types.ReportDefinition{
		ReportName:               aws.String("hourly-usage"),
		TimeUnit:                 types.TimeUnitHourly,
		Format:                   types.ReportFormatParquet,
		Compression:              types.CompressionFormatParquet,
		AdditionalSchemaElements: []types.SchemaElement{types.SchemaElementResources},
		S3Bucket:                 aws.String("billing-reports"),
		S3Prefix:                 aws.String("cur"),
		S3Region:                 types.AWSRegionUsStandard,
	}}
	_, err := client.PutReportDefinition(ctx, in)
	fmt.Println("put failed:", err != nil)
	_, err = client.PutReportDefinition(ctx, in)
	fmt.Println("put again is a duplicate:", errors.As(err, new(*types.DuplicateReportNameException)))

	for _, d := range s.ReportDefinitions() {
		fmt.Println("report exists:", d.ReportName, d.TimeUnit, d.S3Bucket, d.S3Prefix)
	}
	fmt.Println(faults.Injected()[0])
	// Output:
	// put failed: true
	// put again is a duplicate: true
	// report exists: hourly-usage HOURLY billing-reports cur
	// #1 cur.PutReportDefinition hourly-usage: lose response
}
