package serviceusage

import (
	"fmt"
)

type operation struct {
	Name  string
	Done  bool
	Error *rpcStatus
	kind  string
	svc   *Service
}

const (
	enableResponse  = "type.googleapis.com/google.api.serviceusage.v1.EnableServiceResponse"
	disableResponse = "type.googleapis.com/google.api.serviceusage.v1.DisableServiceResponse"
)

// wire is op in the API's JSON shape.
func (op *operation) wire(intEnums bool) any {
	type response struct {
		Type    string `json:"@type"`
		Service any    `json:"service"`
	}
	out := struct {
		Name     string     `json:"name"`
		Done     bool       `json:"done"`
		Error    *rpcStatus `json:"error,omitzero"`
		Response *response  `json:"response,omitzero"`
	}{Name: op.Name, Done: op.Done, Error: op.Error}
	if op.svc != nil {
		out.Response = &response{op.kind, wireService(*op.svc, intEnums)}
	}
	return out
}

// startOperation records a finished operation on the service name in the
// project with ID id that ends in failure or, without one, with a response
// of kind holding the service's state. The caller holds s.mu.
func (s *Server) startOperation(id, number, name, kind string, failure *apiError) *operation {
	s.nextID++
	n := s.nextID
	op := &operation{
		Name: fmt.Sprintf("operations/acat.p2-%s-%08x-%04x-%04x-%04x-%012x", number, n, n%0x10000, 0x4000|n%0x1000, 0x8000|n%0x4000, n*2654435761%(1<<48)),
		Done: true,
		kind: kind,
	}
	if failure != nil {
		op.Error = failure.operationStatus()
	} else {
		svc := s.service(id, number, name)
		op.svc = &svc
	}
	s.keepOperation(op)
	return op
}

func (s *Server) keepOperation(op *operation) {
	s.operations[op.Name] = op
	// opOrder is a ring of the kept operations' names; opNext is the oldest once it is full.
	if len(s.opOrder) < maxOperations {
		s.opOrder = append(s.opOrder, op.Name)
		return
	}
	delete(s.operations, s.opOrder[s.opNext])
	s.opOrder[s.opNext] = op.Name
	s.opNext = (s.opNext + 1) % maxOperations
}

func (s *Server) getOperation(c *call, _ *apiError) (any, *apiError) {
	name := "operations/" + c.PathValue("operation")
	if name == noopOperation {
		return (&operation{Name: name, Done: true}).wire(c.intEnums), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[name]
	if !ok {
		return nil, notFound("Operation " + name)
	}
	return op.wire(c.intEnums), nil
}
