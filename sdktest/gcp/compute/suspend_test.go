package compute_test

import (
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/parallelworks/squall/gcp/compute"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

func TestSuspendAndResume(t *testing.T) {
	c := setup(t)
	if err := c.insert(t, single("vm-1", "s")); err != nil {
		t.Fatal(err)
	}
	suspend := func() error {
		op, err := c.instances.Suspend(t.Context(), &computepb.SuspendInstanceRequest{Project: project, Zone: zone, Instance: "vm-1"})
		if err != nil {
			return err
		}
		return op.Wait(t.Context())
	}
	resume := func() error {
		op, err := c.instances.Resume(t.Context(), &computepb.ResumeInstanceRequest{Project: project, Zone: zone, Instance: "vm-1"})
		if err != nil {
			return err
		}
		return op.Wait(t.Context())
	}
	if err := suspend(); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.get(t, "vm-1"); got.GetStatus() != "SUSPENDED" || got.GetLastSuspendedTimestamp() == "" {
		t.Fatalf("suspended instance = %v", got)
	}
	if err := suspend(); err != nil {
		t.Fatalf("suspending again = %v, want a no-op", err)
	}
	if err := resume(); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.get(t, "vm-1"); got.GetStatus() != "RUNNING" {
		t.Fatalf("resumed instance = %v", got)
	}

	stop, err := c.instances.Stop(t.Context(), &computepb.StopInstanceRequest{Project: project, Zone: zone, Instance: "vm-1"})
	if err != nil || stop.Wait(t.Context()) != nil {
		t.Fatalf("stop = %v", err)
	}
	if err := suspend(); sdkerr.GoogleCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "RESOURCE_NOT_READY") {
		t.Errorf("suspending a stopped instance = %v, want an operation that fails with RESOURCE_NOT_READY", err)
	}
	if err := resume(); sdkerr.GoogleCode(err) != http.StatusBadRequest {
		t.Errorf("resuming a stopped instance = %v, want HTTP 400", err)
	}
}

func TestSuspendInProgress(t *testing.T) {
	c := setup(t, compute.WithSettleAfter(-1))
	if _, err := c.instances.Insert(t.Context(), &computepb.InsertInstanceRequest{Project: project, Zone: zone, InstanceResource: single("vm-1", "s")}); err != nil {
		t.Fatal(err)
	}
	c.server.Settle()
	if _, err := c.instances.Suspend(t.Context(), &computepb.SuspendInstanceRequest{Project: project, Zone: zone, Instance: "vm-1"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.get(t, "vm-1"); got.GetStatus() != "SUSPENDING" {
		t.Errorf("status while suspending = %s", got.GetStatus())
	}
	c.server.Settle()
	if got, _ := c.get(t, "vm-1"); got.GetStatus() != "SUSPENDED" {
		t.Errorf("status once settled = %s", got.GetStatus())
	}
}
