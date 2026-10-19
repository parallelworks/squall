package arm_test

import (
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns"

	"github.com/parallelworks/squall/azure/arm"
)

func (c *clients) createZone(t *testing.T, name string) armprivatedns.PrivateZone {
	t.Helper()
	p, err := c.zones.BeginCreateOrUpdate(t.Context(), group, name, armprivatedns.PrivateZone{Location: new("global")}, nil)
	res, err := wait(t, p, err)
	if err != nil {
		t.Fatal(err)
	}
	return res.PrivateZone
}

func (c *clients) putRecord(t *testing.T, zone, name string, addresses ...string) armprivatedns.RecordSet {
	t.Helper()
	res, err := c.records.CreateOrUpdate(t.Context(), group, zone, armprivatedns.RecordTypeA, name, aRecords(addresses...), nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.RecordSet
}

func aRecords(addresses ...string) armprivatedns.RecordSet {
	var records []*armprivatedns.ARecord
	for _, a := range addresses {
		records = append(records, &armprivatedns.ARecord{IPv4Address: new(a)})
	}
	return armprivatedns.RecordSet{Properties: &armprivatedns.RecordSetProperties{TTL: new(int64(300)), ARecords: records}}
}

func TestPrivateDNS(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	if _, err := c.zones.BeginCreateOrUpdate(t.Context(), group, "example.internal", armprivatedns.PrivateZone{Location: new(location)}, nil); status(err) != http.StatusBadRequest {
		t.Fatalf("a zone outside global = %v", err)
	}
	zone := c.createZone(t, "example.internal")
	if *zone.Properties.ProvisioningState != armprivatedns.ProvisioningStateSucceeded || *zone.Location != "global" || zone.Etag == nil {
		t.Fatalf("zone = %+v", zone)
	}
	link := armprivatedns.VirtualNetworkLink{
		Location:   new("global"),
		Properties: &armprivatedns.VirtualNetworkLinkProperties{VirtualNetwork: &armprivatedns.SubResource{ID: new(vnet)}, RegistrationEnabled: new(false)},
	}
	if _, err := c.links.BeginCreateOrUpdate(t.Context(), group, "missing.internal", "link", link, nil); code(err) != "ParentResourceNotFound" {
		t.Fatalf("a link in a missing zone = %v", err)
	}
	lp, err := c.links.BeginCreateOrUpdate(t.Context(), group, "example.internal", "link", link, nil)
	created, err := wait(t, lp, err)
	if err != nil || *created.Properties.VirtualNetworkLinkState != armprivatedns.VirtualNetworkLinkStateCompleted ||
		*created.ID != *zone.ID+"/virtualNetworkLinks/link" {
		t.Fatalf("link = %+v, %v", created, err)
	}
	if _, err := c.links.BeginCreateOrUpdate(t.Context(), group, "example.internal", "link-2", link, nil); code(err) != "Conflict" {
		t.Fatalf("linking the network twice = %v", err)
	}
	if got, err := c.links.Get(t.Context(), group, "example.internal", "LINK", nil); err != nil || *got.Name != "link" {
		t.Fatalf("get link = %+v, %v", got, err)
	}

	rec := c.putRecord(t, "example.internal", "www", "10.1.0.4", "10.1.0.5")
	if *rec.Properties.Fqdn != "www.example.internal." || len(rec.Properties.ARecords) != 2 || *rec.Type != "Microsoft.Network/privateDnsZones/A" {
		t.Fatalf("record = %+v", rec.Properties)
	}
	if _, err := c.records.CreateOrUpdate(t.Context(), group, "example.internal", armprivatedns.RecordTypeA, "www", aRecords("10.1.0.6"),
		&armprivatedns.RecordSetsClientCreateOrUpdateOptions{IfNoneMatch: new("*")}); status(err) != http.StatusPreconditionFailed {
		t.Fatalf("If-None-Match on an existing record = %v", err)
	}
	if _, err := c.records.CreateOrUpdate(t.Context(), group, "example.internal", armprivatedns.RecordTypeA, "www", aRecords("10.1.0.6"),
		&armprivatedns.RecordSetsClientCreateOrUpdateOptions{IfMatch: rec.Etag}); err != nil {
		t.Fatalf("If-Match with the current ETag = %v", err)
	}
	got, err := c.records.Get(t.Context(), group, "example.internal", armprivatedns.RecordTypeA, "www", nil)
	if err != nil || *got.Properties.ARecords[0].IPv4Address != "10.1.0.6" || *got.Properties.TTL != 300 {
		t.Fatalf("get record = %+v, %v", got, err)
	}
	if _, err := c.records.CreateOrUpdate(t.Context(), group, "example.internal", armprivatedns.RecordTypeA, "bad", aRecords("not-an-ip"), nil); status(err) != http.StatusBadRequest {
		t.Fatalf("a bad address = %v", err)
	}
	if got := c.server.RecordSets(); len(got) != 1 || got[0].FQDN != "www.example.internal." || got[0].TTL != 300 {
		t.Fatalf("RecordSets = %+v", got)
	}
	zg, err := c.zones.Get(t.Context(), group, "example.internal", nil)
	if err != nil || *zg.Properties.NumberOfRecordSets != 2 || *zg.Properties.NumberOfVirtualNetworkLinks != 1 {
		t.Fatalf("zone counts = %+v, %v", zg.Properties, err)
	}
	if _, err := c.records.Delete(t.Context(), group, "example.internal", armprivatedns.RecordTypeA, "www", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.records.Get(t.Context(), group, "example.internal", armprivatedns.RecordTypeA, "www", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("get after delete = %v", err)
	}
	if _, err := c.records.Delete(t.Context(), group, "example.internal", armprivatedns.RecordTypeA, "www", nil); err != nil {
		t.Fatalf("deleting it again = %v", err)
	}
}

func TestPrivateDNSPollersWithSettle(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(1))
	c.createGroup(t, group, nil)
	p, err := c.zones.BeginCreateOrUpdate(t.Context(), group, "example.internal", armprivatedns.PrivateZone{Location: new("global")}, nil)
	if err != nil || p.Done() {
		t.Fatalf("begin = done %v, %v", p.Done(), err)
	}
	zone, err := p.PollUntilDone(t.Context(), nil)
	if err != nil || *zone.Properties.ProvisioningState != armprivatedns.ProvisioningStateSucceeded {
		t.Fatalf("zone = %+v, %v", zone, err)
	}
	if c.polls() != 2 {
		t.Errorf("polled %d times, want 2", c.polls())
	}
}
