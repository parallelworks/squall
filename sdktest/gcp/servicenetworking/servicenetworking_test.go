package servicenetworking_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	gcompute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	sn "google.golang.org/api/servicenetworking/v1"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/compute"
	"github.com/parallelworks/squall/gcp/servicenetworking"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

const (
	project    = "p"
	number     = 123456789
	parent     = "services/servicenetworking.googleapis.com"
	connection = parent + "/connections/servicenetworking-googleapis-com"
	byNumber   = "projects/123456789/global/networks/vpc"
	byID       = "projects/p/global/networks/vpc"
)

type env struct {
	compute   *compute.Server
	server    *servicenetworking.Server
	svc       *sn.APIService
	networks  *gcompute.NetworksClient
	globalIPs *gcompute.GlobalAddressesClient
}

// setup serves a compute server with the network vpc and the VPC_PEERING
// ranges psa-a and psa-b, and a Service Networking server that checks
// against it.
func setup(t *testing.T, opts ...servicenetworking.Option) *env {
	t.Helper()
	e := &env{compute: compute.New()}
	csrv := httptest.NewServer(e.compute)
	t.Cleanup(csrv.Close)
	e.server = servicenetworking.New(append([]servicenetworking.Option{
		servicenetworking.WithCompute(e.compute), servicenetworking.WithProjectNumber(project, number),
	}, opts...)...)
	srv := httptest.NewServer(e.server)
	t.Cleanup(srv.Close)
	ctx := t.Context()
	co := []option.ClientOption{option.WithEndpoint(csrv.URL), option.WithoutAuthentication()}
	var err error
	if e.networks, err = gcompute.NewNetworksRESTClient(ctx, co...); err != nil {
		t.Fatal(err)
	}
	if e.globalIPs, err = gcompute.NewGlobalAddressesRESTClient(ctx, co...); err != nil {
		t.Fatal(err)
	}
	if e.svc, err = sn.NewService(ctx, option.WithEndpoint(srv.URL+"/"), option.WithoutAuthentication()); err != nil {
		t.Fatal(err)
	}
	op, err := e.networks.Insert(ctx, &computepb.InsertNetworkRequest{Project: project, NetworkResource: &computepb.Network{Name: new("vpc"), AutoCreateSubnetworks: new(false)}})
	if err == nil {
		err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"psa-a", "psa-b"} {
		op, err := e.globalIPs.Insert(ctx, &computepb.InsertGlobalAddressRequest{Project: project, AddressResource: &computepb.Address{
			Name: new(name), Purpose: new("VPC_PEERING"), AddressType: new("INTERNAL"), PrefixLength: new(int32(20)),
			Network: new("https://www.googleapis.com/compute/v1/" + byID),
		}})
		if err == nil {
			err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// done waits for op the way callers poll it, and returns its error, if any.
func (e *env) done(t *testing.T, op *sn.Operation, err error) error {
	t.Helper()
	if err != nil {
		return err
	}
	got, err := e.svc.Operations.Get(op.Name).Context(t.Context()).Do()
	if err != nil {
		return err
	}
	if !got.Done {
		t.Fatalf("operation %s is not done", op.Name)
	}
	if got.Error != nil {
		return errors.New(got.Error.Message)
	}
	return nil
}

func (e *env) create(t *testing.T, network string, ranges ...string) error {
	t.Helper()
	op, err := e.svc.Services.Connections.Create(parent, &sn.Connection{Network: network, ReservedPeeringRanges: ranges}).Context(t.Context()).Do()
	return e.done(t, op, err)
}

func (e *env) find(t *testing.T, network string) *sn.Connection {
	t.Helper()
	list, err := e.svc.Services.Connections.List(parent).Network(network).Context(t.Context()).Do()
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Connections) == 0 {
		return nil
	}
	return list.Connections[0]
}

func (e *env) deleteNetwork(t *testing.T) error {
	t.Helper()
	op, err := e.networks.Delete(t.Context(), &computepb.DeleteNetworkRequest{Project: project, Network: "vpc"})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

func TestConnectionLifecycle(t *testing.T) {
	e := setup(t)
	if c := e.find(t, byNumber); c != nil {
		t.Fatalf("a connection before any was made: %+v", c)
	}
	if err := e.create(t, byNumber, "psa-a"); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{byNumber, byID} {
		c := e.find(t, ref)
		if c == nil || c.Network != byNumber || c.Peering != "servicenetworking-googleapis-com" || c.Service != parent || !slices.Equal(c.ReservedPeeringRanges, []string{"psa-a"}) {
			t.Fatalf("list by %s = %+v", ref, c)
		}
	}
	n, _ := e.compute.LookupNetwork(project, "vpc")
	if len(n.Peerings) != 1 || n.Peerings[0].Name != "servicenetworking-googleapis-com" || n.Peerings[0].State != "ACTIVE" {
		t.Fatalf("network peerings = %+v", n.Peerings)
	}

	err := e.create(t, byID, "psa-b")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "use updateconnection") {
		t.Fatalf("a second create = %v, want it to say to use UpdateConnection", err)
	}

	op, err := e.svc.Services.Connections.Patch(connection, &sn.Connection{Network: byNumber, ReservedPeeringRanges: []string{"psa-a", "psa-b"}}).Context(t.Context()).Do()
	if err := e.done(t, op, err); err != nil {
		t.Fatal(err)
	}
	op, err = e.svc.Services.Connections.Patch(connection, &sn.Connection{Network: byNumber, ReservedPeeringRanges: []string{"psa-b"}}).Context(t.Context()).Do()
	if err := e.done(t, op, err); err == nil || !strings.Contains(err.Error(), "force") {
		t.Fatalf("removing a range without force = %v, want an error", err)
	}
	op, err = e.svc.Services.Connections.Patch(connection, &sn.Connection{Network: byNumber, ReservedPeeringRanges: []string{"psa-b"}}).Force(true).Context(t.Context()).Do()
	if err := e.done(t, op, err); err != nil {
		t.Fatal(err)
	}
	if got := e.server.Connections(); len(got) != 1 || !slices.Equal(got[0].ReservedPeeringRanges, []string{"psa-b"}) {
		t.Fatalf("Connections() = %+v", got)
	}

	err = e.deleteNetwork(t)
	if gerr, ok := errors.AsType[*googleapi.Error](err); !ok || gerr.Code != http.StatusBadRequest || len(gerr.Errors) == 0 || gerr.Errors[0].Reason != "resourceInUseByAnotherResource" {
		t.Fatalf("deleting a network a range is on = %v, want HTTP 400 resourceInUseByAnotherResource", err)
	}
	for _, name := range []string{"psa-a", "psa-b"} {
		op, err := e.globalIPs.Delete(t.Context(), &computepb.DeleteGlobalAddressRequest{Project: project, Address: name})
		if err == nil {
			err = op.Wait(t.Context())
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	err = e.deleteNetwork(t)
	if sdkerr.GoogleCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "peerings/servicenetworking-googleapis-com") {
		t.Fatalf("deleting a peered network = %v, want HTTP 400 naming the peering", err)
	}

	dop, err := e.svc.Services.Connections.DeleteConnection(connection, &sn.DeleteConnectionRequest{ConsumerNetwork: byNumber}).Context(t.Context()).Do()
	if err := e.done(t, dop, err); err != nil {
		t.Fatal(err)
	}
	if len(e.server.Connections()) != 0 {
		t.Fatalf("Connections() after delete = %+v", e.server.Connections())
	}
	_, err = e.svc.Services.Connections.DeleteConnection(connection, &sn.DeleteConnectionRequest{ConsumerNetwork: byNumber}).Context(t.Context()).Do()
	if sdkerr.GoogleCode(err) != http.StatusNotFound {
		t.Fatalf("deleting it again = %v, want HTTP 404", err)
	}
	if err := e.deleteNetwork(t); err != nil {
		t.Fatal(err)
	}
}

func TestCreateChecksTheRanges(t *testing.T) {
	e := setup(t)
	if err := e.create(t, byNumber, "missing"); err == nil || !strings.Contains(err.Error(), "Allocated IP range 'missing' not found in network.") {
		t.Fatalf("create with a missing range = %v", err)
	}
	if err := e.create(t, "projects/p/global/networks/other", "psa-a"); err == nil {
		t.Fatal("create on a missing network succeeded")
	}
	_, err := e.svc.Services.Connections.Create(parent, &sn.Connection{Network: "vpc", ReservedPeeringRanges: []string{"psa-a"}}).Context(t.Context()).Do()
	if sdkerr.GoogleCode(err) != http.StatusBadRequest {
		t.Fatalf("create with a bare network name = %v, want HTTP 400", err)
	}
	_, err = e.svc.Services.Connections.List(parent).Context(t.Context()).Do()
	if sdkerr.GoogleCode(err) != http.StatusBadRequest {
		t.Fatalf("list without a network = %v, want HTTP 400", err)
	}
	if len(e.server.Connections()) != 0 {
		t.Fatalf("failed creates made connections: %+v", e.server.Connections())
	}
}

func TestConnectionFaults(t *testing.T) {
	faults := fault.New().Add(
		fault.Rule{Method: "servicenetworking.services.connections.create", Nth: 1, Fault: fault.FailOperation(http.StatusServiceUnavailable)},
		fault.Rule{Method: "servicenetworking.services.connections.deleteConnection", Nth: 1, Fault: fault.LoseResponse()},
	)
	e := setup(t, servicenetworking.WithFaults(faults))
	if err := e.create(t, byNumber, "psa-a"); err == nil {
		t.Fatal("a create whose operation failed succeeded")
	}
	if c := e.find(t, byNumber); c != nil {
		t.Fatalf("a failed create made %+v", c)
	}
	if err := e.create(t, byNumber, "psa-a"); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Services.Connections.DeleteConnection(connection, &sn.DeleteConnectionRequest{ConsumerNetwork: byNumber}).Context(t.Context()).Do()
	if err == nil || sdkerr.GoogleCode(err) != 0 {
		t.Fatalf("deleteConnection = %v, want a transport error", err)
	}
	if c := e.find(t, byNumber); c != nil {
		t.Fatalf("the delete whose response was lost left %+v", c)
	}
	if n, _ := e.compute.LookupNetwork(project, "vpc"); len(n.Peerings) != 0 {
		t.Fatalf("the network is still peered: %+v", n.Peerings)
	}
}

func TestConnectionPermissions(t *testing.T) {
	policy := access.Allow("servicenetworking.services.get")
	e := setup(t, servicenetworking.WithPolicy(policy))
	err := e.create(t, byNumber, "psa-a")
	gerr, ok := errors.AsType[*googleapi.Error](err)
	if !ok || gerr.Code != http.StatusForbidden || !strings.Contains(gerr.Message, "servicenetworking.services.addPeering") {
		t.Fatalf("create = %v, want 403 naming servicenetworking.services.addPeering", err)
	}
	if c := e.find(t, byNumber); c != nil {
		t.Fatalf("a refused create made %+v", c)
	}
}
