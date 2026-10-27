package lustre

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxOperations bounds memory; the oldest operations go first.
const maxOperations = 10_000

type operation struct {
	name                  string
	target                string
	verb                  string
	createTime, endTime   time.Time
	requestedCancellation bool
	requestKey            string

	done   bool
	err    *apiError
	result *Instance
	wait   int
	// complete is nil once the operation is done.
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
		resp := op.result.wireMap(intEnums)
		resp["@type"] = "type.googleapis.com/google.cloud.lustre.v1.Instance"
		out.Response = resp
	default:
		out.Response = struct {
			Type string `json:"@type"`
		}{"type.googleapis.com/google.protobuf.Empty"}
	}
	return out
}

// startOperation fails at once on failure, and otherwise runs complete now or after [WithSettleAfter] reads.
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

// forget drops an operation but not the change it tracks, as the API documents for delete.
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

func (s *Server) done(op *operation) {
	op.done, op.endTime = true, s.now().UTC()
}

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

// Settle finishes every change in progress.
func (s *Server) Settle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for op := range s.pending {
		s.finish(op)
	}
}

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
	project, location := c.PathValue("project"), c.PathValue("location")
	if v := q.Get("returnPartialSuccess"); v != "" {
		partial, err := strconv.ParseBool(v)
		if err != nil || v != "true" && v != "false" {
			return nil, invalid("Invalid value for return_partial_success: %q", v)
		}
		// The reference allows it only when reading across locations.
		if partial && location != "-" {
			return nil, &apiError{Status: http.StatusNotImplemented, Message: "return_partial_success is supported only when listing every location"}
		}
	}
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
		s.observe(op)
		out.Operations = append(out.Operations, op.wire(c.intEnums))
	}
	return out, nil
}

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

func (s *Server) newID() uint64 {
	s.nextID++
	return s.nextID
}
