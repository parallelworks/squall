package lustre

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// maxOperations bounds how many operations the server keeps for get, list and
// requestId replay. The oldest are dropped first.
const maxOperations = 10_000

// operation is a long-running operation the server started.
type operation struct {
	name                  string
	target                string
	verb                  string
	createTime, endTime   time.Time
	requestedCancellation bool
	requestKey            string

	done bool
	err  *apiError
	// result is what the operation answers with once done: an instance, or nil for Empty.
	result *Instance
	// wait is how many more reads see the operation in progress.
	wait int
	// complete applies the call's effect and returns the result; it is nil once the operation is done.
	complete func() *Instance
}

type operationJSON struct {
	Name     string        `json:"name"`
	Metadata operationMeta `json:"metadata"`
	Done     bool          `json:"done,omitzero"`
	Error    *rpcStatus    `json:"error,omitzero"`
	Response any           `json:"response,omitzero"`
}

type operationMeta struct {
	Type                  string     `json:"@type"`
	CreateTime            time.Time  `json:"createTime"`
	EndTime               *time.Time `json:"endTime,omitzero"`
	Target                string     `json:"target"`
	Verb                  string     `json:"verb"`
	RequestedCancellation bool       `json:"requestedCancellation"`
	APIVersion            string     `json:"apiVersion"`
}

// wire is op in the API's JSON shape, with enums as numbers when intEnums is set.
func (op *operation) wire(intEnums bool) operationJSON {
	out := operationJSON{
		Name: op.name,
		Metadata: operationMeta{
			Type:                  "type.googleapis.com/google.cloud.lustre.v1.OperationMetadata",
			CreateTime:            op.createTime,
			Target:                op.target,
			Verb:                  op.verb,
			RequestedCancellation: op.requestedCancellation,
			APIVersion:            "v1",
		},
		Done: op.done,
	}
	if !op.done {
		return out
	}
	out.Metadata.EndTime = new(op.endTime)
	switch {
	case op.err != nil:
		out.Error = op.err.operationStatus()
	case op.result != nil:
		out.Response = struct {
			Type string `json:"@type"`
			instanceJSON
		}{"type.googleapis.com/google.cloud.lustre.v1.Instance", op.result.wire(intEnums)}
	default:
		out.Response = struct {
			Type string `json:"@type"`
		}{"type.googleapis.com/google.protobuf.Empty"}
	}
	return out
}

// startOperation records an operation of verb on target in parent, started by
// a call with requestKey. A non-nil failure ends it in that error without
// complete running. Otherwise complete applies the call's effect when the
// operation finishes: before the call returns, or after the reads
// [WithSettleAfter] sets. The caller holds s.mu.
func (s *Server) startOperation(parent, requestKey, verb, target string, failure *apiError, complete func() *Instance) *operation {
	now := s.now().UTC()
	id := s.newID()
	op := &operation{
		name:       fmt.Sprintf("%s/operations/operation-%d-%013x-%08x-%08x", parent, now.UnixMilli(), id, id*2654435761%(1<<32), id%(1<<32)),
		target:     target,
		verb:       verb,
		createTime: now,
		requestKey: requestKey,
	}
	s.keepOperation(op)
	switch {
	case failure != nil:
		op.err = failure
		s.done(op)
	case s.settleAfter == 0:
		op.result = complete()
		s.done(op)
	default:
		op.complete, op.wait = complete, max(s.settleAfter, 0)
		s.pending[op] = struct{}{}
	}
	return op
}

// keepOperation stores op, and for replay under its request key. The caller holds s.mu.
func (s *Server) keepOperation(op *operation) {
	s.operations[op.name] = op
	if op.requestKey != "" {
		s.requests[op.requestKey] = op
	}
	// opOrder is a ring of the kept operations' names; opNext is the oldest once it is full.
	if len(s.opOrder) < maxOperations {
		s.opOrder = append(s.opOrder, op.name)
		return
	}
	s.forget(s.opOrder[s.opNext])
	s.opOrder[s.opNext] = op.name
	s.opNext = (s.opNext + 1) % maxOperations
}

// forget drops the named operation, leaving its effect in progress. The caller holds s.mu.
func (s *Server) forget(name string) {
	op := s.operations[name]
	if op == nil {
		return
	}
	delete(s.operations, name)
	if op.requestKey != "" && s.requests[op.requestKey] == op {
		delete(s.requests, op.requestKey)
	}
}

// done marks op finished. The caller holds s.mu.
func (s *Server) done(op *operation) {
	op.done, op.endTime = true, s.now().UTC()
}

// finish applies op's effect now, if it is still in progress. The caller holds s.mu.
func (s *Server) finish(op *operation) {
	if op == nil || op.complete == nil {
		return
	}
	complete := op.complete
	op.complete = nil
	delete(s.pending, op)
	op.result = complete()
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
	op.wait--
}

// Settle finishes every operation in progress, with the effects finishing
// has, such as an instance becoming ACTIVE or a deleted one going away.
func (s *Server) Settle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for op := range s.pending {
		s.finish(op)
	}
}

// replay returns the operation an earlier call with key started, if any. The caller holds s.mu.
func (s *Server) replay(key string) (*operation, bool) {
	if key == "" {
		return nil, false
	}
	op, ok := s.requests[key]
	return op, ok
}

func (s *Server) getOperation(c *call, _ *apiError) (any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[c.resource]
	if !ok {
		return nil, notFound(c.resource)
	}
	s.observe(op)
	return op.wire(c.intEnums), nil
}

func (s *Server) listOperations(c *call, _ *apiError) (any, *apiError) {
	q := c.URL.Query()
	if f := q.Get("filter"); f != "" {
		return nil, invalid("squall: operations.list does not support filter %q", f)
	}
	if q.Get("returnPartialSuccess") == "true" {
		return nil, &apiError{Status: 501, Message: "returnPartialSuccess is not supported"}
	}
	project, location := c.PathValue("project"), c.PathValue("location")
	s.mu.Lock()
	defer s.mu.Unlock()
	var ops []*operation
	for _, name := range slices.Sorted(maps.Keys(s.operations)) {
		if location == "-" && strings.HasPrefix(name, "projects/"+project+"/locations/") ||
			strings.HasPrefix(name, c.parent()+"/operations/") {
			ops = append(ops, s.operations[name])
		}
	}
	page, next, err := pageOf(c, ops, func(op *operation) string { return op.name }, false)
	if err != nil {
		return nil, err
	}
	out := struct {
		Operations    []operationJSON `json:"operations,omitzero"`
		NextPageToken string          `json:"nextPageToken,omitzero"`
	}{NextPageToken: next}
	for _, op := range page {
		out.Operations = append(out.Operations, op.wire(c.intEnums))
	}
	return out, nil
}

// deleteOperation forgets an operation; the change it tracks goes on.
func (s *Server) deleteOperation(c *call, _ *apiError) (any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.operations[c.resource]; !ok {
		return nil, notFound(c.resource)
	}
	s.forget(c.resource)
	return struct{}{}, nil
}

func (s *Server) cancelOperation(c *call, _ *apiError) (any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[c.resource]
	if !ok {
		return nil, notFound(c.resource)
	}
	if !op.done {
		op.requestedCancellation = true
	}
	return struct{}{}, nil
}

// newID returns a fresh number for an operation or instance. The caller holds s.mu.
func (s *Server) newID() uint64 {
	s.nextID++
	return s.nextID
}
