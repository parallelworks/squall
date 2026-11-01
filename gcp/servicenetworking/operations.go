package servicenetworking

import (
	"fmt"
	"net/http"
	"strings"
)

type operation struct {
	Name     string     `json:"name"`
	Done     bool       `json:"done"`
	Error    *rpcStatus `json:"error,omitzero"`
	Response any        `json:"response,omitzero"`
}

type typedConnection struct {
	Type string `json:"@type"`
	Connection
}

type empty struct {
	Type string `json:"@type"`
}

// startOperation records a finished operation on n that ends in failure or,
// without one, with result, or empty for a delete. The caller holds s.mu.
func (s *Server) startOperation(n network, failure *apiError, result *Connection) *operation {
	s.nextID++
	id := s.nextID
	project := s.display(n)
	project = strings.TrimSuffix(strings.TrimPrefix(project, "projects/"), "/global/networks/"+n.name)
	op := &operation{
		Name: fmt.Sprintf("operations/pssn.p24-%s-%08x-%04x-%04x-%04x-%012x", project, id, id%0x10000, 0x4000|id%0x1000, 0x8000|id%0x4000, id*2654435761%(1<<48)),
		Done: true,
	}
	switch {
	case failure != nil:
		op.Error = failure.operationStatus()
	case result != nil:
		op.Response = typedConnection{"type.googleapis.com/google.cloud.servicenetworking.v1.Connection", result.clone()}
	default:
		op.Response = empty{"type.googleapis.com/google.protobuf.Empty"}
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

func (s *Server) getOperation(r *http.Request, _ *apiError) (any, *apiError) {
	name := "operations/" + r.PathValue("operation")
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[name]
	if !ok {
		return nil, notFound("Operation " + name)
	}
	return op, nil
}
