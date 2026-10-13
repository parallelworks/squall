package compute

import (
	"fmt"
	"net/http"
	"strings"
	"unicode"
)

// maxOperations bounds how many finished operations the server keeps for
// get and for requestId replay. The oldest are dropped first.
const maxOperations = 10_000

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

	requestID string
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
		op.Error = &operationErrors{[]operationError{{Code: upperSnake(failure.Reason), Message: failure.Message}}}
		op.HTTPErrorStatusCode = failure.Status
		op.HTTPErrorMessage = upperSnake(http.StatusText(failure.Status))
	}
	path := region + "/operations/" + name
	s.operations[path] = op
	if requestID != "" {
		op.requestID = requestID
		s.requests[requestID] = op
	}
	// opOrder is a ring of the kept operations' paths; opNext is the oldest once it is full.
	if len(s.opOrder) < maxOperations {
		s.opOrder = append(s.opOrder, path)
		return op
	}
	oldest := s.operations[s.opOrder[s.opNext]]
	delete(s.operations, s.opOrder[s.opNext])
	if oldest.requestID != "" && s.requests[oldest.requestID] == oldest {
		delete(s.requests, oldest.requestID)
	}
	s.opOrder[s.opNext] = path
	s.opNext = (s.opNext + 1) % maxOperations
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

// upperSnake spells a reason or status text as the API's error codes are:
// "backendError" and "Internal Server Error" become "BACKEND_ERROR" and
// "INTERNAL_SERVER_ERROR".
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
