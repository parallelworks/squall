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

// A workspace change in progress answers 202 with no body and both polling
// URLs on one path, the Location URL adding type=location, which answers 202
// until the change is done.
func TestWorkspaceChangesInProgressAnswer202(t *testing.T) {
	s := arm.New(arm.WithSettleAfter(-1))
	newGroup(t, s)
	rec, _ := do(t, s, http.MethodPut, groupPath+"/providers/Microsoft.MachineLearningServices/workspaces/ws", `{"location":"eastus"}`)
	status, location := rec.Header().Get("Azure-AsyncOperation"), rec.Header().Get("Location")
	if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 ||
		!strings.Contains(status, "/providers/Microsoft.MachineLearningServices/locations/eastus/workspaceOperationsStatus/") ||
		location != status+"&type=location" {
		t.Fatalf("create = %d %q %q %q", rec.Code, rec.Body, status, location)
	}
	u, _ := url.Parse(location)
	if rec, _ := do(t, s, http.MethodGet, u.RequestURI(), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("Location poll in progress = %d", rec.Code)
	}
	s.Settle()
	if rec, body := do(t, s, http.MethodGet, u.RequestURI(), ""); rec.Code != http.StatusOK || body["name"] != "ws" {
		t.Fatalf("Location poll once done = %d %v", rec.Code, body)
	}
}

// A subnet answers as Azure's do: 201 then 200 with an Azure-AsyncOperation
// URL under its virtual network's location, no location of its own, and a
// place in the virtual network's subnets; a NAT gateway lists the subnets
// that use it, and its deletion answers 202 with both polling URLs.
func TestSubnetAndNATGatewayShapes(t *testing.T) {
	s := arm.New(arm.WithSettleAfter(-1))
	newGroup(t, s)
	const (
		network = groupPath + "/providers/Microsoft.Network"
		vnet    = network + "/virtualNetworks/vnet"
		subnet  = vnet + "/subnets/default"
		nat     = network + "/natGateways/nat"
	)
	rec, body := do(t, s, http.MethodPut, vnet, `{"location":"westus","properties":{"addressSpace":{"addressPrefixes":["10.0.0.0/16"]}}}`)
	props, _ := body["properties"].(map[string]any)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Header().Get("Azure-AsyncOperation"), "/providers/Microsoft.Network/locations/westus/operations/") ||
		fmt.Sprint(props["subnets"]) != "[]" || fmt.Sprint(props["virtualNetworkPeerings"]) != "[]" || props["enableDdosProtection"] != false ||
		props["privateEndpointVNetPolicies"] != "Disabled" {
		t.Fatalf("create vnet = %d %v %v", rec.Code, rec.Header(), body)
	}
	rec, body = do(t, s, http.MethodPut, nat, `{"location":"westus","sku":{"name":"Standard"}}`)
	if rec.Code != http.StatusCreated || fmt.Sprint(body["sku"]) != "map[name:Standard tier:Regional]" {
		t.Fatalf("create NAT gateway = %d %v", rec.Code, body)
	}
	for i, want := range []int{http.StatusCreated, http.StatusOK} {
		s.Settle()
		rec, body = do(t, s, http.MethodPut, subnet, `{"properties":{"addressPrefix":"10.0.0.0/24","natGateway":{"id":"`+nat+`"}}}`)
		props, _ = body["properties"].(map[string]any)
		if rec.Code != want || !strings.Contains(rec.Header().Get("Azure-AsyncOperation"), "/locations/westus/operations/") ||
			body["type"] != "Microsoft.Network/virtualNetworks/subnets" || body["location"] != nil || fmt.Sprint(props["natGateway"]) != "map[id:"+nat+"]" ||
			fmt.Sprint(props["delegations"]) != "[]" || props["privateLinkServiceNetworkPolicies"] != "Enabled" {
			t.Fatalf("put subnet %d = %d %v %v", i, rec.Code, rec.Header(), body)
		}
	}
	s.Settle()
	_, body = do(t, s, http.MethodGet, vnet, "")
	subnets, _ := body["properties"].(map[string]any)["subnets"].([]any)
	if len(subnets) != 1 || subnets[0].(map[string]any)["id"] != subnet {
		t.Fatalf("vnet subnets = %v", body)
	}
	_, body = do(t, s, http.MethodGet, nat, "")
	if users := body["properties"].(map[string]any)["subnets"]; fmt.Sprint(users) != "[map[id:"+subnet+"]]" {
		t.Fatalf("NAT gateway subnets = %v", body)
	}
	if rec, body := do(t, s, http.MethodDelete, nat, ""); rec.Code != http.StatusBadRequest || errorCode(body) != "InUseNatGatewayCannotBeDeleted" {
		t.Fatalf("delete NAT gateway in use = %d %v", rec.Code, body)
	}
	do(t, s, http.MethodPut, subnet, `{"properties":{"addressPrefix":"10.0.0.0/24"}}`)
	s.Settle()
	rec, _ = do(t, s, http.MethodDelete, nat, "")
	if rec.Code != http.StatusAccepted || rec.Header().Get("Azure-AsyncOperation") == "" ||
		!strings.Contains(rec.Header().Get("Location"), "/providers/Microsoft.Network/locations/westus/operationResults/") {
		t.Fatalf("delete NAT gateway = %d %v", rec.Code, rec.Header())
	}
}

// A load balancer answers as Azure's does: 201 then 200 with an
// Azure-AsyncOperation URL, its frontend IP configurations, backend pools and
// outbound rules rendered as children that reference one another, and the
// public IP address of its frontend naming the frontend as its
// ipConfiguration; its deletion answers 202 with both polling URLs.
func TestLoadBalancerShapes(t *testing.T) {
	s := arm.New(arm.WithSettleAfter(-1))
	newGroup(t, s)
	const (
		network = groupPath + "/providers/Microsoft.Network"
		pip     = network + "/publicIPAddresses/lb-ip"
		lb      = network + "/loadBalancers/lb"
	)
	do(t, s, http.MethodPut, pip, `{"location":"westus"}`)
	s.Settle()
	body := `{"location":"westus","sku":{"name":"Standard"},"properties":{
		"frontendIPConfigurations":[{"name":"fe","properties":{"publicIPAddress":{"id":"` + pip + `"}}}],
		"backendAddressPools":[{"name":"pool"}],
		"outboundRules":[{"name":"out","properties":{"protocol":"All","allocatedOutboundPorts":1024,
			"backendAddressPool":{"id":"` + lb + `/backendAddressPools/pool"},"frontendIPConfigurations":[{"id":"` + lb + `/frontendIPConfigurations/fe"}]}}]}}`
	for i, want := range []int{http.StatusCreated, http.StatusOK} {
		rec, got := do(t, s, http.MethodPut, lb, body)
		props, _ := got["properties"].(map[string]any)
		if rec.Code != want || !strings.Contains(rec.Header().Get("Azure-AsyncOperation"), "/providers/Microsoft.Network/locations/westus/operations/") ||
			fmt.Sprint(got["sku"]) != "map[name:Standard tier:Regional]" || fmt.Sprint(props["probes"]) != "[]" {
			t.Fatalf("put load balancer %d = %d %v %v", i, rec.Code, rec.Header(), got)
		}
		s.Settle()
	}
	_, got := do(t, s, http.MethodGet, lb, "")
	props := got["properties"].(map[string]any)
	fe := props["frontendIPConfigurations"].([]any)[0].(map[string]any)
	pool := props["backendAddressPools"].([]any)[0].(map[string]any)
	rule := props["outboundRules"].([]any)[0].(map[string]any)
	ruleProps := rule["properties"].(map[string]any)
	if fe["id"] != lb+"/frontendIPConfigurations/fe" || fe["type"] != "Microsoft.Network/loadBalancers/frontendIPConfigurations" || fe["etag"] != got["etag"] ||
		fmt.Sprint(fe["properties"].(map[string]any)["outboundRules"]) != "[map[id:"+lb+"/outboundRules/out]]" ||
		pool["type"] != "Microsoft.Network/loadBalancers/backendAddressPools" ||
		fmt.Sprint(pool["properties"].(map[string]any)["outboundRules"]) != "[map[id:"+lb+"/outboundRules/out]]" ||
		rule["type"] != "Microsoft.Network/loadBalancers/outboundRules" || ruleProps["idleTimeoutInMinutes"] != float64(4) ||
		ruleProps["enableTcpReset"] != false || ruleProps["provisioningState"] != "Succeeded" {
		t.Fatalf("load balancer = %v", got)
	}
	_, got = do(t, s, http.MethodGet, pip, "")
	if ip := got["properties"].(map[string]any)["ipConfiguration"]; fmt.Sprint(ip) != "map[id:"+lb+"/frontendIPConfigurations/fe]" {
		t.Fatalf("public IP ipConfiguration = %v", ip)
	}
	if rec, got := do(t, s, http.MethodDelete, pip, ""); rec.Code != http.StatusBadRequest || errorCode(got) != "PublicIPAddressCannotBeDeleted" {
		t.Fatalf("delete public IP in use = %d %v", rec.Code, got)
	}
	rec, _ := do(t, s, http.MethodDelete, lb, "")
	if rec.Code != http.StatusAccepted || rec.Header().Get("Azure-AsyncOperation") == "" ||
		!strings.Contains(rec.Header().Get("Location"), "/providers/Microsoft.Network/locations/westus/operationResults/") {
		t.Fatalf("delete load balancer = %d %v", rec.Code, rec.Header())
	}
}

// An outbound rule's references must name children of its own load
// balancer, and its ports must come in multiples of 8 and no more than 64,000.
func TestOutboundRulesAreChecked(t *testing.T) {
	s := arm.New()
	newGroup(t, s)
	const (
		network = groupPath + "/providers/Microsoft.Network"
		pip     = network + "/publicIPAddresses/lb-ip"
		lb      = network + "/loadBalancers/lb"
	)
	do(t, s, http.MethodPut, pip, `{"location":"eastus"}`)
	put := func(pool string, ports int) map[string]any {
		_, got := do(t, s, http.MethodPut, lb, fmt.Sprintf(`{"location":"eastus","properties":{
			"frontendIPConfigurations":[{"name":"fe","properties":{"publicIPAddress":{"id":"%s"}}}],
			"backendAddressPools":[{"name":"pool"}],
			"outboundRules":[{"name":"out","properties":{"protocol":"Tcp","allocatedOutboundPorts":%d,
				"backendAddressPool":{"id":"%s"},"frontendIPConfigurations":[{"id":"%s/frontendIPConfigurations/fe"}]}}]}}`, pip, ports, pool, lb))
		return got
	}
	for _, tc := range []struct {
		pool  string
		ports int
		want  string
	}{
		{lb + "/backendAddressPools/none", 1024, "InvalidResourceReference"},
		{lb + "/backendAddressPools/pool", 1004, "SpecifiedAllocatedOutboundPortsForOutboundRuleIsNotAMultipleOfEight"},
		{lb + "/backendAddressPools/pool", 64_008, "SpecifiedAllocatedOutboundPortsForOutboundRuleExceedsTotalNumberOfAllowedPortsPerRule"},
	} {
		if got := put(tc.pool, tc.ports); errorCode(got) != tc.want {
			t.Errorf("rule with pool %s and %d ports = %v, want %s", tc.pool, tc.ports, got, tc.want)
		}
	}
	got := put(lb+"/backendAddressPools/pool", 0)
	rule := got["properties"].(map[string]any)["outboundRules"].([]any)[0].(map[string]any)
	if ports := rule["properties"].(map[string]any)["allocatedOutboundPorts"]; ports != float64(1024) {
		t.Fatalf("default allocation for an empty pool = %v, want 1024", got)
	}
}
