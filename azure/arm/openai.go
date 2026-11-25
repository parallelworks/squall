package arm

import (
	"cmp"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/internal/stub"
)

// OpenAI returns the Azure OpenAI data plane of the server's Cognitive
// Services accounts, an [http.Handler] to serve with [net/http/httptest]. It
// takes the account's custom subdomain from a host name such as
// {subdomain}.openai.azure.com or {subdomain}.cognitiveservices.azure.com,
// or else from the first path segment, so a client reaches the account at
// {server URL}/{subdomain}/.
func (s *Server) OpenAI() http.Handler { return http.HandlerFunc(s.serveOpenAI) }

// ingestionJob is an Azure OpenAI On Your Data ingestion job.
type ingestionJob struct {
	body   map[string]any
	status string
	err    *apiError
	// wait is how many more reads see the job running.
	wait int
}

const (
	jobRunning   = "running"
	jobSucceeded = "succeeded"
	jobFailed    = "failed"
)

type openAICall struct {
	r         *http.Request
	subdomain string
	job       string
	account   *resource
	// opFault, when set, is the error the job must end in.
	opFault *apiError
}

type openAIOp struct {
	// name is the operation's ID in the REST API's specification, which fault rules match.
	name string
	h    func(s *Server, c *openAICall) (*reply, *apiError)
}

var (
	createJobOp = &openAIOp{"IngestionJobs_Create", (*Server).putIngestionJob}
	getJobOp    = &openAIOp{"IngestionJobs_Get", (*Server).getIngestionJob}
)

// openAIKinds are the account kinds that serve the Azure OpenAI data plane.
var openAIKinds = []string{"AIServices", "OpenAI"}

func parseOpenAIRequest(r *http.Request) (*openAICall, *openAIOp) {
	c := &openAICall{r: r}
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	if labels := strings.Split(host, "."); len(labels) > 3 && (strings.EqualFold(labels[1], "openai") || strings.EqualFold(labels[1], "cognitiveservices")) {
		c.subdomain = labels[0]
	} else {
		c.subdomain, path, _ = strings.Cut(path, "/")
	}
	job, ok := strings.CutPrefix(path, "openai/ingestion/jobs/")
	if !ok || job == "" || strings.Contains(job, "/") {
		return c, nil
	}
	c.job, _ = url.PathUnescape(job)
	switch r.Method {
	case http.MethodPut:
		return c, createJobOp
	case http.MethodGet:
		return c, getJobOp
	}
	return c, nil
}

func (s *Server) serveOpenAI(w http.ResponseWriter, r *http.Request) {
	c, op := parseOpenAIRequest(r)
	if op == nil || r.URL.Query().Get("api-version") == "" {
		writeOpenAIError(w, newError(http.StatusNotFound, "404", "Resource not found"))
		return
	}
	if err := s.admitOpenAI(c); err != nil {
		writeOpenAIError(w, err)
		return
	}
	f, _ := s.faults.Decide(fault.Call{Method: op.name, Resource: strings.ToLower(c.subdomain) + "/ingestion/jobs/" + c.job})
	reject := f.Mode == fault.ModeReject || f.Mode == fault.ModeFailOperation && op != createJobOp
	if f.Mode == fault.ModeFailOperation && !reject {
		c.opFault = openAIFaultError(f)
	}
	stub.Serve(w, r, f, reject, "application/json", func() { writeOpenAIError(w, openAIFaultError(f)) }, func() (func(), bool) {
		s.mu.Lock()
		rep, err := op.h(s, c)
		s.mu.Unlock()
		if err != nil {
			return func() { writeOpenAIError(w, err) }, false
		}
		return func() { writeJSON(w, rep.status, nil, rep.body) }, true
	})
}

// admitOpenAI finds the account c names and checks its key.
func (s *Server) admitOpenAI(c *openAICall) *apiError {
	s.mu.Lock()
	defer s.mu.Unlock()
	acct := s.subdomainHolder(c.subdomain)
	if acct == nil || s.resources[key(acct.id)] != acct || !slices.Contains(openAIKinds, str(acct.top, "kind")) {
		return newError(http.StatusNotFound, "404", "Resource not found")
	}
	key := c.r.Header.Get("api-key")
	switch {
	case key == "" && strings.HasPrefix(c.r.Header.Get("Authorization"), "Bearer "):
		return unsupported("Microsoft Entra ID tokens are not supported; send an account key in api-key")
	case key == "":
		return newError(http.StatusUnauthorized, "401",
			"Access denied due to missing subscription key. Make sure to include subscription key when making requests to an API.")
	case key != accessKey(acct, "key1", 84) && key != accessKey(acct, "key2", 84):
		return newError(http.StatusUnauthorized, "401",
			"Access denied due to invalid subscription key or wrong API endpoint. Make sure to provide a valid key for an active subscription and use a correct regional API endpoint for your resource.")
	}
	c.account = acct
	return nil
}

// openAIFaultError is the error a fault answers with. The fault package's
// generic reasons become the HTTP status, as Azure OpenAI writes its codes.
func openAIFaultError(f fault.Fault) *apiError {
	status := cmp.Or(f.Status, http.StatusServiceUnavailable)
	code := f.Reason
	switch code {
	case "", "backendError":
		code = strconv.Itoa(status)
	case "rateLimitExceeded":
		code, status = "429", http.StatusTooManyRequests
	}
	return &apiError{Status: status, Code: code, Message: f.ErrorMessage()}
}

func writeOpenAIError(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.Status, e.Header, encode(map[string]any{"error": map[string]any{"code": e.Code, "message": e.Message}}))
}

// jobMembers are the members an ingestion job takes; status, error, warnings
// and progress are read-only and dropped.
var jobMembers = []string{
	"completionAction", "compute", "dataRefreshIntervalInHours", "datasource", "jobId", "kind", "searchServiceConnection", "target", "workspaceId",
}

var jobReadOnly = []string{"error", "progress", "status", "warnings"}

// putIngestionJob creates the job, or replaces and restarts it, running
// until the reads [WithSettleAfter] sets are spent.
func (s *Server) putIngestionJob(c *openAICall) (*reply, *apiError) {
	body, err := readObject(c.r)
	if err != nil {
		return nil, newError(http.StatusBadRequest, "BadRequest", "%s", err.Message)
	}
	for _, k := range slices.Sorted(maps.Keys(body)) {
		switch {
		case slices.Contains(jobReadOnly, k):
			delete(body, k)
		case !slices.Contains(jobMembers, k):
			return nil, newError(http.StatusBadRequest, "BadRequest", "The ingestion job has an unknown member '%s'.", k)
		}
	}
	if id := str(body, "jobId"); id != "" && id != c.job {
		return nil, newError(http.StatusBadRequest, "BadRequest", "The jobId '%s' in the body does not match the jobId '%s' in the URL.", id, c.job)
	}
	var required []string
	switch str(body, "kind") {
	case "SystemCompute":
		required = []string{"searchServiceConnection", "datasource"}
	case "UserCompute":
		required = []string{"workspaceId", "compute", "target", "datasource"}
	default:
		return nil, newError(http.StatusBadRequest, "BadRequest", "The ingestion job's kind must be SystemCompute or UserCompute.")
	}
	for _, k := range required {
		if body[k] == nil {
			return nil, newError(http.StatusBadRequest, "BadRequest", "A %s ingestion job needs %s.", str(body, "kind"), k)
		}
	}
	if a := str(body, "completionAction"); a != "" && a != "cleanUpAssets" && a != "keepAllAssets" {
		return nil, newError(http.StatusBadRequest, "BadRequest", "The completionAction must be cleanUpAssets or keepAllAssets.")
	}
	body["jobId"] = c.job
	if c.account.jobs == nil {
		c.account.jobs = map[string]*ingestionJob{}
	}
	job := &ingestionJob{body: body, status: jobRunning, wait: max(s.settleAfter, 0)}
	c.account.jobs[c.job] = job
	switch {
	case c.opFault != nil:
		job.status, job.err = jobFailed, c.opFault
	case s.settleAfter == 0:
		job.status = jobSucceeded
	}
	return ok200(job.render()), nil
}

// getIngestionJob answers the job, counting the read toward its run.
func (s *Server) getIngestionJob(c *openAICall) (*reply, *apiError) {
	job := c.account.jobs[c.job]
	if job == nil {
		return nil, newError(http.StatusNotFound, "NotFound", "The ingestion job '%s' was not found.", c.job)
	}
	if job.status == jobRunning && s.settleAfter >= 0 {
		if job.wait == 0 {
			job.status = jobSucceeded
		} else {
			job.wait--
		}
	}
	return ok200(job.render()), nil
}

func (job *ingestionJob) render() map[string]any {
	m := maps.Clone(job.body)
	m["status"], m["warnings"] = job.status, []any{}
	if job.err != nil {
		m["error"] = map[string]any{"code": job.err.Code, "message": job.err.Message}
	}
	return m
}

// settleJobs ends the ingestion jobs of res that are running. The caller holds s.mu.
func settleJobs(res *resource) {
	for _, job := range res.jobs {
		if job.status == jobRunning {
			job.status = jobSucceeded
		}
	}
}
