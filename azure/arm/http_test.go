package arm_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/parallelworks/squall/azure/arm"
	"github.com/parallelworks/squall/fault"
)

const groupPath = "/subscriptions/s/resourceGroups/rg"

// do sends one REST call straight to s with a bearer token and decodes the JSON answer, if any.
func do(t *testing.T, s *arm.Server, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	if !strings.Contains(path, "api-version=") {
		path += "?api-version=2024-01-01"
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: %v in %q", method, path, err, rec.Body)
		}
	}
	return rec, out
}

func errorCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func newGroup(t *testing.T, s *arm.Server) {
	t.Helper()
	if rec, body := do(t, s, http.MethodPut, groupPath, `{"location":"eastus"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create group = %d %v", rec.Code, body)
	}
}

func TestRequestsNeedATokenAndAnAPIVersion(t *testing.T) {
	s := arm.New()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, groupPath+"?api-version=1", nil))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "AuthenticationFailed") {
		t.Errorf("no token = %d %s", rec.Code, rec.Body)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, groupPath, nil)
	req.Header.Set("Authorization", "Bearer token")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "MissingApiVersionParameter") {
		t.Errorf("no api-version = %d %s", rec.Code, rec.Body)
	}
}

func TestUnservedCallsAnswer404(t *testing.T) {
	s := arm.New()
	newGroup(t, s)
	if rec, body := do(t, s, http.MethodGet, groupPath+"/providers/Microsoft.Storage/storageAccounts/a", ""); rec.Code != http.StatusNotFound {
		t.Errorf("an unserved type = %d %v", rec.Code, body)
	}
	if rec, body := do(t, s, http.MethodPatch, groupPath+"/providers/Microsoft.Compute/virtualMachines/vm", "{}"); rec.Code != http.StatusNotFound {
		t.Errorf("an unserved method = %d %v", rec.Code, body)
	}
}

func TestUnknownMembersAreRejected(t *testing.T) {
	s := arm.New()
	newGroup(t, s)
	for path, body := range map[string]string{
		groupPath: `{"location":"eastus","color":"red"}`,
		groupPath + "/providers/Microsoft.Network/networkSecurityGroups/n": `{"location":"eastus","properties":{"color":"red"}}`,
		groupPath + "/providers/Microsoft.Network/networkInterfaces/n":     `{"location":"eastus","properties":{"ipConfigurations":[{"name":"ip","properties":{"subnet":{"id":"s"},"color":"red"}}]}}`,
	} {
		rec, out := do(t, s, http.MethodPut, path, body)
		if rec.Code != http.StatusBadRequest || errorCode(out) != "InvalidRequestContent" || !strings.Contains(fmt.Sprint(out), "color") {
			t.Errorf("PUT %s %s = %d %v", path, body, rec.Code, out)
		}
	}
}

func TestOperationStatus(t *testing.T) {
	s := arm.New(arm.WithSettleAfter(-1))
	newGroup(t, s)
	rec, body := do(t, s, http.MethodPut, groupPath+"/providers/Microsoft.Network/networkSecurityGroups/n", `{"location":"eastus"}`)
	async := rec.Header().Get("Azure-AsyncOperation")
	if rec.Code != http.StatusCreated || !strings.HasPrefix(async, "http://example.com/subscriptions/s/providers/Microsoft.Network/locations/eastus/operations/") ||
		rec.Header().Get("retry-after-ms") == "" || body["properties"].(map[string]any)["provisioningState"] != "Creating" {
		t.Fatalf("create = %d %v %v", rec.Code, rec.Header(), body)
	}
	u, _ := url.Parse(async)
	rec, op := do(t, s, http.MethodGet, u.RequestURI(), "")
	if op["status"] != "InProgress" || rec.Header().Get("retry-after-ms") == "" {
		t.Fatalf("operation in progress = %v", op)
	}
	s.Settle()
	if _, op = do(t, s, http.MethodGet, u.RequestURI(), ""); op["status"] != "Succeeded" || op["endTime"] == nil {
		t.Fatalf("operation done = %v", op)
	}
	if rec, _ := do(t, s, http.MethodGet, strings.Replace(u.RequestURI(), "/operations/", "/operationResults/", 1), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("operation result = %d", rec.Code)
	}
	if rec, _ := do(t, s, http.MethodGet, strings.Replace(u.RequestURI(), "eastus", "westus", 1), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("operation at another path = %d", rec.Code)
	}
}

func TestOldOperationsAreDropped(t *testing.T) {
	s := arm.New(arm.WithSettleAfter(-1))
	newGroup(t, s)
	const path = groupPath + "/providers/Microsoft.Network/networkSecurityGroups/n"
	rec, _ := do(t, s, http.MethodPut, path, `{"location":"eastus"}`)
	first, _ := url.Parse(rec.Header().Get("Azure-AsyncOperation"))
	for i := range 10_000 {
		do(t, s, http.MethodPut, fmt.Sprintf("%s-%d", path, i), `{"location":"eastus"}`)
	}
	if rec, _ := do(t, s, http.MethodGet, first.RequestURI(), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("the oldest operation = %d, want 404 once 10,000 newer ones exist", rec.Code)
	}
}

func TestFailOperationRejectsACallWithNoOperation(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "Microsoft.Resources/subscriptions/resourceGroups/write", Fault: fault.FailOperation(http.StatusConflict)})
	s := arm.New(arm.WithFaults(faults))
	rec, body := do(t, s, http.MethodPut, groupPath, `{"location":"eastus"}`)
	if rec.Code != http.StatusConflict || errorCode(body) != "Conflict" || len(s.ResourceGroups()) != 0 {
		t.Fatalf("create group = %d %v", rec.Code, body)
	}
}

func TestFaultReasonIsTheErrorCode(t *testing.T) {
	faults := fault.New().Add(
		fault.Rule{Method: "Microsoft.Compute/virtualMachines/read", Fault: fault.Reject(http.StatusConflict, "OperationNotAllowed")},
		fault.Rule{Method: "Microsoft.Compute/virtualMachineScaleSets/read", Fault: fault.RateLimited()},
		fault.Rule{Method: "Microsoft.Network/*", Fault: fault.Unavailable()},
	)
	s := arm.New(arm.WithFaults(faults))
	for path, want := range map[string]string{
		groupPath + "/providers/Microsoft.Compute/virtualMachines/vm":        "OperationNotAllowed",
		groupPath + "/providers/Microsoft.Compute/virtualMachineScaleSets/v": "SubscriptionRequestsThrottled",
		groupPath + "/providers/Microsoft.Network/publicIPAddresses/p":       "ServiceUnavailable",
	} {
		if _, body := do(t, s, http.MethodGet, path, ""); errorCode(body) != want {
			t.Errorf("GET %s = %v, want %s", path, body, want)
		}
	}
}

func TestPrivateDNSChangesInProgressAnswer202(t *testing.T) {
	s := arm.New(arm.WithSettleAfter(-1))
	newGroup(t, s)
	rec, body := do(t, s, http.MethodPut, groupPath+"/providers/Microsoft.Network/privateDnsZones/example.internal", `{"location":"global"}`)
	async := rec.Header().Get("Azure-AsyncOperation")
	if rec.Code != http.StatusAccepted || len(body) != 0 || rec.Header().Get("Location") == "" || !strings.Contains(async, "/privateDnsOperationStatuses/") {
		t.Fatalf("create zone = %d %v %v", rec.Code, rec.Header(), body)
	}
	u, _ := url.Parse(async)
	if rec, op := do(t, s, http.MethodGet, u.RequestURI(), ""); rec.Code != http.StatusAccepted || op["status"] != "InProgress" {
		t.Fatalf("operation in progress = %d %v", rec.Code, op)
	}
	s.Settle()
	if rec, op := do(t, s, http.MethodGet, u.RequestURI(), ""); rec.Code != http.StatusOK || op["status"] != "Succeeded" {
		t.Fatalf("operation done = %d %v", rec.Code, op)
	}
}
