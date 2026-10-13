package compute_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gcompute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/compute"
)

const (
	project = "p"
	region  = "us-east1"
)

func newClient(t *testing.T, s *compute.Server) *gcompute.AddressesClient {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	c, err := gcompute.NewAddressesRESTClient(t.Context(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func insert(t *testing.T, c *gcompute.AddressesClient, a *computepb.Address) error {
	t.Helper()
	op, err := c.Insert(t.Context(), &computepb.InsertAddressRequest{Project: project, Region: region, AddressResource: a})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

func get(t *testing.T, c *gcompute.AddressesClient, name string) (*computepb.Address, error) {
	t.Helper()
	return c.Get(t.Context(), &computepb.GetAddressRequest{Project: project, Region: region, Address: name})
}

func apiCode(err error) int {
	if e, ok := errors.AsType[*googleapi.Error](err); ok {
		return e.Code
	}
	if e, ok := errors.AsType[interface {
		error
		HTTPCode() int
	}](err); ok {
		return e.HTTPCode()
	}
	return 0
}

func TestInsertThenGet(t *testing.T) {
	s := compute.New()
	c := newClient(t, s)
	err := insert(t, c, &computepb.Address{Name: new("ip-1"), Description: new("for a test"), Labels: map[string]string{"owner": "a"}})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := get(t, c, "ip-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.GetAddress() != "100.64.0.1" || got.GetStatus() != "RESERVED" || got.GetAddressType() != "EXTERNAL" ||
		got.GetDescription() != "for a test" || got.GetLabels()["owner"] != "a" || got.GetId() == 0 || got.GetLabelFingerprint() == "" {
		t.Fatalf("address = %v", got)
	}
	if want := "https://www.googleapis.com/compute/v1/projects/p/regions/us-east1/addresses/ip-1"; got.GetSelfLink() != want {
		t.Errorf("selfLink = %s", got.GetSelfLink())
	}
	if _, ok := s.LookupAddress(project, region, "ip-1"); !ok {
		t.Error("LookupAddress did not find the inserted address")
	}
}

func TestInsertErrors(t *testing.T) {
	s := compute.New()
	c := newClient(t, s)
	if err := insert(t, c, &computepb.Address{Name: new("taken")}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		address *computepb.Address
		code    int
	}{
		{"a name in use", &computepb.Address{Name: new("taken")}, http.StatusConflict},
		{"an invalid name", &computepb.Address{Name: new("Not_Valid")}, http.StatusBadRequest},
		{"an unknown address type", &computepb.Address{Name: new("ip-2"), AddressType: new("PUBLIC")}, http.StatusBadRequest},
		{"an IP in use", &computepb.Address{Name: new("ip-3"), Address: new("100.64.0.1")}, http.StatusBadRequest},
		{"an invalid IP", &computepb.Address{Name: new("ip-4"), Address: new("100.64.0")}, http.StatusBadRequest},
		{"an uppercase label key", &computepb.Address{Name: new("ip-5"), Labels: map[string]string{"Owner": "a"}}, http.StatusBadRequest},
		{"a label value too long", &computepb.Address{Name: new("ip-6"), Labels: map[string]string{"owner": strings.Repeat("a", 64)}}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := insert(t, c, tc.address); apiCode(err) != tc.code {
				t.Fatalf("insert = %v, want HTTP %d", err, tc.code)
			}
		})
	}
	if got := len(s.Addresses()); got != 1 {
		t.Fatalf("%d addresses stored, want only the first", got)
	}
}

func TestGetOrDeleteAMissingAddressIsNotFound(t *testing.T) {
	c := newClient(t, compute.New())
	if _, err := get(t, c, "missing"); apiCode(err) != http.StatusNotFound {
		t.Fatalf("get = %v", err)
	}
	_, err := c.Delete(t.Context(), &computepb.DeleteAddressRequest{Project: project, Region: region, Address: "missing"})
	if apiCode(err) != http.StatusNotFound {
		t.Fatalf("delete = %v", err)
	}
}

func TestDelete(t *testing.T) {
	s := compute.New()
	c := newClient(t, s)
	if err := insert(t, c, &computepb.Address{Name: new("ip-1")}); err != nil {
		t.Fatal(err)
	}
	op, err := c.Delete(t.Context(), &computepb.DeleteAddressRequest{Project: project, Region: region, Address: "ip-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LookupAddress(project, region, "ip-1"); ok {
		t.Fatal("the address is still there")
	}
	// A freed address can be taken again.
	if err := insert(t, c, &computepb.Address{Name: new("ip-2"), Address: new("100.64.0.1")}); err != nil {
		t.Fatalf("reusing the freed IP: %v", err)
	}
}

func TestSetLabelsChecksTheFingerprint(t *testing.T) {
	s := compute.New()
	c := newClient(t, s)
	if err := insert(t, c, &computepb.Address{Name: new("ip-1")}); err != nil {
		t.Fatal(err)
	}
	before, _ := get(t, c, "ip-1")
	setLabels := func(fp string) error {
		op, err := c.SetLabels(t.Context(), &computepb.SetLabelsAddressRequest{
			Project: project, Region: region, Resource: "ip-1",
			RegionSetLabelsRequestResource: &computepb.RegionSetLabelsRequest{LabelFingerprint: new(fp), Labels: map[string]string{"session-id": "x"}},
		})
		if err != nil {
			return err
		}
		return op.Wait(t.Context())
	}
	if err := setLabels(before.GetLabelFingerprint()); err != nil {
		t.Fatal(err)
	}
	after, _ := get(t, c, "ip-1")
	if after.GetLabels()["session-id"] != "x" || after.GetLabelFingerprint() == before.GetLabelFingerprint() {
		t.Fatalf("after setLabels = %v", after)
	}
	if err := setLabels(before.GetLabelFingerprint()); apiCode(err) != http.StatusPreconditionFailed {
		t.Fatalf("setLabels with a stale fingerprint = %v", err)
	}

	current, _ := s.LookupAddress(project, region, "ip-1")
	current.Labels = map[string]string{"session-id": "changed-elsewhere"}
	if _, err := s.PutAddress(project, region, current); err != nil {
		t.Fatal(err)
	}
	if err := setLabels(after.GetLabelFingerprint()); apiCode(err) != http.StatusPreconditionFailed {
		t.Fatalf("setLabels after PutAddress changed the labels = %v", err)
	}
}

func TestListFiltersAndPages(t *testing.T) {
	c := newClient(t, compute.New())
	for i := range 5 {
		labels := map[string]string{"session-id": "mine"}
		if i == 2 {
			labels["session-id"] = "theirs"
		}
		if err := insert(t, c, &computepb.Address{Name: new(fmt.Sprintf("ip-%d", i)), Labels: labels}); err != nil {
			t.Fatal(err)
		}
	}
	list := func(filter string, pageSize int) ([]string, error) {
		req := &computepb.ListAddressesRequest{Project: project, Region: region}
		if filter != "" {
			req.Filter = new(filter)
		}
		it := c.List(t.Context(), req)
		it.PageInfo().MaxSize = pageSize
		var names []string
		for {
			a, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return names, nil
			}
			if err != nil {
				return names, err
			}
			names = append(names, a.GetName())
		}
	}
	for _, tc := range []struct {
		filter string
		want   string
	}{
		{"", "[ip-0 ip-1 ip-2 ip-3 ip-4]"},
		{"labels.session-id=mine", "[ip-0 ip-1 ip-3 ip-4]"},
		{`(labels.session-id = "theirs") AND (name != ip-0)`, "[ip-2]"},
		{"name = ip-4 status = RESERVED", "[ip-4]"},
		{"( labels.session-id = theirs )", "[ip-2]"},
	} {
		names, err := list(tc.filter, 2)
		if err != nil || fmt.Sprint(names) != tc.want {
			t.Errorf("list %q = %v, %v; want %s", tc.filter, names, err, tc.want)
		}
	}
	if _, err := list("creationTimestamp > 0", 0); apiCode(err) != http.StatusBadRequest {
		t.Errorf("an unsupported filter = %v, want HTTP 400", err)
	}
	if _, err := list("", 501); apiCode(err) != http.StatusBadRequest {
		t.Errorf("maxResults over 500 = %v, want HTTP 400", err)
	}
	it := c.List(t.Context(), &computepb.ListAddressesRequest{Project: project, Region: region, OrderBy: new("creationTimestamp desc")})
	if _, err := it.Next(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("an unsupported orderBy = %v, want HTTP 400", err)
	}
}

func TestPutAddressSetsUpState(t *testing.T) {
	s := compute.New()
	held, err := s.PutAddress(project, region, compute.Address{Name: "held", Labels: map[string]string{"session-id": "someone-else"}})
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, s)
	got, err := get(t, c, "held")
	if err != nil || got.GetLabels()["session-id"] != "someone-else" || got.GetAddress() != held.Address {
		t.Fatalf("get = %v, %v", got, err)
	}
	if err := insert(t, c, &computepb.Address{Name: new("held")}); apiCode(err) != http.StatusConflict {
		t.Fatalf("insert over it = %v", err)
	}
	for _, bad := range []compute.Address{
		{Name: "-bad"},
		{Name: "ok", AddressType: "internal"},
		{Name: "ok", Address: "not-an-ip"},
		{Name: "ok", Labels: map[string]string{"": "x"}},
	} {
		if _, err := s.PutAddress(project, region, bad); err == nil {
			t.Errorf("PutAddress accepted %+v", bad)
		}
	}
}

func TestUnimplementedMethodsAreNotFound(t *testing.T) {
	s := compute.New()
	for _, method := range []string{http.MethodPatch, http.MethodPut} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, "/compute/v1/projects/p/regions/r/addresses/a", nil))
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"reason":"notFound"`) {
			t.Errorf("%s = %d %s, want a 404 API error", method, rec.Code, rec.Body)
		}
	}
}

func TestFaults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fault  fault.Fault
		code   int  // HTTP code of the error the caller sees; -1 for a transport error
		stored bool // whether the insert took effect anyway
	}{
		{"rejected", fault.Unavailable(), http.StatusServiceUnavailable, false},
		{"rate limited", fault.RateLimited(), http.StatusTooManyRequests, false},
		{"response lost", fault.LoseResponse(), -1, true},
		{"error after applying", fault.FailAfter(http.StatusInternalServerError), http.StatusInternalServerError, true},
		{"operation failed", fault.FailOperation(http.StatusForbidden), http.StatusForbidden, false},
		{"slow", fault.Delay(20 * time.Millisecond), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			faults := fault.New().Add(fault.Rule{Method: "compute.addresses.insert", Nth: 1, Fault: tc.fault})
			s := compute.New(compute.WithFaults(faults))
			c := newClient(t, s)
			start := time.Now()
			err := insert(t, c, &computepb.Address{Name: new("ip-1")})
			if took := time.Since(start); took < tc.fault.Delay {
				t.Fatalf("insert took %v, want at least %v", took, tc.fault.Delay)
			}
			switch {
			case tc.code == 0 && err != nil:
				t.Fatalf("insert = %v, want success", err)
			case tc.code == -1 && (err == nil || apiCode(err) != 0):
				t.Fatalf("insert = %v, want a transport error", err)
			case tc.code > 0 && apiCode(err) != tc.code:
				t.Fatalf("insert = %v, want HTTP %d", err, tc.code)
			}
			if _, ok := s.LookupAddress(project, region, "ip-1"); ok != tc.stored {
				t.Fatalf("stored = %v, want %v", ok, tc.stored)
			}
			if got := faults.Injected(); len(got) != 1 {
				t.Fatalf("Injected = %v", got)
			}
		})
	}
}

func TestLostResponseReachesTheCaller(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		call   func(t *testing.T, c *gcompute.AddressesClient) error
	}{
		// The transport would silently retry a read whose response never started.
		{"a read on a reused connection", "compute.addresses.get", func(t *testing.T, c *gcompute.AddressesClient) error {
			t.Helper()
			_, err := get(t, c, "ip-1")
			return err
		}},
		{"a call that fails", "compute.addresses.insert", func(t *testing.T, c *gcompute.AddressesClient) error {
			t.Helper()
			return insert(t, c, &computepb.Address{Name: new("ip-1")})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			faults := fault.New().Add(fault.Rule{Method: tc.method, Nth: 2, Fault: fault.LoseResponse()})
			c := newClient(t, compute.New(compute.WithFaults(faults)))
			if err := insert(t, c, &computepb.Address{Name: new("ip-1")}); err != nil {
				t.Fatal(err)
			}
			if _, err := get(t, c, "ip-1"); err != nil {
				t.Fatal(err)
			}
			if err := tc.call(t, c); err == nil || apiCode(err) != 0 {
				t.Fatalf("call = %v, want a transport error", err)
			}
		})
	}
}

func TestOperationFailedOnAReadFailsTheRead(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "compute.regionOperations.get", Nth: 1, Fault: fault.FailOperation(http.StatusForbidden)})
	s := compute.New(compute.WithFaults(faults))
	c := newClient(t, s)
	if err := insert(t, c, &computepb.Address{Name: new("ip-1")}); apiCode(err) != http.StatusForbidden {
		t.Fatalf("insert = %v, want the operation poll to fail with HTTP 403", err)
	}
	if _, ok := s.LookupAddress(project, region, "ip-1"); !ok {
		t.Fatal("the insert did not take effect")
	}
}

func TestRequestIDReplaysTheFirstOperation(t *testing.T) {
	faults := fault.New().Add(
		fault.Rule{Method: "compute.addresses.insert", Nth: 1, Fault: fault.LoseResponse()},
		fault.Rule{Method: "compute.addresses.delete", Nth: 1, Fault: fault.LoseResponse()},
	)
	s := compute.New(compute.WithFaults(faults))
	c := newClient(t, s)
	const insertID, deleteID = "5f0c3f1e-8a4e-4c39-9d0a-6f2b1f1c2d3e", "0b6e4b9a-2f61-4f0e-8a51-3c7d9e2f4a10"
	ins := &computepb.InsertAddressRequest{Project: project, Region: region, RequestId: new(insertID), AddressResource: &computepb.Address{Name: new("ip-1")}}
	if _, err := c.Insert(t.Context(), ins); err == nil {
		t.Fatal("the first insert did not lose its response")
	}
	op, err := c.Insert(t.Context(), ins)
	if err != nil {
		t.Fatalf("retrying the insert with its request ID: %v", err)
	}
	if err := op.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	del := &computepb.DeleteAddressRequest{Project: project, Region: region, Address: "ip-1", RequestId: new(deleteID)}
	if _, err := c.Delete(t.Context(), del); err == nil {
		t.Fatal("the first delete did not lose its response")
	}
	if _, err := c.Delete(t.Context(), del); err != nil {
		t.Fatalf("retrying the delete with its request ID: %v", err)
	}
	if got := s.Addresses(); len(got) != 0 {
		t.Fatalf("addresses = %v, want none", got)
	}
	ins.RequestId = new("not-a-uuid")
	if _, err := c.Insert(t.Context(), ins); apiCode(err) != http.StatusBadRequest {
		t.Fatalf("an insert with an invalid request ID = %v, want HTTP 400", err)
	}
}

// reserve is the pattern the stand-in exists to test: insert, and on a
// conflict adopt the address only if it carries this owner's label.
func reserve(t *testing.T, c *gcompute.AddressesClient, name, owner string) error {
	t.Helper()
	var last error
	for range 20 {
		err := insert(t, c, &computepb.Address{Name: new(name), Labels: map[string]string{"owner": owner}})
		if err == nil {
			return nil
		}
		last = err
		if apiCode(err) == http.StatusConflict {
			got, err := get(t, c, name)
			if err != nil {
				last = err
				continue
			}
			if got.GetLabels()["owner"] != owner {
				return fmt.Errorf("%s belongs to someone else", name)
			}
			return nil
		}
	}
	return last
}

func TestRetryingReserveConvergesUnderRandomFaults(t *testing.T) {
	for seed := range uint64(50) {
		faults := fault.New(fault.Seed(seed)).Random(0.4,
			fault.Unavailable(), fault.LoseResponse(), fault.FailAfter(http.StatusInternalServerError), fault.FailOperation(http.StatusInternalServerError))
		s := compute.New(compute.WithFaults(faults))
		c := newClient(t, s)
		if err := reserve(t, c, "ip-1", "me"); err != nil {
			t.Fatalf("seed %d: %v\ninjected: %v", seed, err, faults.Injected())
		}
		if got := s.Addresses(); len(got) != 1 || got[0].Labels["owner"] != "me" {
			t.Fatalf("seed %d: addresses = %v\ninjected: %v", seed, got, faults.Injected())
		}
	}
}

func TestConcurrentInserts(t *testing.T) {
	s := compute.New()
	c := newClient(t, s)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			op, err := c.Insert(t.Context(), &computepb.InsertAddressRequest{
				Project: project, Region: region, AddressResource: &computepb.Address{Name: new(fmt.Sprintf("ip-%d", i))},
			})
			if err == nil {
				err = op.Wait(t.Context())
			}
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, a := range s.Addresses() {
		if seen[a.Address] {
			t.Fatalf("IP %s handed out twice", a.Address)
		}
		seen[a.Address] = true
	}
	if len(seen) != 16 {
		t.Fatalf("%d addresses, want 16", len(seen))
	}
}
