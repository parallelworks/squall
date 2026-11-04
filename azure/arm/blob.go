package arm

import (
	"bytes"
	"cmp"
	"encoding/xml"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/internal/stub"
)

const (
	// storageVersion is the x-ms-version answered to a request that names none.
	storageVersion = "2026-02-06"
	maxBlob        = 16 << 20
	maxListResults = 5000

	blobAction      = "Microsoft.Storage/storageAccounts/blobServices/containers/blobs/"
	containerAction = "Microsoft.Storage/storageAccounts/blobServices/containers/"
)

// Blob returns the Blob service of the server's storage accounts, an
// [http.Handler] to serve with [net/http/httptest]. It takes the account from
// a host name such as {account}.blob.core.windows.net, or else from the first
// path segment, as the storage emulator addresses accounts, so an azblob
// client reaches the account at {server URL}/{account}.
func (s *Server) Blob() http.Handler { return http.HandlerFunc(s.serveBlob) }

// blobCall is one Blob service request.
type blobCall struct {
	r                            *http.Request
	query                        url.Values
	account, container, blobName string
	// sharedKey calls are authorized with the account key rather than a token.
	sharedKey bool
}

// resource names what the call acts on, for faults and the policy.
func (c *blobCall) resource() string {
	return strings.TrimSuffix(strings.Join([]string{c.account, c.container, c.blobName}, "/"), "/")
}

type blobReply struct {
	status int
	header http.Header
	body   any
}

// blobOp is a Blob service operation.
type blobOp struct {
	// name is the operation's name in the REST API, such as PutBlob, which fault rules match.
	name string
	// needs returns the RBAC actions the call needs. The caller holds s.mu.
	needs func(s *Server, c *blobCall, acct *resource) []string
	h     func(s *Server, c *blobCall) (*blobReply, *apiError)
}

func always(actions ...string) func(*Server, *blobCall, *resource) []string {
	return func(*Server, *blobCall, *resource) []string { return actions }
}

var (
	listContainersOp = &blobOp{name: "ListContainers", needs: always(containerAction + "read"), h: (*Server).listContainersData}
	createContainer  = &blobOp{name: "CreateContainer", needs: always(containerAction + "write"), h: (*Server).createContainerData}
	deleteContainer  = &blobOp{name: "DeleteContainer", needs: always(containerAction + "delete"), h: (*Server).deleteContainerData}
	containerProps   = &blobOp{name: "GetContainerProperties", needs: always(containerAction + "read"), h: (*Server).containerProperties}
	listBlobsOp      = &blobOp{name: "ListBlobs", needs: listBlobsNeeds, h: (*Server).listBlobs}
	putBlobOp        = &blobOp{name: "PutBlob", needs: writeNeeds, h: (*Server).putBlob}
	putBlockOp       = &blobOp{name: "PutBlock", needs: always(blobAction + "write|" + blobAction + "add/action"), h: (*Server).putBlock}
	putBlockListOp   = &blobOp{name: "PutBlockList", needs: writeNeeds, h: (*Server).putBlockList}
	snapshotBlobOp   = &blobOp{name: "SnapshotBlob", needs: always(blobAction + "write"), h: (*Server).snapshotBlob}
	blobPropsOp      = &blobOp{name: "GetBlobProperties", needs: always(blobAction + "read"), h: (*Server).blobProperties}
	deleteBlobOp     = &blobOp{name: "DeleteBlob", needs: deleteNeeds, h: (*Server).deleteBlob}
)

// blobOperation returns the operation c asks for, or nil if it is not served.
func blobOperation(c *blobCall) *blobOp {
	restype, comp := c.query.Get("restype"), c.query.Get("comp")
	method := c.r.Method
	switch {
	case c.container == "":
		if method == http.MethodGet && comp == "list" && restype == "" {
			return listContainersOp
		}
	case c.blobName == "":
		if restype != "container" {
			return nil
		}
		switch {
		case method == http.MethodPut && comp == "":
			return createContainer
		case method == http.MethodDelete && comp == "":
			return deleteContainer
		case (method == http.MethodGet || method == http.MethodHead) && comp == "":
			return containerProps
		case method == http.MethodGet && comp == "list":
			return listBlobsOp
		}
	case restype == "":
		switch {
		case method == http.MethodPut && comp == "":
			return putBlobOp
		case method == http.MethodPut && comp == "block":
			return putBlockOp
		case method == http.MethodPut && comp == "blocklist":
			return putBlockListOp
		case method == http.MethodPut && comp == "snapshot":
			return snapshotBlobOp
		case method == http.MethodHead && comp == "":
			return blobPropsOp
		case method == http.MethodDelete && comp == "":
			return deleteBlobOp
		}
	}
	return nil
}

// parseBlobRequest finds the account, container and blob r names.
func parseBlobRequest(r *http.Request) *blobCall {
	c := &blobCall{r: r, query: r.URL.Query()}
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	if labels := strings.Split(host, "."); len(labels) > 2 && strings.EqualFold(labels[1], "blob") {
		c.account = strings.ToLower(labels[0])
	} else {
		c.account, path, _ = strings.Cut(path, "/")
	}
	escaped, rest, _ := strings.Cut(path, "/")
	c.container, _ = url.PathUnescape(escaped)
	c.blobName, _ = url.PathUnescape(rest)
	return c
}

func (s *Server) serveBlob(w http.ResponseWriter, r *http.Request) {
	c := parseBlobRequest(r)
	op := blobOperation(c)
	if op == nil {
		s.writeBlobError(w, r, newError(http.StatusNotFound, "NotImplemented", "squall: %s %s is not served", r.Method, r.URL.RequestURI()))
		return
	}
	if err := authenticate(c); err != nil {
		s.writeBlobError(w, r, err)
		return
	}
	needs, err := s.admit(c, op)
	if err != nil {
		s.writeBlobError(w, r, err)
		return
	}
	if !c.sharedKey && s.policy.Check(access.Call{Method: op.name, Resource: c.resource(), Needs: needs}) != nil {
		s.writeBlobError(w, r, newError(http.StatusForbidden, "AuthorizationPermissionMismatch",
			"This request is not authorized to perform this operation using this permission."))
		return
	}
	f, _ := s.faults.Decide(fault.Call{Method: op.name, Resource: c.resource()})
	reject := f.Mode == fault.ModeReject || f.Mode == fault.ModeFailOperation
	stub.Serve(w, r, f, reject, "application/xml", func() { s.writeBlobError(w, r, blobFaultError(f)) }, func() (func(), bool) {
		rep, err := op.h(s, c)
		if err != nil {
			return func() { s.writeBlobError(w, r, err) }, false
		}
		return func() { s.writeBlobReply(w, r, rep) }, true
	})
}

// authenticate reads how c is authorized: a bearer token, which is not
// checked, or Shared Key, whose signature is not.
func authenticate(c *blobCall) *apiError {
	auth := c.r.Header.Get("Authorization")
	switch {
	case strings.HasPrefix(auth, "Bearer "):
		return nil
	case strings.HasPrefix(auth, "SharedKey "):
		c.sharedKey = true
		return nil
	case c.query.Has("sig"):
		return unsupported("shared access signatures are not supported")
	}
	e := newError(http.StatusUnauthorized, "NoAuthenticationInformation",
		"Server failed to authenticate the request. Please refer to the information in the www-authenticate header.")
	e.Header = http.Header{"Www-Authenticate": {"Bearer authorization_uri=https://login.microsoftonline.com/common/oauth2/authorize resource_id=https://storage.azure.com"}}
	return e
}

// admit checks that c's account exists and takes the call, and returns the
// RBAC actions it needs.
func (s *Server) admit(c *blobCall, op *blobOp) ([]string, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acct := s.accountNamed(c.account)
	if acct == nil {
		return nil, newError(http.StatusNotFound, "ResourceNotFound", "The specified resource does not exist.")
	}
	if !reachable(acct, c.r) {
		return nil, newError(http.StatusForbidden, "AuthorizationFailure", "This request is not authorized to perform this operation.")
	}
	if allowed, set := boolean(acct.props, "allowSharedKeyAccess"); c.sharedKey && set && !allowed {
		return nil, newError(http.StatusForbidden, "KeyBasedAuthenticationNotPermitted", "Key based authentication is not permitted on this storage account.")
	}
	return op.needs(s, c, acct), nil
}

// reachable reports whether the account's network rules let r through: its
// public network access is not Disabled, and when networkAcls deny by
// default, r comes from an address an IP rule allows.
func reachable(acct *resource, r *http.Request) bool {
	if strings.EqualFold(str(acct.props, "publicNetworkAccess"), "Disabled") {
		return false
	}
	acls, _ := acct.props["networkAcls"].(map[string]any)
	if !strings.EqualFold(str(acls, "defaultAction"), "Deny") {
		return true
	}
	from, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	rules, _ := acls["ipRules"].([]any)
	for _, rule := range rules {
		m, _ := rule.(map[string]any)
		if a := str(m, "action"); a != "" && !strings.EqualFold(a, "Allow") {
			continue
		}
		value := str(m, "value")
		if p, err := netip.ParsePrefix(value); err == nil && p.Contains(from.Addr().Unmap()) {
			return true
		}
		if a, err := netip.ParseAddr(value); err == nil && a == from.Addr().Unmap() {
			return true
		}
	}
	return false
}

// blobFaultError is the error a fault answers with. The fault package's
// generic reasons become the Blob service's InternalError and ServerBusy.
func blobFaultError(f fault.Fault) *apiError {
	status := cmp.Or(f.Status, http.StatusServiceUnavailable)
	code := f.Reason
	switch code {
	case "", "backendError":
		code = "InternalError"
		if status == http.StatusServiceUnavailable {
			code = "ServerBusy"
		}
	case "rateLimitExceeded":
		code, status = "ServerBusy", http.StatusServiceUnavailable
	}
	return &apiError{Status: status, Code: code, Message: fmt.Sprintf("squall: injected fault (%s)", f)}
}

// storageHeaders sets the headers every Blob service answer carries.
func (s *Server) storageHeaders(h http.Header, r *http.Request) {
	h.Set("x-ms-request-id", fmt.Sprintf("%08x-0000-4000-8000-%012x", 0, s.requests.Add(1)))
	h.Set("x-ms-version", cmp.Or(r.Header.Get("x-ms-version"), storageVersion))
	h.Set("Date", s.now().UTC().Format(http.TimeFormat))
	if id := r.Header.Get("x-ms-client-request-id"); id != "" {
		h.Set("x-ms-client-request-id", id)
	}
}

type storageError struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

func (s *Server) writeBlobError(w http.ResponseWriter, r *http.Request, e *apiError) {
	h := w.Header()
	maps.Copy(h, e.Header)
	s.storageHeaders(h, r)
	h.Set("x-ms-error-code", e.Code)
	if r.Method == http.MethodHead {
		w.WriteHeader(e.Status)
		return
	}
	msg := fmt.Sprintf("%s\nRequestId:%s\nTime:%s", e.Message, h.Get("x-ms-request-id"), h.Get("Date"))
	writeXML(w, e.Status, storageError{Code: e.Code, Message: msg})
}

func (s *Server) writeBlobReply(w http.ResponseWriter, r *http.Request, rep *blobReply) {
	h := w.Header()
	maps.Copy(h, rep.header)
	s.storageHeaders(h, r)
	if rep.body == nil {
		w.WriteHeader(rep.status)
		return
	}
	writeXML(w, rep.status, rep.body)
}

func writeXML(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	if err := xml.NewEncoder(&buf).Encode(v); err != nil {
		panic(fmt.Sprintf("squall: encoding a response: %v", err))
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	// Once the status is sent, a client that went away is all a write error can mean.
	_, _ = w.Write(buf.Bytes())
}
