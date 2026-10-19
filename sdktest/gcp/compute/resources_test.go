package compute_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	betapb "cloud.google.com/go/compute/apiv1beta/computepb"
	"google.golang.org/api/iterator"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/compute"
)

func (c *clients) listPolicies(t *testing.T, filter string) ([]string, error) {
	t.Helper()
	it := c.policies.List(t.Context(), &betapb.ListResourcePoliciesRequest{Project: project, Region: region, Filter: new(filter)})
	var out []string
	for {
		p, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, p.GetName())
	}
}

func (c *clients) insertPolicy(t *testing.T, name string) error {
	t.Helper()
	op, err := c.policies.Insert(t.Context(), &betapb.InsertResourcePolicyRequest{
		Project: project, Region: region,
		ResourcePolicyResource: &betapb.ResourcePolicy{
			Name: new(name), Description: new("owned by s"),
			GroupPlacementPolicy: &betapb.ResourcePolicyGroupPlacementPolicy{Collocation: new("COLLOCATED"), VmCount: new(int32(2))},
		},
	})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

func TestResourcePolicies(t *testing.T) {
	c := setup(t)
	if got, err := c.listPolicies(t, "name eq u-p-1-.*"); err != nil || len(got) != 0 {
		t.Fatalf("list of none = %v, %v", got, err)
	}
	for _, name := range []string{"u-p-1-a", "u-p-1-b", "u-p-2-a"} {
		if err := c.insertPolicy(t, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.insertPolicy(t, "u-p-1-a"); apiCode(err) != http.StatusConflict {
		t.Fatalf("inserting a name in use = %v, want HTTP 409", err)
	}
	if got, err := c.listPolicies(t, "name eq u-p-1-.*"); err != nil || fmt.Sprint(got) != "[u-p-1-a u-p-1-b]" {
		t.Fatalf("list by prefix = %v, %v", got, err)
	}
	got, err := c.policies.Get(t.Context(), &betapb.GetResourcePolicyRequest{Project: project, Region: region, ResourcePolicy: "u-p-1-a"})
	if err != nil || got.GetGroupPlacementPolicy().GetVmCount() != 2 || got.GetStatus() != "READY" ||
		!strings.HasPrefix(got.GetSelfLink(), "https://www.googleapis.com/compute/beta/") {
		t.Fatalf("get = %v, %v", got, err)
	}

	req := bulkRequest("s", "node-1", "node-2")
	req.BulkInsertInstanceResourceResource.InstanceProperties.ResourcePolicies = []string{"u-p-1-a"}
	if err := c.bulkInsert(t, req); err != nil {
		t.Fatal(err)
	}
	inst, _ := c.get(t, "node-1")
	if p := inst.GetResourcePolicies(); len(p) != 1 || p[0] != "https://www.googleapis.com/compute/v1/projects/p/regions/us-east1/resourcePolicies/u-p-1-a" {
		t.Errorf("resourcePolicies = %v", p)
	}
	missing := bulkRequest("s", "node-3")
	missing.BulkInsertInstanceResourceResource.InstanceProperties.ResourcePolicies = []string{"nope"}
	err = c.bulkInsert(t, missing)
	if apiCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(),
		"Invalid value for field 'resource.instanceProperties.resourcePolicies[0]': 'nope'. The referenced resourcePolicy resource cannot be found.") {
		t.Errorf("a missing policy = %v, want HTTP 400 naming the field", err)
	}
	gone := single("vm-3", "s")
	gone.ResourcePolicies = []string{"regions/us-east1/resourcePolicies/nope"}
	if err := c.insert(t, gone); apiCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "resource.resourcePolicies[0]") {
		t.Errorf("insert with a missing policy = %v, want HTTP 400 naming the field", err)
	}

	del := func(name string) error {
		op, err := c.policies.Delete(t.Context(), &betapb.DeleteResourcePolicyRequest{Project: project, Region: region, ResourcePolicy: name})
		if err != nil {
			return err
		}
		return op.Wait(t.Context())
	}
	if err := del("u-p-1-a"); apiCode(err) != http.StatusBadRequest {
		t.Fatalf("deleting a policy in use = %v, want HTTP 400", err)
	}
	if err := del("u-p-1-b"); err != nil {
		t.Fatal(err)
	}
	if err := del("u-p-1-b"); apiCode(err) != http.StatusNotFound {
		t.Fatalf("deleting it again = %v, want HTTP 404", err)
	}
	if got := c.server.ResourcePolicies(); len(got) != 2 {
		t.Errorf("policies = %v", got)
	}
}

func TestInstanceTemplatesAndGroupManagers(t *testing.T) {
	c := setup(t)
	listTemplates := func(filter string) []string {
		it := c.templates.List(t.Context(), &computepb.ListInstanceTemplatesRequest{Project: project, Filter: new(filter)})
		var out []string
		for {
			tmpl, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return out
			}
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, tmpl.GetName())
		}
	}
	if got := listTemplates("name eq u-p-.*"); len(got) != 0 {
		t.Fatalf("templates = %v", got)
	}
	op, err := c.templates.Insert(t.Context(), &computepb.InsertInstanceTemplateRequest{Project: project, InstanceTemplateResource: &computepb.InstanceTemplate{
		Name: new("u-p-00001-tmpl"), Properties: properties("s"),
	}})
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := listTemplates("name eq u-p-.*"); fmt.Sprint(got) != "[u-p-00001-tmpl]" {
		t.Fatalf("templates = %v", got)
	}
	tmpl, err := c.templates.Get(t.Context(), &computepb.GetInstanceTemplateRequest{Project: project, InstanceTemplate: "u-p-00001-tmpl"})
	if err != nil || tmpl.GetProperties().GetLabels()["session-id"] != "s" || tmpl.GetProperties().GetMachineType() != "n2-standard-4" {
		t.Fatalf("get = %v, %v", tmpl, err)
	}
	count := int64(2)
	if err := c.bulkInsert(t, &computepb.BulkInsertInstanceRequest{Project: project, Zone: zone, BulkInsertInstanceResourceResource: &computepb.BulkInsertInstanceResource{
		Count: &count, NamePattern: new("node-#"), SourceInstanceTemplate: new("global/instanceTemplates/u-p-00001-tmpl"),
	}}); err != nil {
		t.Fatalf("bulkInsert from the template: %v", err)
	}
	if insts, _ := c.list(t, "labels.session-id = s", 0); names(insts) != "[node-1 node-2]" {
		t.Fatalf("instances = %s", names(insts))
	}
	one := int64(1)
	if err := c.bulkInsert(t, &computepb.BulkInsertInstanceRequest{Project: project, Zone: zone, BulkInsertInstanceResourceResource: &computepb.BulkInsertInstanceResource{
		Count: &one, NamePattern: new("other-#"), SourceInstanceTemplate: new("u-p-00001-tmpl"),
		InstanceProperties: &computepb.InstanceProperties{Labels: map[string]string{"session-id": "t"}},
	}}); err != nil {
		t.Fatalf("bulkInsert from the template with instanceProperties: %v", err)
	}
	if inst, err := c.get(t, "other-1"); err != nil || inst.GetLabels()["session-id"] != "t" || !strings.HasSuffix(inst.GetMachineType(), "/n2-standard-4") {
		t.Fatalf("instance from the patched template = %v, %v", inst, err)
	}
	err = c.bulkInsert(t, &computepb.BulkInsertInstanceRequest{Project: project, Zone: zone, BulkInsertInstanceResourceResource: &computepb.BulkInsertInstanceResource{
		Count: &one, NamePattern: new("other-#"), SourceInstanceTemplate: new("global/instanceTemplates/missing"),
	}})
	if apiCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(),
		"Invalid value for field 'resource.sourceInstanceTemplate': 'global/instanceTemplates/missing'. The referenced instanceTemplate resource cannot be found.") {
		t.Errorf("bulkInsert from a missing template = %v, want HTTP 400 naming the field", err)
	}
	op, err = c.templates.Delete(t.Context(), &computepb.DeleteInstanceTemplateRequest{Project: project, InstanceTemplate: "u-p-00001-tmpl"})
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil || len(c.server.InstanceTemplates()) != 0 {
		t.Fatalf("delete = %v", err)
	}

	it := c.managers.AggregatedList(t.Context(), &computepb.AggregatedListInstanceGroupManagersRequest{Project: project, Filter: new("name eq u-p-00001-group")})
	if pair, err := it.Next(); !errors.Is(err, iterator.Done) {
		t.Fatalf("group managers = %v, %v; want none", pair, err)
	}
	bad := c.managers.AggregatedList(t.Context(), &computepb.AggregatedListInstanceGroupManagersRequest{Project: project, Filter: new("targetSize = 1")})
	if _, err := bad.Next(); apiCode(err) != http.StatusBadRequest {
		t.Fatalf("an unsupported filter = %v, want HTTP 400", err)
	}
	if _, err := c.managers.Get(t.Context(), &computepb.GetInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: "g"}); apiCode(err) != http.StatusNotFound {
		t.Fatalf("get of a group manager = %v, want HTTP 404", err)
	}
}

func TestRoutes(t *testing.T) {
	c := setup(t)
	_, err := c.routes.Delete(t.Context(), &computepb.DeleteRouteRequest{Project: project, Route: "nat-gateway-s"})
	if apiCode(err) != http.StatusNotFound {
		t.Fatalf("deleting a missing route = %v, want HTTP 404", err)
	}
	route := &computepb.Route{
		Name: new("nat-gateway-s"), Description: new("owned by s"), DestRange: new("0.0.0.0/0"), Priority: new(uint32(90)),
		Network:         new("https://www.googleapis.com/compute/v1/projects/p/global/networks/net"),
		NextHopInstance: new("https://www.googleapis.com/compute/v1/projects/p/zones/us-east1-b/instances/controller"),
		Tags:            []string{"nat-s"},
	}
	insert := func() error {
		op, err := c.routes.Insert(t.Context(), &computepb.InsertRouteRequest{Project: project, RouteResource: route})
		if err != nil {
			return err
		}
		return op.Wait(t.Context())
	}
	if err := insert(); err != nil {
		t.Fatal(err)
	}
	if err := insert(); apiCode(err) != http.StatusConflict {
		t.Fatalf("inserting it again = %v, want HTTP 409", err)
	}
	got, err := c.routes.Get(t.Context(), &computepb.GetRouteRequest{Project: project, Route: "nat-gateway-s"})
	if err != nil || got.GetPriority() != 90 || got.GetDestRange() != "0.0.0.0/0" || got.GetTags()[0] != "nat-s" {
		t.Fatalf("get = %v, %v", got, err)
	}
	route.Name, route.DestRange = new("bad"), new("10.0.0.1/8")
	if err := insert(); apiCode(err) != http.StatusBadRequest {
		t.Fatalf("a range with host bits = %v, want HTTP 400", err)
	}
	op, err := c.routes.Delete(t.Context(), &computepb.DeleteRouteRequest{Project: project, Route: "nat-gateway-s"})
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil || len(c.server.Routes()) != 0 {
		t.Fatalf("delete = %v, routes %v", err, c.server.Routes())
	}
}

func TestDisks(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "compute.disks.insert", Resource: "projects/p/zones/us-east1-b/disks/lost", Fault: fault.LoseResponse()})
	c := setup(t, compute.WithFaults(faults))
	insert := func(d *computepb.Disk) error {
		op, err := c.disks.Insert(t.Context(), &computepb.InsertDiskRequest{Project: project, Zone: zone, DiskResource: d})
		if err != nil {
			return err
		}
		return op.Wait(t.Context())
	}
	if err := insert(&computepb.Disk{Name: new("d-1"), SizeGb: new(int64(30)), SourceImage: new("projects/debian-cloud/global/images/debian-12-v1")}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		disk *computepb.Disk
		code int
	}{
		{"a name in use", &computepb.Disk{Name: new("d-1")}, http.StatusConflict},
		{"smaller than its image", &computepb.Disk{Name: new("d-2"), SizeGb: new(int64(5)), SourceImage: new("projects/debian-cloud/global/images/debian-12-v1")}, http.StatusBadRequest},
		{"an unknown snapshot", &computepb.Disk{Name: new("d-3"), SourceSnapshot: new("global/snapshots/missing")}, http.StatusBadRequest},
		{"an unknown image", &computepb.Disk{Name: new("d-3"), SourceImage: new("projects/debian-cloud/global/images/missing")}, http.StatusBadRequest},
		{"a type in another zone", &computepb.Disk{Name: new("d-4"), Type: new("zones/us-east1-c/diskTypes/pd-ssd")}, http.StatusBadRequest},
	} {
		if err := insert(tc.disk); apiCode(err) != tc.code {
			t.Errorf("insert with %s = %v, want HTTP %d", tc.name, err, tc.code)
		}
	}
	if _, err := c.server.PutSnapshot(project, compute.Snapshot{Name: "snap-1", DiskSizeGB: 40}); err != nil {
		t.Fatal(err)
	}
	if err := insert(&computepb.Disk{Name: new("d-5"), SourceSnapshot: new("global/snapshots/snap-1"), Labels: map[string]string{"session-id": "s"}}); err != nil {
		t.Fatal(err)
	}
	if err := insert(&computepb.Disk{Name: new("lost")}); err == nil || apiCode(err) != 0 {
		t.Fatalf("insert whose response is lost = %v", err)
	}
	it := c.disks.List(t.Context(), &computepb.ListDisksRequest{Project: project, Zone: zone, Filter: new("sizeGb = 40 OR name = lost")})
	var got []string
	for {
		d, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s:%d", d.GetName(), d.GetSizeGb()))
	}
	if fmt.Sprint(got) != "[d-5:40 lost:10]" {
		t.Errorf("disks = %v", got)
	}
	if got := c.aggregatedDisks(t, `labels.session-id="s"`); fmt.Sprint(got) != "[zones/us-east1-b/d-5]" {
		t.Errorf("aggregated = %v", got)
	}
	if _, err := c.disks.Get(t.Context(), &computepb.GetDiskRequest{Project: project, Zone: zone, Disk: "missing"}); apiCode(err) != http.StatusNotFound {
		t.Errorf("get of a missing disk = %v", err)
	}
}

func TestDeletingADiskAgainFinishesTheFirstDelete(t *testing.T) {
	c := setup(t, compute.WithSettleAfter(-1))
	insert := func() {
		t.Helper()
		if _, err := c.disks.Insert(t.Context(), &computepb.InsertDiskRequest{Project: project, Zone: zone, DiskResource: &computepb.Disk{Name: new("d-1")}}); err != nil {
			t.Fatal(err)
		}
		c.server.Settle()
	}
	insert()
	del := &computepb.DeleteDiskRequest{Project: project, Zone: zone, Disk: "d-1"}
	if _, err := c.disks.Delete(t.Context(), del); err != nil {
		t.Fatal(err)
	}
	if _, err := c.disks.Delete(t.Context(), del); apiCode(err) != http.StatusNotFound {
		t.Fatalf("deleting a disk being deleted = %v, want HTTP 404 once the first delete finishes", err)
	}
	insert()
	if _, ok := c.server.LookupDisk(project, zone, "d-1"); !ok {
		t.Error("a disk made after the deletes is gone")
	}
}
