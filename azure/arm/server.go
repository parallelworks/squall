// Package arm is a stand-in for Azure Resource Manager, for testing code that
// calls it through the official Azure SDK for Go.
//
// [Server] is an [http.Handler] that keeps resources in memory and answers the
// REST calls the SDK's arm* client packages make. Serve it with
// [net/http/httptest] and point a client at it by giving the client options a
// cloud configuration whose ResourceManager endpoint is the server's URL, with
// any token credential and InsecureAllowCredentialWithHTTP. A
// [fault.Injector] makes chosen calls fail, including after they took effect,
// so tests can check that code which retries converges.
//
// It covers:
//
//   - resource groups: get, check existence, create or update, list (with
//     tag filters) and delete, which deletes everything in the group that
//     nothing outside it uses;
//   - Microsoft.Network: network security groups, network interfaces, public
//     IP addresses and private endpoints (create or update, update tags, get,
//     delete), and the IP configurations and public IP addresses of scale set
//     VMs (get);
//   - Microsoft.Network private DNS: zones and virtual network links in the
//     global location (create or update, get; another location fails with
//     MissingRegistrationForLocation), and A record sets (create or update,
//     get, delete);
//   - Microsoft.Compute virtual machines: create or update, get, list in a
//     group, instance view, deallocate, start and delete;
//   - Microsoft.Compute scale sets in the Uniform orchestration mode: create
//     or update, update, get, list in a group, delete and delete instances,
//     and their VMs (list, get);
//   - Microsoft.Compute resource SKUs: list, which lists none;
//   - Microsoft.Storage: storage accounts (create, get, list in a group or
//     subscription, list keys and delete), name availability checks, the
//     blob and file services' properties (set and get), and blob containers
//     and file shares (create, get, list and delete).
//
// Other calls answer 404. Every call needs an api-version, which may be any
// value, and a bearer token, which is not checked. Where it differs from
// Azure:
//
//   - Long-running operations finish before the call that starts them
//     returns: a create answers 201, an update 200, with provisioningState
//     Succeeded, and an action or delete answers 200 with no polling header,
//     so the SDK's pollers are done at once. [WithSettleAfter] holds changes in
//     progress instead: the call answers 200, 201 or 202 with an
//     Azure-AsyncOperation URL the server answers (and, for an action or
//     delete, a Location URL), and the resource shows Creating, Updating or
//     Deleting, for more reads of it or its operation, or until
//     [Server.Settle]. Polling URLs carry a retry-after-ms of 10, so the SDK
//     polls quickly. Deleting a resource group answers 202 with a Location
//     URL only, and a private DNS zone or link change in progress answers 202
//     with an empty object, as Azure does.
//   - A change to a resource that has another in progress is answered as its
//     provider answers it. Network security groups, network interfaces and
//     public IP addresses fail it with 429 RetryableError
//     (RetryableErrorDueToAnotherOperation) and virtual network links with 409
//     Conflict, counting the call as a read of the change in progress. A VM
//     or scale set change preempts the one in progress, which takes effect
//     and ends Canceled with OperationPreempted, unless that one is a
//     deletion: then a delete answers with it and other changes fail with 409
//     OperationNotAllowed. Other changes in progress, such as a zone's, and
//     those to resources a call references, finish first. A change to a
//     resource group being deleted, or to anything in it, fails with
//     ResourceGroupBeingDeleted.
//   - Deleting a resource group deletes each resource in it once nothing uses
//     it. When something outside the group still uses a resource in it, such
//     as a VM in another group attached to one of its network interfaces, or
//     an interface elsewhere that references its network security group or
//     public IP address, the deletion deletes the rest and its operation fails
//     with 409 ResourceGroupDeletionBlocked, leaving the group Succeeded with
//     the resources in use and what they use. The error has a detail for each
//     resource left, whose target is its ID and whose message is the error
//     deleting it alone answers, such as NicInUse, as Azure reports it. The
//     status, and that everything else is deleted before the operation
//     fails, are inferred.
//   - Resource names, subscription IDs and locations are not validated, and
//     names match without regard to case. Tags are not validated. Fields the
//     server only records keep the JSON the caller sent; fields it does not
//     know are rejected with InvalidRequestContent, and values it does not
//     model, such as a Flexible scale set, with BadRequest.
//   - Virtual networks and subnets are not modeled: subnet and virtual network
//     IDs are recorded, not checked. Private IP addresses come from
//     10.0.0.0/8 ([WithPrivateRange]), unique within a subnet ID, without the
//     range's first four and last addresses, which Azure reserves in a
//     subnet; public ones from 100.64.0.0/10 ([WithPublicRange]), unique
//     across the server, assigned when the address is created whatever its
//     allocation method. A change that needs more addresses than the range
//     has free fails with SubnetIsFull or PublicIPCountLimitReached, the
//     codes Azure answers for a full subnet and the public IP address quota.
//     Network security groups list no default rules.
//   - A virtual machine takes existing network interfaces only, not
//     networkInterfaceConfigurations. Its OS disk gets a name and a managed
//     disk ID, with the group's name upper-cased as Azure writes it, but disks
//     are not served. Deleting it deletes the interfaces whose deleteOption is
//     Delete and detaches the rest.
//   - A scale set's VMs are numbered from 0 and never reuse a number. Raising
//     sku.capacity creates VMs, each with the network interfaces and public
//     IP addresses its profile describes; lowering it deletes the newest,
//     whatever the scaleInPolicy. Overprovisioning is not modeled. A new
//     scale set needs a sku with a name. One created without a capacity
//     starts with no VMs, as a Flexible one does in Azure; for Uniform, both
//     are inferred. A PUT to an existing one that leaves out sku keeps its
//     sku and capacity. One that gives a sku without a capacity fails, as
//     Azure answers one that changes the model, unless it repeats the create
//     of a scale set made without a capacity and nothing has changed its
//     model since: then it succeeds and changes nothing, so a retry after a
//     lost response converges. That exception is inferred from ARM PUTs being
//     idempotent. A PUT that changes the VM profile marks existing VMs
//     latestModelApplied false, and one that changes computerNamePrefix fails
//     with PropertyChangeNotAllowed. A PATCH takes sku and tags only.
//     Deleting instances that do not exist fails with InvalidParameter and
//     deletes nothing. Scale set VMs are always running.
//   - Resource SKUs list none: a caller that checks availability sees no
//     restrictions.
//   - A storage account create that finishes at once answers 200, as Azure
//     Storage does, and one in progress answers 202 with an empty body and
//     only a Location URL, under Microsoft.Storage/operations, which answers
//     200 with the account once it is done. Account names are unique across
//     the server, as they are across Azure: taking one another group holds
//     fails with 409 StorageAccountInAnotherResourceGroup in the same
//     subscription and StorageAccountAlreadyTaken in another, and a name that
//     is not 3 to 24 lower-case letters and digits fails with
//     AccountNameInvalid. Setting blob service properties keeps the ones the
//     call leaves out, inferred from the Blob service's own Set Blob Service
//     Properties; setting file service properties replaces them all, also
//     inferred. Account keys are derived from the account and never change.
//     Blob and file data are not served.
//   - A private endpoint takes one private link service connection, whose
//     target is recorded, not checked. A new one gets a network interface
//     with an address in its subnet, named customNetworkInterfaceName or
//     {endpoint}.nic.{guid}, without the endpoint's tags, as Azure makes it;
//     deleting the endpoint deletes it. The interface can be deleted on its
//     own, which Azure refuses. An automatic connection
//     (privateLinkServiceConnections) is Approved and a manual one Pending.
//
// Fault method names are the operation's Azure RBAC action, such as
// "Microsoft.Compute/virtualMachines/write",
// "Microsoft.Compute/virtualMachines/deallocate/action" or
// "Microsoft.Resources/subscriptions/resourceGroups/delete". A call's
// resource is the resource ID in its path, as the caller spelled it, or for a
// list, the collection. [fault.ModeFailOperation] answers a long-running call
// with an operation that ends in the fault's error without the call taking
// effect, whatever [WithSettleAfter] says; other calls fail as
// [fault.ModeReject] does. A fault's Reason is the error code, except that the
// fault package's generic reasons become the code ARM uses, such as
// ServiceUnavailable or SubscriptionRequestsThrottled. The SDK retries 408,
// 429 and 5xx answers, and keeps polling a resource group deletion whose
// Location answers one, so fail that operation with a status such as 409.
//
// [WithPolicy] grants the caller a set of RBAC actions and refuses a call
// that needs one it lacks with 403 AuthorizationFailed, before any fault. A
// write that references another resource also needs the action ARM checks on
// it, such as Microsoft.Network/virtualNetworks/subnets/join/action for a
// network interface's subnet or Microsoft.Network/networkInterfaces/join/action
// for a VM's interface, and fails without it with LinkedAuthorizationFailed,
// naming each action and resource it lacks. A private endpoint with an
// automatic connection needs the target's privateEndpointConnectionsApproval
// action, such as
// Microsoft.Storage/storageAccounts/privateEndpointConnectionsApproval/action,
// which Azure documents as needed for automatic approval; that it refuses
// the create without it, rather than leaving the connection pending, is
// inferred from the LinkedAuthorizationFailed it reports for other services. Polling a Microsoft.Compute
// operation result needs Microsoft.Compute/locations/operations/read, as
// Microsoft.Compute defines no operationResults action, and polling a
// Microsoft.Storage operation needs Microsoft.Storage/operations/read. Actions are not
// checked against the scope they act on: a grant covers every subscription.
//
// A lost response aborts the handler with [http.ErrAbortHandler] after
// sending the response header, so the [http.Server] serving it closes the
// connection. Calling ServeHTTP directly propagates that panic. A HEAD's
// header is its whole answer, so a lost one instead ends the connection
// inside its status line.
package arm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/internal/stub"
)

const (
	maxBody       = 1 << 20
	maxOperations = 10_000
	retryAfterMS  = "10"

	stateSucceeded = "Succeeded"
	stateCreating  = "Creating"
	stateUpdating  = "Updating"
	stateDeleting  = "Deleting"
)

// Server is a stand-in for Azure Resource Manager. Create one with [New]. It
// is safe for concurrent use.
type Server struct {
	faults      *fault.Injector
	policy      *access.Policy
	now         func() time.Time
	settleAfter int
	routes      []route

	mu         sync.Mutex
	seq        uint64
	groups     map[string]*group
	resources  map[string]*resource
	ops        map[string]*operation
	opOrder    []string
	opNext     int
	pending    map[*operation]struct{}
	privateIPs map[string]map[netip.Addr]string
	publicIPs  map[netip.Addr]string
	private    *pool
	public     *pool
}

// Option configures a [Server].
type Option func(*Server)

// WithFaults makes the server ask in which calls to fail.
func WithFaults(in *fault.Injector) Option {
	return func(s *Server) { s.faults = in }
}

// WithClock sets the clock for creation and operation times. The default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}

// WithSettleAfter sets how many reads see a change in progress before it
// finishes: gets and lists that return the resource, polls of its operation,
// and calls turned away because of it. The default, 0, finishes every change
// before the call that makes it returns, with no operation to poll. A
// negative n finishes changes only on [Server.Settle].
func WithSettleAfter(n int) Option {
	return func(s *Server) { s.settleAfter = n }
}

// New returns an empty server.
func New(opts ...Option) *Server {
	s := &Server{
		now:        time.Now,
		groups:     map[string]*group{},
		resources:  map[string]*resource{},
		ops:        map[string]*operation{},
		pending:    map[*operation]struct{}{},
		privateIPs: map[string]map[netip.Addr]string{},
		publicIPs:  map[netip.Addr]string{},
		// Neither range is routable on the internet, so a test cannot reach a real host by mistake.
		private: newPool(netip.MustParsePrefix("10.0.0.0/8"), 4, 1),
		public:  newPool(netip.MustParsePrefix("100.64.0.0/10"), 1, 0),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.registerRoutes()
	return s
}

// handler answers one call.
type handler func(c *call) (*reply, *apiError)

type route struct {
	method   string
	segments []string
	action   string
	lro      bool
	h        handler
}

// call is one request matched to a route.
type call struct {
	r    *http.Request
	vals map[string]string
	// opFault, when set, is the error the call's operation must end in, without the call taking effect.
	opFault *apiError
}

func (c *call) val(name string) string { return c.vals[name] }

// base is the scheme and host polling URLs point at.
func (c *call) base() string {
	scheme := "http"
	if c.r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + c.r.Host
}

func (c *call) query(name string) string { return c.r.URL.Query().Get(name) }

type reply struct {
	status int
	header http.Header
	body   json.RawMessage
}

func ok(body json.RawMessage) *reply { return &reply{status: http.StatusOK, body: body} }

const (
	sub     = "subscriptions/{sub}"
	rg      = sub + "/resourceGroups/{rg}"
	compute = rg + "/providers/Microsoft.Compute"
	network = rg + "/providers/Microsoft.Network"
)

func (s *Server) registerRoutes() {
	const groupAction = "Microsoft.Resources/subscriptions/resourceGroups/"
	s.add("GET", sub+"/resourcegroups", groupAction+"read", false, s.listGroups)
	s.add("GET", rg, groupAction+"read", false, s.getGroup)
	s.add("HEAD", rg, groupAction+"read", false, s.checkGroup)
	s.add("PUT", rg, groupAction+"write", false, s.putGroup)
	s.add("DELETE", rg, groupAction+"delete", true, s.deleteGroup)
	s.add("GET", sub+"/operationresults/{op}", "Microsoft.Resources/subscriptions/operationresults/read", false, s.operationResult)

	for _, ns := range []string{"Microsoft.Compute", "Microsoft.Network"} {
		s.add("GET", sub+"/providers/"+ns+"/locations/{loc}/operations/{op}", ns+"/locations/operations/read", false, s.operationStatus)
		s.add("GET", sub+"/providers/"+ns+"/locations/{loc}/operationResults/{op}", ns+"/locations/operationResults/read", false, s.operationResult)
	}
	s.add("GET", network+"/privateDnsOperationStatuses/{op}", "Microsoft.Network/privateDnsOperationStatuses/read", false, s.operationStatus)
	s.add("GET", network+"/privateDnsOperationResults/{op}", "Microsoft.Network/privateDnsOperationResults/read", false, s.operationResult)

	for _, k := range []*kind{nsgKind, nicKind, pipKind, peKind} {
		item := network + "/" + k.plural + "/{name}"
		s.add("PUT", item, k.action("write"), true, s.putNetwork(k))
		s.add("PATCH", item, k.action("write"), false, s.patchTags(k))
		s.add("GET", item, k.action("read"), false, s.getResource(k))
		s.add("DELETE", item, k.action("delete"), true, s.deleteNetwork(k))
	}

	zone := network + "/privateDnsZones/{zone}"
	s.add("PUT", zone, zoneKind.action("write"), true, s.putZone)
	s.add("GET", zone, zoneKind.action("read"), false, s.getZone)
	s.add("PUT", zone+"/virtualNetworkLinks/{name}", linkKind.action("write"), true, s.putLink)
	s.add("GET", zone+"/virtualNetworkLinks/{name}", linkKind.action("read"), false, s.getLink)
	s.add("PUT", zone+"/A/{name}", recordKind.action("write"), false, s.putRecord)
	s.add("GET", zone+"/A/{name}", recordKind.action("read"), false, s.getRecord)
	s.add("DELETE", zone+"/A/{name}", recordKind.action("delete"), false, s.deleteRecord)

	vm := compute + "/virtualMachines/{name}"
	s.add("GET", compute+"/virtualMachines", vmKind.action("read"), false, s.listVMs)
	s.add("PUT", vm, vmKind.action("write"), true, s.putVM)
	s.add("GET", vm, vmKind.action("read"), false, s.getVM)
	s.add("GET", vm+"/instanceView", vmKind.action("instanceView/read"), false, s.vmInstanceView)
	s.add("POST", vm+"/deallocate", vmKind.action("deallocate/action"), true, s.powerVM(powerDeallocated))
	s.add("POST", vm+"/start", vmKind.action("start/action"), true, s.powerVM(powerRunning))
	s.add("DELETE", vm, vmKind.action("delete"), true, s.deleteVM)

	vmss := compute + "/virtualMachineScaleSets/{name}"
	s.add("GET", compute+"/virtualMachineScaleSets", vmssKind.action("read"), false, s.listScaleSets)
	s.add("PUT", vmss, vmssKind.action("write"), true, s.putScaleSet)
	s.add("PATCH", vmss, vmssKind.action("write"), true, s.patchScaleSet)
	s.add("GET", vmss, vmssKind.action("read"), false, s.getScaleSet)
	s.add("DELETE", vmss, vmssKind.action("delete"), true, s.deleteScaleSet)
	s.add("POST", vmss+"/delete", vmssKind.action("delete/action"), true, s.deleteInstances)
	s.add("GET", vmss+"/virtualMachines", vmssKind.action("virtualMachines/read"), false, s.listInstances)
	s.add("GET", vmss+"/virtualMachines/{index}", vmssKind.action("virtualMachines/read"), false, s.getInstance)
	ipConfig := vmss + "/virtualMachines/{index}/networkInterfaces/{nic}/ipConfigurations/{ip}"
	s.add("GET", ipConfig, vmssKind.action("virtualMachines/networkInterfaces/ipConfigurations/read"), false, s.getInstanceIPConfig)
	s.add("GET", ipConfig+"/publicIPAddresses/{pip}", vmssKind.action("virtualMachines/networkInterfaces/ipConfigurations/publicIPAddresses/read"), false, s.getInstancePublicIP)

	s.add("GET", sub+"/providers/Microsoft.Compute/skus", "Microsoft.Compute/skus/read", false, s.listSKUs)
	s.registerStorageRoutes()
}

func (s *Server) add(method, pattern, action string, lro bool, h handler) {
	s.routes = append(s.routes, route{method: method, segments: strings.Split(pattern, "/"), action: action, lro: lro, h: h})
}

// match finds the route for r and the values of its wildcards.
func (s *Server) match(r *http.Request, segments []string) (*route, map[string]string) {
	for i := range s.routes {
		if s.routes[i].method != r.Method {
			continue
		}
		if v, ok := matchSegments(s.routes[i].segments, segments); ok {
			return &s.routes[i], v
		}
	}
	return nil, nil
}

func matchSegments(pattern, segments []string) (map[string]string, bool) {
	if len(pattern) != len(segments) {
		return nil, false
	}
	vals := map[string]string{}
	for i, p := range pattern {
		if name, ok := strings.CutPrefix(p, "{"); ok {
			if segments[i] == "" {
				return nil, false
			}
			vals[strings.TrimSuffix(name, "}")] = segments[i]
			continue
		}
		if !strings.EqualFold(p, segments[i]) {
			return nil, false
		}
	}
	return vals, true
}

// ServeHTTP answers an Azure Resource Manager REST call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		writeError(w, newError(http.StatusUnauthorized, "AuthenticationFailed", "Authentication failed. The 'Authorization' header is missing."))
		return
	}
	if r.URL.Query().Get("api-version") == "" {
		writeError(w, newError(http.StatusBadRequest, "MissingApiVersionParameter",
			"The api-version query parameter (?api-version=) is required for all requests."))
		return
	}
	path := strings.Trim(r.URL.Path, "/")
	rt, vals := s.match(r, strings.Split(path, "/"))
	if rt == nil {
		writeError(w, newError(http.StatusNotFound, "NotFound", "squall: %s /%s is not served", r.Method, path))
		return
	}
	resource := "/" + path
	if err := s.authorize(rt, r, resource); err != nil {
		writeError(w, err)
		return
	}
	f, _ := s.faults.Decide(fault.Call{Method: rt.action, Resource: resource})
	reject := f.Mode == fault.ModeReject || f.Mode == fault.ModeFailOperation && !rt.lro
	stub.Serve(w, r, f, reject, "application/json; charset=utf-8", func() { writeError(w, faultError(f)) }, func() (func(), bool) {
		c := &call{r: r, vals: vals}
		if f.Mode == fault.ModeFailOperation {
			c.opFault = faultError(f)
		}
		rep, err := rt.h(c)
		if err != nil {
			return func() { writeError(w, err) }, false
		}
		return func() { writeJSON(w, rep.status, rep.header, rep.body) }, true
	})
}

// newUUID returns a fresh UUID-shaped ID. The caller holds s.mu.
func (s *Server) newUUID() string {
	s.seq++
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", s.seq>>48, s.seq&(1<<48-1))
}

// newHex returns a fresh 32-digit hex ID, as Azure suffixes disk names. The caller holds s.mu.
func (s *Server) newHex() string {
	s.seq++
	return fmt.Sprintf("%032x", s.seq)
}

func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// readObject reads a JSON object from the request body, keeping numbers as
// the caller wrote them.
func readObject(r *http.Request) (map[string]any, *apiError) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return nil, badRequest("reading the request body: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, invalidContent("", err)
	}
	if m == nil {
		return nil, badRequest("The request body must be a JSON object.")
	}
	return m, nil
}

// checkMembers rejects a key of m that is not in known, as ARM rejects members
// it does not know. path names m in the request, for the message.
func checkMembers(m map[string]any, typeName, path string, known ...string) *apiError {
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !slices.Contains(known, k) {
			return unknownMember(k, typeName, strings.TrimPrefix(path+"."+k, "."))
		}
	}
	return nil
}

// object returns m[key] as an object, or nil when it is absent or null.
func object(m map[string]any, key string) (map[string]any, *apiError) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	o, ok := v.(map[string]any)
	if !ok {
		return nil, invalidContent(key, fmt.Errorf("expected an object, not %T", v))
	}
	return o, nil
}

// objects returns m[key] as an array of objects.
func objects(m map[string]any, key string) ([]map[string]any, *apiError) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, invalidContent(key, fmt.Errorf("expected an array, not %T", v))
	}
	out := make([]map[string]any, 0, len(arr))
	for i, e := range arr {
		o, ok := e.(map[string]any)
		if !ok {
			return nil, invalidContent(fmt.Sprintf("%s[%d]", key, i), fmt.Errorf("expected an object, not %T", e))
		}
		out = append(out, o)
	}
	return out, nil
}

// str returns m[key] as a string, or "" when it is absent or not a string.
func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func boolean(m map[string]any, key string) (value, set bool) {
	v, ok := m[key].(bool)
	return v, ok
}

// refID returns the id of the reference object m[key], such as {"id": "..."}.
func refID(m map[string]any, key string) string {
	o, _ := m[key].(map[string]any)
	return str(o, "id")
}

func ref(id string) map[string]any { return map[string]any{"id": id} }

// tagsOf reads a resource's tags.
func tagsOf(m map[string]any) (map[string]string, *apiError) {
	raw, err := object(m, "tags")
	if err != nil || raw == nil {
		return nil, err
	}
	tags := make(map[string]string, len(raw))
	for k, v := range raw {
		switch v := v.(type) {
		case string:
			tags[k] = v
		case nil:
			tags[k] = ""
		default:
			return nil, invalidContent("tags."+k, fmt.Errorf("expected a string, not %T", v))
		}
	}
	return tags, nil
}

// normalizeLocation spells a location as ARM returns it: "East US" is "eastus".
func normalizeLocation(l string) string {
	return strings.ToLower(strings.ReplaceAll(l, " ", ""))
}

func key(id string) string { return strings.ToLower(id) }
