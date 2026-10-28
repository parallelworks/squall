// Package graph is a stand-in for the Microsoft Graph API, for testing code
// that calls it through the official Microsoft Graph SDK for Go.
//
// [Server] is an [http.Handler] that keeps a directory's users in memory and
// answers the v1.0 REST calls the SDK makes, at /v1.0 under the server's URL.
// Serve it with [net/http/httptest] and point a client at it by setting its
// request adapter's base URL to the server's URL with /v1.0 added, with an
// authentication provider that sends any bearer token. A [fault.Injector]
// makes chosen calls fail, including after they took effect, so tests can
// check that code which retries converges.
//
// It covers the organization (list, as [WithOrganization] sets it), domains
// (list, as [WithDomains] sets them), users (create, get by ID or user
// principal name, and delete) and deleted users (permanently delete). Other
// calls answer 404. Where it differs from Microsoft Graph:
//
//   - The bearer token is not checked; a call without one fails with 401
//     InvalidAuthenticationToken, which is inferred. Query parameters such as
//     $select and $filter are not supported and fail with 400. Request
//     bodies may be gzip-encoded, as the SDK sends them by default.
//   - A new user needs accountEnabled, displayName, mailNickname,
//     passwordProfile with a password, and userPrincipalName, which Graph
//     documents as required. Its user principal name must be alias@domain for
//     a verified domain of the directory's, with an alias of the characters
//     Microsoft Entra allows (A-Z, a-z, 0-9 and ' . - _ ! # ^ ~), and no other
//     user may hold it; its password must be 8 to 256 characters, of at least
//     three of lower case letters, upper case letters, digits and symbols. A
//     user that breaks one of those rules fails with 400 and the message
//     Microsoft Entra documents for it; the code, Request_BadRequest, is
//     inferred, as is the answer to a user missing a required property.
//     Members other than those, @odata.type and the user's other basic
//     profile properties fail with 400.
//   - Users show the properties Graph returns by default. Deleting one keeps
//     it among the deleted users, as Microsoft Entra keeps a deleted user for
//     30 days, which [Server.DeletedUsers] lists; a deleted user is not
//     served, and its user principal name is free again, where Microsoft
//     Entra may hold it until the user is gone for good. Request headers such
//     as Prefer and ConsistencyLevel are ignored.
//   - DELETE /directory/deletedItems/{id} permanently deletes a deleted user,
//     answering 204 as Graph documents, which [Server.PurgedUsers] then lists
//     instead of [Server.DeletedUsers]. An ID that is not a deleted user's,
//     including a user that is not deleted, fails with 404
//     Request_ResourceNotFound, the status and code recorded for the call; the
//     message is the one Graph answers for a missing user, which for this
//     call is inferred. Deleted users are not purged on their own after 30
//     days, and other deleted object types are not served.
//   - A directory's initial domain, which domains and the organization's
//     verified domains show with isInitial, is its onmicrosoft.com domain, as
//     Microsoft Entra documents, whichever domain is the default; that it is
//     the first such domain [WithDomains] sets is inferred.
//   - The organization is the one [WithOrganization] describes, with the
//     verified domains [WithDomains] sets. It shows the members Graph's
//     example of the call shows, with empty plans and notification lists;
//     each verified domain's capabilities, "Email, OfficeCommunicationsOnline"
//     as in that example, and type Managed are inferred.
//
// [WithPolicy] grants the caller a set of Microsoft Graph application
// permissions and refuses a call that needs one it lacks with 403
// Authorization_RequestDenied, before any fault. Each call needs one of the
// permissions Graph documents for it: listing domains Domain.Read.All,
// Domain.ReadWrite.All or Directory.Read.All; creating a user User.Create,
// User.ReadWrite.All or Directory.ReadWrite.All; reading one User.Read.All,
// User.ReadWrite.All, Directory.Read.All or Directory.ReadWrite.All; and
// deleting one User.ReadWrite.All; permanently deleting a deleted user
// User.DeleteRestore.All, the only permission Graph lists for it
// (Directory.ReadWrite.All is documented as not allowing user deletion); and
// reading the organization Organization.Read.All, Directory.Read.All,
// Organization.ReadWrite.All or Directory.ReadWrite.All.
//
// Fault method names are "graph." and the operation, such as
// "graph.users.create", "graph.users.get", "graph.users.delete" and
// "graph.domains.list", "graph.deletedItems.delete" and
// "graph.organization.list". A call's resource is its path after /v1.0, such as
// "/users" or "/users/{id}". Graph has no long-running operations, so
// [fault.ModeFailOperation] fails a call as [fault.ModeReject] does. A
// fault's Reason is the error code, except that the fault package's generic
// reasons become the status's name, such as ServiceUnavailable.
//
// A lost response aborts the handler with [http.ErrAbortHandler] after
// sending the response header, so the [http.Server] serving it closes the
// connection. Calling ServeHTTP directly propagates that panic.
package graph

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/internal/stub"
)

const (
	version = "/v1.0"
	maxBody = 1 << 20
)

// Domain is a domain of the directory's.
type Domain struct {
	// ID is the domain's name, such as contoso.com.
	ID                    string
	IsDefault, IsVerified bool
}

// User is a snapshot of a user the server holds.
type User struct {
	ID, UserPrincipalName, DisplayName, MailNickname string
	AccountEnabled                                   bool
	Created                                          time.Time
	// Deleted is when the user was deleted, or zero.
	Deleted time.Time
	// Purged is when the deleted user was permanently deleted, or zero.
	Purged time.Time
}

// Organization is the directory's tenant, as GET /organization lists it.
type Organization struct {
	// ID is the tenant ID.
	ID, DisplayName string
	// CountryLetterCode is the tenant's country, such as US.
	CountryLetterCode string
	// DefaultUsageLocation is the usage location new users get, such as US.
	DefaultUsageLocation string
}

// Server is a stand-in for Microsoft Graph. Create one with [New]. It is
// safe for concurrent use.
type Server struct {
	faults *fault.Injector
	policy *access.Policy
	now    func() time.Time

	mu      sync.Mutex
	seq     uint64
	org     Organization
	created time.Time
	domains []Domain
	users   map[string]*User
	deleted []*User
	purged  []*User
}

// Option configures a [Server].
type Option func(*Server)

// WithFaults makes the server ask in which calls to fail.
func WithFaults(in *fault.Injector) Option {
	return func(s *Server) { s.faults = in }
}

// WithPolicy makes the server refuse calls that need a permission p lacks,
// as Microsoft Graph refuses them, and records what each call needed.
// Without it every call is allowed.
func WithPolicy(p *access.Policy) Option {
	return func(s *Server) { s.policy = p }
}

// WithClock sets the clock for creation and deletion times. The default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}

// WithDomains sets the directory's domains. The default is one verified,
// default domain, squall.onmicrosoft.com.
func WithDomains(domains ...Domain) Option {
	return func(s *Server) { s.domains = slices.Clone(domains) }
}

// WithOrganization sets the directory's organization. A field left empty
// keeps its default: the tenant ID 00000000-0000-0000-0000-000000000001, the
// display name Squall, and US as the country and default usage location.
func WithOrganization(org Organization) Option {
	return func(s *Server) {
		s.org = Organization{
			ID: cmp.Or(org.ID, s.org.ID), DisplayName: cmp.Or(org.DisplayName, s.org.DisplayName),
			CountryLetterCode:    cmp.Or(org.CountryLetterCode, s.org.CountryLetterCode),
			DefaultUsageLocation: cmp.Or(org.DefaultUsageLocation, s.org.DefaultUsageLocation),
		}
	}
}

// New returns a directory with no users.
func New(opts ...Option) *Server {
	s := &Server{
		now:     time.Now,
		users:   map[string]*User{},
		domains: []Domain{{ID: "squall.onmicrosoft.com", IsDefault: true, IsVerified: true}},
		org: Organization{
			ID: "00000000-0000-0000-0000-000000000001", DisplayName: "Squall", CountryLetterCode: "US", DefaultUsageLocation: "US",
		},
	}
	for _, opt := range opts {
		opt(s)
	}
	s.created = s.now()
	return s
}

type route struct {
	method, pattern, name string
	// needs is the permissions the call needs one of.
	needs string
	h     func(c *call) (*reply, *apiError)
}

func (s *Server) routes() []route {
	return []route{
		{"GET", "/domains", "domains.list", "Domain.Read.All|Domain.ReadWrite.All|Directory.Read.All", s.listDomains},
		{"POST", "/users", "users.create", "User.Create|User.ReadWrite.All|Directory.ReadWrite.All", s.createUser},
		{"GET", "/users/{id}", "users.get", "User.Read.All|User.ReadWrite.All|Directory.Read.All|Directory.ReadWrite.All", s.getUser},
		{"DELETE", "/users/{id}", "users.delete", "User.ReadWrite.All", s.deleteUser},
		{"DELETE", "/directory/deletedItems/{id}", "deletedItems.delete", "User.DeleteRestore.All", s.purgeUser},
		{"GET", "/organization", "organization.list", "Organization.Read.All|Directory.Read.All|Organization.ReadWrite.All|Directory.ReadWrite.All", s.listOrganization},
	}
}

type call struct {
	r    *http.Request
	path string
	id   string
}

type reply struct {
	status int
	body   any
}

// match finds the route for method and path, with the value of its {id}.
func (s *Server) match(method, path string) (*route, string) {
	for _, rt := range s.routes() {
		if rt.method != method {
			continue
		}
		prefix, wild := strings.CutSuffix(rt.pattern, "/{id}")
		if !wild && strings.EqualFold(path, prefix) {
			return &rt, ""
		}
		if id, ok := strings.CutPrefix(path, prefix+"/"); wild && ok && id != "" && !strings.Contains(id, "/") {
			return &rt, id
		}
	}
	return nil, ""
}

// ServeHTTP answers a Microsoft Graph REST call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		writeError(w, &apiError{Status: http.StatusUnauthorized, Code: "InvalidAuthenticationToken", Message: "Access token is empty."})
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, version)
	rt, id := s.match(r.Method, path)
	if !ok || rt == nil {
		writeError(w, &apiError{Status: http.StatusNotFound, Code: "NotFound", Message: fmt.Sprintf("squall: %s %s is not served", r.Method, r.URL.Path)})
		return
	}
	if len(r.URL.Query()) > 0 {
		writeError(w, badRequest("squall: query parameters are not supported"))
		return
	}
	if s.policy.Check(access.Call{Method: "graph." + rt.name, Resource: path, Needs: []string{rt.needs}}) != nil {
		writeError(w, &apiError{Status: http.StatusForbidden, Code: "Authorization_RequestDenied", Message: "Insufficient privileges to complete the operation."})
		return
	}
	f, _ := s.faults.Decide(fault.Call{Method: "graph." + rt.name, Resource: path})
	reject := f.Mode == fault.ModeReject || f.Mode == fault.ModeFailOperation
	stub.Serve(w, r, f, reject, "application/json", func() { writeError(w, faultError(f)) }, func() (func(), bool) {
		rep, err := rt.h(&call{r: r, path: path, id: id})
		if err != nil {
			return func() { writeError(w, err) }, false
		}
		return func() { writeJSON(w, rep.status, rep.body) }, true
	})
}

func (s *Server) listDomains(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := make([]map[string]any, 0, len(s.domains))
	initial := s.initialDomain()
	for _, d := range s.domains {
		value = append(value, map[string]any{
			"id": d.ID, "isDefault": d.IsDefault, "isVerified": d.IsVerified, "isInitial": d.ID == initial, "isRoot": true,
			"isAdminManaged": true, "authenticationType": "Managed", "availabilityStatus": nil, "supportedServices": []string{},
		})
	}
	return &reply{status: http.StatusOK, body: map[string]any{"@odata.context": context(c, "domains"), "value": value}}, nil
}

// initialDomain returns the directory's initial domain, its first
// onmicrosoft.com domain, or "" when it has none. The caller holds s.mu.
func (s *Server) initialDomain() string {
	for _, d := range s.domains {
		if strings.HasSuffix(strings.ToLower(d.ID), ".onmicrosoft.com") {
			return d.ID
		}
	}
	return ""
}

// userMembers are the user members a create may set: those it needs, and the
// basic profile properties.
var userMembers = []string{
	"@odata.type", "accountEnabled", "displayName", "mailNickname", "passwordProfile", "userPrincipalName",
	"givenName", "surname", "jobTitle", "mail", "mobilePhone", "officeLocation", "preferredLanguage", "businessPhones", "usageLocation",
}

func (s *Server) createUser(c *call) (*reply, *apiError) {
	body, err := readBody(c.r)
	if err != nil {
		return nil, badRequest("squall: reading the request body: %v", err)
	}
	var in map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&in); err != nil || in == nil {
		return nil, badRequest("squall: the request body must be a JSON object")
	}
	for _, k := range slices.Sorted(maps.Keys(in)) {
		if !slices.Contains(userMembers, k) {
			return nil, badRequest("squall: the user member %s is not supported", k)
		}
	}
	enabled, enabledSet := in["accountEnabled"].(bool)
	profile, _ := in["passwordProfile"].(map[string]any)
	display, nickname, upn := str(in, "displayName"), str(in, "mailNickname"), str(in, "userPrincipalName")
	password := str(profile, "password")
	for _, need := range []struct {
		name    string
		missing bool
	}{
		{"accountEnabled", !enabledSet},
		{"displayName", display == ""},
		{"mailNickname", nickname == ""},
		{"passwordProfile", password == ""},
		{"userPrincipalName", upn == ""},
	} {
		if need.missing {
			return nil, requestBadRequest("Invalid value specified for property '%s' of resource 'User'.", need.name)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkPrincipalName(upn); err != nil {
		return nil, err
	}
	if !complexPassword(password) {
		return nil, requestBadRequest("The specified password does not comply with password complexity requirements. Please provide a different password.")
	}
	s.seq++
	u := &User{
		ID: fmt.Sprintf("%08x-0000-4000-8000-%012x", s.seq>>48, s.seq&(1<<48-1)), UserPrincipalName: upn, DisplayName: display,
		MailNickname: nickname, AccountEnabled: enabled, Created: s.now(),
	}
	s.users[u.ID] = u
	out := render(u)
	out["@odata.context"] = context(c, "users/$entity")
	return &reply{status: http.StatusCreated, body: out}, nil
}

// readBody reads r's body, decompressing it when the SDK sent it with gzip,
// as it does by default.
func readBody(r *http.Request) ([]byte, error) {
	var body io.Reader = r.Body
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		z, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		defer z.Close()
		body = z
	}
	return io.ReadAll(io.LimitReader(body, maxBody))
}

// checkPrincipalName checks upn as Microsoft Entra checks a new user's. The caller holds s.mu.
func (s *Server) checkPrincipalName(upn string) *apiError {
	alias, domain, ok := strings.Cut(upn, "@")
	if !ok || alias == "" || strings.ContainsFunc(alias, func(r rune) bool { return !principalNameRune(r) }) {
		return requestBadRequest("Property userPrincipalName is invalid.")
	}
	if !slices.ContainsFunc(s.domains, func(d Domain) bool { return d.IsVerified && strings.EqualFold(d.ID, domain) }) {
		return requestBadRequest("The domain portion of the userPrincipalName property is invalid. You must use one of the verified domain names in your organization.")
	}
	for _, u := range s.users {
		if strings.EqualFold(u.UserPrincipalName, upn) {
			return requestBadRequest("Another object with the same value for property userPrincipalName already exists.")
		}
	}
	return nil
}

func principalNameRune(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune("'.-_!#^~", r)
}

// complexPassword reports whether p meets Microsoft Entra's password policy
// for cloud users: 8 to 256 characters, of at least three of lower case
// letters, upper case letters, digits and the symbols it allows.
func complexPassword(p string) bool {
	if len(p) < 8 || len(p) > 256 {
		return false
	}
	const symbols = "@#$%^&*-_!+=[]{}|\\:',.?/`~\"();<> "
	var lower, upper, digit, symbol int
	for _, r := range p {
		switch {
		case 'a' <= r && r <= 'z':
			lower = 1
		case 'A' <= r && r <= 'Z':
			upper = 1
		case '0' <= r && r <= '9':
			digit = 1
		case strings.ContainsRune(symbols, r):
			symbol = 1
		default:
			return false
		}
	}
	return lower+upper+digit+symbol >= 3
}

// user finds a user by ID or user principal name. The caller holds s.mu.
func (s *Server) user(ref string) (*User, *apiError) {
	if u := s.users[strings.ToLower(ref)]; u != nil {
		return u, nil
	}
	for _, u := range s.users {
		if strings.EqualFold(u.UserPrincipalName, ref) {
			return u, nil
		}
	}
	return nil, notFound(ref)
}

func notFound(ref string) *apiError {
	return &apiError{
		Status: http.StatusNotFound, Code: "Request_ResourceNotFound",
		Message: fmt.Sprintf("Resource '%s' does not exist or one of its queried reference-property objects are not present.", ref),
	}
}

func (s *Server) getUser(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, err := s.user(c.id)
	if err != nil {
		return nil, err
	}
	out := render(u)
	out["@odata.context"] = context(c, "users/$entity")
	return &reply{status: http.StatusOK, body: out}, nil
}

func (s *Server) deleteUser(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, err := s.user(c.id)
	if err != nil {
		return nil, err
	}
	delete(s.users, u.ID)
	u.Deleted = s.now()
	s.deleted = append(s.deleted, u)
	return &reply{status: http.StatusNoContent}, nil
}

// purgeUser permanently deletes a deleted user.
func (s *Server) purgeUser(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.deleted, func(u *User) bool { return strings.EqualFold(u.ID, c.id) })
	if i < 0 {
		return nil, notFound(c.id)
	}
	u := s.deleted[i]
	s.deleted = slices.Delete(s.deleted, i, i+1)
	u.Purged = s.now()
	s.purged = append(s.purged, u)
	return &reply{status: http.StatusNoContent}, nil
}

func (s *Server) listOrganization(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	domains := []map[string]any{}
	initial := s.initialDomain()
	for _, d := range s.domains {
		if d.IsVerified {
			domains = append(domains, map[string]any{
				"capabilities": "Email, OfficeCommunicationsOnline", "isDefault": d.IsDefault, "isInitial": d.ID == initial, "name": d.ID, "type": "Managed",
			})
		}
	}
	org := map[string]any{
		"id": s.org.ID, "deletedDateTime": nil, "businessPhones": []string{}, "city": nil, "country": nil,
		"countryLetterCode": s.org.CountryLetterCode, "createdDateTime": s.created.UTC().Format(time.RFC3339),
		"defaultUsageLocation": s.org.DefaultUsageLocation, "displayName": s.org.DisplayName,
		"isMultipleDataLocationsForServicesEnabled": nil, "marketingNotificationEmails": []string{}, "onPremisesLastSyncDateTime": nil,
		"onPremisesSyncEnabled": nil, "postalCode": nil, "preferredLanguage": "en", "securityComplianceNotificationMails": []string{},
		"securityComplianceNotificationPhones": []string{}, "state": nil, "street": nil, "technicalNotificationMails": []string{},
		"tenantType": "AAD", "assignedPlans": []any{}, "provisionedPlans": []any{}, "verifiedDomains": domains,
	}
	return &reply{status: http.StatusOK, body: map[string]any{"@odata.context": context(c, "organization"), "value": []any{org}}}, nil
}

// render shows the properties Graph returns for a user by default.
func render(u *User) map[string]any {
	return map[string]any{
		"id": u.ID, "businessPhones": []string{}, "displayName": u.DisplayName, "givenName": nil, "jobTitle": nil, "mail": nil,
		"mobilePhone": nil, "officeLocation": nil, "preferredLanguage": nil, "surname": nil, "userPrincipalName": u.UserPrincipalName,
	}
}

func context(c *call, what string) string {
	scheme := "http"
	if c.r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + c.r.Host + version + "/$metadata#" + what
}

// Users returns the users the directory holds, ordered by ID.
func (s *Server) Users() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []User
	for _, u := range s.users {
		out = append(out, *u)
	}
	slices.SortFunc(out, func(a, b User) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// DeletedUsers returns the users deleted from the directory, in the order they were deleted.
func (s *Server) DeletedUsers() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]User, 0, len(s.deleted))
	for _, u := range s.deleted {
		out = append(out, *u)
	}
	return out
}

// PurgedUsers returns the deleted users that were permanently deleted, in the order they were purged.
func (s *Server) PurgedUsers() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]User, 0, len(s.purged))
	for _, u := range s.purged {
		out = append(out, *u)
	}
	return out
}

func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// apiError is an error in the shape Microsoft Graph answers with.
type apiError struct {
	Status        int
	Code, Message string
}

func badRequest(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "BadRequest", Message: fmt.Sprintf(format, args...)}
}

func requestBadRequest(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "Request_BadRequest", Message: fmt.Sprintf(format, args...)}
}

func faultError(f fault.Fault) *apiError {
	status := cmp.Or(f.Status, http.StatusServiceUnavailable)
	code := f.Reason
	if code == "" || code == "backendError" || code == "rateLimitExceeded" {
		code = cmp.Or(strings.ReplaceAll(http.StatusText(status), " ", ""), "InternalServerError")
	}
	return &apiError{Status: status, Code: code, Message: fmt.Sprintf("squall: injected fault (%s)", f)}
}

func writeError(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.Status, map[string]any{"error": map[string]any{
		"code": e.Code, "message": e.Message,
		"innerError": map[string]any{"date": time.Now().UTC().Format("2006-01-02T15:04:05")},
	}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	if body == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status is already sent, so a client that went away is all an error here can mean.
	_ = json.NewEncoder(w).Encode(body)
}
