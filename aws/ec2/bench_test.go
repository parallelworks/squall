package ec2_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/parallelworks/squall/aws/ec2"
)

func BenchmarkAllocateDescribeRelease(b *testing.B) {
	s := ec2.New()
	id := regexp.MustCompile(`<allocationId>([^<]+)</allocationId>`)
	do := func(body string) string {
		req := httptest.NewRequestWithContext(b.Context(), http.MethodPost, "/", strings.NewReader(body+"&Version=2016-11-15"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", authorization)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("%s = %d %s", body, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	for b.Loop() {
		alloc := id.FindStringSubmatch(do("Action=AllocateAddress"))[1]
		do("Action=DescribeAddresses&AllocationId.1=" + alloc)
		do("Action=ReleaseAddress&AllocationId=" + alloc)
	}
}

func BenchmarkDescribeInterfacesFiltered(b *testing.B) {
	s := ec2.New()
	for i := range 1000 {
		ni := ec2.NetworkInterface{SubnetID: "subnet-1", Tags: map[string]string{"session": strconv.Itoa(i % 10)}}
		if _, err := s.PutNetworkInterface("us-east-1", ni); err != nil {
			b.Fatal(err)
		}
	}
	body := "Action=DescribeNetworkInterfaces&Version=2016-11-15&Filter.1.Name=tag%3Asession&Filter.1.Value.1=3"
	for b.Loop() {
		req := httptest.NewRequestWithContext(b.Context(), http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", authorization)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatal(rec.Body)
		}
	}
}
