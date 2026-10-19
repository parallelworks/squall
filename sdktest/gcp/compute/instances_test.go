package compute_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	gcompute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	gcomputebeta "cloud.google.com/go/compute/apiv1beta"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/compute"
)

const zone = "us-east1-b"

// clients are the official clients of every service the stand-in answers,
// pointed at one server.
type clients struct {
	server     *compute.Server
	instances  *gcompute.InstancesClient
	zoneOps    *gcompute.ZoneOperationsClient
	zones      *gcompute.ZonesClient
	disks      *gcompute.DisksClient
	addresses  *gcompute.AddressesClient
	routes     *gcompute.RoutesClient
	templates  *gcompute.InstanceTemplatesClient
	managers   *gcompute.InstanceGroupManagersClient
	policies   *gcomputebeta.ResourcePoliciesClient
	httpClient *http.Client
	url        string
}

func setup(t *testing.T, opts ...compute.Option) *clients {
	t.Helper()
	s := compute.New(opts...)
	if _, err := s.PutImage("debian-cloud", compute.Image{Name: "debian-12-v1", Family: "debian-12", DiskSizeGB: 10}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	o := []option.ClientOption{option.WithEndpoint(srv.URL), option.WithoutAuthentication()}
	c := &clients{server: s, httpClient: srv.Client(), url: srv.URL}
	var err error
	ctx := t.Context()
	for _, open := range []func() error{
		func() error { c.instances, err = gcompute.NewInstancesRESTClient(ctx, o...); return err },
		func() error { c.zoneOps, err = gcompute.NewZoneOperationsRESTClient(ctx, o...); return err },
		func() error { c.zones, err = gcompute.NewZonesRESTClient(ctx, o...); return err },
		func() error { c.disks, err = gcompute.NewDisksRESTClient(ctx, o...); return err },
		func() error { c.addresses, err = gcompute.NewAddressesRESTClient(ctx, o...); return err },
		func() error { c.routes, err = gcompute.NewRoutesRESTClient(ctx, o...); return err },
		func() error { c.templates, err = gcompute.NewInstanceTemplatesRESTClient(ctx, o...); return err },
		func() error { c.managers, err = gcompute.NewInstanceGroupManagersRESTClient(ctx, o...); return err },
		func() error { c.policies, err = gcomputebeta.NewResourcePoliciesRESTClient(ctx, o...); return err },
	} {
		if err := open(); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// properties are instance properties shaped the way provisioning code sends
// them to bulkInsert: bare machine and disk type names.
func properties(session string) *computepb.InstanceProperties {
	labels := map[string]string{"session-id": session}
	return &computepb.InstanceProperties{
		MachineType: new("n2-standard-4"),
		Disks: []*computepb.AttachedDisk{{
			Boot: new(true), AutoDelete: new(true), DiskSizeGb: new(int64(20)),
			InitializeParams: &computepb.AttachedDiskInitializeParams{
				SourceImage: new("projects/debian-cloud/global/images/family/debian-12"), Labels: labels,
			},
			GuestOsFeatures: []*computepb.GuestOsFeature{{Type: new("GVNIC")}},
		}},
		NetworkInterfaces: []*computepb.NetworkInterface{{
			Subnetwork:    new("projects/p/regions/us-east1/subnetworks/sub"),
			NicType:       new("GVNIC"),
			AccessConfigs: []*computepb.AccessConfig{{Type: new("ONE_TO_ONE_NAT")}},
		}},
		CanIpForward:             new(false),
		NetworkPerformanceConfig: &computepb.NetworkPerformanceConfig{TotalEgressBandwidthTier: new("DEFAULT")},
		Metadata:                 &computepb.Metadata{Items: []*computepb.Items{{Key: new("startup-script"), Value: new("echo hi")}}},
		Tags:                     &computepb.Tags{Items: []string{"allow-ssh"}},
		Labels:                   labels,
		Scheduling:               &computepb.Scheduling{OnHostMaintenance: new("TERMINATE"), AutomaticRestart: new(false)},
		ServiceAccounts:          []*computepb.ServiceAccount{{Email: new("sa@p.iam.gserviceaccount.com"), Scopes: []string{"https://www.googleapis.com/auth/logging.write"}}},
	}
}

func bulkRequest(session string, names ...string) *computepb.BulkInsertInstanceRequest {
	per := map[string]*computepb.BulkInsertInstanceResourcePerInstanceProperties{}
	for _, name := range names {
		per[name] = &computepb.BulkInsertInstanceResourcePerInstanceProperties{Name: new(name)}
	}
	count := int64(len(names))
	return &computepb.BulkInsertInstanceRequest{
		Project: project, Zone: zone,
		BulkInsertInstanceResourceResource: &computepb.BulkInsertInstanceResource{
			Count: &count, MinCount: &count, PerInstanceProperties: per, InstanceProperties: properties(session),
		},
	}
}

func (c *clients) bulkInsert(t *testing.T, req *computepb.BulkInsertInstanceRequest) error {
	t.Helper()
	op, err := c.instances.BulkInsert(t.Context(), req)
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

func (c *clients) insert(t *testing.T, inst *computepb.Instance) error {
	t.Helper()
	op, err := c.instances.Insert(t.Context(), &computepb.InsertInstanceRequest{Project: project, Zone: zone, InstanceResource: inst})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

// single is an instances.insert body, as a single insert takes it: zone-qualified type URLs.
func single(name, session string) *computepb.Instance {
	p := properties(session)
	p.Disks[0].InitializeParams.DiskType = new("zones/" + zone + "/diskTypes/pd-balanced")
	return &computepb.Instance{
		Name: new(name), MachineType: new("zones/" + zone + "/machineTypes/n2-standard-2"),
		Disks: p.Disks, NetworkInterfaces: p.NetworkInterfaces, Metadata: p.Metadata, Tags: p.Tags,
		Labels: p.Labels, Scheduling: p.Scheduling, ServiceAccounts: p.ServiceAccounts,
	}
}

func (c *clients) list(t *testing.T, filter string, pageSize int) ([]*computepb.Instance, error) {
	t.Helper()
	req := &computepb.ListInstancesRequest{Project: project, Zone: zone}
	if filter != "" {
		req.Filter = new(filter)
	}
	it := c.instances.List(t.Context(), req)
	it.PageInfo().MaxSize = pageSize
	var out []*computepb.Instance
	for {
		inst, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, inst)
	}
}

func apiReason(err error) string {
	if e, ok := errors.AsType[*googleapi.Error](err); ok && len(e.Errors) > 0 {
		return e.Errors[0].Reason
	}
	return ""
}

func names(insts []*computepb.Instance) string {
	var out []string
	for _, inst := range insts {
		out = append(out, inst.GetName())
	}
	return fmt.Sprint(out)
}

func (c *clients) get(t *testing.T, name string) (*computepb.Instance, error) {
	t.Helper()
	return c.instances.Get(t.Context(), &computepb.GetInstanceRequest{Project: project, Zone: zone, Instance: name})
}

func TestBulkInsertCreatesRunningInstances(t *testing.T) {
	c := setup(t)
	op, err := c.instances.BulkInsert(t.Context(), bulkRequest("s1", "node-2", "node-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p := op.Proto(); p.GetOperationType() != "bulkInsert" || !strings.HasSuffix(p.GetZone(), "/zones/"+zone) ||
		p.GetInstancesBulkInsertOperationMetadata().GetPerLocationStatus()["zones/"+zone].GetCreatedVmCount() != 2 {
		t.Errorf("operation = %v", p)
	}
	insts, err := c.list(t, "labels.session-id=s1 AND (name=node-1 OR name=node-2)", 0)
	if err != nil || names(insts) != "[node-1 node-2]" {
		t.Fatalf("list = %s, %v", names(insts), err)
	}
	inst := insts[0]
	nic := inst.GetNetworkInterfaces()[0]
	boot := inst.GetDisks()[0]
	switch {
	case inst.GetStatus() != "RUNNING" || inst.GetId() == 0 || inst.GetLabels()["session-id"] != "s1":
		t.Errorf("instance = %v", inst)
	case inst.GetMachineType() != "https://www.googleapis.com/compute/v1/projects/p/zones/us-east1-b/machineTypes/n2-standard-4":
		t.Errorf("machineType = %s", inst.GetMachineType())
	case inst.GetZone() != "https://www.googleapis.com/compute/v1/projects/p/zones/us-east1-b":
		t.Errorf("zone = %s", inst.GetZone())
	case nic.GetName() != "nic0" || !strings.HasPrefix(nic.GetNetworkIP(), "10.") || !strings.HasPrefix(nic.GetAccessConfigs()[0].GetNatIP(), "100.64."):
		t.Errorf("interface = %v", nic)
	case nic.GetSubnetwork() != "https://www.googleapis.com/compute/v1/projects/p/regions/us-east1/subnetworks/sub":
		t.Errorf("subnetwork = %s", nic.GetSubnetwork())
	case !boot.GetBoot() || !boot.GetAutoDelete() || boot.GetDiskSizeGb() != 20 || !strings.HasSuffix(boot.GetSource(), "/disks/node-1"):
		t.Errorf("boot disk = %v", boot)
	case inst.GetMetadata().GetItems()[0].GetKey() != "startup-script" || inst.GetTags().GetItems()[0] != "allow-ssh":
		t.Errorf("metadata and tags = %v %v", inst.GetMetadata(), inst.GetTags())
	case inst.GetScheduling().GetOnHostMaintenance() != "TERMINATE" || inst.GetScheduling().GetAutomaticRestart():
		t.Errorf("scheduling = %v", inst.GetScheduling())
	case inst.GetServiceAccounts()[0].GetEmail() != "sa@p.iam.gserviceaccount.com" || inst.GetNetworkPerformanceConfig().GetTotalEgressBandwidthTier() != "DEFAULT":
		t.Errorf("instance = %v", inst)
	}
	disk, err := c.disks.Get(t.Context(), &computepb.GetDiskRequest{Project: project, Zone: zone, Disk: "node-1"})
	if err != nil {
		t.Fatal(err)
	}
	if disk.GetStatus() != "READY" || disk.GetSizeGb() != 20 || !strings.HasSuffix(disk.GetSourceImage(), "/images/debian-12-v1") ||
		!strings.HasSuffix(disk.GetType(), "/diskTypes/pd-standard") || len(disk.GetUsers()) != 1 || disk.GetLabels()["session-id"] != "s1" {
		t.Errorf("boot disk = %v", disk)
	}
	if nic.GetNetworkIP() == insts[1].GetNetworkInterfaces()[0].GetNetworkIP() {
		t.Error("two instances share an internal IP")
	}
}

func TestBulkInsertNamesFromAPattern(t *testing.T) {
	c := setup(t)
	pattern := func(count int64) *computepb.BulkInsertInstanceRequest {
		return &computepb.BulkInsertInstanceRequest{Project: project, Zone: zone, BulkInsertInstanceResourceResource: &computepb.BulkInsertInstanceResource{
			Count: new(count), NamePattern: new("vm-####"), InstanceProperties: properties("s"),
		}}
	}
	if err := c.bulkInsert(t, pattern(3)); err != nil {
		t.Fatal(err)
	}
	if err := c.bulkInsert(t, pattern(2)); err != nil {
		t.Fatal(err)
	}
	insts, err := c.list(t, "", 0)
	if err != nil || names(insts) != "[vm-0001 vm-0002 vm-0003 vm-0004 vm-0005]" {
		t.Fatalf("list = %s, %v", names(insts), err)
	}
	bad := pattern(2)
	bad.BulkInsertInstanceResourceResource.NamePattern = new("vm-##-##")
	if err := c.bulkInsert(t, bad); apiCode(err) != http.StatusBadRequest {
		t.Errorf("a pattern with two runs = %v, want HTTP 400", err)
	}
	none := pattern(3)
	none.BulkInsertInstanceResourceResource.NamePattern = nil
	if err := c.bulkInsert(t, none); apiCode(err) != http.StatusBadRequest {
		t.Errorf("a count with neither names nor a pattern = %v, want HTTP 400", err)
	}
}

func TestBulkInsertRetriedAfterALostResponse(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "compute.instances.bulkInsert", Nth: 1, Fault: fault.LoseResponse()})
	c := setup(t, compute.WithFaults(faults))
	req := bulkRequest("s", "node-1", "node-2")
	if _, err := c.instances.BulkInsert(t.Context(), req); err == nil || apiCode(err) != 0 {
		t.Fatalf("first bulkInsert = %v, want a transport error", err)
	}
	if got := len(c.server.Instances()); got != 2 {
		t.Fatalf("%d instances after the lost response, want 2", got)
	}
	// Without a request ID the retry is a new request, and the names are taken.
	err := c.bulkInsert(t, req)
	if apiCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "RESOURCE_ALREADY_EXISTS") ||
		!strings.Contains(err.Error(), "VM_MIN_COUNT_NOT_REACHED") || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("retry = %v, want a 400 operation error naming the existing instances", err)
	}
	if got := faults.Injected(); len(got) != 1 || got[0].Call.Resource != "projects/p/zones/us-east1-b/instances/node-1" {
		t.Errorf("injected = %v", got)
	}

	// With one, the retry gets the first call's operation back.
	faults.Add(fault.Rule{Method: "compute.instances.bulkInsert", Nth: 1, Fault: fault.LoseResponse()})
	again := bulkRequest("s", "node-3")
	again.RequestId = new("1b4e28ba-2fa1-11d2-883f-0016d3cca427")
	if _, err := c.instances.BulkInsert(t.Context(), again); err == nil {
		t.Fatal("the bulkInsert with a request ID did not lose its response")
	}
	if err := c.bulkInsert(t, again); err != nil {
		t.Fatalf("retrying with the request ID: %v", err)
	}
	if got := len(c.server.Instances()); got != 3 {
		t.Fatalf("%d instances, want 3", got)
	}
}

func TestInstanceInsertErrors(t *testing.T) {
	c := setup(t)
	if err := c.insert(t, single("taken", "s")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*computepb.Instance)
		code   int
	}{
		{"a name in use", func(i *computepb.Instance) { i.Name = new("taken") }, http.StatusConflict},
		{"an invalid name", func(i *computepb.Instance) { i.Name = new("Bad_Name") }, http.StatusBadRequest},
		{"no machine type", func(i *computepb.Instance) { i.MachineType = nil }, http.StatusBadRequest},
		{"a machine type in another zone", func(i *computepb.Instance) { i.MachineType = new("zones/us-east1-c/machineTypes/n2-standard-2") }, http.StatusBadRequest},
		{"an unknown image", func(i *computepb.Instance) {
			i.Disks[0].InitializeParams.SourceImage = new("projects/debian-cloud/global/images/missing")
		}, http.StatusNotFound},
		{"a subnetwork in another region", func(i *computepb.Instance) {
			i.NetworkInterfaces[0].Subnetwork = new("regions/europe-west1/subnetworks/sub")
		}, http.StatusBadRequest},
		{"no boot disk", func(i *computepb.Instance) { i.Disks[0].Boot = new(false) }, http.StatusBadRequest},
		{"an unknown scheduling value", func(i *computepb.Instance) { i.Scheduling.ProvisioningModel = new("CHEAP") }, http.StatusBadRequest},
		{"a field the server does not model", func(i *computepb.Instance) { i.KeyRevocationActionType = new("STOP") }, http.StatusBadRequest},
		{"deletion protection", func(i *computepb.Instance) { i.DeletionProtection = new(true) }, http.StatusBadRequest},
		{"an internal IP in use", func(i *computepb.Instance) { i.NetworkInterfaces[0].NetworkIP = new("10.0.0.2") }, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := single("vm-new", "s")
			tc.change(inst)
			if err := c.insert(t, inst); apiCode(err) != tc.code {
				t.Fatalf("insert = %v, want HTTP %d", err, tc.code)
			}
		})
	}
	if got := len(c.server.Instances()); got != 1 {
		t.Fatalf("%d instances, want only the first", got)
	}
	_, err := c.instances.Insert(t.Context(), &computepb.InsertInstanceRequest{Project: project, Zone: "us-east1-z", InstanceResource: single("vm", "s")})
	if apiCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "Unknown zone") {
		t.Errorf("insert in an unknown zone = %v", err)
	}
}

func TestGetAndDeleteByID(t *testing.T) {
	c := setup(t)
	if err := c.insert(t, single("vm-1", "s")); err != nil {
		t.Fatal(err)
	}
	inst, _ := c.get(t, "vm-1")
	id := strconv.FormatUint(inst.GetId(), 10)
	if got, err := c.get(t, id); err != nil || got.GetName() != "vm-1" {
		t.Fatalf("get by ID = %v, %v", got, err)
	}
	op, err := c.instances.Delete(t.Context(), &computepb.DeleteInstanceRequest{Project: project, Zone: zone, Instance: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if insts, err := c.list(t, "id="+id, 0); err != nil || len(insts) != 0 {
		t.Fatalf("list by ID after delete = %s, %v", names(insts), err)
	}
	if _, err := c.get(t, "vm-1"); apiCode(err) != http.StatusNotFound {
		t.Errorf("get after delete = %v", err)
	}
	_, err = c.instances.Delete(t.Context(), &computepb.DeleteInstanceRequest{Project: project, Zone: zone, Instance: id})
	if apiCode(err) != http.StatusNotFound {
		t.Errorf("deleting it again = %v, want HTTP 404", err)
	}
}

func TestDeleteHonorsAutoDelete(t *testing.T) {
	c := setup(t)
	inst := single("vm-1", "s")
	inst.Disks = append(inst.Disks,
		&computepb.AttachedDisk{AutoDelete: new(true), DeviceName: new("scratch"), InitializeParams: &computepb.AttachedDiskInitializeParams{DiskSizeGb: new(int64(50))}},
		&computepb.AttachedDisk{AutoDelete: new(false), DeviceName: new("data"), InitializeParams: &computepb.AttachedDiskInitializeParams{
			DiskName: new("vm-1-data"), DiskSizeGb: new(int64(100)), DiskType: new("zones/" + zone + "/diskTypes/pd-ssd"), Labels: map[string]string{"session-id": "s"},
		}},
	)
	if err := c.insert(t, inst); err != nil {
		t.Fatal(err)
	}
	got, _ := c.get(t, "vm-1")
	if d := got.GetDisks(); len(d) != 3 || d[1].GetDeviceName() != "scratch" || !strings.HasSuffix(d[1].GetSource(), "/disks/vm-1-1") || d[2].GetIndex() != 2 {
		t.Fatalf("disks = %v", d)
	}
	_, err := c.disks.Delete(t.Context(), &computepb.DeleteDiskRequest{Project: project, Zone: zone, Disk: "vm-1-data"})
	if apiCode(err) != http.StatusBadRequest || apiReason(err) != "resourceInUseByAnotherResource" {
		t.Fatalf("deleting an attached disk = %v", err)
	}
	op, err := c.instances.Delete(t.Context(), &computepb.DeleteInstanceRequest{Project: project, Zone: zone, Instance: "vm-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	disks := c.aggregatedDisks(t, `labels.session-id="s"`)
	if fmt.Sprint(disks) != "[zones/us-east1-b/vm-1-data]" {
		t.Fatalf("disks after delete = %v, want only the one not auto-deleted", disks)
	}
	if d, _ := c.server.LookupDisk(project, zone, "vm-1-data"); len(d.Users) != 0 || d.LastDetachTimestamp == "" {
		t.Errorf("kept disk = %+v", d)
	}
	op, err = c.disks.Delete(t.Context(), &computepb.DeleteDiskRequest{Project: project, Zone: zone, Disk: "vm-1-data"})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(t.Context()); err != nil || len(c.server.Disks()) != 0 {
		t.Fatalf("disk delete = %v, disks %v", err, c.server.Disks())
	}
}

func (c *clients) aggregatedDisks(t *testing.T, filter string) []string {
	t.Helper()
	it := c.disks.AggregatedList(t.Context(), &computepb.AggregatedListDisksRequest{Project: project, Filter: new(filter)})
	var out []string
	for {
		pair, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range pair.Value.GetDisks() {
			out = append(out, pair.Key+"/"+d.GetName())
		}
	}
}

func TestStopStartAndSetMachineType(t *testing.T) {
	c := setup(t)
	if err := c.insert(t, single("vm-1", "s")); err != nil {
		t.Fatal(err)
	}
	before, _ := c.get(t, "vm-1")
	setType := func(machineType string) error {
		op, err := c.instances.SetMachineType(t.Context(), &computepb.SetMachineTypeInstanceRequest{
			Project: project, Zone: zone, Instance: "vm-1",
			InstancesSetMachineTypeRequestResource: &computepb.InstancesSetMachineTypeRequest{MachineType: new(machineType)},
		})
		if err != nil {
			return err
		}
		return op.Wait(t.Context())
	}
	if err := setType("zones/" + zone + "/machineTypes/n2-standard-8"); apiCode(err) != http.StatusBadRequest {
		t.Fatalf("setMachineType while running = %v, want HTTP 400", err)
	}
	stop, err := c.instances.Stop(t.Context(), &computepb.StopInstanceRequest{Project: project, Zone: zone, Instance: "vm-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := stop.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	stopped, _ := c.get(t, "vm-1")
	if stopped.GetStatus() != "TERMINATED" || stopped.GetNetworkInterfaces()[0].GetAccessConfigs()[0].GetNatIP() != "" ||
		stopped.GetNetworkInterfaces()[0].GetNetworkIP() != before.GetNetworkInterfaces()[0].GetNetworkIP() || stopped.GetLastStopTimestamp() == "" {
		t.Fatalf("stopped instance = %v", stopped)
	}
	// Stopping a stopped instance is a no-op that succeeds.
	again, err := c.instances.Stop(t.Context(), &computepb.StopInstanceRequest{Project: project, Zone: zone, Instance: "vm-1"})
	if err != nil || again.Wait(t.Context()) != nil {
		t.Fatalf("stopping again = %v", err)
	}
	if err := setType("zones/" + zone + "/machineTypes/n2-standard-8"); err != nil {
		t.Fatalf("setMachineType while stopped: %v", err)
	}
	start, err := c.instances.Start(t.Context(), &computepb.StartInstanceRequest{Project: project, Zone: zone, Instance: "vm-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := start.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	started, _ := c.get(t, "vm-1")
	if started.GetStatus() != "RUNNING" || !strings.HasSuffix(started.GetMachineType(), "/machineTypes/n2-standard-8") ||
		started.GetNetworkInterfaces()[0].GetAccessConfigs()[0].GetNatIP() == "" {
		t.Fatalf("started instance = %v", started)
	}
}

func TestSetLabelsChecksTheInstanceFingerprint(t *testing.T) {
	c := setup(t)
	if err := c.insert(t, single("vm-1", "s")); err != nil {
		t.Fatal(err)
	}
	inst, _ := c.get(t, "vm-1")
	setLabels := func(fp string, labels map[string]string) error {
		op, err := c.instances.SetLabels(t.Context(), &computepb.SetLabelsInstanceRequest{
			Project: project, Zone: zone, Instance: "vm-1",
			InstancesSetLabelsRequestResource: &computepb.InstancesSetLabelsRequest{LabelFingerprint: new(fp), Labels: labels},
		})
		if err != nil {
			return err
		}
		return op.Wait(t.Context())
	}
	labels := inst.GetLabels()
	labels["stopped-by"] = "me"
	if err := setLabels(inst.GetLabelFingerprint(), labels); err != nil {
		t.Fatal(err)
	}
	after, _ := c.get(t, "vm-1")
	if after.GetLabels()["stopped-by"] != "me" || after.GetLabelFingerprint() == inst.GetLabelFingerprint() {
		t.Fatalf("labels = %v", after.GetLabels())
	}
	if err := setLabels(inst.GetLabelFingerprint(), nil); apiCode(err) != http.StatusPreconditionFailed {
		t.Fatalf("a stale fingerprint = %v, want HTTP 412", err)
	}
}

func TestAddAccessConfigClaimsAReservedAddress(t *testing.T) {
	c := setup(t)
	inst := single("vm-1", "s")
	inst.NetworkInterfaces[0].AccessConfigs = nil
	if err := c.insert(t, inst); err != nil {
		t.Fatal(err)
	}
	if err := insert(t, c.addresses, &computepb.Address{Name: new("ip-1")}); err != nil {
		t.Fatal(err)
	}
	reserved, _ := c.server.LookupAddress(project, region, "ip-1")
	add := func(nic, natIP string) error {
		op, err := c.instances.AddAccessConfig(t.Context(), &computepb.AddAccessConfigInstanceRequest{
			Project: project, Zone: zone, Instance: "vm-1", NetworkInterface: nic,
			AccessConfigResource: &computepb.AccessConfig{Name: new("External NAT"), Type: new("ONE_TO_ONE_NAT"), NatIP: new(natIP)},
		})
		if err != nil {
			return err
		}
		return op.Wait(t.Context())
	}
	if err := add("nic0", "100.64.9.9"); apiCode(err) != http.StatusBadRequest {
		t.Fatalf("an address nobody reserved = %v, want HTTP 400", err)
	}
	if err := add("nic1", reserved.Address); apiCode(err) != http.StatusBadRequest {
		t.Fatalf("a missing interface = %v, want HTTP 400", err)
	}
	if err := add("nic0", reserved.Address); err != nil {
		t.Fatal(err)
	}
	got, _ := c.get(t, "vm-1")
	if ac := got.GetNetworkInterfaces()[0].GetAccessConfigs(); len(ac) != 1 || ac[0].GetNatIP() != reserved.Address {
		t.Fatalf("access configs = %v", ac)
	}
	if a, _ := c.server.LookupAddress(project, region, "ip-1"); a.Status != "IN_USE" || len(a.Users) != 1 {
		t.Fatalf("address = %+v", a)
	}
	if err := add("nic0", reserved.Address); apiCode(err) != http.StatusBadRequest {
		t.Fatalf("a second access config = %v, want HTTP 400", err)
	}
	_, err := c.addresses.Delete(t.Context(), &computepb.DeleteAddressRequest{Project: project, Region: region, Address: "ip-1"})
	if apiCode(err) != http.StatusBadRequest {
		t.Fatalf("deleting an address in use = %v, want HTTP 400", err)
	}
	op, err := c.instances.Delete(t.Context(), &computepb.DeleteInstanceRequest{Project: project, Zone: zone, Instance: "vm-1"})
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := c.server.LookupAddress(project, region, "ip-1"); a.Status != "RESERVED" || len(a.Users) != 0 {
		t.Fatalf("address after the instance went = %+v", a)
	}
}

func TestAttachAndDetachDisk(t *testing.T) {
	c := setup(t)
	if err := c.insert(t, single("vm-1", "s")); err != nil {
		t.Fatal(err)
	}
	op, err := c.disks.Insert(t.Context(), &computepb.InsertDiskRequest{Project: project, Zone: zone, DiskResource: &computepb.Disk{
		Name: new("data-1"), SizeGb: new(int64(200)), Type: new("zones/" + zone + "/diskTypes/pd-balanced"), Labels: map[string]string{"session-id": "s"},
	}})
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
	attach, err := c.instances.AttachDisk(t.Context(), &computepb.AttachDiskInstanceRequest{
		Project: project, Zone: zone, Instance: "vm-1",
		AttachedDiskResource: &computepb.AttachedDisk{Source: new("projects/p/zones/" + zone + "/disks/data-1"), DeviceName: new("data-1")},
	})
	if err == nil {
		err = attach.Wait(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := c.server.LookupDisk(project, zone, "data-1"); len(d.Users) != 1 {
		t.Fatalf("users = %v", d.Users)
	}
	detach := func(device string) error {
		op, err := c.instances.DetachDisk(t.Context(), &computepb.DetachDiskInstanceRequest{Project: project, Zone: zone, Instance: "vm-1", DeviceName: device})
		if err != nil {
			return err
		}
		return op.Wait(t.Context())
	}
	if err := detach("data-1"); err != nil {
		t.Fatal(err)
	}
	if err := detach("data-1"); apiCode(err) != http.StatusBadRequest {
		t.Fatalf("detaching it again = %v, want HTTP 400", err)
	}
	if d, _ := c.server.LookupDisk(project, zone, "data-1"); len(d.Users) != 0 {
		t.Fatalf("users after detach = %v", d.Users)
	}
}

func TestListFiltersAndPagesInstances(t *testing.T) {
	c := setup(t)
	for i := range 5 {
		session := "mine"
		if i == 2 {
			session = "theirs"
		}
		if err := c.insert(t, single(fmt.Sprintf("vm-%d", i), session)); err != nil {
			t.Fatal(err)
		}
	}
	vm3, _ := c.get(t, "vm-3")
	vm4, _ := c.get(t, "vm-4")
	for _, tc := range []struct {
		filter string
		want   string
	}{
		{"", "[vm-0 vm-1 vm-2 vm-3 vm-4]"},
		{"labels.session-id=mine", "[vm-0 vm-1 vm-3 vm-4]"},
		{fmt.Sprintf("id=%d OR id=%d", vm3.GetId(), vm4.GetId()), "[vm-3 vm-4]"},
		{"labels.session-id=mine AND (name=vm-1 OR name=vm-2 OR name=vm-3)", "[vm-1 vm-3]"},
		{"name eq vm-[0-2]", "[vm-0 vm-1 vm-2]"},
		{"name ne vm-[0-2]", "[vm-3 vm-4]"},
		{"(name eq vm-.*) (labels.session-id eq th.*)", "[vm-2]"},
		{`status = "RUNNING" labels.session-id != mine`, "[vm-2]"},
	} {
		insts, err := c.list(t, tc.filter, 2)
		if err != nil || names(insts) != tc.want {
			t.Errorf("list %q = %s, %v; want %s", tc.filter, names(insts), err, tc.want)
		}
	}
	for _, bad := range []string{"cpuPlatform = x", "name eq a AND name eq b", "name = vm-*"} {
		if _, err := c.list(t, bad, 0); apiCode(err) != http.StatusBadRequest {
			t.Errorf("list %q = %v, want HTTP 400", bad, err)
		}
	}
	other := single("vm-9", "mine")
	other.MachineType = new("zones/us-east1-c/machineTypes/n2-standard-2")
	other.Disks[0].InitializeParams.DiskType = nil
	op, err := c.instances.Insert(t.Context(), &computepb.InsertInstanceRequest{Project: project, Zone: "us-east1-c", InstanceResource: other})
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
	it := c.instances.AggregatedList(t.Context(), &computepb.AggregatedListInstancesRequest{Project: project, Filter: new("labels.session-id=mine")})
	it.PageInfo().MaxSize = 2
	var seen []string
	for {
		pair, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, inst := range pair.Value.GetInstances() {
			seen = append(seen, pair.Key+"/"+inst.GetName())
		}
	}
	slices.Sort(seen)
	if want := "[zones/us-east1-b/vm-0 zones/us-east1-b/vm-1 zones/us-east1-b/vm-3 zones/us-east1-b/vm-4 zones/us-east1-c/vm-9]"; fmt.Sprint(seen) != want {
		t.Errorf("aggregatedList = %v, want %s", seen, want)
	}
}

func TestZonesList(t *testing.T) {
	c := setup(t, compute.WithZones("us-east1", "us-east1-b", "us-east1-c"), compute.WithZones("asia-east1"))
	list := func(filter string) []string {
		req := &computepb.ListZonesRequest{Project: project}
		if filter != "" {
			req.Filter = new(filter)
		}
		it := c.zones.List(t.Context(), req)
		var out []string
		for {
			z, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return out
			}
			if err != nil {
				t.Fatal(err)
			}
			if z.GetStatus() != "UP" || !strings.HasSuffix(z.GetRegion(), "/regions/"+z.GetName()[:strings.LastIndexByte(z.GetName(), '-')]) {
				t.Errorf("zone = %v", z)
			}
			out = append(out, z.GetName())
		}
	}
	if got := list("name eq us-east1-.*"); fmt.Sprint(got) != "[us-east1-b us-east1-c]" {
		t.Errorf("us-east1 zones = %v", got)
	}
	if got := list("region eq .*/regions/asia-east1"); len(got) != 0 {
		t.Errorf("asia-east1 zones = %v, want none once removed", got)
	}
	if got := list(""); len(got) < 10 {
		t.Errorf("all zones = %v", got)
	}
	_, err := c.instances.Get(t.Context(), &computepb.GetInstanceRequest{Project: project, Zone: "us-east1-d", Instance: "vm"})
	if apiCode(err) != http.StatusBadRequest {
		t.Errorf("a zone WithZones removed = %v, want HTTP 400", err)
	}
	if _, err := c.zones.Get(t.Context(), &computepb.GetZoneRequest{Project: project, Zone: "us-east1-d"}); apiCode(err) != http.StatusNotFound {
		t.Errorf("zones.get of a removed zone = %v, want HTTP 404", err)
	}
}

func TestOperationsSettleAfterReads(t *testing.T) {
	c := setup(t, compute.WithSettleAfter(2))
	op, err := c.instances.Insert(t.Context(), &computepb.InsertInstanceRequest{Project: project, Zone: zone, InstanceResource: single("vm-1", "s")})
	if err != nil {
		t.Fatal(err)
	}
	if op.Done() {
		t.Fatal("the insert's operation is done before any read")
	}
	var statuses []string
	for range 3 {
		inst, err := c.get(t, "vm-1")
		if err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, inst.GetStatus())
	}
	if fmt.Sprint(statuses) != "[PROVISIONING STAGING RUNNING]" {
		t.Errorf("statuses = %v", statuses)
	}
	if err := op.Wait(t.Context()); err != nil || !op.Done() {
		t.Fatalf("wait = %v", err)
	}

	// An operations.wait finishes the operation it waits for.
	del, err := c.instances.Delete(t.Context(), &computepb.DeleteInstanceRequest{Project: project, Zone: zone, Instance: "vm-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c.get(t, "vm-1"); got.GetStatus() != "STOPPING" {
		t.Errorf("status while deleting = %s", got.GetStatus())
	}
	// A change waits for the one in progress, which here leaves nothing to change.
	_, err = c.instances.Stop(t.Context(), &computepb.StopInstanceRequest{Project: project, Zone: zone, Instance: "vm-1"})
	if apiCode(err) != http.StatusNotFound {
		t.Fatalf("stop while deleting = %v, want HTTP 404", err)
	}
	waited, err := c.zoneOps.Wait(t.Context(), &computepb.WaitZoneOperationRequest{Project: project, Zone: zone, Operation: del.Name()})
	if err != nil || waited.GetStatus() != computepb.Operation_DONE {
		t.Fatalf("zoneOperations.wait = %v, %v", waited, err)
	}
	if _, err := c.get(t, "vm-1"); apiCode(err) != http.StatusNotFound {
		t.Fatalf("get after the delete finished = %v", err)
	}
	got, err := c.zoneOps.Get(t.Context(), &computepb.GetZoneOperationRequest{Project: project, Zone: zone, Operation: del.Name()})
	if err != nil || got.GetStatus() != computepb.Operation_DONE || got.GetOperationType() != "delete" || got.GetEndTime() == "" {
		t.Fatalf("zoneOperations.get = %v, %v", got, err)
	}
	if _, err := c.zoneOps.Get(t.Context(), &computepb.GetZoneOperationRequest{Project: project, Zone: zone, Operation: "operation-1"}); apiCode(err) != http.StatusNotFound {
		t.Errorf("a missing operation = %v", err)
	}
}

func TestSettleFinishesWhatOnlyItCan(t *testing.T) {
	c := setup(t, compute.WithSettleAfter(-1))
	op, err := c.instances.BulkInsert(t.Context(), bulkRequest("s", "node-1", "node-2"))
	if err != nil {
		t.Fatal(err)
	}
	if p := op.Proto().GetInstancesBulkInsertOperationMetadata().GetPerLocationStatus()["zones/"+zone]; p.GetStatus() != "CREATING" {
		t.Errorf("bulk status = %v", p)
	}
	waited, err := c.zoneOps.Wait(t.Context(), &computepb.WaitZoneOperationRequest{Project: project, Zone: zone, Operation: op.Name()})
	if err != nil || waited.GetStatus() != computepb.Operation_RUNNING {
		t.Fatalf("wait = %v, %v; want the operation still running", waited, err)
	}
	if insts, _ := c.list(t, "status = PROVISIONING", 0); len(insts) != 2 {
		t.Fatalf("provisioning instances = %s", names(insts))
	}
	c.server.Settle()
	if err := op.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if insts, _ := c.list(t, "status = RUNNING", 0); len(insts) != 2 {
		t.Fatalf("running instances = %s", names(insts))
	}
	if d, _ := c.server.LookupDisk(project, zone, "node-1"); d.Status != "READY" {
		t.Errorf("boot disk = %s", d.Status)
	}
}

func TestInstanceFaults(t *testing.T) {
	faults := fault.New().Add(
		fault.Rule{Method: "compute.instances.delete", Nth: 1, Fault: fault.FailOperation(http.StatusInternalServerError)},
		fault.Rule{Method: "compute.zoneOperations.get", Nth: 3, Fault: fault.Reject(http.StatusForbidden, "forbidden")},
	)
	c := setup(t, compute.WithFaults(faults))
	if err := c.insert(t, single("vm-1", "s")); err != nil {
		t.Fatal(err)
	}
	del := func() error {
		op, err := c.instances.Delete(t.Context(), &computepb.DeleteInstanceRequest{Project: project, Zone: zone, Instance: "vm-1"})
		if err != nil {
			return err
		}
		return op.Wait(t.Context())
	}
	if err := del(); apiCode(err) != http.StatusInternalServerError {
		t.Fatalf("delete with a failed operation = %v, want HTTP 500", err)
	}
	if _, ok := c.server.LookupInstance(project, zone, "vm-1"); !ok {
		t.Fatal("a failed delete took effect")
	}
	if err := del(); apiCode(err) != http.StatusForbidden {
		t.Fatalf("delete whose poll fails = %v, want HTTP 403", err)
	}
	if _, ok := c.server.LookupInstance(project, zone, "vm-1"); ok {
		t.Fatal("the delete whose poll failed did not take effect")
	}
}

// createNodes is the pattern the stand-in exists to test: a bulk insert of a
// session's nodes, adopting them when a name conflict says an earlier attempt
// made them.
func createNodes(t *testing.T, c *clients, session string, nodes ...string) error {
	t.Helper()
	var last error
	for range 30 {
		err := c.bulkInsert(t, bulkRequest(session, nodes...))
		if err == nil {
			return nil
		}
		last = err
		if apiCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "RESOURCE_ALREADY_EXISTS") {
			continue
		}
		filter := fmt.Sprintf("labels.session-id=%s AND (name=%s)", session, strings.Join(nodes, " OR name="))
		found, err := c.list(t, filter, 0)
		if err != nil {
			last = err
			continue
		}
		if len(found) == len(nodes) {
			return nil
		}
		return fmt.Errorf("nodes belong to someone else: found %s", names(found))
	}
	return last
}

func TestCreateNodesConvergesUnderRandomFaults(t *testing.T) {
	for seed := range uint64(30) {
		faults := fault.New(fault.Seed(seed)).Random(0.3,
			fault.Unavailable(), fault.LoseResponse(), fault.FailAfter(http.StatusInternalServerError), fault.FailOperation(http.StatusInternalServerError))
		c := setup(t, compute.WithFaults(faults))
		if err := createNodes(t, c, "s", "node-1", "node-2", "node-3"); err != nil {
			t.Fatalf("seed %d: %v\ninjected: %v", seed, err, faults.Injected())
		}
		if got := c.server.Instances(); len(got) != 3 {
			t.Fatalf("seed %d: %d instances\ninjected: %v", seed, len(got), faults.Injected())
		}
		if got := c.server.Disks(); len(got) != 3 {
			t.Fatalf("seed %d: %d disks, want one boot disk per node", seed, len(got))
		}
	}
}
