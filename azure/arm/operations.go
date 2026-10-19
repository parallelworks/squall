package arm

import (
	"cmp"
	"net/http"
	"strings"
	"time"
)

const (
	opInProgress = "InProgress"
	opSucceeded  = "Succeeded"
	opFailed     = "Failed"
	opCanceled   = "Canceled"
)

// operation is a long-running operation a call started.
type operation struct {
	id string
	// statusPath answers the Azure-AsyncOperation pattern; it is empty for an
	// operation polled through its Location only.
	statusPath string
	// resultPath answers the Location pattern.
	resultPath string
	// resultStatus answers a poll of resultPath once the operation succeeds.
	resultStatus int
	// pollStatus answers a poll of statusPath while the operation runs.
	pollStatus int
	status     string
	err        *apiError
	start, end time.Time
	// wait is how many more reads see the operation in progress.
	wait int
	// complete applies the call's effect; it is nil once the operation is done.
	complete func()
}

type opStatusBody struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Status    string       `json:"status"`
	StartTime string       `json:"startTime"`
	EndTime   string       `json:"endTime,omitzero"`
	Error     *errorDetail `json:"error,omitzero"`
}

// opPaths are the polling paths of an operation, before its ID, and the
// status a poll of its status path answers while it runs, if not 200.
type opPaths struct {
	status, result string
	pollStatus     int
}

func providerOps(c *call, namespace, location string) opPaths {
	prefix := "/subscriptions/" + c.val("sub") + "/providers/" + namespace + "/locations/" + location
	return opPaths{status: prefix + "/operations/", result: prefix + "/operationResults/"}
}

func computeOps(c *call, location string) opPaths {
	return providerOps(c, "Microsoft.Compute", location)
}

func networkOps(c *call, location string) opPaths {
	return providerOps(c, "Microsoft.Network", location)
}

func dnsOps(c *call) opPaths {
	prefix := "/subscriptions/" + c.val("sub") + "/resourceGroups/" + c.val("rg") + "/providers/Microsoft.Network/"
	return opPaths{prefix + "privateDnsOperationStatuses/", prefix + "privateDnsOperationResults/", http.StatusAccepted}
}

func groupOps(c *call) opPaths {
	return opPaths{result: "/subscriptions/" + c.val("sub") + "/operationresults/"}
}

// begin starts an operation. With the call's opFault it fails without
// complete running. Otherwise complete applies the call's effect, at once or
// after the reads [WithSettleAfter] sets; until then the caller shows the
// resource in progress. The caller holds s.mu.
func (s *Server) begin(c *call, paths opPaths, resultStatus int, complete func()) *operation {
	id := s.newUUID()
	op := &operation{
		id: id, resultPath: paths.result + id, resultStatus: cmp.Or(resultStatus, http.StatusNoContent),
		pollStatus: cmp.Or(paths.pollStatus, http.StatusOK), start: s.now(),
	}
	if paths.status != "" {
		op.statusPath = paths.status + id
	}
	s.keepOperation(op)
	switch {
	case c.opFault != nil:
		op.status, op.err, op.end = opFailed, c.opFault, s.now()
	case s.settleAfter == 0:
		complete()
		op.status, op.end = opSucceeded, s.now()
	default:
		op.status, op.complete, op.wait = opInProgress, complete, max(s.settleAfter, 0)
		s.pending[op] = struct{}{}
	}
	return op
}

// keepOperation stores op, dropping the oldest once the server keeps
// maxOperations. The caller holds s.mu.
func (s *Server) keepOperation(op *operation) {
	s.ops[op.id] = op
	if len(s.opOrder) < maxOperations {
		s.opOrder = append(s.opOrder, op.id)
		return
	}
	delete(s.ops, s.opOrder[s.opNext])
	s.opOrder[s.opNext] = op.id
	s.opNext = (s.opNext + 1) % maxOperations
}

// finish applies op's effect now, if it is still in progress. The caller holds s.mu.
func (s *Server) finish(op *operation) {
	if op == nil || op.complete == nil {
		return
	}
	complete := op.complete
	op.complete = nil
	delete(s.pending, op)
	complete()
	op.status, op.end = opSucceeded, s.now()
}

// preempt ends op as Compute ends an operation a newer call supersedes: what
// it did stands, and it reports Canceled with OperationPreempted. The caller
// holds s.mu.
func (s *Server) preempt(op *operation) {
	if op == nil || op.complete == nil {
		return
	}
	s.finish(op)
	op.status = opCanceled
	op.err = newError(http.StatusConflict, "OperationPreempted", "Operation execution has been preempted by a more recent operation.")
}

// observe counts one read of op while it is in progress, and finishes it once
// the reads [WithSettleAfter] sets are spent. The caller holds s.mu.
func (s *Server) observe(op *operation) {
	if op == nil || op.complete == nil || s.settleAfter < 0 {
		return
	}
	if op.wait == 0 {
		s.finish(op)
		return
	}
	op.wait--
}

// observer returns a function that observes each operation once, for a call
// that reads several resources one operation changes. The caller holds s.mu.
func (s *Server) observer() func(*operation) {
	seen := map[*operation]bool{}
	return func(op *operation) {
		if op != nil && !seen[op] {
			seen[op] = true
			s.observe(op)
		}
	}
}

// Settle finishes every operation in progress, with the effects finishing
// has, such as a VM reaching running or a deleted resource group going away.
func (s *Server) Settle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for op := range s.pending {
		s.finish(op)
	}
}

// accepted answers a call that started op. A finished operation answers
// syncStatus with body at once; one in progress or failed answers asyncStatus
// with the headers the SDK polls by, adding a Location URL when location is
// set.
func accepted(c *call, op *operation, syncStatus, asyncStatus int, location bool, body func() []byte) *reply {
	if op.status == opSucceeded {
		return &reply{status: syncStatus, body: body()}
	}
	h := http.Header{}
	h.Set("retry-after-ms", retryAfterMS)
	query := "?api-version=" + c.query("api-version")
	if op.statusPath != "" {
		h.Set("Azure-AsyncOperation", c.base()+op.statusPath+query)
	}
	if location || op.statusPath == "" {
		h.Set("Location", c.base()+op.resultPath+query)
	}
	return &reply{status: asyncStatus, header: h, body: body()}
}

func noBody() []byte { return nil }

func emptyObject() []byte { return []byte("{}") }

// lookupOperation returns the operation the call's path names. The caller holds s.mu.
func (s *Server) lookupOperation(c *call, result bool) (*operation, *apiError) {
	op, ok := s.ops[c.val("op")]
	if ok {
		path := op.statusPath
		if result {
			path = op.resultPath
		}
		if strings.EqualFold(path, c.r.URL.Path) {
			return op, nil
		}
	}
	return nil, newError(http.StatusNotFound, "NotFound", "The operation '%s' was not found.", c.val("op"))
}

// operationStatus answers a poll of an Azure-AsyncOperation URL.
func (s *Server) operationStatus(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, err := s.lookupOperation(c, false)
	if err != nil {
		return nil, err
	}
	s.observe(op)
	body := opStatusBody{ID: op.statusPath, Name: op.id, Status: op.status, StartTime: timestamp(op.start)}
	if op.status != opInProgress {
		body.EndTime = timestamp(op.end)
	}
	if op.err != nil {
		body.Error = new(op.err.detail())
	}
	rep := ok(encode(body))
	if op.status == opInProgress {
		rep.status = op.pollStatus
		rep.header = http.Header{"Retry-After-Ms": {retryAfterMS}}
	}
	return rep, nil
}

// operationResult answers a poll of a Location URL: 202 while the operation
// runs, then its result, or its error.
func (s *Server) operationResult(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, err := s.lookupOperation(c, true)
	if err != nil {
		return nil, err
	}
	s.observe(op)
	switch op.status {
	case opInProgress:
		h := http.Header{}
		h.Set("Location", c.base()+c.r.URL.RequestURI())
		h.Set("retry-after-ms", retryAfterMS)
		return &reply{status: http.StatusAccepted, header: h}, nil
	case opFailed, opCanceled:
		return nil, op.err
	}
	return &reply{status: op.resultStatus}, nil
}
