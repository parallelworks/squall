package lustre_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	glustre "cloud.google.com/go/lustre/apiv1"
	"cloud.google.com/go/lustre/apiv1/lustrepb"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/lustre"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

const (
	project  = "p"
	zone     = "us-central1-a"
	parent   = "projects/" + project + "/locations/" + zone
	network  = "projects/" + project + "/global/networks/default"
	sessions = "session-id"
)

func setup(t *testing.T, opts ...lustre.Option) (*lustre.Server, *glustre.Client) {
	t.Helper()
	s := lustre.New(opts...)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	c, err := glustre.NewRESTClient(t.Context(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return s, c
}

func createRequest(id, session string) *lustrepb.CreateInstanceRequest {
	return &lustrepb.CreateInstanceRequest{
		Parent:     parent,
		InstanceId: id,
		Instance: &lustrepb.Instance{
			Filesystem:               "scratch",
			CapacityGib:              18_000,
			PerUnitStorageThroughput: 1000,
			Network:                  network,
			Description:              "scratch space",
			Labels:                   map[string]string{sessions: session},
			AccessRulesOptions: &lustrepb.AccessRulesOptions{
				DefaultSquashMode: lustrepb.AccessRulesOptions_ROOT_SQUASH,
				DefaultSquashUid:  65534,
				DefaultSquashGid:  65534,
			},
		},
	}
}

func create(t *testing.T, c *glustre.Client, id, session string) *lustrepb.Instance {
	t.Helper()
	op, err := c.CreateInstance(t.Context(), createRequest(id, session))
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	inst, err := op.Wait(t.Context())
	if err != nil {
		t.Fatalf("wait for %s: %v", id, err)
	}
	return inst
}

func list(t *testing.T, c *glustre.Client, req *lustrepb.ListInstancesRequest) []string {
	t.Helper()
	var names []string
	it := c.ListInstances(t.Context(), req)
	for {
		inst, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return names
		}
		if err != nil {
			t.Fatalf("list %v: %v", req, err)
		}
		names = append(names, inst.GetName())
	}
}

// code is the gRPC code of an API error, from a finished operation or, as the
// client maps it, from an HTTP status (which reads 409 as Aborted).
func code(err error) codes.Code {
	if e, ok := errors.AsType[*apierror.APIError](err); ok {
		return e.GRPCStatus().Code()
	}
	return codes.Unknown
}

func TestLifecycle(t *testing.T) {
	_, c := setup(t)
	inst := create(t, c, "scratch-1", "a")
	if inst.GetName() != parent+"/instances/scratch-1" || inst.GetState() != lustrepb.Instance_ACTIVE ||
		inst.GetCapacityGib() != 18_000 || inst.GetPerUnitStorageThroughput() != 1000 || inst.GetNetwork() != network ||
		inst.GetMountPoint() == "" || inst.GetLabels()[sessions] != "a" ||
		inst.GetCreateTime() == nil || inst.GetUid() == "" ||
		inst.GetAccessRulesOptions().GetDefaultSquashMode() != lustrepb.AccessRulesOptions_ROOT_SQUASH {
		t.Fatalf("created %+v", inst)
	}
	got, err := c.GetInstance(t.Context(), &lustrepb.GetInstanceRequest{Name: inst.GetName()})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.GetMountPoint() != inst.GetMountPoint() || got.GetDescription() != "scratch space" {
		t.Errorf("get = %+v", got)
	}

	op, err := c.DeleteInstance(t.Context(), &lustrepb.DeleteInstanceRequest{Name: inst.GetName()})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := op.Wait(t.Context()); err != nil {
		t.Fatalf("wait for delete: %v", err)
	}
	if meta, err := op.Metadata(); err != nil || meta.GetVerb() != "delete" || meta.GetTarget() != inst.GetName() {
		t.Errorf("delete metadata = %+v, %v", meta, err)
	}
	if _, err := c.GetInstance(t.Context(), &lustrepb.GetInstanceRequest{Name: inst.GetName()}); code(err) != codes.NotFound || sdkerr.GoogleCode(err) != http.StatusNotFound {
		t.Errorf("get after delete = %v, want NotFound", err)
	}
}

func TestListFiltersByLabelAndState(t *testing.T) {
	s, c := setup(t)
	for _, id := range []string{"a-1", "a-2", "b-1"} {
		create(t, c, id, id[:1])
	}
	if _, err := s.PutInstance(project, "us-east1-b", "a-3", lustre.Instance{
		Filesystem: "old", CapacityGib: 9_000, PerUnitStorageThroughput: 1000, Network: network,
		Labels: map[string]string{sessions: "a"}, State: lustre.StateStopped,
	}); err != nil {
		t.Fatal(err)
	}

	got := list(t, c, &lustrepb.ListInstancesRequest{Parent: parent, Filter: `labels.session-id = "a"`})
	if want := []string{parent + "/instances/a-1", parent + "/instances/a-2"}; !slices.Equal(got, want) {
		t.Errorf("one zone = %v, want %v", got, want)
	}
	got = list(t, c, &lustrepb.ListInstancesRequest{Parent: "projects/p/locations/-", Filter: "labels.session-id=a", PageSize: 1})
	if len(got) != 3 {
		t.Errorf("every zone, a page at a time = %v, want 3", got)
	}
	got = list(t, c, &lustrepb.ListInstancesRequest{Parent: "projects/p/locations/-", Filter: "labels.session-id = a AND state = ACTIVE"})
	if len(got) != 2 {
		t.Errorf("active only = %v, want 2", got)
	}
	got = list(t, c, &lustrepb.ListInstancesRequest{Parent: parent, Filter: "labels:session-id AND NOT labels.session-id = a", OrderBy: "name desc"})
	if want := []string{parent + "/instances/b-1"}; !slices.Equal(got, want) {
		t.Errorf("negated = %v, want %v", got, want)
	}

	it := c.ListInstances(t.Context(), &lustrepb.ListInstancesRequest{Parent: parent, Filter: "createTime > 0"})
	if _, err := it.Next(); code(err) != codes.InvalidArgument {
		t.Errorf("unsupported filter = %v, want InvalidArgument", err)
	}
}

func TestDuplicateCreateAndMissingDelete(t *testing.T) {
	_, c := setup(t)
	create(t, c, "dup", "a")
	if _, err := c.CreateInstance(t.Context(), createRequest("dup", "b")); sdkerr.GoogleCode(err) != http.StatusConflict {
		t.Errorf("duplicate create = %v, want 409", err)
	}
	if _, err := c.DeleteInstance(t.Context(), &lustrepb.DeleteInstanceRequest{Name: parent + "/instances/missing"}); code(err) != codes.NotFound {
		t.Errorf("delete missing = %v, want NotFound", err)
	}
	req := createRequest("bad", "a")
	req.Instance.CapacityGib = 20_000
	if _, err := c.CreateInstance(t.Context(), req); code(err) != codes.InvalidArgument {
		t.Errorf("create off the tier's step = %v, want InvalidArgument", err)
	}
}

func TestRequestIDReplaysCreate(t *testing.T) {
	s, c := setup(t)
	req := createRequest("once", "a")
	req.RequestId = "6f1e2d3c-4b5a-4978-8796-a5b4c3d2e1f0"
	first, err := c.CreateInstance(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.CreateInstance(t.Context(), req)
	if err != nil || again.Name() != first.Name() {
		t.Fatalf("retried create = %v, %v; want operation %s", again.Name(), err, first.Name())
	}
	if n := len(s.Instances()); n != 1 {
		t.Errorf("%d instances, want 1", n)
	}
}

func TestRejectedCreateTakesNoEffect(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "lustre.projects.locations.instances.create", Nth: 1, Fault: fault.RateLimited()})
	s, c := setup(t, lustre.WithFaults(faults))
	if _, err := c.CreateInstance(t.Context(), createRequest("x", "a")); code(err) != codes.ResourceExhausted || sdkerr.GoogleCode(err) != http.StatusTooManyRequests {
		t.Fatalf("create = %v, want ResourceExhausted", err)
	}
	if n := len(s.Instances()); n != 0 {
		t.Errorf("a rejected create left %d instances", n)
	}
}

func TestLostCreateLeavesAnInstanceToFindByLabel(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "lustre.projects.locations.instances.create", Nth: 1, Fault: fault.LoseResponse()})
	_, c := setup(t, lustre.WithFaults(faults))
	if _, err := c.CreateInstance(t.Context(), createRequest("lost", "a")); err == nil {
		t.Fatal("create succeeded, want its response lost")
	}
	if got := list(t, c, &lustrepb.ListInstancesRequest{Parent: "projects/p/locations/-", Filter: "labels.session-id = a"}); len(got) != 1 {
		t.Fatalf("instances labeled a = %v, want the lost one", got)
	}
	if _, err := c.CreateInstance(t.Context(), createRequest("lost", "a")); sdkerr.GoogleCode(err) != http.StatusConflict {
		t.Errorf("retried create = %v, want 409", err)
	}
}

func TestLostDeleteStillDeletes(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "lustre.projects.locations.instances.delete", Nth: 1, Fault: fault.LoseResponse()})
	_, c := setup(t, lustre.WithFaults(faults))
	inst := create(t, c, "gone", "a")
	if _, err := c.DeleteInstance(t.Context(), &lustrepb.DeleteInstanceRequest{Name: inst.GetName()}); err == nil {
		t.Fatal("delete succeeded, want its response lost")
	}
	if _, err := c.GetInstance(t.Context(), &lustrepb.GetInstanceRequest{Name: inst.GetName()}); code(err) != codes.NotFound {
		t.Errorf("get = %v, want NotFound", err)
	}
	if _, err := c.DeleteInstance(t.Context(), &lustrepb.DeleteInstanceRequest{Name: inst.GetName()}); code(err) != codes.NotFound {
		t.Errorf("retried delete = %v, want NotFound", err)
	}
}

func TestFailedOperations(t *testing.T) {
	faults := fault.New().Add(
		fault.Rule{Method: "lustre.projects.locations.instances.create", Resource: parent + "/instances/fails", Fault: fault.FailOperation(http.StatusInternalServerError)},
		fault.Rule{Method: "lustre.projects.locations.instances.delete", Nth: 1, Fault: fault.FailOperation(http.StatusServiceUnavailable)},
	)
	s, c := setup(t, lustre.WithFaults(faults))
	op, err := c.CreateInstance(t.Context(), createRequest("fails", "a"))
	if err != nil {
		t.Fatalf("create = %v, want an operation", err)
	}
	if _, err := op.Wait(t.Context()); code(err) != codes.Internal {
		t.Errorf("wait = %v, want Internal", err)
	}
	if n := len(s.Instances()); n != 0 {
		t.Errorf("a failed create left %d instances", n)
	}

	inst := create(t, c, "kept", "a")
	del, err := c.DeleteInstance(t.Context(), &lustrepb.DeleteInstanceRequest{Name: inst.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if err := del.Wait(t.Context()); code(err) != codes.Unavailable {
		t.Errorf("wait for delete = %v, want Unavailable", err)
	}
	if got, err := c.GetInstance(t.Context(), &lustrepb.GetInstanceRequest{Name: inst.GetName()}); err != nil || got.GetState() != lustrepb.Instance_ACTIVE {
		t.Errorf("after a failed delete = %v, %v; want ACTIVE", got, err)
	}
}

func TestSlowSettling(t *testing.T) {
	s, c := setup(t, lustre.WithSettleAfter(2))
	op, err := c.CreateInstance(t.Context(), createRequest("slow", "a"))
	if err != nil {
		t.Fatal(err)
	}
	name := parent + "/instances/slow"
	got, err := c.GetInstance(t.Context(), &lustrepb.GetInstanceRequest{Name: name})
	if err != nil || got.GetState() != lustrepb.Instance_CREATING || got.GetMountPoint() != "" {
		t.Fatalf("get while creating = %+v, %v", got, err)
	}
	polls := 0
	for !op.Done() {
		polls++
		inst, err := op.Poll(t.Context())
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		if op.Done() && inst.GetState() != lustrepb.Instance_ACTIVE {
			t.Errorf("finished create = %+v, want ACTIVE", inst)
		}
	}
	if polls != 2 {
		t.Errorf("create took %d polls after a get, want 2", polls)
	}

	// Discard the delete's operation and poll the instance until it is gone.
	if _, err := c.DeleteInstance(t.Context(), &lustrepb.DeleteInstanceRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	var seen []lustrepb.Instance_State
	for {
		got, err := c.GetInstance(t.Context(), &lustrepb.GetInstanceRequest{Name: name})
		if code(err) == codes.NotFound {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, got.GetState())
	}
	if want := []lustrepb.Instance_State{lustrepb.Instance_DELETING, lustrepb.Instance_DELETING}; !slices.Equal(seen, want) {
		t.Errorf("states while deleting = %v, want %v", seen, want)
	}
	if n := len(s.Instances()); n != 0 {
		t.Errorf("%d instances left", n)
	}
}

func TestSettleFinishesWhatOnlyItCan(t *testing.T) {
	s, c := setup(t, lustre.WithSettleAfter(-1))
	op, err := c.CreateInstance(t.Context(), createRequest("held", "a"))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := op.Poll(t.Context()); err != nil || op.Done() {
			t.Fatalf("poll = %v, done %v; want still creating", err, op.Done())
		}
	}
	s.Settle()
	if inst, err := op.Poll(t.Context()); err != nil || !op.Done() || inst.GetState() != lustrepb.Instance_ACTIVE {
		t.Fatalf("poll after Settle = %+v, %v, done %v", inst, err, op.Done())
	}
}

func TestOperationsListAndDelete(t *testing.T) {
	_, c := setup(t, lustre.WithSettleAfter(-1))
	op, err := c.CreateInstance(t.Context(), createRequest("a", "a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CancelOperation(t.Context(), &longrunningpb.CancelOperationRequest{Name: op.Name()}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	it := c.ListOperations(t.Context(), &longrunningpb.ListOperationsRequest{Name: parent})
	got, err := it.Next()
	if err != nil || got.GetName() != op.Name() || got.GetDone() {
		t.Fatalf("list = %v, %v", got, err)
	}
	if err := c.DeleteOperation(t.Context(), &longrunningpb.DeleteOperationRequest{Name: op.Name()}); err != nil {
		t.Fatalf("delete operation: %v", err)
	}
	if _, err := c.GetOperation(t.Context(), &longrunningpb.GetOperationRequest{Name: op.Name()}); code(err) != codes.NotFound {
		t.Errorf("get deleted operation = %v, want NotFound", err)
	}
}
