package compute_test

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/compute"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

const group = "workers"

func (c *clients) insertTemplate(t *testing.T, name string, props *computepb.InstanceProperties) {
	t.Helper()
	op, err := c.templates.Insert(t.Context(), &computepb.InsertInstanceTemplateRequest{Project: project, InstanceTemplateResource: &computepb.InstanceTemplate{
		Name: new(name), Properties: props,
	}})
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil {
		t.Fatalf("insert template %s: %v", name, err)
	}
}

// insertGroup creates group from a new template of the same name.
func (c *clients) insertGroup(t *testing.T, targetSize int32) {
	t.Helper()
	c.insertTemplate(t, group, properties("s"))
	op, err := c.managers.Insert(t.Context(), &computepb.InsertInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManagerResource: &computepb.InstanceGroupManager{
		Name: new(group), Description: new("owned"), TargetSize: new(targetSize),
		InstanceTemplate: new("projects/" + project + "/global/instanceTemplates/" + group),
		InstanceLifecyclePolicy: &computepb.InstanceGroupManagerInstanceLifecyclePolicy{
			DefaultActionOnFailure: new("DO_NOTHING"),
		},
	}})
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
}

func (c *clients) getGroup(t *testing.T) *computepb.InstanceGroupManager {
	t.Helper()
	g, err := c.managers.Get(t.Context(), &computepb.GetInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: group})
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	return g
}

func (c *clients) createInstances(t *testing.T, names ...string) error {
	t.Helper()
	var configs []*computepb.PerInstanceConfig
	for _, name := range names {
		configs = append(configs, &computepb.PerInstanceConfig{Name: new(name)})
	}
	op, err := c.managers.CreateInstances(t.Context(), &computepb.CreateInstancesInstanceGroupManagerRequest{
		Project: project, Zone: zone, InstanceGroupManager: group,
		InstanceGroupManagersCreateInstancesRequestResource: &computepb.InstanceGroupManagersCreateInstancesRequest{Instances: configs},
	})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

func (c *clients) deleteInstances(t *testing.T, skip bool, refs ...string) error {
	t.Helper()
	op, err := c.managers.DeleteInstances(t.Context(), &computepb.DeleteInstancesInstanceGroupManagerRequest{
		Project: project, Zone: zone, InstanceGroupManager: group,
		InstanceGroupManagersDeleteInstancesRequestResource: &computepb.InstanceGroupManagersDeleteInstancesRequest{
			Instances: refs, SkipInstancesOnValidationError: new(skip),
		},
	})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

// managed lists the group's instances as name:status:action.
func (c *clients) managed(t *testing.T) []string {
	t.Helper()
	it := c.managers.ListManagedInstances(t.Context(), &computepb.ListManagedInstancesInstanceGroupManagersRequest{Project: project, Zone: zone, InstanceGroupManager: group})
	var out []string
	for {
		m, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatalf("listManagedInstances: %v", err)
		}
		out = append(out, m.GetName()+":"+m.GetInstanceStatus()+":"+m.GetCurrentAction())
	}
}

func TestInstanceGroupManagerCreatesAndDeletesInstances(t *testing.T) {
	c := setup(t)
	c.insertGroup(t, 0)
	g := c.getGroup(t)
	if g.GetTargetSize() != 0 || g.GetBaseInstanceName() != group || g.GetDescription() != "owned" ||
		g.GetZone() != "https://www.googleapis.com/compute/v1/projects/p/zones/"+zone ||
		g.GetInstanceTemplate() != "https://www.googleapis.com/compute/v1/projects/p/global/instanceTemplates/"+group ||
		g.GetInstanceLifecyclePolicy().GetDefaultActionOnFailure() != "DO_NOTHING" || !g.GetStatus().GetIsStable() {
		t.Fatalf("group = %v", g)
	}

	if err := c.createInstances(t, "node-1", "node-2"); err != nil {
		t.Fatal(err)
	}
	inst, err := c.get(t, "node-1")
	if err != nil || inst.GetStatus() != "RUNNING" || !strings.HasSuffix(inst.GetMachineType(), "/n2-standard-4") {
		t.Fatalf("a group instance = %v, %v", inst, err)
	}
	var createdBy string
	for _, item := range inst.GetMetadata().GetItems() {
		if item.GetKey() == "created-by" {
			createdBy = item.GetValue()
		}
	}
	if createdBy != "projects/p/zones/"+zone+"/instanceGroupManagers/"+group {
		t.Errorf("created-by = %q", createdBy)
	}
	if got := c.managed(t); fmt.Sprint(got) != "[node-1:RUNNING:NONE node-2:RUNNING:NONE]" {
		t.Errorf("managed instances = %v", got)
	}
	if g := c.getGroup(t); g.GetTargetSize() != 2 || g.GetCurrentActions().GetNone() != 2 {
		t.Errorf("group after createInstances = %v", g)
	}

	if err := c.createInstances(t, "node-2", "node-3"); sdkerr.GoogleCode(err) != http.StatusConflict {
		t.Errorf("createInstances naming a member = %v, want HTTP 409", err)
	}
	if err := c.insert(t, single("loner", "s")); err != nil {
		t.Fatal(err)
	}
	if err := c.createInstances(t, "loner"); sdkerr.GoogleCode(err) != http.StatusConflict {
		t.Errorf("createInstances naming an existing instance = %v, want HTTP 409", err)
	}

	if err := c.deleteInstances(t, false, "zones/"+zone+"/instances/node-1", "zones/"+zone+"/instances/loner"); sdkerr.GoogleCode(err) != http.StatusBadRequest {
		t.Errorf("deleteInstances naming a non-member = %v, want HTTP 400", err)
	}
	if got := c.managed(t); len(got) != 2 {
		t.Errorf("a failed deleteInstances took effect: %v", got)
	}
	if err := c.deleteInstances(t, true, "zones/"+zone+"/instances/node-1", "zones/"+zone+"/instances/loner"); err != nil {
		t.Fatalf("deleteInstances skipping a non-member = %v", err)
	}
	if _, err := c.get(t, "node-1"); sdkerr.GoogleCode(err) != http.StatusNotFound {
		t.Errorf("a deleted member = %v, want HTTP 404", err)
	}
	if _, err := c.get(t, "loner"); err != nil {
		t.Errorf("a skipped non-member = %v", err)
	}
	if err := c.deleteInstances(t, true, "zones/us-east1-c/instances/node-2"); sdkerr.GoogleCode(err) != http.StatusBadRequest {
		t.Errorf("deleteInstances in another zone = %v, want HTTP 400", err)
	}

	_, err = c.templates.Delete(t.Context(), &computepb.DeleteInstanceTemplateRequest{Project: project, InstanceTemplate: group})
	if sdkerr.GoogleCode(err) != http.StatusBadRequest || apiReason(err) != "resourceInUseByAnotherResource" {
		t.Errorf("deleting the group's template = %v, want resourceInUseByAnotherResource", err)
	}

	op, err := c.managers.Delete(t.Context(), &computepb.DeleteInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: group})
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.get(t, "node-2"); sdkerr.GoogleCode(err) != http.StatusNotFound {
		t.Errorf("an instance of a deleted group = %v, want HTTP 404", err)
	}
	if _, ok := c.server.LookupDisk(project, zone, "node-2"); ok {
		t.Error("the boot disk of a deleted group's instance remains")
	}
	op, err = c.templates.Delete(t.Context(), &computepb.DeleteInstanceTemplateRequest{Project: project, InstanceTemplate: group})
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil {
		t.Errorf("deleting a template no group uses = %v", err)
	}
}

func TestInstanceGroupManagerListsAndTemplates(t *testing.T) {
	c := setup(t)
	c.insertGroup(t, 2)
	insts, _ := c.list(t, "", 0)
	if len(insts) != 2 || !strings.HasPrefix(insts[0].GetName(), group+"-") || len(insts[0].GetName()) != len(group)+5 {
		t.Fatalf("instances of a group created with targetSize 2 = %s", names(insts))
	}

	it := c.managers.AggregatedList(t.Context(), &computepb.AggregatedListInstanceGroupManagersRequest{Project: project, Filter: new("name eq work.*")})
	var found []string
	for {
		pair, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range pair.Value.GetInstanceGroupManagers() {
			found = append(found, pair.Key+"/"+g.GetName()+fmt.Sprintf(":%d", g.GetTargetSize()))
		}
	}
	if fmt.Sprint(found) != "[zones/"+zone+"/workers:2]" {
		t.Errorf("aggregated groups = %v", found)
	}
	list := c.managers.List(t.Context(), &computepb.ListInstanceGroupManagersRequest{Project: project, Zone: zone, Filter: new("name = other")})
	if g, err := list.Next(); !errors.Is(err, iterator.Done) {
		t.Errorf("filtered list = %v, %v; want none", g, err)
	}
	if _, err := c.managers.AggregatedList(t.Context(), &computepb.AggregatedListInstanceGroupManagersRequest{Project: project, Filter: new("targetSize = 1")}).Next(); sdkerr.GoogleCode(err) != http.StatusBadRequest {
		t.Errorf("an unsupported filter = %v, want HTTP 400", err)
	}

	p := properties("t")
	p.MachineType = new("n2-standard-8")
	c.insertTemplate(t, "bigger", p)
	set := func(template string) error {
		op, err := c.managers.SetInstanceTemplate(t.Context(), &computepb.SetInstanceTemplateInstanceGroupManagerRequest{
			Project: project, Zone: zone, InstanceGroupManager: group,
			InstanceGroupManagersSetInstanceTemplateRequestResource: &computepb.InstanceGroupManagersSetInstanceTemplateRequest{InstanceTemplate: new(template)},
		})
		if err != nil {
			return err
		}
		return op.Wait(t.Context())
	}
	if err := set("global/instanceTemplates/missing"); sdkerr.GoogleCode(err) != http.StatusBadRequest {
		t.Errorf("setInstanceTemplate to a missing template = %v, want HTTP 400", err)
	}
	if err := set("projects/p/global/instanceTemplates/bigger"); err != nil {
		t.Fatal(err)
	}
	if err := c.createInstances(t, "big-1"); err != nil {
		t.Fatal(err)
	}
	if inst, err := c.get(t, "big-1"); err != nil || !strings.HasSuffix(inst.GetMachineType(), "/n2-standard-8") || inst.GetLabels()["session-id"] != "t" {
		t.Errorf("an instance after setInstanceTemplate = %v, %v", inst, err)
	}
	if inst, _ := c.get(t, insts[0].GetName()); !strings.HasSuffix(inst.GetMachineType(), "/n2-standard-4") {
		t.Errorf("an existing instance changed template: %s", inst.GetMachineType())
	}
	if got := c.server.InstanceGroupManagers(); len(got) != 1 || got[0].TargetSize != 3 || !strings.HasSuffix(got[0].InstanceTemplate, "/bigger") {
		t.Errorf("InstanceGroupManagers() = %+v", got)
	}

	_, err := c.managers.Insert(t.Context(), &computepb.InsertInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManagerResource: &computepb.InstanceGroupManager{
		Name: new("other"), TargetSize: new(int32(0)), InstanceTemplate: new("global/instanceTemplates/missing"),
	}})
	if sdkerr.GoogleCode(err) != http.StatusBadRequest {
		t.Errorf("a group of a missing template = %v, want HTTP 400", err)
	}
	_, err = c.managers.Insert(t.Context(), &computepb.InsertInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManagerResource: &computepb.InstanceGroupManager{
		Name: new(group), TargetSize: new(int32(0)), InstanceTemplate: new("global/instanceTemplates/bigger"),
	}})
	if sdkerr.GoogleCode(err) != http.StatusConflict {
		t.Errorf("a second group of the same name = %v, want HTTP 409", err)
	}
}

func TestGroupInstancesSettleAfterReads(t *testing.T) {
	c := setup(t, compute.WithSettleAfter(2))
	c.insertGroup(t, 0)
	if err := c.createInstances(t, "node-1"); err != nil {
		t.Fatal(err)
	}
	if got := c.managed(t); fmt.Sprint(got) != "[node-1:PROVISIONING:CREATING]" {
		t.Errorf("managed instances while creating = %v", got)
	}
	var statuses []string
	for range 3 {
		inst, err := c.get(t, "node-1")
		if err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, inst.GetStatus())
	}
	if fmt.Sprint(statuses) != "[PROVISIONING STAGING RUNNING]" {
		t.Errorf("statuses = %v", statuses)
	}
	if err := c.deleteInstances(t, false, "node-1"); err != nil {
		t.Fatal(err)
	}
	if got := c.managed(t); fmt.Sprint(got) != "[node-1:STOPPING:DELETING]" {
		t.Errorf("managed instances while deleting = %v", got)
	}
	c.server.Settle()
	if got := c.managed(t); len(got) != 0 {
		t.Errorf("managed instances after the delete = %v", got)
	}
}

func TestGroupKeepsTryingAnInstanceItCannotCreate(t *testing.T) {
	c := setup(t)
	p := properties("s")
	p.ResourcePolicies = []string{"missing-policy"}
	c.insertTemplate(t, "broken", p)
	c.insertGroup(t, 0)
	op, err := c.managers.SetInstanceTemplate(t.Context(), &computepb.SetInstanceTemplateInstanceGroupManagerRequest{
		Project: project, Zone: zone, InstanceGroupManager: group,
		InstanceGroupManagersSetInstanceTemplateRequestResource: &computepb.InstanceGroupManagersSetInstanceTemplateRequest{InstanceTemplate: new("global/instanceTemplates/broken")},
	})
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := c.createInstances(t, "node-1"); err != nil {
		t.Fatalf("createInstances = %v; the group accepts it and keeps trying", err)
	}
	it := c.managers.ListManagedInstances(t.Context(), &computepb.ListManagedInstancesInstanceGroupManagersRequest{Project: project, Zone: zone, InstanceGroupManager: group})
	m, err := it.Next()
	if err != nil {
		t.Fatal(err)
	}
	errs := m.GetLastAttempt().GetErrors().GetErrors()
	if m.GetCurrentAction() != "CREATING" || m.GetInstance() != "" || len(errs) != 1 || !strings.Contains(errs[0].GetMessage(), "missing-policy") {
		t.Errorf("managed instance = %v", m)
	}
	if _, err := c.get(t, "node-1"); sdkerr.GoogleCode(err) != http.StatusNotFound {
		t.Errorf("an instance the group could not create = %v, want HTTP 404", err)
	}
	if err := c.deleteInstances(t, false, "node-1"); err != nil || len(c.managed(t)) != 0 {
		t.Errorf("deleting it by name = %v, leaving %v", err, c.managed(t))
	}
}

func (c *clients) insertResize(t *testing.T, name string, instances ...string) error {
	t.Helper()
	req := &computepb.InstanceGroupManagerResizeRequest{Name: new(name), RequestedRunDuration: &computepb.Duration{Seconds: new(int64(3600))}}
	for _, inst := range instances {
		req.Instances = append(req.Instances, &computepb.PerInstanceConfig{Name: new(inst)})
	}
	op, err := c.resizes.Insert(t.Context(), &computepb.InsertInstanceGroupManagerResizeRequestRequest{
		Project: project, Zone: zone, InstanceGroupManager: group, InstanceGroupManagerResizeRequestResource: req,
	})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

func (c *clients) getResize(t *testing.T, name string) (*computepb.InstanceGroupManagerResizeRequest, error) {
	t.Helper()
	return c.resizes.Get(t.Context(), &computepb.GetInstanceGroupManagerResizeRequestRequest{Project: project, Zone: zone, InstanceGroupManager: group, ResizeRequest: name})
}

func (c *clients) cancelResize(t *testing.T, name string) error {
	t.Helper()
	op, err := c.resizes.Cancel(t.Context(), &computepb.CancelInstanceGroupManagerResizeRequestRequest{Project: project, Zone: zone, InstanceGroupManager: group, ResizeRequest: name})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

func (c *clients) deleteResize(t *testing.T, name string) error {
	t.Helper()
	op, err := c.resizes.Delete(t.Context(), &computepb.DeleteInstanceGroupManagerResizeRequestRequest{Project: project, Zone: zone, InstanceGroupManager: group, ResizeRequest: name})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

// resizeStates lists the group's resize requests as name:state.
func (c *clients) resizeStates(t *testing.T) []string {
	t.Helper()
	it := c.resizes.List(t.Context(), &computepb.ListInstanceGroupManagerResizeRequestsRequest{Project: project, Zone: zone, InstanceGroupManager: group})
	var out []string
	for {
		req, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatalf("list resize requests: %v", err)
		}
		out = append(out, req.GetName()+":"+req.GetState())
	}
}

func TestResizeRequestCreatesItsInstances(t *testing.T) {
	c := setup(t)
	c.insertGroup(t, 0)
	if err := c.insertResize(t, "rr-1", "node-1", "node-2"); err != nil {
		t.Fatal(err)
	}
	req, err := c.getResize(t, "rr-1")
	if err != nil || req.GetState() != "SUCCEEDED" || req.GetRequestedRunDuration().GetSeconds() != 3600 || len(req.GetInstances()) != 2 ||
		req.GetSelfLink() != "https://www.googleapis.com/compute/v1/projects/p/zones/"+zone+"/instanceGroupManagers/workers/resizeRequests/rr-1" {
		t.Fatalf("resize request = %v, %v", req, err)
	}
	if got := c.managed(t); fmt.Sprint(got) != "[node-1:RUNNING:NONE node-2:RUNNING:NONE]" {
		t.Errorf("managed instances = %v", got)
	}
	if g := c.getGroup(t); g.GetTargetSize() != 2 {
		t.Errorf("target size = %d", g.GetTargetSize())
	}
	if err := c.insertResize(t, "rr-1", "node-3"); sdkerr.GoogleCode(err) != http.StatusConflict {
		t.Errorf("a second request of the same name = %v, want HTTP 409", err)
	}
	err = c.insertResize(t, "rr-2", "node-3", "node-2")
	if sdkerr.GoogleCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "Instance 'node-2' already exists") {
		t.Errorf("a request naming an existing instance = %v, want its operation to fail with HTTP 400", err)
	}
	if _, err := c.getResize(t, "rr-2"); sdkerr.GoogleCode(err) != http.StatusNotFound {
		t.Errorf("the failed request = %v, want HTTP 404", err)
	}

	err = c.cancelResize(t, "rr-1")
	if sdkerr.GoogleCode(err) != http.StatusPreconditionFailed || !strings.Contains(err.Error(), "reached a final state") {
		t.Errorf("cancelling a SUCCEEDED request = %v, want HTTP 412", err)
	}
	if err := c.deleteResize(t, "rr-1"); err != nil {
		t.Fatal(err)
	}
	if got := c.resizeStates(t); len(got) != 0 {
		t.Errorf("requests after the delete = %v", got)
	}
	if got := c.managed(t); len(got) != 2 {
		t.Errorf("deleting a request deleted its instances: %v", got)
	}

	resizeBy := &computepb.InstanceGroupManagerResizeRequest{Name: new("rr-3"), ResizeBy: new(int32(2))}
	op, err := c.resizes.Insert(t.Context(), &computepb.InsertInstanceGroupManagerResizeRequestRequest{
		Project: project, Zone: zone, InstanceGroupManager: group, InstanceGroupManagerResizeRequestResource: resizeBy,
	})
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil || len(c.managed(t)) != 4 {
		t.Errorf("a request to resize by 2 = %v, leaving %v", err, c.managed(t))
	}
	_, err = c.resizes.Insert(t.Context(), &computepb.InsertInstanceGroupManagerResizeRequestRequest{
		Project: project, Zone: zone, InstanceGroupManager: "missing", InstanceGroupManagerResizeRequestResource: resizeBy,
	})
	if sdkerr.GoogleCode(err) != http.StatusNotFound {
		t.Errorf("a request to a missing group = %v, want HTTP 404", err)
	}
}

func TestResizeRequestWaitsUntilSettled(t *testing.T) {
	c := setup(t, compute.WithSettleAfter(-1))
	c.insertGroup(t, 0)
	for _, name := range []string{"rr-wait", "rr-cancel", "rr-fail", "rr-stockout"} {
		if err := c.insertResize(t, name, name+"-node"); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}
	if got := c.resizeStates(t); fmt.Sprint(got) != "[rr-cancel:ACCEPTED rr-fail:ACCEPTED rr-stockout:ACCEPTED rr-wait:ACCEPTED]" {
		t.Fatalf("requests = %v", got)
	}
	if got := c.managed(t); len(got) != 0 {
		t.Errorf("a queued request created instances: %v", got)
	}
	if err := c.insertResize(t, "rr-again", "rr-wait-node"); sdkerr.GoogleCode(err) != http.StatusBadRequest {
		t.Errorf("a request naming a queued instance = %v, want HTTP 400", err)
	}
	if err := c.deleteResize(t, "rr-wait"); sdkerr.GoogleCode(err) != http.StatusPreconditionFailed {
		t.Errorf("deleting an ACCEPTED request = %v, want HTTP 412", err)
	}

	if err := c.cancelResize(t, "rr-cancel"); err != nil {
		t.Fatal(err)
	}
	if err := c.server.FailResizeRequest(project, zone, group, "rr-fail", "QUOTA_EXCEEDED", "Quota 'CPUS' exceeded."); err != nil {
		t.Fatal(err)
	}
	if err := c.server.FailResizeAttempt(project, zone, group, "rr-stockout", "ZONE_RESOURCE_POOL_EXHAUSTED", "Not enough resources."); err != nil {
		t.Fatal(err)
	}
	failed, err := c.getResize(t, "rr-fail")
	if errs := failed.GetStatus().GetError().GetErrors(); err != nil || failed.GetState() != "FAILED" || len(errs) != 1 || errs[0].GetCode() != "QUOTA_EXCEEDED" {
		t.Errorf("failed request = %v, %v", failed, err)
	}
	waiting, err := c.getResize(t, "rr-stockout")
	if errs := waiting.GetStatus().GetLastAttempt().GetError().GetErrors(); err != nil || waiting.GetState() != "ACCEPTED" || len(errs) != 1 || errs[0].GetMessage() != "Not enough resources." {
		t.Errorf("waiting request = %v, %v", waiting, err)
	}
	if err := c.server.FailResizeRequest(project, zone, group, "rr-cancel", "", ""); err == nil {
		t.Error("failing a CANCELLED request succeeded")
	}
	if err := c.cancelResize(t, "rr-cancel"); sdkerr.GoogleCode(err) != http.StatusPreconditionFailed || !strings.Contains(err.Error(), "final state") {
		t.Errorf("cancelling a CANCELLED request = %v, want HTTP 412", err)
	}

	c.server.Settle()
	if got := c.resizeStates(t); fmt.Sprint(got) != "[rr-cancel:CANCELLED rr-fail:FAILED rr-stockout:SUCCEEDED rr-wait:SUCCEEDED]" {
		t.Errorf("requests after Settle = %v", got)
	}
	if got := c.managed(t); fmt.Sprint(got) != "[rr-stockout-node:RUNNING:NONE rr-wait-node:RUNNING:NONE]" {
		t.Errorf("managed instances after Settle = %v", got)
	}
	if req, ok := c.server.LookupResizeRequest(project, zone, group, "rr-stockout"); !ok || req.State != "SUCCEEDED" || req.Status.LastAttempt != nil {
		t.Errorf("LookupResizeRequest = %+v, %v", req, ok)
	}
	for _, name := range []string{"rr-cancel", "rr-fail"} {
		if err := c.deleteResize(t, name); err != nil {
			t.Errorf("deleting the %s request: %v", name, err)
		}
	}
}

func TestResizeRequestIsCreatingOnItsLastReadInProgress(t *testing.T) {
	c := setup(t, compute.WithSettleAfter(1))
	c.insertGroup(t, 0)
	if err := c.insertResize(t, "rr-1", "node-1"); err != nil {
		t.Fatal(err)
	}
	if got := c.resizeStates(t); fmt.Sprint(got) != "[rr-1:CREATING]" {
		t.Fatalf("requests = %v", got)
	}
	if err := c.cancelResize(t, "rr-1"); sdkerr.GoogleCode(err) != http.StatusPreconditionFailed {
		t.Errorf("cancelling a CREATING request = %v, want HTTP 412", err)
	}
	if got := c.managed(t); fmt.Sprint(got) != "[node-1:PROVISIONING:CREATING]" {
		t.Errorf("managed instances = %v", got)
	}
	if req, err := c.getResize(t, "rr-1"); err != nil || req.GetState() != "SUCCEEDED" {
		t.Errorf("request after its reads = %v, %v", req, err)
	}
	if inst, err := c.get(t, "node-1"); err != nil || inst.GetStatus() != "RUNNING" {
		t.Errorf("instance = %v, %v", inst, err)
	}

	if err := c.insertResize(t, "rr-2", "node-2"); err != nil {
		t.Fatal(err)
	}
	if req, _ := c.getResize(t, "rr-2"); req.GetState() != "CREATING" {
		t.Fatalf("state = %s", req.GetState())
	}
	if err := c.server.FailResizeRequest(project, zone, group, "rr-2", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.get(t, "node-2"); sdkerr.GoogleCode(err) != http.StatusNotFound {
		t.Errorf("an instance of a failed request = %v, want HTTP 404", err)
	}
	if req, _ := c.getResize(t, "rr-2"); req.GetState() != "FAILED" || req.GetStatus().GetError() != nil {
		t.Errorf("failed request = %v", req)
	}
}

func TestGroupFaultsAndPermissions(t *testing.T) {
	faults := fault.New()
	c := setup(t, compute.WithFaults(faults))
	c.insertGroup(t, 0)
	groupPath := "projects/p/zones/" + zone + "/instanceGroupManagers/" + group
	faults.Add(
		fault.Rule{Method: "compute.instanceGroupManagers.createInstances", Resource: groupPath, Times: 1, Fault: fault.FailOperation(http.StatusInternalServerError)},
		fault.Rule{
			Method: "compute.instanceGroupManagerResizeRequests.insert", Resource: groupPath + "/resizeRequests/rr-1", Times: 1,
			Fault: fault.Reject(http.StatusServiceUnavailable, "").WithMessage("Internal error."),
		},
		fault.Rule{Method: "compute.instanceGroupManagers.listManagedInstances", Resource: groupPath, Times: 1, Fault: fault.FailOperation(http.StatusInternalServerError)},
	)
	if err := c.createInstances(t, "node-1"); sdkerr.GoogleCode(err) != http.StatusInternalServerError {
		t.Errorf("createInstances with a failed operation = %v, want HTTP 500", err)
	}
	if got := c.server.Instances(); len(got) != 0 {
		t.Errorf("a failed createInstances took effect: %d instances", len(got))
	}
	if err := c.insertResize(t, "rr-1", "node-1"); sdkerr.GoogleCode(err) != http.StatusServiceUnavailable || !strings.Contains(err.Error(), "Internal error.") {
		t.Errorf("a rejected resize request = %v, want HTTP 503", err)
	}
	it := c.managers.ListManagedInstances(t.Context(), &computepb.ListManagedInstancesInstanceGroupManagersRequest{Project: project, Zone: zone, InstanceGroupManager: group})
	if _, err := it.Next(); sdkerr.GoogleCode(err) != http.StatusInternalServerError {
		t.Errorf("listManagedInstances with an operation fault = %v, want HTTP 500", err)
	}
	if err := c.insertResize(t, "rr-1", "node-1"); err != nil {
		t.Errorf("the resize request after its fault = %v", err)
	}

	policy := access.Allow("compute.instanceGroupManagers.get", "compute.zoneOperations.get")
	d := setup(t, compute.WithPolicy(policy))
	_, err := d.managers.CreateInstances(t.Context(), &computepb.CreateInstancesInstanceGroupManagerRequest{
		Project: project, Zone: zone, InstanceGroupManager: group,
		InstanceGroupManagersCreateInstancesRequestResource: &computepb.InstanceGroupManagersCreateInstancesRequest{},
	})
	if sdkerr.GoogleCode(err) != http.StatusForbidden || !strings.Contains(err.Error(), "compute.instanceGroupManagers.update") {
		t.Errorf("createInstances without compute.instanceGroupManagers.update = %v, want HTTP 403", err)
	}
	if denials := policy.Denials(); len(denials) != 1 || !slices.Equal(denials[0].Missing, []string{"compute.instanceGroupManagers.update"}) {
		t.Errorf("Denials() = %+v", denials)
	}
}
