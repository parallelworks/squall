package cur_test

import (
	"net/http"
	"testing"

	"github.com/parallelworks/squall/aws/cur"
)

func BenchmarkPutDescribeDelete(b *testing.B) {
	s := cur.New()
	for b.Loop() {
		if code, typ, body := do(b, s, "PutReportDefinition", definition("bench", "bucket")); code != http.StatusOK {
			b.Fatalf("put = %d %s %s", code, typ, body)
		}
		if code, typ, body := do(b, s, "DescribeReportDefinitions", `{"MaxResults": 5}`); code != http.StatusOK {
			b.Fatalf("describe = %d %s %s", code, typ, body)
		}
		if code, typ, body := do(b, s, "DeleteReportDefinition", `{"ReportName": "bench"}`); code != http.StatusOK {
			b.Fatalf("delete = %d %s %s", code, typ, body)
		}
	}
}
