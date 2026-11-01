package route53_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/parallelworks/squall/aws/route53"
)

const createZone = `<CreateHostedZoneRequest xmlns="https://route53.amazonaws.com/doc/2013-04-01/">
<Name>example.internal</Name><CallerReference>ref</CallerReference>
<VPC><VPCRegion>us-east-1</VPCRegion><VPCId>vpc-1</VPCId></VPC></CreateHostedZoneRequest>`

// do sends one request straight to s and returns the status and body.
func do(tb testing.TB, s *route53.Server, method, target, body string) (int, string) {
	tb.Helper()
	req := httptest.NewRequestWithContext(tb.Context(), method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func zoneID(tb testing.TB, s *route53.Server) string {
	tb.Helper()
	if code, body := do(tb, s, http.MethodPost, "/2013-04-01/hostedzone", createZone); code != http.StatusCreated {
		tb.Fatalf("create = %d %s", code, body)
	}
	return s.HostedZones()[0].ID
}

func TestErrorsUseTheRoute53Shape(t *testing.T) {
	s := route53.New()
	code, body := do(t, s, http.MethodGet, "/2013-04-01/hostedzone/ZMISSING", "")
	if want := `<ErrorResponse xmlns="https://route53.amazonaws.com/doc/2013-04-01/"><Error><Type>Sender</Type><Code>NoSuchHostedZone</Code>` +
		`<Message>No hosted zone found with ID: ZMISSING</Message></Error><RequestId>`; code != http.StatusNotFound || !strings.Contains(body, want) {
		t.Fatalf("get missing = %d %s", code, body)
	}

	id := zoneID(t, s)
	code, body = do(t, s, http.MethodPost, "/2013-04-01/hostedzone/"+id+"/rrset", `<ChangeResourceRecordSetsRequest><ChangeBatch><Changes>
<Change><Action>DELETE</Action><ResourceRecordSet><Name>a.example.internal</Name><Type>A</Type><TTL>60</TTL>
<ResourceRecords><ResourceRecord><Value>10.0.0.1</Value></ResourceRecord></ResourceRecords></ResourceRecordSet></Change>
</Changes></ChangeBatch></ChangeResourceRecordSetsRequest>`)
	if want := `<InvalidChangeBatch xmlns="https://route53.amazonaws.com/doc/2013-04-01/"><Messages><Message>Tried to delete resource record set ` +
		`[name=&#39;a.example.internal.&#39;, type=&#39;A&#39;] but it was not found</Message></Messages><RequestId>`; code != http.StatusBadRequest || !strings.Contains(body, want) {
		t.Fatalf("bad batch = %d %s", code, body)
	}
}

func TestRequestsTheServerCannotRead(t *testing.T) {
	s := route53.New()
	id := zoneID(t, s)
	for _, tc := range []struct {
		name, method, target, body string
		status                     int
		code                       string
	}{
		{"another API version", http.MethodGet, "/2012-12-12/hostedzone", "", http.StatusNotFound, "NotImplemented"},
		{"an unimplemented operation", http.MethodGet, "/2013-04-01/healthcheck", "", http.StatusNotFound, "NotImplemented"},
		{"an unknown parameter", http.MethodGet, "/2013-04-01/hostedzonesbyname?bogus=1", "", http.StatusBadRequest, "InvalidInput"},
		{"malformed XML", http.MethodPost, "/2013-04-01/hostedzone", "<CreateHostedZoneRequest>", http.StatusBadRequest, "InvalidInput"},
		{"a zone ID without a name", http.MethodGet, "/2013-04-01/hostedzonesbyname?hostedzoneid=" + id, "", http.StatusBadRequest, "InvalidInput"},
		{"a bad maxitems", http.MethodGet, "/2013-04-01/hostedzone/" + id + "/rrset?maxitems=0", "", http.StatusBadRequest, "InvalidInput"},
		{"a type without a name", http.MethodGet, "/2013-04-01/hostedzone/" + id + "/rrset?type=A", "", http.StatusBadRequest, "InvalidInput"},
		{"an empty change batch", http.MethodPost, "/2013-04-01/hostedzone/" + id + "/rrset", "<ChangeResourceRecordSetsRequest/>", http.StatusBadRequest, "InvalidInput"},
		{"a weighted record set", http.MethodPost, "/2013-04-01/hostedzone/" + id + "/rrset", `<ChangeResourceRecordSetsRequest><ChangeBatch><Changes>
<Change><Action>CREATE</Action><ResourceRecordSet><Name>a.example.internal</Name><Type>A</Type><SetIdentifier>one</SetIdentifier><Weight>1</Weight>
<TTL>60</TTL><ResourceRecords><ResourceRecord><Value>10.0.0.1</Value></ResourceRecord></ResourceRecords></ResourceRecordSet></Change>
</Changes></ChangeBatch></ChangeResourceRecordSetsRequest>`, http.StatusBadRequest, "InvalidInput"},
		{"a health check's tags", http.MethodGet, "/2013-04-01/tags/healthcheck/abc", "", http.StatusNotFound, "NoSuchHealthCheck"},
		{"an unknown resource type", http.MethodGet, "/2013-04-01/tags/bucket/abc", "", http.StatusBadRequest, "InvalidInput"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := do(t, s, tc.method, tc.target, tc.body)
			if code != tc.status || !strings.Contains(body, "<Code>"+tc.code+"</Code>") {
				t.Fatalf("%s %s = %d %s, want %d %s", tc.method, tc.target, code, body, tc.status, tc.code)
			}
		})
	}
}

func TestNamesReadBackAsRoute53ListsThem(t *testing.T) {
	s := route53.New()
	id := zoneID(t, s)
	code, body := do(t, s, http.MethodPost, "/2013-04-01/hostedzone/"+id+"/rrset", `<ChangeResourceRecordSetsRequest><ChangeBatch><Changes>
<Change><Action>CREATE</Action><ResourceRecordSet><Name>*.Example.Internal.</Name><Type>A</Type><TTL>60</TTL>
<ResourceRecords><ResourceRecord><Value>10.0.0.1</Value></ResourceRecord></ResourceRecords></ResourceRecordSet></Change>
<Change><Action>CREATE</Action><ResourceRecordSet><Name>\052.sub.example.internal</Name><Type>A</Type><TTL>60</TTL>
<ResourceRecords><ResourceRecord><Value>10.0.0.2</Value></ResourceRecord></ResourceRecords></ResourceRecordSet></Change>
</Changes></ChangeBatch></ChangeResourceRecordSetsRequest>`)
	if code != http.StatusOK {
		t.Fatalf("change = %d %s", code, body)
	}
	var names []string
	for _, rs := range s.RecordSets(id) {
		names = append(names, rs.Name)
	}
	if got, want := strings.Join(names, " "), `example.internal. example.internal. \052.example.internal. \052.sub.example.internal.`; got != want {
		t.Fatalf("names = %s, want %s", got, want)
	}
}
