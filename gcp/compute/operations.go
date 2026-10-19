package compute

import (
	"fmt"
	"net/http"
	"strings"
	"unicode"
)

// maxOperations bounds how many operations the server keeps for get and for
// requestId replay. The oldest are dropped first.
const maxOperations = 10_000

const (
	operationRunning = "RUNNING"
	operationDone    = "DONE"
)

// operation is a zonal, regional or global operation in the API's JSON shape.
type operation struct {
	Kind                string              `json:"kind"`
	ID                  uint64              `json:"id,string"`
	Name                string              `json:"name"`
	OperationType       string              `json:"operationType"`
	TargetLink          string              `json:"targetLink"`
	TargetID            uint64              `json:"targetId,string,omitzero"`
	Status              string              `json:"status"`
	Progress            int                 `json:"progress"`
	InsertTime          string              `json:"insertTime"`
	StartTime           string              `json:"startTime"`
	EndTime             string              `json:"endTime,omitzero"`
	Zone                string              `json:"zone,omitzero"`
	Region              string              `json:"region,omitzero"`
	SelfLink            string              `json:"selfLink"`
	Error               *operationErrors    `json:"error,omitzero"`
	HTTPErrorStatusCode int                 `json:"httpErrorStatusCode,omitzero"`
	HTTPErrorMessage    string              `json:"httpErrorMessage,omitzero"`
	BulkInsert          *bulkInsertMetadata `json:"instancesBulkInsertOperationMetadata,omitzero"`

	requestID string
	// wait is how many more reads see the operation in progress.
	wait int
	// complete applies the call's effect; it is nil once the operation is done.
	complete func()
	// staging are instances that show STAGING on the operation's last read in progress.
	staging []*Instance
}

type operationErrors struct {
	Errors []operationError `json:"errors"`
}

type operationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type bulkInsertMetadata struct {
	PerLocationStatus map[string]bulkInsertStatus `json:"perLocationStatus"`
}

type bulkInsertStatus struct {
	Status                string `json:"status"`
	TargetVMCount         int    `json:"targetVmCount"`
	CreatedVMCount        int    `json:"createdVmCount,omitzero"`
	FailedToCreateVMCount int    `json:"failedToCreateVmCount,omitzero"`
}

// scopeOf is the zone, region or global scope a call's path names, such as
// "projects/p/zones/us-east1-b".
func scopeOf(r *http.Request) string {
	project := "projects/" + r.PathValue("project")
	if zone := r.PathValue("zone"); zone != "" {
		return project + "/zones/" + zone
	}
	if region := r.PathValue("region"); region != "" {
		return project + "/regions/" + region
	}
	return project + "/global"
}

// linkBase is the selfLink prefix of the API version r calls.
func linkBase(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/compute/beta/") {
		return betaSelfLinkBase
	}
	return selfLinkBase
}

// startOperation records an operation of opType on target, started by the
// call r with requestID. A non-nil failure ends it in that error without
// complete running. Otherwise complete, when not nil, applies the call's
// effect when the operation finishes: before the call returns, or after the
// reads [WithSettleAfter] sets. The caller holds s.mu.
func (s *Server) startOperation(r *http.Request, requestID, opType, target string, targetID uint64, failure *apiError, complete func()) *operation {
	id := s.newID()
	now := timestamp(s.now())
	scope := scopeOf(r)
	name := fmt.Sprintf("operation-%d", id)
	base := linkBase(r)
	op := &operation{
		Kind:          "compute#operation",
		ID:            id,
		Name:          name,
		OperationType: opType,
		TargetLink:    base + target,
		TargetID:      targetID,
		Status:        operationRunning,
		InsertTime:    now,
		StartTime:     now,
		SelfLink:      base + scope + "/operations/" + name,
		requestID:     requestID,
	}
	switch {
	case strings.Contains(scope, "/zones/"):
		op.Zone = base + scope
	case strings.Contains(scope, "/regions/"):
		op.Region = base + scope
	}
	s.keepOperation(scope+"/operations/"+name, op)
	switch {
	case failure != nil:
		errs := failure.opErrors
		if errs == nil {
			errs = []operationError{{Code: upperSnake(failure.Reason), Message: failure.Message}}
		}
		op.Error = &operationErrors{errs}
		op.HTTPErrorStatusCode = failure.Status
		op.HTTPErrorMessage = strings.ToUpper(http.StatusText(failure.Status))
		s.done(op)
	case complete == nil || s.settleAfter == 0:
		if complete != nil {
			complete()
		}
		s.done(op)
	default:
		op.complete, op.wait = complete, max(s.settleAfter, 0)
		s.pending[op] = struct{}{}
	}
	return op
}

// keepOperation stores op under path, and for replay under its request ID.
// The caller holds s.mu.
func (s *Server) keepOperation(path string, op *operation) {
	s.operations[path] = op
	if op.requestID != "" {
		s.requests[op.requestID] = op
	}
	// opOrder is a ring of the kept operations' paths; opNext is the oldest once it is full.
	if len(s.opOrder) < maxOperations {
		s.opOrder = append(s.opOrder, path)
		return
	}
	oldest := s.operations[s.opOrder[s.opNext]]
	delete(s.operations, s.opOrder[s.opNext])
	if oldest.requestID != "" && s.requests[oldest.requestID] == oldest {
		delete(s.requests, oldest.requestID)
	}
	s.opOrder[s.opNext] = path
	s.opNext = (s.opNext + 1) % maxOperations
}

// done marks op finished. The caller holds s.mu.
func (s *Server) done(op *operation) {
	op.Status, op.Progress, op.EndTime = operationDone, 100, timestamp(s.now())
}

// finish applies op's effect now, if it is still in progress. The caller holds s.mu.
func (s *Server) finish(op *operation) {
	if op == nil || op.complete == nil {
		return
	}
	complete := op.complete
	op.complete, op.staging = nil, nil
	delete(s.pending, op)
	complete()
	s.done(op)
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
	if op.wait--; op.wait == 0 {
		for _, inst := range op.staging {
			if inst.Status == statusProvisioning {
				inst.Status = statusStaging
			}
		}
	}
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

func (s *Server) getOperation(r *http.Request, _ *apiError) (any, *apiError) {
	path := scopeOf(r) + "/operations/" + r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[path]
	if !ok {
		return nil, notFound(path)
	}
	s.observe(op)
	return encoded(op), nil
}

// waitOperation answers an operations.wait: the operation once it is done,
// or as it is when only [Server.Settle] can finish it, as a real wait that
// runs out of time would.
func (s *Server) waitOperation(r *http.Request, _ *apiError) (any, *apiError) {
	path := scopeOf(r) + "/operations/" + r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[path]
	if !ok {
		return nil, notFound(path)
	}
	if s.settleAfter >= 0 {
		s.finish(op)
	}
	return encoded(op), nil
}

// Settle finishes every operation in progress, with the effects finishing
// has, such as an instance reaching RUNNING or a deleted one going away.
func (s *Server) Settle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for op := range s.pending {
		s.finish(op)
	}
}

// upperSnake spells a reason as the API's error codes are: "backendError"
// becomes "BACKEND_ERROR".
func upperSnake(s string) string {
	var b strings.Builder
	prev := ' '
	for _, c := range s {
		switch {
		case c == ' ' || c == '-':
			c = '_'
		case unicode.IsUpper(c) && (unicode.IsLower(prev) || unicode.IsDigit(prev)):
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToUpper(c))
		prev = c
	}
	return b.String()
}
