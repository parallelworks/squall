package compute

import (
	"fmt"
	"net/http"
)

// operation is a region operation in the API's JSON shape. Operations finish
// before the call that started them returns.
type operation struct {
	Kind                string           `json:"kind"`
	ID                  uint64           `json:"id,string"`
	Name                string           `json:"name"`
	OperationType       string           `json:"operationType"`
	TargetLink          string           `json:"targetLink"`
	TargetID            uint64           `json:"targetId,string,omitzero"`
	Status              string           `json:"status"`
	Progress            int              `json:"progress"`
	InsertTime          string           `json:"insertTime"`
	StartTime           string           `json:"startTime"`
	EndTime             string           `json:"endTime"`
	Region              string           `json:"region"`
	SelfLink            string           `json:"selfLink"`
	Error               *operationErrors `json:"error,omitzero"`
	HTTPErrorStatusCode int              `json:"httpErrorStatusCode,omitzero"`
	HTTPErrorMessage    string           `json:"httpErrorMessage,omitzero"`
}

type operationErrors struct {
	Errors []operationError `json:"errors"`
}

type operationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// finishOperation records a done operation of kind opType on target, started
// by the call with requestID. A non-nil failure ends it in that error. The
// caller holds s.mu.
func (s *Server) finishOperation(requestID, region, opType, target string, targetID uint64, failure *apiError) *operation {
	id := s.newID()
	now := timestamp(s.now())
	name := fmt.Sprintf("operation-%d", id)
	op := &operation{
		Kind:          "compute#operation",
		ID:            id,
		Name:          name,
		OperationType: opType,
		TargetLink:    selfLinkBase + target,
		TargetID:      targetID,
		Status:        "DONE",
		Progress:      100,
		InsertTime:    now,
		StartTime:     now,
		EndTime:       now,
		Region:        selfLinkBase + region,
		SelfLink:      selfLinkBase + region + "/operations/" + name,
	}
	if failure != nil {
		op.Error = &operationErrors{[]operationError{{Code: failure.Reason, Message: failure.Message}}}
		op.HTTPErrorStatusCode = failure.Status
		op.HTTPErrorMessage = http.StatusText(failure.Status)
	}
	s.operations[region+"/operations/"+name] = op
	if requestID != "" {
		s.requests[requestID] = op
	}
	return op
}

func (s *Server) getOperation(r *http.Request, _ *apiError) (any, *apiError) {
	path := regionPath(r) + "/operations/" + r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[path]
	if !ok {
		return nil, notFound(path)
	}
	copied := *op
	return &copied, nil
}
