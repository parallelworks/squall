package compute_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/compute"
)

func (c *clients) insertDisk(t *testing.T, d *computepb.Disk) {
	t.Helper()
	op, err := c.disks.Insert(t.Context(), &computepb.InsertDiskRequest{Project: project, Zone: zone, DiskResource: d})
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
}

func (c *clients) insertSnapshot(t *testing.T, snap *computepb.Snapshot) error {
	t.Helper()
	op, err := c.snapshots.Insert(t.Context(), &computepb.InsertSnapshotRequest{Project: project, SnapshotResource: snap})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

func (c *clients) deleteSnapshot(t *testing.T, name string) error {
	t.Helper()
	op, err := c.snapshots.Delete(t.Context(), &computepb.DeleteSnapshotRequest{Project: project, Snapshot: name})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

func (c *clients) listSnapshots(t *testing.T, filter string) ([]string, error) {
	t.Helper()
	it := c.snapshots.List(t.Context(), &computepb.ListSnapshotsRequest{Project: project, Filter: new(filter)})
	var out []string
	for {
		snap, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, snap.GetName())
	}
}

func TestSnapshots(t *testing.T) {
	c := setup(t)
	c.insertDisk(t, &computepb.Disk{Name: new("data"), SizeGb: new(int64(50))})
	disk := "https://www.googleapis.com/compute/v1/projects/p/zones/us-east1-b/disks/data"
	if err := c.insertSnapshot(t, &computepb.Snapshot{
		Name: new("data-1"), Description: new("nightly"), SourceDisk: new("zones/us-east1-b/disks/data"), Labels: map[string]string{"session-id": "s"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := c.snapshots.Get(t.Context(), &computepb.GetSnapshotRequest{Project: project, Snapshot: "data-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetStatus() != "READY" || got.GetSourceDisk() != disk || got.GetDiskSizeGb() != 50 || got.GetDescription() != "nightly" ||
		fmt.Sprint(got.GetStorageLocations()) != "[us]" || got.GetLabels()["session-id"] != "s" || got.GetId() == 0 || got.GetSourceDiskId() == "" ||
		got.GetSelfLink() != "https://www.googleapis.com/compute/v1/projects/p/global/snapshots/data-1" {
		t.Fatalf("snapshot = %v", got)
	}
	if err := c.insertSnapshot(t, &computepb.Snapshot{Name: new("data-2"), SourceDisk: new(disk), StorageLocations: []string{"us-east1"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		snap *computepb.Snapshot
		code int
	}{
		{"a name in use", &computepb.Snapshot{Name: new("data-1"), SourceDisk: new(disk)}, http.StatusConflict},
		{"an unknown disk", &computepb.Snapshot{Name: new("data-3"), SourceDisk: new("zones/us-east1-b/disks/missing")}, http.StatusBadRequest},
		{"no disk", &computepb.Snapshot{Name: new("data-3")}, http.StatusBadRequest},
		{"an invalid name", &computepb.Snapshot{Name: new("Data"), SourceDisk: new(disk)}, http.StatusBadRequest},
		{"an unsupported field", &computepb.Snapshot{Name: new("data-3"), SourceDisk: new(disk), SnapshotType: new("ARCHIVE")}, http.StatusBadRequest},
	} {
		if err := c.insertSnapshot(t, tc.snap); apiCode(err) != tc.code {
			t.Errorf("insert with %s = %v, want HTTP %d", tc.name, err, tc.code)
		}
	}
	for filter, want := range map[string]string{
		`labels.session-id = "s"`: "[data-1]",
		"diskSizeGb = 50":         "[data-1 data-2]",
		"name eq data-.*":         "[data-1 data-2]",
		"name = other":            "[]",
	} {
		if got, err := c.listSnapshots(t, filter); err != nil || fmt.Sprint(got) != want {
			t.Errorf("list %q = %v, %v; want %s", filter, got, err, want)
		}
	}
	if _, err := c.listSnapshots(t, "storageBytes = 1"); apiCode(err) != http.StatusBadRequest {
		t.Errorf("an unsupported filter = %v, want HTTP 400", err)
	}

	c.insertDisk(t, &computepb.Disk{Name: new("restored"), SourceSnapshot: new("global/snapshots/data-1")})
	if d, ok := c.server.LookupDisk(project, zone, "restored"); !ok || d.SizeGB != 50 || !strings.HasSuffix(d.SourceSnapshot, "/global/snapshots/data-1") {
		t.Fatalf("disk from the snapshot = %+v, %v", d, ok)
	}
	op, err := c.disks.Delete(t.Context(), &computepb.DeleteDiskRequest{Project: project, Zone: zone, Disk: "data"})
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil {
		t.Fatalf("deleting a disk with snapshots: %v", err)
	}
	if err := c.deleteSnapshot(t, "data-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.snapshots.Get(t.Context(), &computepb.GetSnapshotRequest{Project: project, Snapshot: "data-1"}); apiCode(err) != http.StatusNotFound {
		t.Fatalf("get of a deleted snapshot = %v, want HTTP 404", err)
	}
	if err := c.deleteSnapshot(t, "data-1"); apiCode(err) != http.StatusNotFound {
		t.Fatalf("deleting it again = %v, want HTTP 404", err)
	}
	if got := c.server.Snapshots(); len(got) != 1 || got[0].Name != "data-2" || fmt.Sprint(got[0].StorageLocations) != "[us-east1]" {
		t.Fatalf("snapshots = %+v", got)
	}
}

func TestSnapshotsSettle(t *testing.T) {
	c := setup(t, compute.WithSettleAfter(-1))
	if _, err := c.disks.Insert(t.Context(), &computepb.InsertDiskRequest{Project: project, Zone: zone, DiskResource: &computepb.Disk{Name: new("data")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.snapshots.Insert(t.Context(), &computepb.InsertSnapshotRequest{Project: project, SnapshotResource: &computepb.Snapshot{
		Name: new("data-1"), SourceDisk: new("zones/us-east1-b/disks/data"),
	}}); apiCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "is not ready") {
		t.Fatalf("a snapshot of a disk being created = %v, want resourceNotReady", err)
	}
	c.server.Settle()
	op, err := c.snapshots.Insert(t.Context(), &computepb.InsertSnapshotRequest{Project: project, SnapshotResource: &computepb.Snapshot{
		Name: new("data-1"), SourceDisk: new("zones/us-east1-b/disks/data"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if op.Done() {
		t.Fatal("the insert's operation is done before the snapshot settles")
	}
	if got, ok := c.server.LookupSnapshot(project, "data-1"); !ok || got.Status != "CREATING" {
		t.Fatalf("snapshot = %+v, %v; want CREATING", got, ok)
	}
	_, err = c.disks.Insert(t.Context(), &computepb.InsertDiskRequest{Project: project, Zone: zone, DiskResource: &computepb.Disk{
		Name: new("restored"), SourceSnapshot: new("global/snapshots/data-1"),
	}})
	if apiCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "is not ready") {
		t.Fatalf("a disk from a snapshot being created = %v, want resourceNotReady", err)
	}
	c.server.Settle()
	if err := op.Poll(t.Context()); err != nil || !op.Done() {
		t.Fatalf("poll after Settle = %v, done %v", err, op.Done())
	}
	if _, err := c.snapshots.Delete(t.Context(), &computepb.DeleteSnapshotRequest{Project: project, Snapshot: "data-1"}); err != nil {
		t.Fatal(err)
	}
	if got, err := c.snapshots.Get(t.Context(), &computepb.GetSnapshotRequest{Project: project, Snapshot: "data-1"}); err != nil || got.GetStatus() != "DELETING" {
		t.Fatalf("snapshot being deleted = %v, %v", got, err)
	}
	c.server.Settle()
	if got := c.server.Snapshots(); len(got) != 0 {
		t.Fatalf("snapshots after Settle = %+v", got)
	}
}

func TestSnapshotFaults(t *testing.T) {
	faults := fault.New().Add(
		fault.Rule{Method: "compute.snapshots.insert", Resource: "projects/p/global/snapshots/lost", Fault: fault.LoseResponse()},
		fault.Rule{Method: "compute.snapshots.insert", Resource: "projects/p/global/snapshots/failed", Fault: fault.FailOperation(http.StatusInternalServerError)},
		fault.Rule{Method: "compute.snapshots.delete", Nth: 1, Fault: fault.FailOperation(http.StatusServiceUnavailable)},
	)
	c := setup(t, compute.WithFaults(faults))
	c.insertDisk(t, &computepb.Disk{Name: new("data")})
	source := new("zones/us-east1-b/disks/data")
	if err := c.insertSnapshot(t, &computepb.Snapshot{Name: new("lost"), SourceDisk: source}); err == nil || apiCode(err) != 0 {
		t.Fatalf("insert whose response is lost = %v", err)
	}
	if _, ok := c.server.LookupSnapshot(project, "lost"); !ok {
		t.Fatal("the snapshot whose response was lost was not taken")
	}
	if err := c.insertSnapshot(t, &computepb.Snapshot{Name: new("failed"), SourceDisk: source}); err == nil {
		t.Fatal("insert whose operation fails succeeded")
	}
	if _, ok := c.server.LookupSnapshot(project, "failed"); ok {
		t.Fatal("an insert whose operation failed took the snapshot")
	}
	if err := c.deleteSnapshot(t, "lost"); err == nil {
		t.Fatal("delete whose operation fails succeeded")
	}
	if err := c.deleteSnapshot(t, "lost"); err != nil {
		t.Fatalf("deleting again: %v", err)
	}
	if got := c.server.Snapshots(); len(got) != 0 {
		t.Fatalf("snapshots = %+v", got)
	}
}

func TestPutSnapshot(t *testing.T) {
	c := setup(t)
	put, err := c.server.PutSnapshot(project, compute.Snapshot{Name: "golden", Labels: map[string]string{"os": "linux"}})
	if err != nil || put.Status != "READY" || put.DiskSizeGB != 10 || put.ID == 0 {
		t.Fatalf("PutSnapshot = %+v, %v", put, err)
	}
	if _, err := c.server.PutSnapshot(project, compute.Snapshot{Name: "Golden"}); err == nil {
		t.Fatal("PutSnapshot took an invalid name")
	}
	got, err := c.snapshots.Get(t.Context(), &computepb.GetSnapshotRequest{Project: project, Snapshot: "golden"})
	if err != nil || got.GetId() != put.ID || got.GetLabels()["os"] != "linux" || got.GetSourceDisk() != "" {
		t.Fatalf("get = %v, %v", got, err)
	}
}
