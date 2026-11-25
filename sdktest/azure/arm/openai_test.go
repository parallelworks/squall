package arm_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/cognitiveservices/armcognitiveservices"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/operationalinsights/armoperationalinsights/v3"

	"github.com/parallelworks/squall/azure/arm"
	"github.com/parallelworks/squall/fault"
)

// openAI creates an AIServices account with the subdomain domain and returns
// a client of its Azure OpenAI data plane that sends its second key.
func (c *clients) openAI(t *testing.T, name, domain string) *rest {
	t.Helper()
	c.createGroup(t, group, nil)
	c.createCognitiveAccount(t, name, domain)
	keys, err := c.cognitive.ListKeys(t.Context(), group, name, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(c.server.OpenAI())
	t.Cleanup(srv.Close)
	return &rest{t: t, base: srv.URL + "/" + domain + "/openai", key: *keys.Key2, version: "2024-05-01-preview"}
}

func ingestionJob() map[string]any {
	return map[string]any{
		"kind": "SystemCompute",
		"searchServiceConnection": map[string]any{
			"kind": "EndpointWithKey", "endpoint": "https://find.search.windows.net", "key": "k",
		},
		"datasource": map[string]any{
			"kind": "Storage", "connection": map[string]any{"kind": "ConnectionString", "connectionString": "c"}, "containerName": "docs",
			"chunking":   map[string]any{"maxChunkSizeInTokens": 1024},
			"embeddings": []any{map[string]any{"connection": map[string]any{"kind": "RelativeConnection"}, "deploymentName": "ada"}},
		},
		"dataRefreshIntervalInHours": 24,
		"completionAction":           "keepAllAssets",
	}
}

func TestIngestionJobs(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(1))
	api := c.openAI(t, "ai", "ai-sub")
	status, got := api.do("PUT", "/ingestion/jobs/nightly", ingestionJob())
	if status != http.StatusOK || got["jobId"] != "nightly" || got["status"] != "running" || got["completionAction"] != "keepAllAssets" {
		t.Fatalf("create = %d %v", status, got)
	}
	if _, got := api.do("GET", "/ingestion/jobs/nightly", nil); got["status"] != "running" {
		t.Fatalf("a job's first read = %v", got)
	}
	if status, got := api.do("GET", "/ingestion/jobs/nightly", nil); status != http.StatusOK || got["status"] != "succeeded" {
		t.Fatalf("a job after its reads = %d %v", status, got)
	}
	api.do("PUT", "/ingestion/jobs/nightly", ingestionJob())
	c.server.Settle()
	if _, got := api.do("GET", "/ingestion/jobs/nightly", nil); got["status"] != "succeeded" {
		t.Fatalf("a settled job = %v", got)
	}
	if status, got := api.do("GET", "/ingestion/jobs/weekly", nil); status != http.StatusNotFound {
		t.Fatalf("a job that does not exist = %d %v", status, got)
	}
	for name, body := range map[string]map[string]any{
		"no kind":              {"datasource": map[string]any{}},
		"no search connection": {"kind": "SystemCompute", "datasource": map[string]any{}},
		"an unknown member":    {"kind": "SystemCompute", "colour": "red"},
		"a bad action":         {"kind": "SystemCompute", "searchServiceConnection": map[string]any{}, "datasource": map[string]any{}, "completionAction": "x"},
	} {
		if status, _ := api.do("PUT", "/ingestion/jobs/bad", body); status != http.StatusBadRequest {
			t.Fatalf("a job with %s = %d", name, status)
		}
	}
}

func TestIngestionJobAuthentication(t *testing.T) {
	c := setup(t)
	api := c.openAI(t, "ai", "ai-sub")
	keys, err := c.cognitive.ListKeys(t.Context(), group, "ai", nil)
	if err != nil {
		t.Fatal(err)
	}
	api.key = *keys.Key1
	if status, _ := api.do("PUT", "/ingestion/jobs/nightly", ingestionJob()); status != http.StatusOK {
		t.Fatalf("the first key = %d", status)
	}
	api.key = "wrong"
	if status, got := api.do("GET", "/ingestion/jobs/nightly", nil); status != http.StatusUnauthorized ||
		!strings.HasPrefix(errorMessage(got), "Access denied due to invalid subscription key") {
		t.Fatalf("a wrong key = %d %v", status, got)
	}
	api.key = ""
	if status, got := api.do("GET", "/ingestion/jobs/nightly", nil); status != http.StatusUnauthorized ||
		!strings.HasPrefix(errorMessage(got), "Access denied due to missing subscription key") {
		t.Fatalf("no key = %d %v", status, got)
	}
	api.key, api.version = *keys.Key1, ""
	if status, _ := api.do("GET", "/ingestion/jobs/nightly", nil); status != http.StatusNotFound {
		t.Fatalf("no api-version = %d", status)
	}
	api.version = "2024-05-01-preview"

	p, err := c.cognitive.BeginCreate(t.Context(), group, "speech", armcognitiveservices.Account{
		Location: new(location), Kind: new("SpeechServices"), SKU: &armcognitiveservices.SKU{Name: new("S0")},
		Properties: &armcognitiveservices.AccountProperties{CustomSubDomainName: new("speech-sub")},
	}, nil)
	if _, err := wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	speechKeys, err := c.cognitive.ListKeys(t.Context(), group, "speech", nil)
	if err != nil {
		t.Fatal(err)
	}
	speech := &rest{t: t, base: strings.Replace(api.base, "/ai-sub/", "/speech-sub/", 1), key: *speechKeys.Key1, version: api.version}
	if status, _ := speech.do("GET", "/ingestion/jobs/nightly", nil); status != http.StatusNotFound {
		t.Fatalf("an account of another kind = %d", status)
	}

	if _, err := c.cognitive.BeginDelete(t.Context(), group, "ai", nil); err != nil {
		t.Fatal(err)
	}
	if status, _ := api.do("GET", "/ingestion/jobs/nightly", nil); status != http.StatusNotFound {
		t.Fatalf("a deleted account = %d", status)
	}
}

func TestIngestionJobFaults(t *testing.T) {
	faults := fault.New()
	c := setup(t, arm.WithFaults(faults), arm.WithSettleAfter(-1))
	api := c.openAI(t, "ai", "ai-sub")
	faults.Add(
		fault.Rule{Method: "IngestionJobs_Create", Resource: "ai-sub/ingestion/jobs/broken", Fault: fault.FailOperation(http.StatusBadRequest).WithMessage("The container is empty.")},
		fault.Rule{Method: "IngestionJobs_Create", Resource: "ai-sub/ingestion/jobs/refused", Fault: fault.Reject(http.StatusBadRequest, "BadRequest")},
		fault.Rule{Method: "IngestionJobs_Get", Times: 1, Fault: fault.Unavailable()},
	)
	if status, got := api.do("PUT", "/ingestion/jobs/broken", ingestionJob()); status != http.StatusOK || got["status"] != "failed" ||
		got["error"].(map[string]any)["message"] != "The container is empty." {
		t.Fatalf("a job that fails = %d %v", status, got)
	}
	if status, got := api.do("PUT", "/ingestion/jobs/refused", ingestionJob()); status != http.StatusBadRequest || got["error"].(map[string]any)["code"] != "BadRequest" {
		t.Fatalf("a refused job = %d %v", status, got)
	}
	api.do("PUT", "/ingestion/jobs/held", ingestionJob())
	if status, got := api.do("GET", "/ingestion/jobs/held", nil); status != http.StatusServiceUnavailable || got["error"].(map[string]any)["code"] != "503" {
		t.Fatalf("a faulted get = %d %v", status, got)
	}
	for range 3 {
		if _, got := api.do("GET", "/ingestion/jobs/held", nil); got["status"] != "running" {
			t.Fatalf("a held job = %v", got)
		}
	}
	c.server.Settle()
	if _, got := api.do("GET", "/ingestion/jobs/held", nil); got["status"] != "succeeded" {
		t.Fatalf("a held job after Settle = %v", got)
	}
}

func TestFailProvisioning(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	ws := c.createLogWorkspace(t, "logs")
	if err := c.server.FailProvisioning(*ws.ID); err != nil {
		t.Fatal(err)
	}
	got, err := c.logs.Get(t.Context(), group, "logs", nil)
	if err != nil || *got.Properties.ProvisioningState != armoperationalinsights.WorkspaceEntityStatusFailed {
		t.Fatalf("a failed workspace = %+v, %v", got.Properties, err)
	}
	again := c.createLogWorkspace(t, "logs")
	if *again.Properties.ProvisioningState != armoperationalinsights.WorkspaceEntityStatusSucceeded || *again.Properties.CustomerID != *ws.Properties.CustomerID {
		t.Fatalf("a failed workspace put again = %+v", again.Properties)
	}
	if err := c.server.FailProvisioning(*ws.ID + "x"); err == nil {
		t.Fatal("failing a resource that does not exist succeeded")
	}
}
