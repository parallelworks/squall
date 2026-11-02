package cur_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/parallelworks/squall/aws/cur"
)

const target = "AWSOrigamiServiceGatewayService."

// do sends one call straight to s and returns the status, error type and body.
func do(tb testing.TB, s *cur.Server, op, body string) (int, string, string) {
	tb.Helper()
	req := httptest.NewRequestWithContext(tb.Context(), http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-Amz-Target", target+op)
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Code, rec.Header().Get("X-Amzn-ErrorType"), rec.Body.String()
}

func definition(name, bucket string) string {
	return `{"ReportDefinition": {"ReportName": "` + name + `", "TimeUnit": "HOURLY", "Format": "textORcsv",
"Compression": "GZIP", "AdditionalSchemaElements": ["RESOURCES"], "S3Bucket": "` + bucket + `", "S3Prefix": "cur",
"S3Region": "us-east-1"}}`
}

func TestErrorsUseTheJSONShape(t *testing.T) {
	s := cur.New()
	if code, typ, body := do(t, s, "PutReportDefinition", definition("r1", "bucket")); code != http.StatusOK || typ != "" || strings.TrimSpace(body) != "{}" {
		t.Fatalf("put = %d %s %s", code, typ, body)
	}
	code, typ, body := do(t, s, "PutReportDefinition", definition("r1", "bucket"))
	if want := `{"__type":"DuplicateReportNameException","Message":"A report with the name r1 already exists in the account."}`; code != http.StatusBadRequest ||
		typ != "DuplicateReportNameException" || strings.TrimSpace(body) != want {
		t.Fatalf("put again = %d %s %s", code, typ, body)
	}
}

func TestRequestsTheServerCannotRead(t *testing.T) {
	s := cur.New()
	for _, tc := range []struct {
		name, op, body string
		status         int
		code, message  string
	}{
		{"an unimplemented operation", "ModifyReportDefinition", "{}", http.StatusNotFound, "UnknownOperationException", ""},
		{"malformed JSON", "PutReportDefinition", "{", http.StatusBadRequest, "SerializationException", ""},
		{"an unknown member", "DescribeReportDefinitions", `{"Filter": "x"}`, http.StatusBadRequest, "SerializationException", ""},
		{
			"no definition", "PutReportDefinition", "{}", http.StatusBadRequest, "ValidationException",
			"1 validation error detected: Value null at 'reportDefinition' failed to satisfy constraint: Member must not be null",
		},
		{
			"missing fields", "PutReportDefinition", `{"ReportDefinition": {"ReportName": "r", "TimeUnit": "HOURLY", "Format": "textORcsv", "Compression": "ZIP"}}`,
			http.StatusBadRequest, "ValidationException", "4 validation errors detected: Value null at 'reportDefinition.additionalSchemaElements'",
		},
		{
			"a bad region", "PutReportDefinition", strings.Replace(definition("r", "b"), "us-east-1", "mars-1", 1), http.StatusBadRequest, "ValidationException",
			"Value 'mars-1' at 'reportDefinition.s3Region' failed to satisfy constraint: Member must satisfy enum value set",
		},
		{
			"a bad schema element", "PutReportDefinition", strings.Replace(definition("r", "b"), `"RESOURCES"`, `"RESOURCES", "TAGS"`, 1), http.StatusBadRequest,
			"ValidationException", "Value 'TAGS' at 'reportDefinition.additionalSchemaElements.2.member'",
		},
		{"a delete without a name", "DeleteReportDefinition", "{}", http.StatusBadRequest, "ValidationException", "Value null at 'reportName'"},
		{"a bad token", "DescribeReportDefinitions", `{"NextToken": "!"}`, http.StatusBadRequest, "ValidationException", "at 'nextToken'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, typ, body := do(t, s, tc.op, tc.body)
			if code != tc.status || typ != tc.code || !strings.Contains(body, tc.message) {
				t.Fatalf("%s = %d %s %s, want %d %s %q", tc.op, code, typ, body, tc.status, tc.code, tc.message)
			}
		})
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || rec.Header().Get("X-Amzn-ErrorType") != "UnknownOperationException" {
		t.Fatalf("GET / = %d %s", rec.Code, rec.Body)
	}
}

func TestBucketPolicyGrants(t *testing.T) {
	const (
		service = `"Principal": {"Service": "billingreports.amazonaws.com"}`
		acl     = `{"Effect": "Allow", ` + service + `, "Action": ["s3:GetBucketAcl", "s3:GetBucketPolicy"], "Resource": "arn:aws:s3:::reports"}`
		put     = `{"Effect": "Allow", ` + service + `, "Action": "s3:PutObject", "Resource": "arn:aws:s3:::reports/*"}`
	)
	for _, tc := range []struct {
		name, policy string
		ok           bool
	}{
		{"the documented grants", `{"Version": "2008-10-17", "Statement": [` + acl + `, ` + put + `]}`, true},
		{"a single statement object", `{"Statement": {"Effect": "Allow", ` + service + `, "Action": "s3:*", "Resource": ["arn:aws:s3:::reports", "arn:aws:s3:::reports/*"]}}`, true},
		{"a principal list and wildcards", `{"Statement": [{"Effect": "Allow", "Principal": {"Service": ["ec2.amazonaws.com", "billingreports.amazonaws.com"]},
"Action": ["s3:GetBucket*", "S3:PUTOBJECT"], "Resource": ["arn:aws:s3:::report?", "arn:aws:s3:::*"]}]}`, true},
		{"anyone", `{"Statement": [{"Effect": "Allow", "Principal": "*", "Action": "*", "Resource": "*"}]}`, true},
		{"no policy", "", false},
		{"no object grant", `{"Statement": [` + acl + `]}`, false},
		{"objects under a prefix only", `{"Statement": [` + acl + `, {"Effect": "Allow", ` + service + `, "Action": "s3:PutObject", "Resource": "arn:aws:s3:::reports/cur/*"}]}`, false},
		{"one object only", `{"Statement": [` + acl + `, {"Effect": "Allow", ` + service + `, "Action": "s3:PutObject", "Resource": "arn:aws:s3:::reports/?"}]}`, false},
		{"another bucket", strings.ReplaceAll(`{"Statement": [`+acl+`, `+put+`]}`, ":::reports", ":::others"), false},
		{"another principal", strings.ReplaceAll(`{"Statement": [`+acl+`, `+put+`]}`, "billingreports", "ec2"), false},
		{"no policy read", `{"Statement": [` + strings.Replace(acl, `, "s3:GetBucketPolicy"`, "", 1) + `, ` + put + `]}`, false},
		{"an explicit deny", `{"Statement": [` + acl + `, ` + put + `, {"Effect": "Deny", "Principal": "*", "Action": "s3:PutObject", "Resource": "arn:aws:s3:::reports/*"}]}`, false},
		{"not JSON", "Allow", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := cur.New(cur.WithBucketPolicy(func(bucket string) (string, bool) { return tc.policy, bucket == "reports" }))
			code, typ, body := do(t, s, "PutReportDefinition", definition("r", "reports"))
			if tc.ok != (code == http.StatusOK) {
				t.Fatalf("put = %d %s %s, want success %v", code, typ, body, tc.ok)
			}
			if !tc.ok && (typ != "ValidationException" || !strings.Contains(body, "Failed to verify customer bucket permission.")) {
				t.Fatalf("put = %d %s %s, want the bucket permission error", code, typ, body)
			}
		})
	}
}
