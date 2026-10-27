// Package ec2 is a stand-in for the Amazon EC2 API, for testing code that
// calls it through the official SDKs.
//
// [Server] is an [http.Handler] that keeps EC2 resources in memory and answers
// the EC2 Query API: form-encoded calls with an Action and Version 2016-11-15,
// answered in EC2's XML. Serve it with [net/http/httptest] and point a client
// at it with the SDK's base endpoint setting, or the AWS_ENDPOINT_URL_EC2 or
// AWS_ENDPOINT_URL environment variable, and any static credentials. A
// [fault.Injector] makes chosen calls fail, including after they took effect,
// so tests can check that code which retries converges.
//
// It covers Elastic IP addresses (AllocateAddress, DescribeAddresses,
// ReleaseAddress, AssociateAddress, DisassociateAddress), network interfaces
// (CreateNetworkInterface, DescribeNetworkInterfaces, DeleteNetworkInterface),
// instances (RunInstances, DescribeInstances, StartInstances, StopInstances,
// TerminateInstances, ModifyInstanceAttribute for the instance type and
// DeleteOnTermination of attached volumes), images
// (DescribeImages, RegisterImage, DeregisterImage), instance types
// (DescribeInstanceTypes), EBS volumes (CreateVolume, DescribeVolumes,
// DeleteVolume, AttachVolume, DetachVolume), EBS snapshots (CreateSnapshot,
// CopySnapshot, DescribeSnapshots, DeleteSnapshot), placement groups (CreatePlacementGroup, DescribePlacementGroups,
// DeletePlacementGroup) and tags (CreateTags, DeleteTags), with the real API's
// error codes. Other actions answer 404 with the code InvalidAction. Where it
// differs from EC2:
//
//   - Signatures are not checked. The region comes from the credential scope
//     of the SigV4 Authorization header (or the X-Amz-Credential query
//     parameter), and each region keeps its own resources. There is one
//     account: the server keeps no owner IDs.
//   - Subnet and security group IDs and key pair names are not validated, and
//     there are no VPCs: interfaces report no VPC ID, and their availability
//     zone is the region with "a" appended unless an instance launched in
//     another. ReleaseAddress fails with
//     InvalidIPAddress.InUse for an associated address, as in a nondefault
//     VPC, rather than disassociating it. Private IP addresses are unique
//     within a subnet and come from 10.0.0.0/8; public ones are unique across
//     every region and come from 100.64.0.0/10, never from routable ranges.
//   - There is no quota on addresses unless [WithAddressLimit] sets one, and
//     no capacity or instance limit: a fault rule with a code such as
//     InsufficientInstanceCapacity stands in for one.
//   - Each interface has one private IP address; an address associates with
//     it. Associating an address with an interface that already has another
//     disassociates the other, and associating it again with the interface it
//     is on returns its current association. AssociateAddress with an
//     InstanceId associates with the instance's primary interface, and fails
//     with InvalidInstanceID unless the instance is running or stopped; with
//     a NetworkInterfaceId it fails with IncorrectInstanceState while the
//     interface's instance is pending.
//   - A CreateNetworkInterface, RunInstances or CreateVolume that repeats a
//     ClientToken in the same region with the same parameters gets what the
//     first call created without taking effect again: the interface or volume
//     as first created, or the reservation's instances as they are now. With
//     other parameters it fails with IdempotentParameterMismatch. The server
//     keeps the last 10,000 tokens.
//   - Filter values take the * and ? wildcards. A filter name the server does
//     not support, like any parameter it does not support, is rejected with
//     400 rather than ignored.
//
// Instances:
//
//   - There is no default VPC: RunInstances needs a SubnetId or one network
//     interface at device index 0, either new or an existing available one,
//     and fails with VPCIdNotSpecified without. Instances are EBS-backed HVM
//     instances with one network interface; launch templates, spot options,
//     instance profiles and the like are rejected as unknown parameters.
//   - RunInstances records MetadataOptions, PrivateDnsNameOptions and
//     CpuOptions, with EC2's defaults for what it leaves out, and
//     DescribeInstances returns them. A resource-name hostname type makes the
//     private DNS name the instance ID's. The primary interface's
//     InterfaceType may be "interface" or "efa", or with an existing interface
//     only that interface's type, and its NetworkCardIndex only 0, which
//     counts as the same launch as leaving it out. Metadata options read
//     pending until the instance first settles. EFA and AMD SEV-SNP are not
//     checked against the instance type.
//     CpuOptions needs both CoreCount and ThreadsPerCore or neither, and
//     takes up to the type's default cores and threads per core, which
//     DescribeInstanceTypes reports. Any core count from 1 up to the default
//     is accepted, where EC2 accepts only the counts it lists for each type.
//     A type change resets the core count and threads to the new type's
//     defaults.
//   - ModifyInstanceAttribute changes one attribute per call: the instance
//     type, only while the instance is stopped, or the DeleteOnTermination
//     of volumes attached to it, in any state, which defaults to true when a
//     mapping leaves it out. A mapping's device must have a volume of the
//     instance attached, and its VolumeId, if set, must be that volume;
//     otherwise the call fails with InvalidBlockDeviceMapping and changes
//     nothing. Both attributes in one call fail with
//     InvalidParameterCombination.
//   - An instance's image must be registered with [Server.PutImage], and its
//     type must be one [Server.PutInstanceType] added or one of a small
//     built-in set, which DescribeInstanceTypes lists, that runs the image's
//     architecture. The default type is m1.small, as in EC2.
//   - Instances move through EC2's states, pending to running, stopping to
//     stopped, and shutting-down to terminated, when describes see them: by
//     default the first DescribeInstances after a change shows the state it
//     settles in, so the SDK's waiters return at once. [WithSettleAfter]
//     holds them in transition for more describes, or until [Server.Settle].
//     Describes are consistent at once; EC2's are eventually consistent.
//   - A terminated instance stays visible, as in EC2, until
//     [Server.ForgetInstance] removes it. On termination it releases its
//     interface and volumes and deletes those marked to delete on
//     termination: a new interface and the image's volumes by default, an
//     existing interface not.
//   - An instance launched with AssociatePublicIpAddress gets a public IP
//     address, released while it is stopped or has an Elastic IP address and
//     assigned again when it starts or the Elastic IP is disassociated.
//   - DescribeImages lists only registered images. ExecutableBy is not
//     supported; the owner "self" matches images with no OwnerID, and other
//     owners match the OwnerID or OwnerAlias. Unknown image IDs fail with
//     InvalidAMIID.NotFound.
//   - RegisterImage registers an EBS-backed HVM image of the caller's own
//     from snapshots the server holds: its root device needs a mapping with a
//     snapshot ID. The snapshot's state is not checked, and an image is
//     available at once. A mapping's volume type is gp2 unless set.
//     DeregisterImage removes any registered image, whatever its owner,
//     and leaves its snapshots.
//   - Volumes go from creating to available, and attachments from attaching
//     to attached and from detaching to detached, the same way instances
//     move. Snapshots go from pending to completed the same way, as
//     DescribeSnapshots sees them, and record the volume's size and
//     encryption, not its data. A volume from a snapshot the server holds is
//     the snapshot's size unless set; any other snapshot ID is recorded, not
//     read, and a volume from it without a size is 8 GiB. Volumes, snapshots
//     and placement groups are deleted at once rather than passing through a
//     deleting state. DeleteSnapshot fails with InvalidSnapshot.InUse while a
//     registered image maps the snapshot. Every snapshot is the caller's own:
//     DescribeSnapshots with an owner other than "self" lists none, and
//     snapshots report no owner ID. CreatePlacementGroup needs a GroupName
//     and a Strategy, which EC2 makes optional.
//   - CopySnapshot, called in the destination region, copies a completed
//     snapshot from SourceRegion, which may be the same region, into a new
//     snapshot that starts pending and completes as a new snapshot does. It
//     fails with InvalidSnapshot.NotFound when SourceRegion does not hold the
//     snapshot and with IncorrectState while the source is pending. The copy
//     has the source's size, the request's Description and tags, and the
//     volume ID vol-ffffffff, and it is independent of its source: deleting
//     either leaves the other. It is encrypted if the source is or Encrypted
//     is set; KmsKeyId, PresignedUrl and DestinationRegion are accepted and
//     ignored, and Outpost, Local Zone and time-based copies are not
//     supported.
//   - CreateTags and DeleteTags work on instances, volumes, snapshots,
//     images, placement groups (by group ID), network interfaces and Elastic
//     IP addresses, and change nothing unless every resource exists.
//
// [WithPolicy] grants the caller a set of IAM actions and refuses a call that
// needs one it lacks with 403 UnauthorizedOperation, before any fault. A call
// needs ec2: and its action, and ec2:CreateTags when it has a
// TagSpecification. Actions are not checked against the resources they act
// on or against condition keys: a grant covers every resource.
//
// Fault method names are "ec2." and the action, such as "ec2.AllocateAddress".
// A call's resource is the region and the ID it acts on, such as
// "us-east-1/eipalloc-00000000000000001", or the first of them for a call
// that takes several; a create call names the resource it creates (the first
// instance, for RunInstances), placement group calls name the group by name,
// and a describe call names the collection, such as "us-east-1/addresses".
// EC2 has no long-running operations, so [fault.ModeFailOperation] fails a
// call as [fault.ModeReject] does. A fault's
// Reason is the error code, except that the fault package's generic reasons
// become InternalError, Unavailable (for 503) and RequestLimitExceeded. A call
// a fault rejects has no effect, so a rejected describe does not move an
// instance along.
//
// A lost response aborts the handler with [http.ErrAbortHandler] after
// sending the response header, so the [http.Server] serving it closes the
// connection. Calling ServeHTTP directly propagates that panic.
package ec2

import (
	"encoding/xml"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/internal/stub"
)

const (
	apiVersion = "2016-11-15"
	namespace  = "http://ec2.amazonaws.com/doc/" + apiVersion + "/"

	maxBody   = 1 << 20
	maxTokens = 10_000
)

// Server is a stand-in for the Amazon EC2 API. Create one with [New]. It is
// safe for concurrent use.
type Server struct {
	faults       *fault.Injector
	policy       *access.Policy
	addressLimit int
	settleAfter  int
	actions      map[string]action
	requests     atomic.Uint64

	mu            sync.Mutex
	nextID        uint64
	regions       map[string]*regionState
	instanceTypes map[string]InstanceType
	publicIPs     map[string]string
	privateIPs    map[string]string
	nextPublic    netip.Addr
	nextLocal     netip.Addr
	tokens        map[string]*idempotent
	tokenOrder    []string
	tokenNext     int
}

type regionState struct {
	addresses       map[string]*Address
	interfaces      map[string]*NetworkInterface
	instances       map[string]*Instance
	images          map[string]*Image
	volumes         map[string]*Volume
	snapshots       map[string]*Snapshot
	placementGroups map[string]*PlacementGroup
}

// Option configures a [Server].
type Option func(*Server)

// WithFaults makes the server ask in which calls to fail.
func WithFaults(in *fault.Injector) Option {
	return func(s *Server) { s.faults = in }
}

// WithAddressLimit caps the Elastic IP addresses a region holds, as the
// account quota does. AllocateAddress past it fails with
// AddressLimitExceeded. The default, 0, is no limit.
func WithAddressLimit(n int) Option {
	return func(s *Server) { s.addressLimit = n }
}

// WithSettleAfter sets how many describes see an instance, volume or snapshot
// in a transitional state, such as pending or stopping, before one shows the
// state it settles in: DescribeInstances calls for an instance,
// DescribeVolumes calls for a volume and DescribeSnapshots calls for a
// snapshot, counting each call that names it or names no IDs. The default, 0,
// settles it on the first describe, so the SDK's waiters return at once. A
// negative n settles it only on [Server.Settle].
func WithSettleAfter(n int) Option {
	return func(s *Server) { s.settleAfter = n }
}

// New returns an empty server.
func New(opts ...Option) *Server {
	s := &Server{
		regions:    map[string]*regionState{},
		publicIPs:  map[string]string{},
		privateIPs: map[string]string{},
		// Neither range is routable on the internet, so a test cannot reach a real host by mistake.
		nextPublic:    netip.MustParseAddr("100.64.0.1"),
		nextLocal:     netip.MustParseAddr("10.0.0.4"),
		tokens:        map[string]*idempotent{},
		instanceTypes: maps.Clone(builtinInstanceTypes),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.actions = map[string]action{
		"AllocateAddress":           {s.allocateTarget, s.allocateAddress},
		"DescribeAddresses":         {collection("addresses"), s.describeAddresses},
		"ReleaseAddress":            {named("AllocationId"), s.releaseAddress},
		"AssociateAddress":          {named("AllocationId"), s.associateAddress},
		"DisassociateAddress":       {named("AssociationId"), s.disassociateAddress},
		"CreateNetworkInterface":    {s.createInterfaceTarget, s.createNetworkInterface},
		"DescribeNetworkInterfaces": {collection("network-interfaces"), s.describeNetworkInterfaces},
		"DeleteNetworkInterface":    {named("NetworkInterfaceId"), s.deleteNetworkInterface},
		"RunInstances":              {s.runTarget, s.runInstances},
		"DescribeInstances":         {collection("instances"), s.describeInstances},
		"StartInstances":            {named("InstanceId.1"), s.startInstances},
		"StopInstances":             {named("InstanceId.1"), s.stopInstances},
		"TerminateInstances":        {named("InstanceId.1"), s.terminateInstances},
		"ModifyInstanceAttribute":   {named("InstanceId"), s.modifyInstanceAttribute},
		"DescribeInstanceTypes":     {collection("instance-types"), s.describeInstanceTypes},
		"DescribeImages":            {collection("images"), s.describeImages},
		"RegisterImage":             {s.registerImageTarget, s.registerImage},
		"DeregisterImage":           {named("ImageId"), s.deregisterImage},
		"CreateVolume":              {s.createVolumeTarget, s.createVolume},
		"DescribeVolumes":           {collection("volumes"), s.describeVolumes},
		"DeleteVolume":              {named("VolumeId"), s.deleteVolume},
		"AttachVolume":              {named("VolumeId"), s.attachVolume},
		"DetachVolume":              {named("VolumeId"), s.detachVolume},
		"CreateSnapshot":            {s.createSnapshotTarget, s.createSnapshot},
		"DescribeSnapshots":         {collection("snapshots"), s.describeSnapshots},
		"DeleteSnapshot":            {named("SnapshotId"), s.deleteSnapshot},
		"CopySnapshot":              {s.createSnapshotTarget, s.copySnapshot},
		"CreatePlacementGroup":      {named("GroupName"), s.createPlacementGroup},
		"DescribePlacementGroups":   {collection("placement-groups"), s.describePlacementGroups},
		"DeletePlacementGroup":      {named("GroupName"), s.deletePlacementGroup},
		"CreateTags":                {named("ResourceId.1"), s.createTags},
		"DeleteTags":                {named("ResourceId.1"), s.deleteTags},
	}
	return s
}

type call struct {
	region string
	p      *params
	// id is the ID a create call's resource gets, chosen before the call's
	// fault is decided so the fault can name it.
	id string
}

// response is an action's answer. Every response embeds meta.
type response interface{ setRequestID(id string) }

type meta struct {
	RequestID string `xml:"requestId"`
}

func (m *meta) setRequestID(id string) { m.RequestID = id }

type returnResponse struct {
	meta
	Return bool `xml:"return"`
}

// action answers one EC2 action. target names the call's resource for the
// fault injector.
type action struct {
	target func(c *call) string
	handle func(c *call) (response, *apiError)
}

func named(param string) func(*call) string {
	return func(c *call) string { return c.region + "/" + c.p.values.Get(param) }
}

func collection(name string) func(*call) string {
	return func(c *call) string { return c.region + "/" + name }
}

// ServeHTTP answers an EC2 Query API call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := s.requestID()
	name, a, c, err := s.parse(w, r)
	if err != nil {
		writeError(w, requestID, err)
		return
	}
	resource := a.target(c)
	if s.policy != nil {
		if missing := s.policy.Check(access.Call{Method: "ec2." + name, Resource: resource, Needs: needs(name, c.p)}); missing != nil {
			writeError(w, requestID, unauthorized(missing))
			return
		}
	}
	f, _ := s.faults.Decide(fault.Call{Method: "ec2." + name, Resource: resource})
	reject := f.Mode == fault.ModeReject || f.Mode == fault.ModeFailOperation
	stub.Serve(w, r, f, reject, "text/xml;charset=UTF-8", func() { writeError(w, requestID, faultError(f)) }, func() (func(), bool) {
		body, err := a.handle(c)
		if err != nil {
			return func() { writeError(w, requestID, err) }, false
		}
		return func() {
			body.setRequestID(requestID)
			writeXML(w, http.StatusOK, xml.StartElement{Name: xml.Name{Space: namespace, Local: name + "Response"}}, body)
		}, true
	})
}

func (s *Server) parse(w http.ResponseWriter, r *http.Request) (string, action, *call, *apiError) {
	if r.URL.Path != "/" {
		return "", action{}, nil, &apiError{http.StatusNotFound, "InvalidAction", "squall: no EC2 action is served at " + r.URL.Path}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		return "", action{}, nil, clientError("MalformedQueryString", "The request could not be parsed: %v", err)
	}
	name := r.Form.Get("Action")
	if name == "" {
		return "", action{}, nil, clientError("MissingAction", "The request must contain the parameter Action")
	}
	if v := r.Form.Get("Version"); v != apiVersion {
		if v == "" {
			return "", action{}, nil, missing("Version")
		}
		return "", action{}, nil, clientError("NoSuchVersion", "The requested version (%s) of service AmazonEC2 does not exist", v)
	}
	region, err := regionOf(r)
	if err != nil {
		return "", action{}, nil, err
	}
	a, ok := s.actions[name]
	if !ok {
		return "", action{}, nil, &apiError{http.StatusNotFound, "InvalidAction", fmt.Sprintf("The action %s is not valid for this web service.", name)}
	}
	return name, a, &call{region: region, p: newParams(r.Form)}, nil
}

// regionOf reads the region from the SigV4 credential scope,
// "AKID/20260101/us-east-1/ec2/aws4_request", without checking the signature.
func regionOf(r *http.Request) (string, *apiError) {
	credential := r.Form.Get("X-Amz-Credential")
	if h := r.Header.Get("Authorization"); h != "" {
		algorithm, fields, _ := strings.Cut(h, " ")
		if algorithm != "AWS4-HMAC-SHA256" {
			return "", clientError("IncompleteSignature", "Unsupported signing algorithm %q", algorithm)
		}
		for field := range strings.SplitSeq(fields, ",") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(field), "Credential="); ok {
				credential = v
			}
		}
	}
	if credential == "" {
		return "", &apiError{http.StatusUnauthorized, "MissingAuthenticationToken", "The request must contain valid credentials."}
	}
	scope := strings.Split(credential, "/")
	if len(scope) != 5 || scope[2] == "" || scope[4] != "aws4_request" {
		return "", clientError("IncompleteSignature", "The credential scope %q is malformed.", credential)
	}
	return scope[2], nil
}

// requestID returns a fresh request ID in the UUID form the API uses.
func (s *Server) requestID() string {
	n := s.requests.Add(1)
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", n>>48, n&(1<<48-1))
}

// newID returns a fresh resource ID with prefix. The caller holds s.mu.
func (s *Server) newID(prefix string) string {
	s.nextID++
	return fmt.Sprintf("%s-%017x", prefix, s.nextID)
}

// unusedID returns a fresh resource ID with prefix that taken does not hold,
// so a generated ID never replaces a resource put with an ID of the same form.
// The caller holds s.mu.
func unusedID[V any](s *Server, prefix string, taken map[string]V) string {
	for {
		id := s.newID(prefix)
		if _, ok := taken[id]; !ok {
			return id
		}
	}
}

// region returns the state of region, creating it. The caller holds s.mu.
func (s *Server) region(region string) *regionState {
	rs, ok := s.regions[region]
	if !ok {
		rs = &regionState{
			addresses: map[string]*Address{}, interfaces: map[string]*NetworkInterface{}, instances: map[string]*Instance{},
			images: map[string]*Image{}, volumes: map[string]*Volume{}, snapshots: map[string]*Snapshot{},
			placementGroups: map[string]*PlacementGroup{},
		}
		s.regions[region] = rs
	}
	return rs
}

// lookupRegion returns the state of region without creating it: an empty,
// read-only state when the server holds nothing there. The caller holds s.mu.
func (s *Server) lookupRegion(region string) *regionState {
	if rs, ok := s.regions[region]; ok {
		return rs
	}
	return &regionState{}
}

// idempotent is a call that used a client token: id names what it created,
// for fault rules, and created is what a repeat answers with.
type idempotent struct {
	key         string
	fingerprint string
	id          string
	created     any
}

// usedToken returns the call that first used token in region for kind. The
// caller holds s.mu.
func (s *Server) usedToken(region, kind, token string) (*idempotent, bool) {
	if token == "" {
		return nil, false
	}
	t, ok := s.tokens[region+"/"+kind+"/"+token]
	return t, ok
}

// replay returns the call that first used token in region for kind, and
// whether a call with fingerprint may repeat it. The caller holds s.mu.
func (s *Server) replay(region, kind, token, fingerprint string) (*idempotent, *apiError) {
	t, ok := s.usedToken(region, kind, token)
	if !ok {
		return nil, nil
	}
	if t.fingerprint != fingerprint {
		return nil, clientError("IdempotentParameterMismatch",
			"The client token '%s' was already used for a request with different parameters.", token)
	}
	return t, nil
}

// tokenTarget returns what the call that first used the client token of c
// for kind created. The caller holds s.mu.
func (s *Server) tokenTarget(c *call, kind string) (string, bool) {
	t, ok := s.usedToken(c.region, kind, c.p.values.Get("ClientToken"))
	if !ok {
		return "", false
	}
	return t.id, true
}

// remember keeps the call that used token in region for kind, unless it had
// none. The caller holds s.mu.
func (s *Server) remember(region, kind, token, fingerprint, id string, created any) {
	if token == "" {
		return
	}
	t := &idempotent{key: region + "/" + kind + "/" + token, fingerprint: fingerprint, id: id, created: created}
	s.tokens[t.key] = t
	// tokenOrder is a ring of the kept tokens; tokenNext is the oldest once it is full.
	if len(s.tokenOrder) < maxTokens {
		s.tokenOrder = append(s.tokenOrder, t.key)
		return
	}
	delete(s.tokens, s.tokenOrder[s.tokenNext])
	s.tokenOrder[s.tokenNext] = t.key
	s.tokenNext = (s.tokenNext + 1) % maxTokens
}
