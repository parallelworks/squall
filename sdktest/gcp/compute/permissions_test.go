package compute_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/gcp/compute"
)

func insertWithMetadata(t *testing.T, c *clients) error {
	t.Helper()
	op, err := c.instances.Insert(t.Context(), &computepb.InsertInstanceRequest{
		Project: "p",
		Zone:    zone,
		InstanceResource: &computepb.Instance{
			Name:        new("vm"),
			MachineType: new("zones/" + zone + "/machineTypes/e2-small"),
			Disks: []*computepb.AttachedDisk{{
				Boot:             new(true),
				AutoDelete:       new(true),
				InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: new("projects/debian-cloud/global/images/family/debian-12")},
			}},
			NetworkInterfaces: []*computepb.NetworkInterface{{Subnetwork: new("regions/us-east1/subnetworks/default")}},
			Metadata:          &computepb.Metadata{Items: []*computepb.Items{{Key: new("startup-script"), Value: new("true")}}},
		},
	})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

func TestPolicyRefusesACallMissingAPermissionAFieldNeeds(t *testing.T) {
	policy := access.Allow(
		"compute.instances.create",
		"compute.disks.create",
		"compute.images.useReadOnly",
		"compute.subnetworks.use",
		"compute.zoneOperations.get",
	)
	c := setup(t, compute.WithPolicy(policy))

	err := insertWithMetadata(t, c)
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusForbidden {
		t.Fatalf("insert = %v, want 403", err)
	}
	if !strings.Contains(gerr.Message, "Required 'compute.instances.setMetadata' permission") {
		t.Errorf("message = %q, want it to name compute.instances.setMetadata", gerr.Message)
	}
	if got := c.server.Instances(); len(got) != 0 {
		t.Errorf("a refused insert created %d instances", len(got))
	}
	denials := policy.Denials()
	if len(denials) != 1 || denials[0].Method != "compute.instances.insert" || !slices.Equal(denials[0].Missing, []string{"compute.instances.setMetadata"}) {
		t.Errorf("Denials() = %+v", denials)
	}
}

func TestPolicyRecordsWhatAllowedCallsNeeded(t *testing.T) {
	policy := access.Allow(
		"compute.instances.create",
		"compute.instances.setMetadata",
		"compute.disks.create",
		"compute.images.useReadOnly",
		"compute.subnetworks.use",
		"compute.zoneOperations.get",
		"compute.routes.delete",
	)
	c := setup(t, compute.WithPolicy(policy))

	if err := insertWithMetadata(t, c); err != nil {
		t.Fatal(err)
	}
	if got := policy.Unused(); !slices.Equal(got, []string{"compute.routes.delete"}) {
		t.Errorf("Unused() = %v, want [compute.routes.delete]", got)
	}
	if len(policy.Denials()) != 0 {
		t.Errorf("Denials() = %+v, want none", policy.Denials())
	}
}

func TestNoPolicyAllowsEveryCall(t *testing.T) {
	c := setup(t)
	if err := insertWithMetadata(t, c); err != nil {
		t.Fatal(err)
	}
}
