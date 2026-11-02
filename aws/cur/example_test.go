package cur_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/parallelworks/squall/aws/cur"
	"github.com/parallelworks/squall/fault"
)

// The first put takes effect but its response is lost, so the caller sees an
// error even though the report now exists, and putting it again fails with
// DuplicateReportNameException. Code under test must find the report it made
// rather than fail. Real tests point the official SDK at srv.URL with its base
// endpoint setting.
func Example() {
	faults := fault.New().Add(fault.Rule{Method: "cur.PutReportDefinition", Nth: 1, Fault: fault.LoseResponse()})
	s := cur.New(cur.WithFaults(faults))
	srv := httptest.NewServer(s)
	defer srv.Close()

	put := func() (string, error) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"ReportDefinition": {"ReportName": "daily",
"TimeUnit": "DAILY", "Format": "Parquet", "Compression": "Parquet", "AdditionalSchemaElements": ["RESOURCES"],
"S3Bucket": "reports", "S3Prefix": "cur", "S3Region": "us-east-1"}}`))
		req.Header.Set("X-Amz-Target", "AWSOrigamiServiceGatewayService.PutReportDefinition")
		req.Header.Set("Content-Type", "application/x-amz-json-1.1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if _, err := io.ReadAll(resp.Body); err != nil {
			return "", err
		}
		return resp.Header.Get("X-Amzn-ErrorType"), nil
	}
	_, err := put()
	fmt.Println("put failed:", err != nil)
	code, _ := put()
	fmt.Println("put again:", code)

	for _, d := range s.ReportDefinitions() {
		fmt.Println("report exists:", d.ReportName, d.TimeUnit, d.Format, d.S3Bucket)
	}
	fmt.Println(faults.Injected()[0])
	// Output:
	// put failed: true
	// put again: DuplicateReportNameException
	// report exists: daily DAILY Parquet reports
	// #1 cur.PutReportDefinition daily: lose response
}
