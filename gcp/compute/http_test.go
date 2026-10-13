package compute_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/compute"
)

// do sends one REST call straight to s and decodes the JSON answer.
func do(t *testing.T, s *compute.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: %v in %q", method, path, err, rec.Body)
	}
	return rec.Code, out
}

func addresses(project, region string) string {
	return fmt.Sprintf("/compute/v1/projects/%s/regions/%s/addresses", project, region)
}

func TestExternalAddressesAreUniqueEverywhere(t *testing.T) {
	s := compute.New()
	if code, body := do(t, s, http.MethodPost, addresses("p", "us-east1"), `{"name":"a"}`); code != http.StatusOK {
		t.Fatalf("insert = %d %v", code, body)
	}
	a, _ := s.LookupAddress("p", "us-east1", "a")
	for _, where := range [][2]string{{"p", "europe-west1"}, {"q", "us-east1"}} {
		code, _ := do(t, s, http.MethodPost, addresses(where[0], where[1]), `{"name":"b","address":"`+a.Address+`"}`)
		if code != http.StatusBadRequest {
			t.Errorf("taking %s in %v = %d, want 400", a.Address, where, code)
		}
	}
	// Internal addresses only need to be unique where they live.
	for _, region := range []string{"us-east1", "europe-west1"} {
		code, body := do(t, s, http.MethodPost, addresses("p", region), `{"name":"i","addressType":"INTERNAL","address":"10.1.2.3"}`)
		if code != http.StatusOK {
			t.Errorf("internal 10.1.2.3 in %s = %d %v", region, code, body)
		}
	}
	// Allocation skips an address in use in another region.
	code, _ := do(t, s, http.MethodPost, addresses("q", "asia-east1"), `{"name":"c"}`)
	if c, _ := s.LookupAddress("q", "asia-east1", "c"); code != http.StatusOK || c.Address == a.Address {
		t.Errorf("allocated %q in another region, %d", c.Address, code)
	}
}

func TestRequestIDsReplayWithinProjectAndMethod(t *testing.T) {
	s := compute.New()
	const id = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	first, op := do(t, s, http.MethodPost, addresses("p", "r")+"?requestId="+id, `{"name":"a"}`)
	again, replay := do(t, s, http.MethodPost, addresses("p", "r")+"?requestId="+id, `{"name":"a"}`)
	if first != http.StatusOK || again != http.StatusOK || op["name"] != replay["name"] {
		t.Fatalf("replayed insert = %d %v, want the first operation %v", again, replay, op)
	}
	// The same ID in another project, or for another method, is another request.
	if code, _ := do(t, s, http.MethodPost, addresses("q", "r")+"?requestId="+id, `{"name":"a"}`); code != http.StatusOK {
		t.Errorf("insert in another project = %d", code)
	}
	code, del := do(t, s, http.MethodDelete, addresses("p", "r")+"/a?requestId="+id, "")
	if code != http.StatusOK || del["operationType"] != "delete" {
		t.Errorf("delete with the insert's ID = %d %v, want a delete", code, del)
	}
}

func TestOperationErrorsUseTheAPIsCodes(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "compute.addresses.insert", Fault: fault.FailOperation(http.StatusForbidden)})
	s := compute.New(compute.WithFaults(faults))
	_, op := do(t, s, http.MethodPost, addresses("p", "r"), `{"name":"a"}`)
	errs := op["error"].(map[string]any)["errors"].([]any)
	if code := errs[0].(map[string]any)["code"]; code != "BACKEND_ERROR" || op["httpErrorMessage"] != "FORBIDDEN" {
		t.Fatalf("operation = %v", op)
	}
}

func TestOldOperationsAreDropped(t *testing.T) {
	s := compute.New()
	_, first := do(t, s, http.MethodPost, addresses("p", "r"), `{"name":"keep"}`)
	for i := range 5000 {
		do(t, s, http.MethodPost, addresses("p", "r"), fmt.Sprintf(`{"name":"a%d"}`, i))
		do(t, s, http.MethodDelete, addresses("p", "r")+fmt.Sprintf("/a%d", i), "")
	}
	if code, _ := do(t, s, http.MethodGet, "/compute/v1/projects/p/regions/r/operations/"+first["name"].(string), ""); code != http.StatusNotFound {
		t.Fatalf("the oldest operation = %d, want 404 once 10,000 newer ones exist", code)
	}
	if _, ok := s.LookupAddress("p", "r", "keep"); !ok {
		t.Fatal("dropping an operation dropped its address")
	}
}

func TestAFaultRuleCanTargetTheInsertOfOneName(t *testing.T) {
	faults := fault.New().Add(fault.Rule{
		Method: "compute.addresses.insert", Resource: "projects/p/regions/r/addresses/target", Fault: fault.Unavailable(),
	})
	s := compute.New(compute.WithFaults(faults))
	if code, _ := do(t, s, http.MethodPost, addresses("p", "r"), `{"name":"other"}`); code != http.StatusOK {
		t.Errorf("insert of another name = %d", code)
	}
	if code, _ := do(t, s, http.MethodPost, addresses("p", "r"), `{"name":"target"}`); code != http.StatusServiceUnavailable {
		t.Errorf("insert of the targeted name = %d, want 503", code)
	}
}
