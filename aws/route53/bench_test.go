package route53_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/parallelworks/squall/aws/route53"
)

func BenchmarkChangeAndListRecordSets(b *testing.B) {
	s := route53.New()
	id := zoneID(b, s)
	var batch strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&batch, `<Change><Action>CREATE</Action><ResourceRecordSet><Name>node-%d.example.internal</Name><Type>A</Type><TTL>60</TTL>
<ResourceRecords><ResourceRecord><Value>10.0.%d.%d</Value></ResourceRecord></ResourceRecords></ResourceRecordSet></Change>`, i, i/256, i%256)
	}
	rrset := "/2013-04-01/hostedzone/" + id + "/rrset"
	if code, body := do(b, s, http.MethodPost, rrset, "<ChangeResourceRecordSetsRequest><ChangeBatch><Changes>"+batch.String()+"</Changes></ChangeBatch></ChangeResourceRecordSetsRequest>"); code != http.StatusOK {
		b.Fatalf("seed = %d %s", code, body)
	}
	upsert := `<ChangeResourceRecordSetsRequest><ChangeBatch><Changes><Change><Action>UPSERT</Action><ResourceRecordSet>
<Name>head.example.internal</Name><Type>A</Type><TTL>60</TTL><ResourceRecords><ResourceRecord><Value>10.1.0.1</Value></ResourceRecord>
</ResourceRecords></ResourceRecordSet></Change></Changes></ChangeBatch></ChangeResourceRecordSetsRequest>`
	for b.Loop() {
		if code, body := do(b, s, http.MethodPost, rrset, upsert); code != http.StatusOK {
			b.Fatalf("upsert = %d %s", code, body)
		}
		if code, body := do(b, s, http.MethodGet, rrset+"?name=node-500.example.internal&maxitems=100", ""); code != http.StatusOK {
			b.Fatalf("list = %d %s", code, body)
		}
	}
}
