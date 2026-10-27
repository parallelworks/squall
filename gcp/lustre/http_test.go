package lustre_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/lustre"
)

const instances = "/v1/projects/p/locations/us-central1-a/instances"

// do sends one REST call straight to s and decodes the JSON answer.
func do(t *testing.T, s *lustre.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: %v in %q", method, path, err, rec.Body)
	}
	return rec.Code, out
}

func body(capacity, throughput int, extra string) string {
	return fmt.Sprintf(`{"filesystem":"lfs","capacityGib":"%d","perUnitStorageThroughput":"%d","network":"projects/p/global/networks/default"%s}`, capacity, throughput, extra)
}

func create(t *testing.T, s *lustre.Server, id, body string) (int, map[string]any) {
	t.Helper()
	return do(t, s, http.MethodPost, instances+"?instanceId="+id, body)
}

func TestCreateChecksWhatTheAPIDocuments(t *testing.T) {
	for _, tc := range []struct {
		name, id, body string
		want           int
	}{
		{"smallest 1000 tier", "a", body(9_000, 1000, ""), http.StatusOK},
		{"1000 tier small step", "a", body(2_556_000, 1000, ""), http.StatusOK},
		{"1000 tier large step", "a", body(2_592_000, 1000, ""), http.StatusOK},
		{"1000 tier largest", "a", body(10_008_000, 1000, ""), http.StatusOK},
		{"1000 tier off step", "a", body(9_500, 1000, ""), http.StatusBadRequest},
		{"1000 tier small step above threshold", "a", body(2_565_000, 1000, ""), http.StatusBadRequest},
		{"1000 tier too large", "a", body(10_080_000, 1000, ""), http.StatusBadRequest},
		{"500 tier below minimum", "a", body(9_000, 500, ""), http.StatusBadRequest},
		{"500 tier", "a", body(36_000, 500, ""), http.StatusOK},
		{"250 tier", "a", body(36_000, 250, ""), http.StatusOK},
		{"125 tier", "a", body(144_000, 125, ""), http.StatusOK},
		{"125 tier off step", "a", body(108_000, 125, ""), http.StatusBadRequest},
		{"unknown throughput", "a", body(9_000, 2000, ""), http.StatusBadRequest},
		{"no throughput without the Dynamic tier", "a", body(472_000, 0, ""), http.StatusBadRequest},
		{"Dynamic tier", "a", body(944_000, 0, `,"dynamicTierOptions":{"mode":"DEFAULT_CACHE"}`), http.StatusOK},
		{"Dynamic tier as a number", "a", body(472_000, 0, `,"dynamicTierOptions":{"mode":2}`), http.StatusOK},
		{"Dynamic tier with throughput", "a", body(472_000, 125, `,"dynamicTierOptions":{"mode":"DEFAULT_CACHE"}`), http.StatusBadRequest},
		{"Dynamic tier off step", "a", body(500_000, 0, `,"dynamicTierOptions":{"mode":"DEFAULT_CACHE"}`), http.StatusBadRequest},
		{"no instance ID", "", body(9_000, 1000, ""), http.StatusBadRequest},
		{"instance ID with a capital", "Abc", body(9_000, 1000, ""), http.StatusBadRequest},
		{"instance ID starting with a digit", "1abc", body(9_000, 1000, ""), http.StatusBadRequest},
		{"instance ID ending with a hyphen", "abc-", body(9_000, 1000, ""), http.StatusBadRequest},
		{"instance ID of 63", strings.Repeat("a", 63), body(9_000, 1000, ""), http.StatusOK},
		{"instance ID of 64", strings.Repeat("a", 64), body(9_000, 1000, ""), http.StatusBadRequest},
		{"no capacity", "a", `{"filesystem":"lfs","perUnitStorageThroughput":"1000","network":"projects/p/global/networks/n"}`, http.StatusBadRequest},
		{"no filesystem", "a", `{"capacityGib":"9000","perUnitStorageThroughput":"1000","network":"projects/p/global/networks/n"}`, http.StatusBadRequest},
		{"filesystem of 9", "a", `{"filesystem":"lustrefs1","capacityGib":"9000","perUnitStorageThroughput":"1000","network":"projects/p/global/networks/n"}`, http.StatusBadRequest},
		{"filesystem with a hyphen", "a", `{"filesystem":"l-fs","capacityGib":"9000","perUnitStorageThroughput":"1000","network":"projects/p/global/networks/n"}`, http.StatusBadRequest},
		{"no network", "a", `{"filesystem":"lfs","capacityGib":"9000","perUnitStorageThroughput":"1000"}`, http.StatusBadRequest},
		{"short network", "a", `{"filesystem":"lfs","capacityGib":"9000","perUnitStorageThroughput":"1000","network":"default"}`, http.StatusBadRequest},
		{"bad label", "a", body(9_000, 1000, `,"labels":{"Owner":"x"}`), http.StatusBadRequest},
		{"unknown field", "a", body(9_000, 1000, `,"size":1`), http.StatusBadRequest},
		{"output-only fields are ignored", "a", body(9_000, 1000, `,"state":"ACTIVE","mountPoint":"x","uid":"u"`), http.StatusOK},
		{"root squash", "a", body(9_000, 1000, `,"accessRulesOptions":{"defaultSquashMode":"ROOT_SQUASH","defaultSquashUid":65534,"accessRules":[{"name":"admins","ipAddressRanges":["10.0.0.0/24"],"squashMode":"NO_SQUASH"}]}`), http.StatusOK},
		{"no squash with a uid", "a", body(9_000, 1000, `,"accessRulesOptions":{"defaultSquashMode":"NO_SQUASH","defaultSquashUid":65534}`), http.StatusBadRequest},
		{"no default squash mode", "a", body(9_000, 1000, `,"accessRulesOptions":{"accessRules":[]}`), http.StatusBadRequest},
		{"root squash inside a rule", "a", body(9_000, 1000, `,"accessRulesOptions":{"defaultSquashMode":1,"accessRules":[{"name":"x","ipAddressRanges":["10.0.0.1"],"squashMode":"ROOT_SQUASH"}]}`), http.StatusBadRequest},
		{"long rule name", "a", body(9_000, 1000, `,"accessRulesOptions":{"defaultSquashMode":1,"accessRules":[{"name":"a_rule_name_of_17","ipAddressRanges":["10.0.0.1"],"squashMode":1}]}`), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := create(t, lustre.New(), tc.id, tc.body)
			if code != tc.want {
				t.Fatalf("create = %d %v, want %d", code, out, tc.want)
			}
		})
	}
}

func TestErrorsCarryTheirStatus(t *testing.T) {
	s := lustre.New()
	create(t, s, "a", body(9_000, 1000, ""))
	code, out := create(t, s, "a", body(9_000, 1000, ""))
	if e := out["error"].(map[string]any); code != http.StatusConflict || e["status"] != "ALREADY_EXISTS" || e["code"] != 409.0 {
		t.Errorf("duplicate create = %d %v", code, out)
	}
	code, out = do(t, s, http.MethodGet, instances+"/missing", "")
	if e := out["error"].(map[string]any); code != http.StatusNotFound || e["status"] != "NOT_FOUND" {
		t.Errorf("get missing = %d %v", code, out)
	}
	if code, _ := do(t, s, http.MethodGet, instances+"?bogus=1", ""); code != http.StatusBadRequest {
		t.Errorf("list with an unknown parameter = %d, want 400", code)
	}
	if code, _ := do(t, s, http.MethodPost, instances+"/a:importData", "{}"); code != http.StatusNotFound {
		t.Errorf("an unimplemented method = %d, want 404", code)
	}
}

func TestEnumsAreNumbersWhenAskedFor(t *testing.T) {
	s := lustre.New()
	create(t, s, "a", body(9_000, 1000, ""))
	if _, out := do(t, s, http.MethodGet, instances+"/a", ""); out["state"] != "ACTIVE" {
		t.Errorf("state = %v, want ACTIVE", out["state"])
	}
	if _, out := do(t, s, http.MethodGet, instances+"/a?%24alt=json%3Benum-encoding%3Dint", ""); out["state"] != 1.0 {
		t.Errorf("state with enum-encoding=int = %v, want 1", out["state"])
	}
}

func TestRequestIDsReplayWithinProjectAndMethod(t *testing.T) {
	s := lustre.New()
	const id = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	_, first := create(t, s, "a&requestId="+id, body(9_000, 1000, ""))
	code, again := create(t, s, "a&requestId="+id, body(9_000, 1000, ""))
	if code != http.StatusOK || again["name"] != first["name"] {
		t.Fatalf("replayed create = %d %v, want the first operation %v", code, again, first)
	}
	code, del := do(t, s, http.MethodDelete, instances+"/a?requestId="+id, "")
	if code != http.StatusOK || del["name"] == first["name"] {
		t.Errorf("delete with the create's ID = %d %v, want a new operation", code, del)
	}
	if code, _ := create(t, s, "b&requestId=00000000-0000-0000-0000-000000000000", body(9_000, 1000, "")); code != http.StatusBadRequest {
		t.Errorf("create with the zero UUID = %d, want 400", code)
	}
}

func TestOperationsCarryTheResultOrTheError(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "lustre.projects.locations.instances.delete", Fault: fault.FailOperation(http.StatusInternalServerError)})
	s := lustre.New(lustre.WithFaults(faults))
	_, op := create(t, s, "a", body(9_000, 1000, `,"labels":{"owner":"me"}`))
	resp := op["response"].(map[string]any)
	meta := op["metadata"].(map[string]any)
	if op["done"] != true || resp["@type"] != "type.googleapis.com/google.cloud.lustre.v1.Instance" || resp["state"] != "ACTIVE" ||
		meta["verb"] != "create" || meta["target"] != "projects/p/locations/us-central1-a/instances/a" {
		t.Fatalf("create operation = %v", op)
	}
	if !strings.HasPrefix(resp["mountPoint"].(string), "10.") || !strings.HasSuffix(resp["mountPoint"].(string), "@tcp:/lfs") {
		t.Errorf("mountPoint = %v", resp["mountPoint"])
	}
	_, op = do(t, s, http.MethodDelete, instances+"/a", "")
	if e := op["error"].(map[string]any); op["done"] != true || e["code"] != 13.0 {
		t.Fatalf("failed delete operation = %v, want code 13", op)
	}
	if _, ok := s.LookupInstance("p", "us-central1-a", "a"); !ok {
		t.Error("a failed delete removed the instance")
	}
}

func TestSettleAfterHoldsChangesForReads(t *testing.T) {
	s := lustre.New(lustre.WithSettleAfter(2))
	_, op := create(t, s, "a", body(9_000, 1000, ""))
	opPath := "/v1/" + op["name"].(string)
	if op["done"] == true {
		t.Fatalf("create = %v, want in progress", op)
	}
	for range 2 {
		if _, inst := do(t, s, http.MethodGet, instances+"/a", ""); inst["state"] != "CREATING" || inst["mountPoint"] != nil {
			t.Fatalf("instance = %v, want CREATING without a mount point", inst)
		}
	}
	if _, op := do(t, s, http.MethodGet, opPath, ""); op["done"] != true {
		t.Fatalf("operation after 3 reads = %v, want done", op)
	}
	do(t, s, http.MethodDelete, instances+"/a", "")
	if _, inst := do(t, s, http.MethodGet, instances+"/a", ""); inst["state"] != "DELETING" {
		t.Fatalf("instance = %v, want DELETING", inst)
	}
	s.Settle()
	if code, _ := do(t, s, http.MethodGet, instances+"/a", ""); code != http.StatusNotFound {
		t.Errorf("get after Settle = %d, want 404", code)
	}
}

func TestPatchGrowsCapacityWithinItsStep(t *testing.T) {
	s := lustre.New()
	create(t, s, "a", body(18_000, 1000, ""))
	patch := func(mask, body string) int {
		code, _ := do(t, s, http.MethodPatch, instances+"/a?updateMask="+mask, body)
		return code
	}
	if code := patch("capacityGib", `{"capacityGib":"27000"}`); code != http.StatusOK {
		t.Errorf("growing by a step = %d", code)
	}
	if code := patch("capacityGib", `{"capacityGib":"18000"}`); code != http.StatusBadRequest {
		t.Errorf("shrinking = %d, want 400", code)
	}
	if code := patch("capacity_gib", `{"capacityGib":"2592000"}`); code != http.StatusBadRequest {
		t.Errorf("growing past the small step's threshold = %d, want 400", code)
	}
	if code := patch("labels,description", `{"labels":{"k":"v"},"description":"d"}`); code != http.StatusOK {
		t.Errorf("labels and description = %d", code)
	}
	if code := patch("network", `{"network":"projects/p/global/networks/other"}`); code != http.StatusBadRequest {
		t.Errorf("network = %d, want 400", code)
	}
	got, _ := s.LookupInstance("p", "us-central1-a", "a")
	if got.CapacityGib != 27_000 || got.Labels["k"] != "v" || got.Description != "d" || got.State != lustre.StateActive {
		t.Errorf("instance = %+v", got)
	}
}

func TestPatchWithoutAMaskChangesTheFieldsTheBodyHolds(t *testing.T) {
	s := lustre.New()
	create(t, s, "a", body(18_000, 1000, `,"labels":{"k":"v"}`))
	_, current := do(t, s, http.MethodGet, instances+"/a", "")
	current["description"] = "new"
	current["capacityGib"] = "27000"
	sent, _ := json.Marshal(current)
	if code, out := do(t, s, http.MethodPatch, instances+"/a", string(sent)); code != http.StatusOK {
		t.Fatalf("patch of the instance as read, output-only fields and all = %d %v", code, out)
	}
	got, _ := s.LookupInstance("p", "us-central1-a", "a")
	if got.Description != "new" || got.CapacityGib != 27_000 || got.Labels["k"] != "v" {
		t.Errorf("instance = %+v", got)
	}
	for _, body := range []string{
		`{"network":"projects/p/global/networks/other"}`,
		`{"filesystem":"other"}`,
		`{"perUnitStorageThroughput":"500"}`,
	} {
		if code, _ := do(t, s, http.MethodPatch, instances+"/a", body); code != http.StatusBadRequest {
			t.Errorf("patch %s = %d, want 400", body, code)
		}
	}
	if code, out := do(t, s, http.MethodPatch, instances+"/a", `{"network":"projects/p/global/networks/default","perUnitStorageThroughput":1000,"gkeSupportEnabled":true}`); code != http.StatusOK {
		t.Errorf("patch repeating immutable fields = %d %v", code, out)
	}
}

func TestPatchKeepsTheInstancesState(t *testing.T) {
	s := lustre.New()
	in := lustre.Instance{Filesystem: "lfs", CapacityGib: 9_000, PerUnitStorageThroughput: 1000, Network: "projects/p/global/networks/n"}
	for id, state := range map[string]string{"stopped": lustre.StateStopped, "creating": lustre.StateCreating} {
		in.State = state
		if _, err := s.PutInstance("p", "us-central1-a", id, in); err != nil {
			t.Fatal(err)
		}
		if code, out := do(t, s, http.MethodPatch, instances+"/"+id+"?updateMask=description", `{"description":"d"}`); code != http.StatusOK {
			t.Fatalf("patch %s = %d %v", id, code, out)
		}
		got, _ := s.LookupInstance("p", "us-central1-a", id)
		if got.State != state || got.Description != "d" || state == lustre.StateCreating && got.MountPoint != "" {
			t.Errorf("patched %s = %+v, want %s", id, got, state)
		}
	}
}

func TestOptionsRejectWhatTheAPIDoesNotTake(t *testing.T) {
	for extra, want := range map[string]int{
		`,"dynamicTierOptions":{"mode":"DISABLED","size":1}`:                                                 http.StatusBadRequest,
		`,"maintenancePolicy":{"weeklyMaintenanceWindows":[{"dayOfWeek":"MONDAY","startTime":{"hours":2}}]}`: http.StatusOK,
		`,"maintenancePolicy":{"weeklyMaintenanceWindows":[{"dayOfWeek":1,"startTime":{}}],"maintenanceExclusionWindow":[{"startDate":{"year":2026,"month":1,"day":1},"endDate":{"month":12,"day":24},"time":{}}]}`: http.StatusOK,
		`,"maintenancePolicy":{"weeklyMaintenanceWindows":[{"dayOfWeek":"MONDAY","startTime":{}},{"dayOfWeek":"TUESDAY","startTime":{}}]}`:                                                                          http.StatusBadRequest,
		`,"maintenancePolicy":{"weeklyMaintenanceWindows":[{"dayOfWeek":"MONDAY","startTime":{"hours":24}}]}`:                                                                                                       http.StatusBadRequest,
		`,"maintenancePolicy":{"weeklyMaintenanceWindows":[{"dayOfWeek":"MONDAY","startTime":{}}],"maintenanceExclusionWindow":[{"startDate":{"year":2026,"month":1,"day":1}}]}`:                                    http.StatusBadRequest,
		`,"upcomingMaintenanceSchedule":{"bogus":1}`:                                                                                             http.StatusBadRequest,
		`,"upcomingMaintenanceSchedule":{"startTime":"2026-01-01T00:00:00Z"}`:                                                                    http.StatusOK,
		`,"maintenancePolicy":{"weeklyMaintenanceWindows":[{"dayOfWeek":"MONDAY","duration":"1h"}]}`:                                             http.StatusBadRequest,
		`,"accessRulesOptions":{"defaultSquashMode":"ROOT_SQUASH","defaultSquashUid":"65534","defaultSquashGid":"65534"}`:                        http.StatusOK,
		`,"accessRulesOptions":{"defaultSquashMode":"ROOT_SQUASH","defaultSquashUid":"4294967296"}`:                                              http.StatusBadRequest,
		`,"accessRulesOptions":{"defaultSquashMode":2,"accessRules":[{"name":"a","ipAddressRanges":["10.0.0.0/24","10.1.0.1"],"squashMode":1}]}`: http.StatusOK,
		`,"accessRulesOptions":{"defaultSquashMode":2,"accessRules":[{"name":"a","ipAddressRanges":["not-an-ip"],"squashMode":1}]}`:              http.StatusBadRequest,
		`,"accessRulesOptions":{"defaultSquashMode":2,"accessRules":[{"name":"a","ipAddressRanges":["10.0.0.0/16","10.0.3.4"],"squashMode":1}]}`: http.StatusBadRequest,
	} {
		if code, out := create(t, lustre.New(), "a", body(9_000, 1000, extra)); code != want {
			t.Errorf("create with %s = %d %v, want %d", extra, code, out, want)
		}
	}
}

func TestListCountsReadsOnlyOfWhatItReturns(t *testing.T) {
	s := lustre.New(lustre.WithSettleAfter(1))
	create(t, s, "a", body(9_000, 1000, ""))
	create(t, s, "b", body(9_000, 1000, ""))
	do(t, s, http.MethodGet, instances+"?pageSize=1", "")
	do(t, s, http.MethodGet, instances+"?filter=labels.none%3Dx", "")
	// Each first get is the instance's one read in progress, unless a list spent it.
	for _, id := range []string{"a", "b"} {
		want := "CREATING"
		if id == "a" {
			want = "ACTIVE"
		}
		if _, got := do(t, s, http.MethodGet, instances+"/"+id, ""); got["state"] != want {
			t.Errorf("%s after the lists = %v, want %s", id, got["state"], want)
		}
	}
}

func TestListDropsAnInstanceItsReadFinishesDeleting(t *testing.T) {
	s := lustre.New(lustre.WithSettleAfter(1))
	create(t, s, "a", body(9_000, 1000, ""))
	s.Settle()
	do(t, s, http.MethodDelete, instances+"/a", "")
	if _, got := do(t, s, http.MethodGet, instances, ""); got["instances"] == nil {
		t.Fatalf("first list = %v, want the instance DELETING", got)
	}
	if _, got := do(t, s, http.MethodGet, instances, ""); got["instances"] != nil {
		t.Errorf("second list = %v, want none", got)
	}
}

func TestFailOperationOnAnOperationDeleteRejectsIt(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "lustre.projects.locations.operations.delete", Fault: fault.FailOperation(http.StatusInternalServerError)})
	s := lustre.New(lustre.WithFaults(faults))
	_, op := create(t, s, "a", body(9_000, 1000, ""))
	name := "/v1/" + op["name"].(string)
	if code, _ := do(t, s, http.MethodDelete, name, ""); code != http.StatusInternalServerError {
		t.Errorf("delete operation = %d, want 500", code)
	}
	if code, _ := do(t, s, http.MethodGet, name, ""); code != http.StatusOK {
		t.Errorf("get operation after a failed delete = %d, want 200", code)
	}
}

func TestOperationsListDeleteAndCancel(t *testing.T) {
	s := lustre.New(lustre.WithSettleAfter(-1))
	_, op := create(t, s, "a", body(9_000, 1000, ""))
	create(t, s, "b", body(9_000, 1000, ""))
	name := op["name"].(string)
	if code, _ := do(t, s, http.MethodPost, "/v1/"+name+":cancel", ""); code != http.StatusOK {
		t.Fatalf("cancel = %d", code)
	}
	_, list := do(t, s, http.MethodGet, "/v1/projects/p/locations/-/operations?pageSize=1", "")
	ops := list["operations"].([]any)
	if len(ops) != 1 || list["nextPageToken"] == nil {
		t.Fatalf("first page = %v", list)
	}
	_, got := do(t, s, http.MethodGet, "/v1/"+name, "")
	if meta := got["metadata"].(map[string]any); meta["requestedCancellation"] != true || got["done"] == true {
		t.Errorf("canceled operation = %v, want requestedCancellation and still running", got)
	}
	if code, _ := do(t, s, http.MethodDelete, "/v1/"+name, ""); code != http.StatusOK {
		t.Fatalf("delete operation = %d", code)
	}
	if code, _ := do(t, s, http.MethodGet, "/v1/"+name, ""); code != http.StatusNotFound {
		t.Errorf("get deleted operation = %d, want 404", code)
	}
	s.Settle()
	if inst, _ := s.LookupInstance("p", "us-central1-a", "a"); inst.State != lustre.StateActive {
		t.Errorf("instance after its operation was deleted and Settle = %+v, want ACTIVE", inst)
	}
}

func TestAFaultRuleCanTargetTheCreateOfOneInstance(t *testing.T) {
	faults := fault.New().Add(fault.Rule{
		Method: "lustre.projects.locations.instances.create", Resource: "projects/p/locations/us-central1-a/instances/target", Fault: fault.Unavailable(),
	})
	s := lustre.New(lustre.WithFaults(faults))
	if code, _ := create(t, s, "other", body(9_000, 1000, "")); code != http.StatusOK {
		t.Errorf("create of another instance = %d", code)
	}
	if code, out := create(t, s, "target", body(9_000, 1000, "")); code != http.StatusServiceUnavailable || out["error"].(map[string]any)["status"] != "UNAVAILABLE" {
		t.Errorf("create of the targeted instance = %d %v, want 503", code, out)
	}
}

func TestPutInstanceSetsUpState(t *testing.T) {
	s := lustre.New()
	put, err := s.PutInstance("p", "us-central1-a", "left", lustre.Instance{
		Filesystem: "lfs", CapacityGib: 9_000, PerUnitStorageThroughput: 1000, Network: "projects/p/global/networks/n",
		Labels: map[string]string{"run": "old"}, State: lustre.StateStopped,
	})
	if err != nil {
		t.Fatal(err)
	}
	if put.Name != "projects/p/locations/us-central1-a/instances/left" || put.State != lustre.StateStopped || put.MountPoint == "" || put.UID == "" {
		t.Errorf("PutInstance = %+v", put)
	}
	if _, err := s.PutInstance("p", "z", "bad", lustre.Instance{Filesystem: "lfs", CapacityGib: 1, PerUnitStorageThroughput: 1000, Network: "projects/p/global/networks/n"}); err == nil {
		t.Error("PutInstance took a capacity off its tier")
	}
	_, list := do(t, s, http.MethodGet, "/v1/projects/p/locations/-/instances?filter=labels.run%3Dold", "")
	if items := list["instances"].([]any); len(items) != 1 {
		t.Errorf("list = %v", list)
	}
}

func TestPatchIgnoresUnchangedOptionsHoweverTheyAreSpelled(t *testing.T) {
	s := lustre.New()
	const options = `,"accessRulesOptions":{"defaultSquashMode":"ROOT_SQUASH","defaultSquashUid":"65534","defaultSquashGid":0,"accessRules":[]},` +
		`"maintenancePolicy":{"weeklyMaintenanceWindows":[{"dayOfWeek":"MONDAY","startTime":{"hours":0,"minutes":30}}]}`
	create(t, s, "a", body(9_000, 1000, options))
	patch := `{"description":"d","accessRulesOptions":{"defaultSquashMode":2,"defaultSquashUid":65534},` +
		`"maintenancePolicy":{"weeklyMaintenanceWindows":[{"dayOfWeek":1,"startTime":{"minutes":30}}]}}`
	if code, out := do(t, s, http.MethodPatch, instances+"/a", patch); code != http.StatusOK {
		t.Fatalf("patch spelling the options as numbers and leaving out zeros = %d %v", code, out)
	}
	if code, _ := do(t, s, http.MethodPatch, instances+"/a", `{"accessRulesOptions":{"defaultSquashMode":1}}`); code != http.StatusBadRequest {
		t.Errorf("patch changing the squash mode = %d, want 400", code)
	}
	_, got := do(t, s, http.MethodGet, instances+"/a?%24alt=json%3Benum-encoding%3Dint", "")
	rules := got["accessRulesOptions"].(map[string]any)
	day := got["maintenancePolicy"].(map[string]any)["weeklyMaintenanceWindows"].([]any)[0].(map[string]any)["dayOfWeek"]
	if rules["defaultSquashMode"] != 2.0 || day != 1.0 || got["state"] != 1.0 {
		t.Errorf("enums as numbers = %v, day %v", rules, day)
	}
	_, got = do(t, s, http.MethodGet, instances+"/a", "")
	if got["accessRulesOptions"].(map[string]any)["defaultSquashMode"] != "ROOT_SQUASH" {
		t.Errorf("enums as names = %v", got["accessRulesOptions"])
	}
}

func TestPutInstanceChecksItsMaintenancePolicy(t *testing.T) {
	s := lustre.New()
	in := lustre.Instance{
		Filesystem: "lfs", CapacityGib: 9_000, PerUnitStorageThroughput: 1000, Network: "projects/p/global/networks/n",
		MaintenancePolicy: &lustre.MaintenancePolicy{},
	}
	if _, err := s.PutInstance("p", "us-central1-a", "a", in); err == nil {
		t.Error("PutInstance took a maintenance policy with no weekly window")
	}
	in.MaintenancePolicy.WeeklyMaintenanceWindows = []lustre.WeeklyMaintenanceWindow{{DayOfWeek: "FRIDAY", StartTime: &lustre.TimeOfDay{Hours: 3}}}
	if _, err := s.PutInstance("p", "us-central1-a", "a", in); err != nil {
		t.Fatal(err)
	}
	if code, out := do(t, s, http.MethodGet, instances+"/a", ""); code != http.StatusOK || out["maintenancePolicy"] == nil {
		t.Errorf("get = %d %v", code, out)
	}
}

func TestUpdateMaskPaths(t *testing.T) {
	s := lustre.New()
	create(t, s, "a", body(18_000, 1000, ""))
	_, current := do(t, s, http.MethodGet, instances+"/a", "")
	for _, tc := range []struct {
		mask, body string
		want       int
	}{
		{"updateTime,description", `{"updateTime":"2020-01-01T00:00:00Z","description":"x"}`, http.StatusOK},
		{"update_time", `{}`, http.StatusOK},
		{"network", `{"network":"projects/p/global/networks/default"}`, http.StatusOK},
		{"network", `{"network":"projects/p/global/networks/other"}`, http.StatusBadRequest},
		{"filesystem", `{}`, http.StatusBadRequest},
		{"labels.k", `{"labels":{"k":"v"}}`, http.StatusBadRequest},
		{"bogus", `{}`, http.StatusBadRequest},
		{"description", `{"maintenancePolicy":{"bogus":1}}`, http.StatusBadRequest},
		{"description", `{"accessRulesOptions":{"x":1}}`, http.StatusBadRequest},
		{"*", `{"description":"y"}`, http.StatusBadRequest},
	} {
		if code, out := do(t, s, http.MethodPatch, instances+"/a?updateMask="+tc.mask, tc.body); code != tc.want {
			t.Errorf("patch %s %s = %d %v, want %d", tc.mask, tc.body, code, out, tc.want)
		}
	}
	current["description"] = "all"
	current["labels"] = map[string]any{"k": "v"}
	sent, _ := json.Marshal(current)
	if code, out := do(t, s, http.MethodPatch, instances+"/a?updateMask=*", string(sent)); code != http.StatusOK {
		t.Fatalf("patch * of the whole instance = %d %v", code, out)
	}
	got, _ := s.LookupInstance("p", "us-central1-a", "a")
	if got.Description != "all" || got.Labels["k"] != "v" || got.CapacityGib != 18_000 {
		t.Errorf("instance = %+v", got)
	}
}

func TestPatchWithAnEmptyBodyChangesNothing(t *testing.T) {
	s := lustre.New()
	create(t, s, "a", body(9_000, 1000, `,"description":"d"`))
	if code, out := do(t, s, http.MethodPatch, instances+"/a", ""); code != http.StatusOK {
		t.Fatalf("patch with no body = %d %v", code, out)
	}
	if got, _ := s.LookupInstance("p", "us-central1-a", "a"); got.Description != "d" {
		t.Errorf("instance = %+v", got)
	}
}

func TestStandardParametersAreAccepted(t *testing.T) {
	s := lustre.New()
	create(t, s, "a", body(9_000, 1000, ""))
	for _, q := range []string{"oauth_token=t", "access_token=t", "%24.xgafv=2", "callback=f", "upload_protocol=raw", "uploadType=media", "key=k", "quotaUser=u", "prettyPrint=false", "fields=name", "alt=json"} {
		if code, out := do(t, s, http.MethodGet, instances+"/a?"+q, ""); code != http.StatusOK {
			t.Errorf("get with %s = %d %v", q, code, out)
		}
	}
}

func TestListPagesInDescendingOrder(t *testing.T) {
	s := lustre.New()
	for _, id := range []string{"a", "b", "c"} {
		create(t, s, id, body(9_000, 1000, ""))
	}
	var got []string
	token := ""
	for range 4 {
		_, page := do(t, s, http.MethodGet, instances+"?orderBy=name+desc&pageSize=1&pageToken="+token, "")
		for _, item := range page["instances"].([]any) {
			got = append(got, strings.TrimPrefix(item.(map[string]any)["name"].(string), "projects/p/locations/us-central1-a/instances/"))
		}
		next, _ := page["nextPageToken"].(string)
		if next == "" {
			break
		}
		token = next
	}
	if strings.Join(got, ",") != "c,b,a" {
		t.Errorf("pages = %v, want c,b,a", got)
	}
}
