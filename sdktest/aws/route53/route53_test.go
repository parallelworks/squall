package route53_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awsroute53 "github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"

	"github.com/parallelworks/squall/aws/route53"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
	"github.com/parallelworks/squall/sdktest/internal/sdkhttp"
)

const (
	region = "us-east-1"
	vpcA   = "vpc-0aaaaaaaaaaaaaaaa"
	vpcB   = "vpc-0bbbbbbbbbbbbbbbb"
)

// newClient returns a client for s that does not retry, so each call is one request.
func newClient(t *testing.T, s *route53.Server) *awsroute53.Client {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return awsroute53.New(awsroute53.Options{
		Region:           region,
		BaseEndpoint:     aws.String(srv.URL),
		Credentials:      credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:       sdkhttp.CopyBodies(awshttp.NewBuildableClient()),
		RetryMaxAttempts: 1,
	})
}

func createInput(name, ref, vpc string) *awsroute53.CreateHostedZoneInput {
	return &awsroute53.CreateHostedZoneInput{
		Name:             aws.String(name),
		CallerReference:  aws.String(ref),
		VPC:              &types.VPC{VPCId: aws.String(vpc), VPCRegion: types.VPCRegion(region)},
		HostedZoneConfig: &types.HostedZoneConfig{PrivateZone: true, Comment: aws.String("test zone")},
	}
}

func createZone(t *testing.T, c *awsroute53.Client, name, ref, vpc string) string {
	t.Helper()
	out, err := c.CreateHostedZone(t.Context(), createInput(name, ref, vpc))
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return aws.ToString(out.HostedZone.Id)
}

func change(action types.ChangeAction, name string, rrType types.RRType, ttl int64, values ...string) types.Change {
	rs := &types.ResourceRecordSet{Name: aws.String(name), Type: rrType, TTL: aws.Int64(ttl)}
	for _, v := range values {
		rs.ResourceRecords = append(rs.ResourceRecords, types.ResourceRecord{Value: aws.String(v)})
	}
	return types.Change{Action: action, ResourceRecordSet: rs}
}

func changeRecords(t *testing.T, c *awsroute53.Client, zoneID string, changes ...types.Change) (*awsroute53.ChangeResourceRecordSetsOutput, error) {
	t.Helper()
	return c.ChangeResourceRecordSets(t.Context(), &awsroute53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID),
		ChangeBatch:  &types.ChangeBatch{Changes: changes},
	})
}

// listRecords lists a zone's record sets as "name type" through the SDK's paginator, pageSize at a time.
func listRecords(t *testing.T, c *awsroute53.Client, zoneID string, pageSize int32) []string {
	t.Helper()
	var got []string
	p := awsroute53.NewListResourceRecordSetsPaginator(c, &awsroute53.ListResourceRecordSetsInput{HostedZoneId: aws.String(zoneID)},
		func(o *awsroute53.ListResourceRecordSetsPaginatorOptions) { o.Limit = pageSize })
	for p.HasMorePages() {
		page, err := p.NextPage(t.Context())
		if err != nil {
			t.Fatalf("list record sets: %v", err)
		}
		if len(page.ResourceRecordSets) > int(pageSize) {
			t.Fatalf("page of %d, want at most %d", len(page.ResourceRecordSets), pageSize)
		}
		for _, rs := range page.ResourceRecordSets {
			got = append(got, aws.ToString(rs.Name)+" "+string(rs.Type))
		}
	}
	return got
}

func TestCreateGetAndListHostedZones(t *testing.T) {
	s := route53.New()
	c := newClient(t, s)
	out, err := c.CreateHostedZone(t.Context(), createInput("Cluster.Example.Internal", "ref-1", vpcA))
	if err != nil {
		t.Fatal(err)
	}
	id := aws.ToString(out.HostedZone.Id)
	if !strings.HasPrefix(id, "/hostedzone/Z") || aws.ToString(out.HostedZone.Name) != "cluster.example.internal." ||
		!out.HostedZone.Config.PrivateZone || aws.ToString(out.HostedZone.Config.Comment) != "test zone" ||
		aws.ToInt64(out.HostedZone.ResourceRecordSetCount) != 2 || aws.ToString(out.VPC.VPCId) != vpcA ||
		out.ChangeInfo.Status != types.ChangeStatusInsync || !strings.HasSuffix(aws.ToString(out.Location), id) {
		t.Fatalf("create = %+v %+v %+v %v", out.HostedZone, out.VPC, out.ChangeInfo, aws.ToString(out.Location))
	}
	ch, err := c.GetChange(t.Context(), &awsroute53.GetChangeInput{Id: out.ChangeInfo.Id})
	if err != nil || ch.ChangeInfo.Status != types.ChangeStatusInsync {
		t.Fatalf("get change = %+v, %v", ch, err)
	}
	if _, err := c.GetChange(t.Context(), &awsroute53.GetChangeInput{Id: aws.String("CMISSING")}); sdkerr.AWSCode(err) != "NoSuchChange" {
		t.Fatalf("get missing change = %v", err)
	}

	got, err := c.GetHostedZone(t.Context(), &awsroute53.GetHostedZoneInput{Id: aws.String(id)})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(got.HostedZone.Id) != id || len(got.VPCs) != 1 || aws.ToString(got.VPCs[0].VPCId) != vpcA || got.VPCs[0].VPCRegion != region {
		t.Fatalf("get = %+v %+v", got.HostedZone, got.VPCs)
	}
	_, err = c.GetHostedZone(t.Context(), &awsroute53.GetHostedZoneInput{Id: aws.String("ZMISSING")})
	if _, ok := errors.AsType[*types.NoSuchHostedZone](err); !ok || sdkerr.AWSStatus(err) != http.StatusNotFound {
		t.Fatalf("get missing = %v", err)
	}

	createZone(t, c, "b.example.internal", "ref-2", vpcA)
	createZone(t, c, "example.com", "ref-3", vpcA)
	createZone(t, c, "a.example.internal", "ref-4", vpcA)
	var names []string
	in := &awsroute53.ListHostedZonesByNameInput{MaxItems: aws.Int32(1)}
	for {
		page, err := c.ListHostedZonesByName(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		for _, z := range page.HostedZones {
			names = append(names, aws.ToString(z.Name))
		}
		if !page.IsTruncated {
			break
		}
		in.DNSName, in.HostedZoneId = page.NextDNSName, page.NextHostedZoneId
	}
	want := []string{"example.com.", "a.example.internal.", "b.example.internal.", "cluster.example.internal."}
	if !slices.Equal(names, want) {
		t.Fatalf("listed %v, want %v", names, want)
	}
	page, err := c.ListHostedZonesByName(t.Context(), &awsroute53.ListHostedZonesByNameInput{DNSName: aws.String("b.example.internal")})
	if err != nil || len(page.HostedZones) != 2 || aws.ToString(page.HostedZones[0].Name) != "b.example.internal." || page.IsTruncated {
		t.Fatalf("list from b = %+v, %v", page, err)
	}

	zones := s.HostedZones()
	if len(zones) != 4 || zones[0].Name != "example.com." {
		t.Fatalf("HostedZones() = %+v", zones)
	}
	z, ok := s.LookupHostedZone(id)
	if !ok || z.CallerReference != "ref-1" || !slices.Equal(z.VPCs, []route53.VPC{{Region: region, ID: vpcA}}) {
		t.Fatalf("LookupHostedZone = %+v, %v", z, ok)
	}
}

func TestCreateHostedZoneErrors(t *testing.T) {
	s := route53.New(route53.WithVPCLookup(func(r, id string) bool { return r == region && id != "vpc-0missing" }))
	c := newClient(t, s)
	createZone(t, c, "example.internal", "taken", vpcA)
	for _, tc := range []struct {
		name   string
		in     *awsroute53.CreateHostedZoneInput
		status int
		code   string
	}{
		{"a reused caller reference", createInput("other.internal", "taken", vpcB), http.StatusConflict, "HostedZoneAlreadyExists"},
		{"the same name in the same VPC", createInput("Example.Internal.", "new", vpcA), http.StatusBadRequest, "ConflictingDomainExists"},
		{"an unknown VPC", createInput("other.internal", "new", "vpc-0missing"), http.StatusBadRequest, "InvalidVPCId"},
		{"an invalid name", createInput("bad..name", "new", vpcA), http.StatusBadRequest, "InvalidDomainName"},
		{"no VPC", &awsroute53.CreateHostedZoneInput{Name: aws.String("public.example"), CallerReference: aws.String("new")}, http.StatusBadRequest, "InvalidInput"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.CreateHostedZone(t.Context(), tc.in)
			if sdkerr.AWSStatus(err) != tc.status || sdkerr.AWSCode(err) != tc.code {
				t.Fatalf("create = %v, want %d %s", err, tc.status, tc.code)
			}
		})
	}
	createZone(t, c, "example.internal", "another-vpc", vpcB)
	if n := len(s.HostedZones()); n != 2 {
		t.Fatalf("%d zones, want 2", n)
	}
}

func TestVPCAssociations(t *testing.T) {
	s := route53.New()
	c := newClient(t, s)
	id := createZone(t, c, "example.internal", "ref", vpcA)
	other := createZone(t, c, "example.internal", "ref-other", vpcB)
	vpc := func(id string) *types.VPC { return &types.VPC{VPCId: aws.String(id), VPCRegion: region} }

	if _, err := c.AssociateVPCWithHostedZone(t.Context(), &awsroute53.AssociateVPCWithHostedZoneInput{HostedZoneId: aws.String(id), VPC: vpc(vpcB)}); sdkerr.AWSCode(err) != "ConflictingDomainExists" {
		t.Fatalf("associate a VPC another zone of the name has = %v", err)
	}
	if _, err := c.AssociateVPCWithHostedZone(t.Context(), &awsroute53.AssociateVPCWithHostedZoneInput{HostedZoneId: aws.String(id), VPC: vpc(vpcA)}); sdkerr.AWSCode(err) != "ConflictingDomainExists" {
		t.Fatalf("associate an associated VPC = %v", err)
	}
	if _, err := c.AssociateVPCWithHostedZone(t.Context(), &awsroute53.AssociateVPCWithHostedZoneInput{HostedZoneId: aws.String(id), VPC: vpc("vpc-0ccccccccccccccc")}); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetHostedZone(t.Context(), &awsroute53.GetHostedZoneInput{Id: aws.String(id)})
	if err != nil || len(got.VPCs) != 2 {
		t.Fatalf("get = %+v, %v", got, err)
	}

	_, err = c.DisassociateVPCFromHostedZone(t.Context(), &awsroute53.DisassociateVPCFromHostedZoneInput{HostedZoneId: aws.String(id), VPC: vpc(vpcB)})
	if sdkerr.AWSCode(err) != "VPCAssociationNotFound" || sdkerr.AWSStatus(err) != http.StatusNotFound {
		t.Fatalf("disassociate an unassociated VPC = %v", err)
	}
	if _, err := c.DisassociateVPCFromHostedZone(t.Context(), &awsroute53.DisassociateVPCFromHostedZoneInput{HostedZoneId: aws.String(id), VPC: vpc(vpcA)}); err != nil {
		t.Fatal(err)
	}
	_, err = c.DisassociateVPCFromHostedZone(t.Context(), &awsroute53.DisassociateVPCFromHostedZoneInput{HostedZoneId: aws.String(id), VPC: vpc("vpc-0ccccccccccccccc")})
	if _, ok := errors.AsType[*types.LastVPCAssociation](err); !ok || sdkerr.AWSStatus(err) != http.StatusBadRequest {
		t.Fatalf("disassociate the last VPC = %v", err)
	}
	if _, err := c.AssociateVPCWithHostedZone(t.Context(), &awsroute53.AssociateVPCWithHostedZoneInput{HostedZoneId: aws.String("ZMISSING"), VPC: vpc(vpcA)}); sdkerr.AWSCode(err) != "NoSuchHostedZone" {
		t.Fatalf("associate with a missing zone = %v", err)
	}
	if z, _ := s.LookupHostedZone(other); !slices.Equal(z.VPCs, []route53.VPC{{Region: region, ID: vpcB}}) {
		t.Fatalf("other zone's VPCs = %+v", z.VPCs)
	}
}

func TestRecordSets(t *testing.T) {
	s := route53.New()
	c := newClient(t, s)
	id := createZone(t, c, "example.internal", "ref", vpcA)
	out, err := changeRecords(t, c, id,
		change(types.ChangeActionCreate, "head.example.internal", types.RRTypeA, 60, "10.0.0.1"),
		change(types.ChangeActionCreate, "*.example.internal", types.RRTypeA, 60, "10.0.0.9"),
		change(types.ChangeActionCreate, "node-1.example.internal", types.RRTypeA, 60, "10.0.0.2"),
		change(types.ChangeActionCreate, "node-1.example.internal", types.RRTypeTxt, 60, `"hello"`),
		change(types.ChangeActionUpsert, "login.example.internal", types.RRTypeCname, 300, "head.example.internal"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.ChangeInfo.Status != types.ChangeStatusInsync {
		t.Fatalf("change info = %+v", out.ChangeInfo)
	}
	want := []string{
		"example.internal. NS", "example.internal. SOA", `\052.example.internal. A`, "head.example.internal. A",
		"login.example.internal. CNAME", "node-1.example.internal. A", "node-1.example.internal. TXT",
	}
	if got := listRecords(t, c, id, 2); !slices.Equal(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	from, err := c.ListResourceRecordSets(t.Context(), &awsroute53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String(id), StartRecordName: aws.String("node-1.example.internal"), StartRecordType: types.RRTypeTxt,
	})
	if err != nil || len(from.ResourceRecordSets) != 1 || from.ResourceRecordSets[0].Type != types.RRTypeTxt {
		t.Fatalf("list from node-1 TXT = %+v, %v", from, err)
	}

	if _, err := changeRecords(t, c, id, change(types.ChangeActionUpsert, "head.example.internal", types.RRTypeA, 30, "10.0.0.5")); err != nil {
		t.Fatal(err)
	}
	if _, err := changeRecords(t, c, id,
		change(types.ChangeActionDelete, "node-1.example.internal", types.RRTypeA, 60, "10.0.0.2"),
		change(types.ChangeActionCreate, "node-1.example.internal", types.RRTypeA, 60, "10.0.0.3"),
	); err != nil {
		t.Fatalf("delete and create in one batch: %v", err)
	}
	head := s.RecordSets(id)[3]
	if head.Name != "head.example.internal." || head.TTL != 30 || !slices.Equal(head.Values, []string{"10.0.0.5"}) {
		t.Fatalf("head after upsert = %+v", head)
	}

	_, err = changeRecords(t, c, id,
		change(types.ChangeActionCreate, "new.example.internal", types.RRTypeA, 60, "10.0.0.7"),
		change(types.ChangeActionCreate, "head.example.internal", types.RRTypeA, 60, "10.0.0.1"),
		change(types.ChangeActionDelete, "gone.example.internal", types.RRTypeA, 60, "10.0.0.1"),
		change(types.ChangeActionDelete, "node-1.example.internal", types.RRTypeA, 60, "10.0.0.2"),
		change(types.ChangeActionCreate, "outside.example.com", types.RRTypeA, 60, "10.0.0.1"),
		change(types.ChangeActionCreate, "bad.example.internal", types.RRTypeA, 60, "not-an-address"),
		change(types.ChangeActionCreate, "login.example.internal", types.RRTypeTxt, 60, `"x"`),
	)
	batch, ok := errors.AsType[*types.InvalidChangeBatch](err)
	if !ok || sdkerr.AWSStatus(err) != http.StatusBadRequest || len(batch.Messages) != 6 {
		t.Fatalf("bad batch = %v (%+v)", err, batch)
	}
	for i, want := range []string{
		"Tried to create resource record set [name='head.example.internal.', type='A'] but it already exists",
		"Tried to delete resource record set [name='gone.example.internal.', type='A'] but it was not found",
		"Tried to delete resource record set [name='node-1.example.internal.', type='A'] but the values provided do not match the current values",
		"RRSet with DNS name outside.example.com. is not permitted in zone example.internal.",
		"ARRDATAIllegalIPv4Address",
		"conflicts with other records",
	} {
		if !strings.Contains(batch.Messages[i], want) {
			t.Errorf("message %d = %q, want %q", i, batch.Messages[i], want)
		}
	}
	if n := len(s.RecordSets(id)); n != 7 {
		t.Fatalf("%d record sets after a failed batch, want 7: it must change nothing", n)
	}

	_, err = changeRecords(t, c, id, types.Change{Action: types.ChangeActionCreate, ResourceRecordSet: &types.ResourceRecordSet{Name: aws.String("x.example.internal"), Type: types.RRTypeA}})
	if sdkerr.AWSCode(err) != "InvalidInput" {
		t.Fatalf("a record set with no records = %v", err)
	}
	ns := s.RecordSets(id)[0]
	_, err = changeRecords(t, c, id, change(types.ChangeActionDelete, ns.Name, types.RRTypeNs, ns.TTL, ns.Values...))
	if _, ok := errors.AsType[*types.InvalidChangeBatch](err); !ok {
		t.Fatalf("delete the zone's NS records = %v", err)
	}
}

func TestDeleteHostedZoneWithRecords(t *testing.T) {
	s := route53.New()
	c := newClient(t, s)
	id := createZone(t, c, "example.internal", "ref", vpcA)
	if _, err := changeRecords(t, c, id, change(types.ChangeActionCreate, "head.example.internal", types.RRTypeA, 60, "10.0.0.1")); err != nil {
		t.Fatal(err)
	}
	_, err := c.DeleteHostedZone(t.Context(), &awsroute53.DeleteHostedZoneInput{Id: aws.String(id)})
	if _, ok := errors.AsType[*types.HostedZoneNotEmpty](err); !ok || sdkerr.AWSStatus(err) != http.StatusBadRequest {
		t.Fatalf("delete a zone with records = %v", err)
	}
	if _, err := changeRecords(t, c, id, change(types.ChangeActionDelete, "head.example.internal", types.RRTypeA, 60, "10.0.0.1")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteHostedZone(t.Context(), &awsroute53.DeleteHostedZoneInput{Id: aws.String(id)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteHostedZone(t.Context(), &awsroute53.DeleteHostedZoneInput{Id: aws.String(id)}); sdkerr.AWSCode(err) != "NoSuchHostedZone" {
		t.Fatalf("delete again = %v", err)
	}
	if _, err := c.CreateHostedZone(t.Context(), createInput("example.internal", "ref", vpcA)); sdkerr.AWSCode(err) != "HostedZoneAlreadyExists" {
		t.Fatalf("create with a deleted zone's caller reference = %v", err)
	}
	if zones := s.HostedZones(); len(zones) != 0 || s.RecordSets(id) != nil {
		t.Fatalf("HostedZones() = %+v after delete", zones)
	}
}

func TestTags(t *testing.T) {
	s := route53.New()
	c := newClient(t, s)
	id := strings.TrimPrefix(createZone(t, c, "example.internal", "ref", vpcA), "/hostedzone/")
	if _, err := c.ChangeTagsForResource(t.Context(), &awsroute53.ChangeTagsForResourceInput{
		ResourceType: types.TagResourceTypeHostedzone, ResourceId: aws.String(id),
		AddTags: []types.Tag{{Key: aws.String("owner"), Value: aws.String("abc")}, {Key: aws.String("cluster"), Value: aws.String("c1")}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ChangeTagsForResource(t.Context(), &awsroute53.ChangeTagsForResourceInput{
		ResourceType: types.TagResourceTypeHostedzone, ResourceId: aws.String(id), RemoveTagKeys: []string{"cluster"},
	}); err != nil {
		t.Fatal(err)
	}
	out, err := c.ListTagsForResource(t.Context(), &awsroute53.ListTagsForResourceInput{ResourceType: types.TagResourceTypeHostedzone, ResourceId: aws.String(id)})
	if err != nil {
		t.Fatal(err)
	}
	set := out.ResourceTagSet
	if aws.ToString(set.ResourceId) != id || set.ResourceType != types.TagResourceTypeHostedzone || len(set.Tags) != 1 ||
		aws.ToString(set.Tags[0].Key) != "owner" || aws.ToString(set.Tags[0].Value) != "abc" {
		t.Fatalf("tags = %+v", set)
	}
	if z, _ := s.LookupHostedZone(id); len(z.Tags) != 1 || z.Tags["owner"] != "abc" {
		t.Fatalf("zone tags = %v", z.Tags)
	}
	_, err = c.ListTagsForResource(t.Context(), &awsroute53.ListTagsForResourceInput{ResourceType: types.TagResourceTypeHostedzone, ResourceId: aws.String("ZMISSING")})
	if sdkerr.AWSCode(err) != "NoSuchHostedZone" {
		t.Fatalf("list tags of a missing zone = %v", err)
	}
}

func TestFaults(t *testing.T) {
	faults := fault.New().Add(
		fault.Rule{Method: "route53.CreateHostedZone", Nth: 1, Fault: fault.LoseResponse()},
		fault.Rule{Method: "route53.ChangeResourceRecordSets", Nth: 1, Fault: fault.Reject(http.StatusBadRequest, "PriorRequestNotComplete")},
		fault.Rule{Method: "route53.ListHostedZonesByName", Nth: 1, Fault: fault.RateLimited()},
	)
	s := route53.New(route53.WithFaults(faults))
	c := newClient(t, s)

	if _, err := c.CreateHostedZone(t.Context(), createInput("example.internal", "ref", vpcA)); err == nil {
		t.Fatal("create with a lost response succeeded")
	}
	if _, err := c.CreateHostedZone(t.Context(), createInput("example.internal", "ref", vpcA)); sdkerr.AWSCode(err) != "HostedZoneAlreadyExists" {
		t.Fatalf("retried create = %v", err)
	}
	zones := s.HostedZones()
	if len(zones) != 1 || zones[0].Name != "example.internal." {
		t.Fatalf("HostedZones() = %+v", zones)
	}

	_, err := changeRecords(t, c, zones[0].ID, change(types.ChangeActionCreate, "a.example.internal", types.RRTypeA, 60, "10.0.0.1"))
	if sdkerr.AWSCode(err) != "PriorRequestNotComplete" || sdkerr.AWSStatus(err) != http.StatusBadRequest {
		t.Fatalf("rejected change = %v", err)
	}
	if n := len(s.RecordSets(zones[0].ID)); n != 2 {
		t.Fatalf("%d record sets after a rejected change, want 2", n)
	}
	if _, err := c.ListHostedZonesByName(t.Context(), &awsroute53.ListHostedZonesByNameInput{}); sdkerr.AWSCode(err) != "Throttling" {
		t.Fatalf("throttled list = %v", err)
	}
	injected := faults.Injected()
	if len(injected) != 3 || injected[0].Call.Resource != "example.internal." || injected[1].Call.Resource != zones[0].ID {
		t.Fatalf("Injected() = %v", injected)
	}
}
