package compute_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	gcompute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/option"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/compute"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

func regionInstancesClient(t *testing.T, c *clients) *gcompute.RegionInstancesClient {
	t.Helper()
	client, err := gcompute.NewRegionInstancesRESTClient(t.Context(), option.WithEndpoint(c.url), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func regionBulkRequest(policy *computepb.LocationPolicy, names ...string) *computepb.BulkInsertRegionInstanceRequest {
	zonal := bulkRequest("s", names...)
	zonal.BulkInsertInstanceResourceResource.LocationPolicy = policy
	return &computepb.BulkInsertRegionInstanceRequest{Project: project, Region: "us-east1", BulkInsertInstanceResourceResource: zonal.BulkInsertInstanceResourceResource}
}

// placement is the zone of each instance, as name@zone in name order.
func placement(c *clients) []string {
	var out []string
	for _, inst := range c.server.Instances() {
		out = append(out, inst.Name+"@"+inst.Zone[strings.LastIndexByte(inst.Zone, '/')+1:])
	}
	slices.Sort(out)
	return out
}

func TestRegionBulkInsertPlacesInstancesInTheRegionsZones(t *testing.T) {
	c := setup(t)
	region := regionInstancesClient(t, c)

	op, err := region.BulkInsert(t.Context(), regionBulkRequest(nil, "node-1", "node-2"))
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
	p := op.Proto()
	st := p.GetInstancesBulkInsertOperationMetadata().GetPerLocationStatus()
	if p.GetOperationType() != "bulkInsert" || !strings.HasSuffix(p.GetRegion(), "/regions/us-east1") || p.GetZone() != "" ||
		p.GetTargetLink() != "https://www.googleapis.com/compute/v1/projects/p" ||
		len(st) != 1 || st["zones/us-east1-b"].GetCreatedVmCount() != 2 || st["zones/us-east1-b"].GetStatus() != "DONE" {
		t.Errorf("operation = %v", p)
	}
	if got := placement(c); !slices.Equal(got, []string{"node-1@us-east1-b", "node-2@us-east1-b"}) {
		t.Errorf("placement = %v, want every instance in the first zone", got)
	}

	balanced := &computepb.LocationPolicy{
		TargetShape: new("BALANCED"),
		Locations:   map[string]*computepb.LocationPolicyLocation{"zones/us-east1-b": {Preference: new("DENY")}, "zones/us-east1-c": {Preference: new("ALLOW")}},
	}
	op, err = region.BulkInsert(t.Context(), regionBulkRequest(balanced, "node-3", "node-4", "node-5"))
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
	st = op.Proto().GetInstancesBulkInsertOperationMetadata().GetPerLocationStatus()
	if len(st) != 2 || st["zones/us-east1-c"].GetCreatedVmCount() != 2 || st["zones/us-east1-d"].GetCreatedVmCount() != 1 {
		t.Errorf("perLocationStatus = %v", st)
	}
	want := []string{"node-1@us-east1-b", "node-2@us-east1-b", "node-3@us-east1-c", "node-4@us-east1-d", "node-5@us-east1-c"}
	if got := placement(c); !slices.Equal(got, want) {
		t.Errorf("placement = %v, want %v", got, want)
	}
	if _, ok := c.server.LookupDisk(project, "us-east1-d", "node-4"); !ok {
		t.Error("node-4's boot disk is not in its zone")
	}
}

func TestRegionBulkInsertSharesTheZonalRules(t *testing.T) {
	c := setup(t)
	region := regionInstancesClient(t, c)
	if err := c.bulkInsert(t, bulkRequest("s", "vm-07")); err != nil {
		t.Fatal(err)
	}

	pattern := &computepb.BulkInsertRegionInstanceRequest{Project: project, Region: "us-east1", BulkInsertInstanceResourceResource: &computepb.BulkInsertInstanceResource{
		Count: new(int64(2)), NamePattern: new("vm-##"), InstanceProperties: properties("s"),
		LocationPolicy: &computepb.LocationPolicy{Locations: map[string]*computepb.LocationPolicyLocation{"zones/" + zone: {Preference: new("DENY")}}},
	}}
	op, err := region.BulkInsert(t.Context(), pattern)
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := placement(c); !slices.Equal(got, []string{"vm-07@us-east1-b", "vm-08@us-east1-c", "vm-09@us-east1-c"}) {
		t.Errorf("placement = %v, want the pattern numbered after every zone's instances", got)
	}

	op, err = region.BulkInsert(t.Context(), regionBulkRequest(nil, "vm-08"))
	if err == nil {
		err = op.Wait(t.Context())
	}
	if sdkerr.GoogleCode(err) != http.StatusConflict || !strings.Contains(err.Error(), "VM_MIN_COUNT_NOT_REACHED") ||
		!strings.Contains(err.Error(), "projects/p/zones/us-east1-c/instances/vm-08' already exists") {
		t.Errorf("a name another zone of the region holds = %v, want a 409 operation error", err)
	}

	for name, policy := range map[string]*computepb.LocationPolicy{
		"a zone of another region": {Locations: map[string]*computepb.LocationPolicyLocation{"zones/us-west1-a": {Preference: new("ALLOW")}}},
		"every zone denied": {Locations: map[string]*computepb.LocationPolicyLocation{
			"zones/us-east1-b": {Preference: new("DENY")}, "zones/us-east1-c": {Preference: new("DENY")}, "zones/us-east1-d": {Preference: new("DENY")},
		}},
	} {
		if _, err := region.BulkInsert(t.Context(), regionBulkRequest(policy, "other")); sdkerr.GoogleCode(err) != http.StatusBadRequest {
			t.Errorf("%s = %v, want HTTP 400", name, err)
		}
	}
	unknown := regionBulkRequest(nil, "other")
	unknown.Region = "mars-north1"
	if _, err := region.BulkInsert(t.Context(), unknown); sdkerr.GoogleCode(err) != http.StatusBadRequest {
		t.Errorf("a region with no zones = %v, want HTTP 400", err)
	}
}

func TestRegionBulkInsertFaults(t *testing.T) {
	faults := fault.New().Add(fault.Rule{
		Method: "compute.regionInstances.bulkInsert", Resource: "projects/p/regions/us-east1/instances/node-1",
		Fault: fault.FailOperation(http.StatusBadRequest).WithMessage("no capacity"),
	})
	c := setup(t, compute.WithFaults(faults))
	region := regionInstancesClient(t, c)

	op, err := region.BulkInsert(t.Context(), regionBulkRequest(nil, "node-2", "node-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(t.Context()); sdkerr.GoogleCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "no capacity") {
		t.Errorf("wait = %v, want the fault's message", err)
	}
	if st := op.Proto().GetInstancesBulkInsertOperationMetadata().GetPerLocationStatus()["zones/us-east1-b"]; st.GetFailedToCreateVmCount() != 2 {
		t.Errorf("perLocationStatus = %v", st)
	}
	if len(c.server.Instances()) != 0 {
		t.Error("a failed operation created instances")
	}
	if got := faults.Injected(); len(got) != 1 {
		t.Errorf("injected = %v", got)
	}
}

func TestRegionBulkInsertSettles(t *testing.T) {
	c := setup(t, compute.WithSettleAfter(-1))
	region := regionInstancesClient(t, c)
	op, err := region.BulkInsert(t.Context(), regionBulkRequest(nil, "node-1"))
	if err != nil {
		t.Fatal(err)
	}
	if st := op.Proto().GetInstancesBulkInsertOperationMetadata().GetPerLocationStatus()["zones/us-east1-b"]; st.GetStatus() != "CREATING" || op.Done() {
		t.Errorf("before Settle: done %v, perLocationStatus %v", op.Done(), st)
	}
	c.server.Settle()
	if err := op.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st := op.Proto().GetInstancesBulkInsertOperationMetadata().GetPerLocationStatus()["zones/us-east1-b"]; !op.Done() || st.GetCreatedVmCount() != 1 {
		t.Errorf("after Settle: done %v, perLocationStatus %v", op.Done(), st)
	}
}

func TestRegionBulkInsertNeedsInstancesCreate(t *testing.T) {
	c := setup(t, compute.WithPolicy(access.Allow(
		"compute.instances.create", "compute.disks.create", "compute.images.useReadOnly", "compute.disks.setLabels",
		"compute.subnetworks.use", "compute.subnetworks.useExternalIp", "compute.instances.setTags",
		"compute.instances.setLabels", "compute.instances.setServiceAccount", "iam.serviceAccounts.actAs",
	)))
	region := regionInstancesClient(t, c)
	if _, err := region.BulkInsert(t.Context(), regionBulkRequest(nil, "node-1")); sdkerr.GoogleCode(err) != http.StatusForbidden ||
		!strings.Contains(err.Error(), "compute.instances.setMetadata") {
		t.Errorf("bulkInsert without compute.instances.setMetadata = %v, want HTTP 403 naming it", err)
	}
}
