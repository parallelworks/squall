package lustre_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/lustre/apiv1/lustrepb"
	"github.com/googleapis/gax-go/v2/apierror"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/gcp/lustre"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

func TestPolicyRefusesADeleteItDoesNotGrant(t *testing.T) {
	policy := access.Allow("lustre.instances.create", "lustre.instances.list", "lustre.operations.get")
	s, c := setup(t, lustre.WithPolicy(policy))
	inst := create(t, c, "kept", "a")

	_, err := c.DeleteInstance(t.Context(), &lustrepb.DeleteInstanceRequest{Name: inst.GetName()})
	if sdkerr.GoogleCode(err) != http.StatusForbidden {
		t.Fatalf("delete = %v, want 403", err)
	}
	e, _ := errors.AsType[*apierror.APIError](err)
	if !strings.Contains(e.Error(), "Permission 'lustre.instances.delete' denied") || e.Reason() != "IAM_PERMISSION_DENIED" {
		t.Errorf("error = %v, reason %q; want it to name lustre.instances.delete", e, e.Reason())
	}
	if n := len(s.Instances()); n != 1 {
		t.Errorf("a refused delete left %d instances, want 1", n)
	}
	denials := policy.Denials()
	if len(denials) != 1 || denials[0].Method != "lustre.projects.locations.instances.delete" || denials[0].Resource != inst.GetName() {
		t.Errorf("Denials() = %+v", denials)
	}
	if got := policy.Unused(); !slices.Equal(got, []string{"lustre.instances.list", "lustre.operations.get"}) {
		t.Errorf("Unused() = %v", got)
	}
}

func TestPolicyRefusesPollingWithoutOperationsGet(t *testing.T) {
	policy := access.Allow("lustre.instances.create", "lustre.instances.get")
	_, c := setup(t, lustre.WithPolicy(policy), lustre.WithSettleAfter(1))
	op, err := c.CreateInstance(t.Context(), createRequest("polled", "a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Poll(t.Context()); sdkerr.GoogleCode(err) != http.StatusForbidden {
		t.Fatalf("poll = %v, want 403", err)
	}
	// An instance get is how code without lustre.operations.get can wait instead.
	if _, err := c.GetInstance(t.Context(), &lustrepb.GetInstanceRequest{Name: parent + "/instances/polled"}); err != nil {
		t.Errorf("get = %v", err)
	}
	if d := policy.Denials(); len(d) != 1 || !slices.Equal(d[0].Missing, []string{"lustre.operations.get"}) {
		t.Errorf("Denials() = %+v", d)
	}
}

func TestPolicyRefusesAListBeforeItsFilterIsRead(t *testing.T) {
	policy := access.Allow("lustre.instances.get")
	_, c := setup(t, lustre.WithPolicy(policy))
	it := c.ListInstances(t.Context(), &lustrepb.ListInstancesRequest{Parent: parent, Filter: "labels.a = b"})
	if _, err := it.Next(); sdkerr.GoogleCode(err) != http.StatusForbidden || !strings.Contains(err.Error(), "//lustre.googleapis.com/"+parent+"'") {
		t.Errorf("list = %v, want 403 on the parent", err)
	}
}
