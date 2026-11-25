package compute_test

import (
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/parallelworks/squall/gcp/compute"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

func TestPutDiskSetsUpAFailedDisk(t *testing.T) {
	c := setup(t)
	put, err := c.server.PutDisk(project, zone, compute.Disk{Name: "broken", Status: "FAILED", SizeGB: 20, Labels: map[string]string{"session-id": "s"}})
	if err != nil || put.Status != "FAILED" || put.ID == 0 || put.SizeGB != 20 || put.CreationTimestamp == "" {
		t.Fatalf("PutDisk = %+v, %v", put, err)
	}
	got, err := c.disks.Get(t.Context(), &computepb.GetDiskRequest{Project: project, Zone: zone, Disk: "broken"})
	if err != nil || got.GetStatus() != "FAILED" || got.GetId() != put.ID || got.GetLabels()["session-id"] != "s" ||
		got.GetType() != "https://www.googleapis.com/compute/v1/projects/p/zones/us-east1-b/diskTypes/pd-standard" {
		t.Fatalf("get = %v, %v", got, err)
	}
	if listed := c.aggregatedDisks(t, "status=FAILED"); len(listed) != 1 || !strings.HasSuffix(listed[0], "broken") {
		t.Fatalf("aggregated list of FAILED disks = %v", listed)
	}

	if err := c.insert(t, single("vm-1", "s")); err != nil {
		t.Fatal(err)
	}
	attach, err := c.instances.AttachDisk(t.Context(), &computepb.AttachDiskInstanceRequest{
		Project: project, Zone: zone, Instance: "vm-1",
		AttachedDiskResource: &computepb.AttachedDisk{Source: new("zones/" + zone + "/disks/broken")},
	})
	if err == nil {
		err = attach.Wait(t.Context())
	}
	if sdkerr.GoogleCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "is not ready") {
		t.Fatalf("attaching a FAILED disk = %v, want resourceNotReady", err)
	}
	if err := c.insertSnapshot(t, &computepb.Snapshot{Name: new("broken-1"), SourceDisk: new("zones/" + zone + "/disks/broken")}); sdkerr.GoogleCode(err) != http.StatusBadRequest {
		t.Fatalf("a snapshot of a FAILED disk = %v, want HTTP 400", err)
	}

	op, err := c.disks.Delete(t.Context(), &computepb.DeleteDiskRequest{Project: project, Zone: zone, Disk: "broken"})
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil {
		t.Fatalf("deleting a FAILED disk = %v", err)
	}
	if _, ok := c.server.LookupDisk(project, zone, "broken"); ok {
		t.Fatal("the FAILED disk is still there after its delete")
	}
}

func TestPutDiskRejectsWhatAnInsertWould(t *testing.T) {
	c := setup(t)
	if _, err := c.server.PutDisk(project, zone, compute.Disk{Name: "data"}); err != nil {
		t.Fatal(err)
	}
	if got, ok := c.server.LookupDisk(project, zone, "data"); !ok || got.Status != "READY" {
		t.Fatalf("a disk put with no status = %+v, %v; want READY", got, ok)
	}
	for name, d := range map[string]struct {
		zone string
		disk compute.Disk
	}{
		"a name already taken":    {zone, compute.Disk{Name: "data"}},
		"an invalid name":         {zone, compute.Disk{Name: "Data"}},
		"a status in progress":    {zone, compute.Disk{Name: "creating", Status: "CREATING"}},
		"an unknown zone":         {"mars-1a", compute.Disk{Name: "elsewhere"}},
		"an image that is absent": {zone, compute.Disk{Name: "imaged", SourceImage: "projects/p/global/images/missing"}},
	} {
		if _, err := c.server.PutDisk(project, d.zone, d.disk); err == nil {
			t.Errorf("PutDisk took %s", name)
		}
	}
	if got := c.server.Disks(); len(got) != 1 {
		t.Fatalf("disks = %+v", got)
	}
}
