package cur_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awscur "github.com/aws/aws-sdk-go-v2/service/costandusagereportservice"
	"github.com/aws/aws-sdk-go-v2/service/costandusagereportservice/types"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/aws/cur"
	"github.com/parallelworks/squall/aws/s3"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
	"github.com/parallelworks/squall/sdktest/internal/sdkhttp"
)

const region = "us-east-1"

func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// newClient returns a client for s that does not retry, so each call is one request.
func newClient(t *testing.T, s *cur.Server) *awscur.Client {
	t.Helper()
	return awscur.New(awscur.Options{
		Region:           region,
		BaseEndpoint:     aws.String(serve(t, s)),
		Credentials:      credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:       sdkhttp.CopyBodies(awshttp.NewBuildableClient()),
		RetryMaxAttempts: 1,
	})
}

func definition(name, bucket string) *types.ReportDefinition {
	return &types.ReportDefinition{
		ReportName:               aws.String(name),
		TimeUnit:                 types.TimeUnitHourly,
		Format:                   types.ReportFormatParquet,
		Compression:              types.CompressionFormatParquet,
		AdditionalSchemaElements: []types.SchemaElement{types.SchemaElementResources, types.SchemaElementSplitCostAllocationData},
		S3Bucket:                 aws.String(bucket),
		S3Prefix:                 aws.String("reports/cur"),
		S3Region:                 types.AWSRegionUsStandard,
	}
}

func put(t *testing.T, c *awscur.Client, d *types.ReportDefinition) error {
	t.Helper()
	_, err := c.PutReportDefinition(t.Context(), &awscur.PutReportDefinitionInput{ReportDefinition: d})
	return err
}

// describe lists every report's name through the SDK's paginator.
func describe(t *testing.T, c *awscur.Client) (names []string, pages int) {
	t.Helper()
	p := awscur.NewDescribeReportDefinitionsPaginator(c, &awscur.DescribeReportDefinitionsInput{MaxResults: aws.Int32(5)})
	for p.HasMorePages() {
		out, err := p.NextPage(t.Context())
		if err != nil {
			t.Fatalf("describe: %v", err)
		}
		pages++
		for _, d := range out.ReportDefinitions {
			names = append(names, aws.ToString(d.ReportName))
		}
	}
	return names, pages
}

func TestReportLifecycle(t *testing.T) {
	s := cur.New()
	c := newClient(t, s)
	d := definition("hourly-usage", "billing-reports")
	d.AdditionalArtifacts = []types.AdditionalArtifact{types.AdditionalArtifactAthena}
	d.RefreshClosedReports = aws.Bool(true)
	d.ReportVersioning = types.ReportVersioningOverwriteReport
	_, err := c.PutReportDefinition(t.Context(), &awscur.PutReportDefinitionInput{
		ReportDefinition: d,
		Tags:             []types.Tag{{Key: aws.String("team"), Value: aws.String("finance")}},
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	csv := definition("daily-csv", "billing-reports")
	csv.TimeUnit, csv.Format, csv.Compression = types.TimeUnitDaily, types.ReportFormatCsv, types.CompressionFormatGzip
	csv.AdditionalSchemaElements = []types.SchemaElement{}
	if err := put(t, c, csv); err != nil {
		t.Fatalf("put csv: %v", err)
	}

	out, err := c.DescribeReportDefinitions(t.Context(), &awscur.DescribeReportDefinitionsInput{})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if len(out.ReportDefinitions) != 2 || out.NextToken != nil {
		t.Fatalf("describe = %+v", out)
	}
	got := out.ReportDefinitions[1]
	if aws.ToString(got.ReportName) != "hourly-usage" || got.TimeUnit != types.TimeUnitHourly || got.Format != types.ReportFormatParquet ||
		got.Compression != types.CompressionFormatParquet || aws.ToString(got.S3Bucket) != "billing-reports" ||
		aws.ToString(got.S3Prefix) != "reports/cur" || got.S3Region != types.AWSRegionUsStandard ||
		!slices.Equal(got.AdditionalSchemaElements, d.AdditionalSchemaElements) || !slices.Equal(got.AdditionalArtifacts, d.AdditionalArtifacts) ||
		!aws.ToBool(got.RefreshClosedReports) || got.ReportVersioning != types.ReportVersioningOverwriteReport || got.ReportStatus != nil {
		t.Fatalf("described = %+v", got)
	}
	if first := out.ReportDefinitions[0]; aws.ToString(first.ReportName) != "daily-csv" || first.AdditionalSchemaElements == nil || len(first.AdditionalSchemaElements) != 0 {
		t.Fatalf("described csv = %+v", first)
	}
	if defs := s.ReportDefinitions(); len(defs) != 2 || defs[1].Tags["team"] != "finance" || defs[0].Tags != nil {
		t.Fatalf("ReportDefinitions() = %+v", defs)
	}

	del, err := c.DeleteReportDefinition(t.Context(), &awscur.DeleteReportDefinitionInput{ReportName: aws.String("hourly-usage")})
	if err != nil || aws.ToString(del.ResponseMessage) == "" {
		t.Fatalf("delete = %+v, %v", del, err)
	}
	if names, _ := describe(t, c); !slices.Equal(names, []string{"daily-csv"}) {
		t.Fatalf("after delete = %v", names)
	}
	if _, err := c.DeleteReportDefinition(t.Context(), &awscur.DeleteReportDefinitionInput{ReportName: aws.String("hourly-usage")}); err != nil {
		t.Fatalf("delete again: %v", err)
	}
}

func TestPutErrors(t *testing.T) {
	s := cur.New()
	c := newClient(t, s)
	if err := put(t, c, definition("taken", "bucket")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := put(t, c, definition("taken", "bucket")); !errors.As(err, new(*types.DuplicateReportNameException)) {
		t.Fatalf("put duplicate = %v, want DuplicateReportNameException", err)
	}

	gzipParquet := definition("gzip-parquet", "bucket")
	gzipParquet.Compression = types.CompressionFormatGzip
	parquetCSV := definition("parquet-csv", "bucket")
	parquetCSV.Format = types.ReportFormatCsv
	for _, tc := range []struct {
		name    string
		d       *types.ReportDefinition
		message string
	}{
		{"Parquet format with GZIP", gzipParquet, "Parquet format requires Parquet compression."},
		{"textORcsv format with Parquet", parquetCSV, "textORcsv format requires ZIP or GZIP compression."},
		{"a name with a space", definition("my report", "bucket"), "at 'reportDefinition.reportName' failed to satisfy constraint: Member must satisfy regular expression pattern"},
		{"a long name", definition(strings.Repeat("r", 257), "bucket"), "Member must have length less than or equal to 256"},
		{"a bucket with a slash", definition("slash", "bucket/x"), "at 'reportDefinition.s3Bucket'"},
		{"an unknown time unit", func() *types.ReportDefinition {
			d := definition("weekly", "bucket")
			d.TimeUnit = "WEEKLY"
			return d
		}(), "Value 'WEEKLY' at 'reportDefinition.timeUnit' failed to satisfy constraint: Member must satisfy enum value set: [HOURLY, DAILY, MONTHLY]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := put(t, c, tc.d)
			v, ok := errors.AsType[*types.ValidationException](err)
			if !ok || !strings.Contains(v.ErrorMessage(), tc.message) || sdkerr.AWSStatus(err) != http.StatusBadRequest {
				t.Fatalf("put = %v, want ValidationException with %q", err, tc.message)
			}
		})
	}
	if err := put(t, c, &types.ReportDefinition{ReportName: aws.String("incomplete")}); err == nil || sdkerr.AWSCode(err) != "" {
		t.Fatalf("put incomplete = %v, want the SDK's own required field error", err)
	}

	for i := range 4 {
		if err := put(t, c, definition(fmt.Sprintf("report-%d", i), "bucket")); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if err := put(t, c, definition("sixth", "bucket")); !errors.As(err, new(*types.ReportLimitReachedException)) {
		t.Fatalf("put sixth = %v, want ReportLimitReachedException", err)
	}
	if got := len(s.ReportDefinitions()); got != 5 {
		t.Fatalf("%d reports, want 5", got)
	}
}

func TestDescribePages(t *testing.T) {
	s := cur.New(cur.WithReportLimit(12))
	c := newClient(t, s)
	var want []string
	for i := range 12 {
		name := fmt.Sprintf("report-%02d", i)
		want = append(want, name)
		if err := put(t, c, definition(name, "bucket")); err != nil {
			t.Fatalf("put %s: %v", name, err)
		}
	}
	if names, pages := describe(t, c); !slices.Equal(names, want) || pages != 3 {
		t.Fatalf("describe = %v in %d pages", names, pages)
	}
	_, err := c.DescribeReportDefinitions(t.Context(), &awscur.DescribeReportDefinitionsInput{MaxResults: aws.Int32(10)})
	if v, ok := errors.AsType[*types.ValidationException](err); !ok || !strings.Contains(v.ErrorMessage(), "at 'maxResults' failed to satisfy constraint: Member must have value less than or equal to 5") {
		t.Fatalf("describe 10 = %v, want ValidationException", err)
	}
}

func TestBucketPermissionsAreVerified(t *testing.T) {
	buckets := s3.New()
	s3c := awss3.New(awss3.Options{
		Region:           region,
		BaseEndpoint:     aws.String(serve(t, buckets)),
		Credentials:      credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:       sdkhttp.CopyBodies(awshttp.NewBuildableClient()),
		RetryMaxAttempts: 1,
	})
	if _, err := s3c.CreateBucket(t.Context(), &awss3.CreateBucketInput{Bucket: aws.String("billing-reports")}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	s := cur.New(cur.WithBucketPolicy(func(bucket string) (string, bool) {
		b, ok := buckets.LookupBucket(bucket)
		return b.Policy, ok
	}))
	c := newClient(t, s)

	refused := func(name, bucket string) {
		t.Helper()
		err := put(t, c, definition(name, bucket))
		if v, ok := errors.AsType[*types.ValidationException](err); !ok || v.ErrorMessage() != "Failed to verify customer bucket permission." {
			t.Fatalf("put %s to %s = %v, want the bucket permission error", name, bucket, err)
		}
	}
	refused("missing-bucket", "no-such-bucket")
	refused("no-policy", "billing-reports")

	setPolicy := func(policy string) {
		t.Helper()
		if _, err := s3c.PutBucketPolicy(t.Context(), &awss3.PutBucketPolicyInput{Bucket: aws.String("billing-reports"), Policy: aws.String(policy)}); err != nil {
			t.Fatalf("put bucket policy: %v", err)
		}
	}
	setPolicy(`{"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Principal": {"Service": "billingreports.amazonaws.com"},
"Action": ["s3:GetBucketAcl", "s3:GetBucketPolicy"], "Resource": "arn:aws:s3:::billing-reports"}]}`)
	refused("bucket-grants-only", "billing-reports")

	setPolicy(`{"Version": "2012-10-17", "Statement": [
{"Effect": "Allow", "Principal": {"Service": "billingreports.amazonaws.com"},
 "Action": ["s3:GetBucketAcl", "s3:GetBucketPolicy"], "Resource": "arn:aws:s3:::billing-reports",
 "Condition": {"StringEquals": {"aws:SourceAccount": "111122223333"}}},
{"Effect": "Allow", "Principal": {"Service": "billingreports.amazonaws.com"},
 "Action": "s3:PutObject", "Resource": "arn:aws:s3:::billing-reports/*"}]}`)
	if err := put(t, c, definition("granted", "billing-reports")); err != nil {
		t.Fatalf("put with the grants: %v", err)
	}
	if defs := s.ReportDefinitions(); len(defs) != 1 || defs[0].ReportName != "granted" {
		t.Fatalf("ReportDefinitions() = %+v", defs)
	}
}

func TestLostPutResponse(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "cur.PutReportDefinition", Resource: "lost", Nth: 1, Fault: fault.LoseResponse()})
	s := cur.New(cur.WithFaults(faults))
	c := newClient(t, s)
	if err := put(t, c, definition("lost", "bucket")); err == nil {
		t.Fatal("put succeeded despite the lost response")
	}
	if names, _ := describe(t, c); !slices.Equal(names, []string{"lost"}) {
		t.Fatalf("after a lost response = %v", names)
	}
	if err := put(t, c, definition("lost", "bucket")); !errors.As(err, new(*types.DuplicateReportNameException)) {
		t.Fatalf("put again = %v, want DuplicateReportNameException", err)
	}
	if got := faults.Injected(); len(got) != 1 {
		t.Fatalf("Injected() = %v", got)
	}

	unavailable := fault.New().Add(fault.Rule{Method: "cur.DescribeReportDefinitions", Nth: 1, Fault: fault.Reject(http.StatusInternalServerError, "")})
	_, err := newClient(t, cur.New(cur.WithFaults(unavailable))).DescribeReportDefinitions(t.Context(), &awscur.DescribeReportDefinitionsInput{})
	if !errors.As(err, new(*types.InternalErrorException)) {
		t.Fatalf("describe = %v, want InternalErrorException", err)
	}
}

func TestPolicyRefusesAnOperationByItsAction(t *testing.T) {
	policy := access.Allow("cur:DescribeReportDefinitions", "cur:DeleteReportDefinition")
	c := newClient(t, cur.New(cur.WithPolicy(policy)))
	err := put(t, c, definition("denied", "bucket"))
	if sdkerr.AWSCode(err) != "AccessDeniedException" || sdkerr.AWSStatus(err) != http.StatusForbidden || !strings.Contains(err.Error(), "cur:PutReportDefinition") {
		t.Fatalf("put = %v, want 403 AccessDeniedException naming cur:PutReportDefinition", err)
	}
	denials := policy.Denials()
	if len(denials) != 1 || denials[0].Method != "cur.PutReportDefinition" || denials[0].Resource != "denied" ||
		!slices.Equal(denials[0].Missing, []string{"cur:PutReportDefinition"}) {
		t.Fatalf("Denials() = %+v", denials)
	}
	if _, err := c.DescribeReportDefinitions(t.Context(), &awscur.DescribeReportDefinitionsInput{}); err != nil {
		t.Fatalf("describe: %v", err)
	}
	if _, err := c.DeleteReportDefinition(t.Context(), &awscur.DeleteReportDefinitionInput{ReportName: aws.String("denied")}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if unused := policy.Unused(); len(unused) != 0 {
		t.Errorf("Unused() = %v", unused)
	}
}
