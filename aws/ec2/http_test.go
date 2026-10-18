package ec2_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/parallelworks/squall/aws/ec2"
)

const authorization = "AWS4-HMAC-SHA256 Credential=AKID/20260101/us-east-1/ec2/aws4_request, SignedHeaders=host, Signature=0"

// do sends one Query API call straight to s and returns the status and body.
func do(t *testing.T, s *ec2.Server, auth string, form url.Values) (int, string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func form(action string, kv ...string) url.Values {
	v := url.Values{"Action": {action}, "Version": {"2016-11-15"}}
	for i := 0; i < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func TestResponsesUseTheEC2Shape(t *testing.T) {
	s := ec2.New()
	code, body := do(t, s, authorization, form("AllocateAddress"))
	for _, want := range []string{
		`<AllocateAddressResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">`,
		"<requestId>", "<allocationId>eipalloc-", "<domain>vpc</domain>",
	} {
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("allocate = %d %s, want %s", code, body, want)
		}
	}
	code, body = do(t, s, authorization, form("ReleaseAddress", "AllocationId", "eipalloc-missing"))
	if code != http.StatusBadRequest || !strings.Contains(body,
		"<Response><Errors><Error><Code>InvalidAllocationID.NotFound</Code><Message>The allocation ID &#39;eipalloc-missing&#39; does not exist</Message></Error></Errors><RequestID>") {
		t.Fatalf("release = %d %s", code, body)
	}
}

func TestRequestsTheServerCannotRead(t *testing.T) {
	s := ec2.New()
	for _, tc := range []struct {
		name   string
		auth   string
		form   url.Values
		status int
		code   string
	}{
		{"no credentials", "", form("DescribeAddresses"), http.StatusUnauthorized, "MissingAuthenticationToken"},
		{"a malformed credential scope", "AWS4-HMAC-SHA256 Credential=AKID/us-east-1", form("DescribeAddresses"), http.StatusBadRequest, "IncompleteSignature"},
		{"no action", authorization, url.Values{"Version": {"2016-11-15"}}, http.StatusBadRequest, "MissingAction"},
		{"another version", authorization, url.Values{"Action": {"DescribeAddresses"}, "Version": {"2014-10-01"}}, http.StatusBadRequest, "NoSuchVersion"},
		{"an unimplemented action", authorization, form("CreateSnapshot"), http.StatusNotFound, "InvalidAction"},
		{"an unknown parameter", authorization, form("DescribeAddresses", "Bogus", "1"), http.StatusBadRequest, "UnknownParameter"},
		{"a missing parameter", authorization, form("CreateNetworkInterface"), http.StatusBadRequest, "MissingParameter"},
		{"a filter without values", authorization, form("DescribeAddresses", "Filter.1.Name", "domain"), http.StatusBadRequest, "InvalidParameterValue"},
		{"a bad boolean", authorization, form("DescribeAddresses", "DryRun", "yes"), http.StatusBadRequest, "InvalidParameterValue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := do(t, s, tc.auth, tc.form)
			if code != tc.status || !strings.Contains(body, "<Code>"+tc.code+"</Code>") {
				t.Fatalf("= %d %s, want %d %s", code, body, tc.status, tc.code)
			}
		})
	}
}

func TestPresignedCredentialsNameTheRegion(t *testing.T) {
	s := ec2.New()
	f := form("AllocateAddress", "X-Amz-Credential", "AKID/20260101/ap-south-1/ec2/aws4_request", "X-Amz-Signature", "0")
	if code, body := do(t, s, "", f); code != http.StatusOK {
		t.Fatalf("allocate = %d %s", code, body)
	}
	if got := s.Addresses(); len(got) != 1 || got[0].Region != "ap-south-1" {
		t.Fatalf("addresses = %+v", got)
	}
}

func TestFilterWildcards(t *testing.T) {
	s := ec2.New()
	for _, name := range []string{"web-1", "web-10", "a*b", "a?b", `a\b`} {
		if _, err := s.PutAddress("us-east-1", ec2.Address{Tags: map[string]string{"Name": name}}); err != nil {
			t.Fatal(err)
		}
	}
	for pattern, want := range map[string]int{
		"web-*":  2,
		"web-?":  1,
		"*":      5,
		"web-1":  1,
		`a\*b`:   1,
		`a\?b`:   1,
		`a\\b`:   1,
		"a?b":    3,
		"?????":  1,
		"*-1*":   2,
		"web":    0,
		"*web-1": 1,
	} {
		_, body := do(t, s, authorization, form("DescribeAddresses", "Filter.1.Name", "tag:Name", "Filter.1.Value.1", pattern))
		if got := strings.Count(body, "<allocationId>"); got != want {
			t.Errorf("tag:Name=%s matched %d, want %d", pattern, got, want)
		}
	}
}

func TestFilterWildcardsDoNotBacktrackExponentially(t *testing.T) {
	s := ec2.New()
	if _, err := s.PutAddress("us-east-1", ec2.Address{Tags: map[string]string{"Name": strings.Repeat("a", 256)}}); err != nil {
		t.Fatal(err)
	}
	_, body := do(t, s, authorization, form("DescribeAddresses", "Filter.1.Name", "tag:Name", "Filter.1.Value.1", strings.Repeat("*a", 8)+"*b"))
	if strings.Contains(body, "<allocationId>") {
		t.Fatalf("a pattern ending in b matched a value of only a: %s", body)
	}
}
