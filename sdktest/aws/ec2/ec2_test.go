package ec2_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/parallelworks/squall/aws/ec2"
	"github.com/parallelworks/squall/fault"
)

const (
	region = "us-east-1"
	subnet = "subnet-0123456789abcdef0"
)

func serve(t *testing.T, s *ec2.Server) string {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv.URL
}

// newClient returns a client for s in region that does not retry, so each
// call is one request.
func newClient(t *testing.T, s *ec2.Server, opts ...func(*awsec2.Options)) *awsec2.Client {
	t.Helper()
	return clientAt(serve(t, s), region, append([]func(*awsec2.Options){withRetries(1)}, opts...)...)
}

func clientAt(url, region string, opts ...func(*awsec2.Options)) *awsec2.Client {
	return awsec2.New(awsec2.Options{
		Region:       region,
		BaseEndpoint: aws.String(url),
		Credentials:  credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:   readBodies{awshttp.NewBuildableClient()},
	}, opts...)
}

// readBodies hides the request body's WriteTo: once the SDK closes the body after a fast answer, WriteTo returns
// io.EOF, which net/http reports as a failed write and closes the connection mid-response.
type readBodies struct{ c awsec2.HTTPClient }

func (r readBodies) Do(req *http.Request) (*http.Response, error) {
	if req.Body != nil && req.Body != http.NoBody {
		req.Body = struct{ io.ReadCloser }{req.Body}
	}
	return r.c.Do(req)
}

func withRetries(attempts int) func(*awsec2.Options) {
	return func(o *awsec2.Options) { o.RetryMaxAttempts = attempts }
}

func code(err error) string {
	if e, ok := errors.AsType[smithy.APIError](err); ok {
		return e.ErrorCode()
	}
	return ""
}

func status(err error) int {
	if e, ok := errors.AsType[*awshttp.ResponseError](err); ok {
		return e.HTTPStatusCode()
	}
	return 0
}

func tagSpec(resource types.ResourceType, kv ...string) []types.TagSpecification {
	spec := types.TagSpecification{ResourceType: resource}
	for i := 0; i < len(kv); i += 2 {
		spec.Tags = append(spec.Tags, types.Tag{Key: aws.String(kv[i]), Value: aws.String(kv[i+1])})
	}
	return []types.TagSpecification{spec}
}

func tags(ts []types.Tag) map[string]string {
	out := map[string]string{}
	for _, tag := range ts {
		out[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	return out
}

func allocate(t *testing.T, c *awsec2.Client, kv ...string) *awsec2.AllocateAddressOutput {
	t.Helper()
	out, err := c.AllocateAddress(t.Context(), &awsec2.AllocateAddressInput{
		Domain: types.DomainTypeVpc, TagSpecifications: tagSpec(types.ResourceTypeElasticIp, kv...),
	})
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	return out
}

func createInterface(t *testing.T, c *awsec2.Client, kv ...string) *types.NetworkInterface {
	t.Helper()
	out, err := c.CreateNetworkInterface(t.Context(), &awsec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnet), Groups: []string{"sg-1"},
		TagSpecifications: tagSpec(types.ResourceTypeNetworkInterface, kv...),
	})
	if err != nil {
		t.Fatalf("create interface: %v", err)
	}
	return out.NetworkInterface
}

func describeAddresses(t *testing.T, c *awsec2.Client, in *awsec2.DescribeAddressesInput) ([]types.Address, error) {
	t.Helper()
	out, err := c.DescribeAddresses(t.Context(), in)
	if err != nil {
		return nil, err
	}
	return out.Addresses, nil
}

func describeInterfaces(t *testing.T, c *awsec2.Client, in *awsec2.DescribeNetworkInterfacesInput) ([]types.NetworkInterface, error) {
	t.Helper()
	out, err := c.DescribeNetworkInterfaces(t.Context(), in)
	if err != nil {
		return nil, err
	}
	return out.NetworkInterfaces, nil
}

func associate(t *testing.T, c *awsec2.Client, allocationID, interfaceID string, reassociate bool) (string, error) {
	t.Helper()
	out, err := c.AssociateAddress(t.Context(), &awsec2.AssociateAddressInput{
		AllocationId: aws.String(allocationID), NetworkInterfaceId: aws.String(interfaceID), AllowReassociation: aws.Bool(reassociate),
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.AssociationId), nil
}

func filter(name string, values ...string) types.Filter {
	return types.Filter{Name: aws.String(name), Values: values}
}

func TestAllocateDescribeRelease(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	out := allocate(t, c, "session", "s1", "Name", "web")
	id := aws.ToString(out.AllocationId)
	if aws.ToString(out.PublicIp) != "100.64.0.1" || out.Domain != types.DomainTypeVpc || id == "" {
		t.Fatalf("allocate = %+v", out)
	}
	for _, in := range []*awsec2.DescribeAddressesInput{
		{AllocationIds: []string{id}},
		{PublicIps: []string{"100.64.0.1"}},
		{},
	} {
		got, err := describeAddresses(t, c, in)
		if err != nil || len(got) != 1 {
			t.Fatalf("describe %+v = %v, %v", in, got, err)
		}
		if a := got[0]; aws.ToString(a.AllocationId) != id || a.Domain != types.DomainTypeVpc ||
			tags(a.Tags)["session"] != "s1" || a.AssociationId != nil || aws.ToString(a.NetworkBorderGroup) != region {
			t.Fatalf("address = %+v", a)
		}
	}
	if a, ok := s.LookupAddress(region, id); !ok || a.Tags["Name"] != "web" {
		t.Fatalf("LookupAddress = %+v, %v", a, ok)
	}
	if _, err := c.ReleaseAddress(t.Context(), &awsec2.ReleaseAddressInput{AllocationId: aws.String(id)}); err != nil {
		t.Fatal(err)
	}
	if got := s.Addresses(); len(got) != 0 {
		t.Fatalf("addresses after release = %v", got)
	}
	if _, err := describeAddresses(t, c, &awsec2.DescribeAddressesInput{AllocationIds: []string{id}}); code(err) != "InvalidAllocationID.NotFound" {
		t.Fatalf("describe a released address = %v", err)
	}
}

func TestAddressErrors(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	held := aws.ToString(allocate(t, c).AllocationId)
	free := aws.ToString(allocate(t, c).AllocationId)
	ni := aws.ToString(createInterface(t, c).NetworkInterfaceId)
	other := aws.ToString(createInterface(t, c).NetworkInterfaceId)
	assoc, err := associate(t, c, held, ni, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		call func() error
		code string
	}{
		{"describe an unknown allocation ID", func() error {
			_, err := describeAddresses(t, c, &awsec2.DescribeAddressesInput{AllocationIds: []string{held, "eipalloc-missing"}})
			return err
		}, "InvalidAllocationID.NotFound"},
		{"describe an unknown public IP", func() error {
			_, err := describeAddresses(t, c, &awsec2.DescribeAddressesInput{PublicIps: []string{"100.127.0.1"}})
			return err
		}, "InvalidAddress.NotFound"},
		{"describe a public IP that is not IPv4", func() error {
			_, err := describeAddresses(t, c, &awsec2.DescribeAddressesInput{PublicIps: []string{"2001:db8::1"}})
			return err
		}, "InvalidAddress.Malformed"},
		{"describe with an unknown filter", func() error {
			_, err := describeAddresses(t, c, &awsec2.DescribeAddressesInput{Filters: []types.Filter{filter("creation-time", "x")}})
			return err
		}, "InvalidParameterValue"},
		{"release an unknown address", func() error {
			_, err := c.ReleaseAddress(t.Context(), &awsec2.ReleaseAddressInput{AllocationId: aws.String("eipalloc-missing")})
			return err
		}, "InvalidAllocationID.NotFound"},
		{"release an associated address", func() error {
			_, err := c.ReleaseAddress(t.Context(), &awsec2.ReleaseAddressInput{AllocationId: aws.String(held)})
			return err
		}, "InvalidIPAddress.InUse"},
		{"associate an unknown address", func() error {
			_, err := associate(t, c, "eipalloc-missing", ni, false)
			return err
		}, "InvalidAllocationID.NotFound"},
		{"associate with an unknown interface", func() error {
			_, err := associate(t, c, free, "eni-missing", false)
			return err
		}, "InvalidNetworkInterfaceID.NotFound"},
		{"associate an address in use elsewhere", func() error {
			_, err := associate(t, c, held, other, false)
			return err
		}, "Resource.AlreadyAssociated"},
		{"disassociate an unknown association", func() error {
			_, err := c.DisassociateAddress(t.Context(), &awsec2.DisassociateAddressInput{AssociationId: aws.String("eipassoc-missing")})
			return err
		}, "InvalidAssociationID.NotFound"},
		{"tag an address as an interface", func() error {
			_, err := c.AllocateAddress(t.Context(), &awsec2.AllocateAddressInput{TagSpecifications: tagSpec(types.ResourceTypeNetworkInterface, "k", "v")})
			return err
		}, "InvalidParameterValue"},
		{"a reserved tag key", func() error {
			_, err := c.AllocateAddress(t.Context(), &awsec2.AllocateAddressInput{TagSpecifications: tagSpec(types.ResourceTypeElasticIp, "aws:k", "v")})
			return err
		}, "InvalidParameterValue"},
		{"an unsupported parameter", func() error {
			_, err := c.AllocateAddress(t.Context(), &awsec2.AllocateAddressInput{PublicIpv4Pool: aws.String("ipv4pool-ec2-1")})
			return err
		}, "UnknownParameter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if code(err) != tc.code || status(err) != http.StatusBadRequest {
				t.Fatalf("err = %v, want %s with HTTP 400", err, tc.code)
			}
		})
	}
	if got := s.Addresses(); len(got) != 2 || got[0].AssociationID != assoc {
		t.Fatalf("addresses = %+v, want the two allocated, unchanged", got)
	}
}

func TestAddressLimit(t *testing.T) {
	s := ec2.New(ec2.WithAddressLimit(2))
	c := newClient(t, s)
	allocate(t, c)
	allocate(t, c)
	if _, err := c.AllocateAddress(t.Context(), &awsec2.AllocateAddressInput{}); code(err) != "AddressLimitExceeded" {
		t.Fatalf("a third allocation = %v, want AddressLimitExceeded", err)
	}
	other := clientAt(serve(t, s), "eu-west-1", withRetries(1))
	if _, err := other.AllocateAddress(t.Context(), &awsec2.AllocateAddressInput{}); err != nil {
		t.Fatalf("the limit is per region: %v", err)
	}
}

func TestAssociateAndDisassociate(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	eip := allocate(t, c)
	id := aws.ToString(eip.AllocationId)
	first := createInterface(t, c)
	second := createInterface(t, c)
	firstID, secondID := aws.ToString(first.NetworkInterfaceId), aws.ToString(second.NetworkInterfaceId)

	assoc, err := associate(t, c, id, firstID, false)
	if err != nil || assoc == "" {
		t.Fatalf("associate = %q, %v", assoc, err)
	}
	if again, err := associate(t, c, id, firstID, false); err != nil || again != assoc {
		t.Fatalf("associating again with the same interface = %q, %v; want %q", again, err, assoc)
	}
	got, _ := describeAddresses(t, c, &awsec2.DescribeAddressesInput{AllocationIds: []string{id}})
	if a := got[0]; aws.ToString(a.AssociationId) != assoc || aws.ToString(a.NetworkInterfaceId) != firstID ||
		aws.ToString(a.PrivateIpAddress) != aws.ToString(first.PrivateIpAddress) {
		t.Fatalf("associated address = %+v", a)
	}
	nis, _ := describeInterfaces(t, c, &awsec2.DescribeNetworkInterfacesInput{NetworkInterfaceIds: []string{firstID}})
	if as := nis[0].Association; as == nil || aws.ToString(as.PublicIp) != aws.ToString(eip.PublicIp) ||
		aws.ToString(as.AllocationId) != id || aws.ToString(as.AssociationId) != assoc {
		t.Fatalf("interface association = %+v", as)
	}

	moved, err := c.AssociateAddress(t.Context(), &awsec2.AssociateAddressInput{AllocationId: aws.String(id), NetworkInterfaceId: aws.String(secondID)})
	if err != nil || aws.ToString(moved.AssociationId) == assoc {
		t.Fatalf("reassociating, which is automatic unless AllowReassociation is false = %+v, %v", moved, err)
	}
	if a, _ := s.LookupAddress(region, id); a.NetworkInterfaceID != secondID {
		t.Fatalf("after reassociating, the address is on %s", a.NetworkInterfaceID)
	}
	nis, _ = describeInterfaces(t, c, &awsec2.DescribeNetworkInterfacesInput{NetworkInterfaceIds: []string{firstID}})
	if nis[0].Association != nil {
		t.Fatalf("the first interface kept its association: %+v", nis[0].Association)
	}

	// An interface holds one address: associating another replaces it.
	replacement := aws.ToString(allocate(t, c).AllocationId)
	if _, err := associate(t, c, replacement, secondID, false); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.LookupAddress(region, id); a.AssociationID != "" {
		t.Fatalf("the replaced address is still associated: %+v", a)
	}

	r, _ := s.LookupAddress(region, replacement)
	if _, err := c.DisassociateAddress(t.Context(), &awsec2.DisassociateAddressInput{AssociationId: aws.String(r.AssociationID)}); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.LookupAddress(region, replacement); a.AssociationID != "" || a.NetworkInterfaceID != "" || a.PrivateIPAddress != "" {
		t.Fatalf("after disassociating = %+v", a)
	}
}

func TestCreateDescribeDeleteInterface(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	out, err := c.CreateNetworkInterface(t.Context(), &awsec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnet), Groups: []string{"sg-1", "sg-2"}, Description: aws.String("for a test"),
		TagSpecifications: tagSpec(types.ResourceTypeNetworkInterface, "session", "s1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ni := out.NetworkInterface
	id := aws.ToString(ni.NetworkInterfaceId)
	if ni.Status != types.NetworkInterfaceStatusAvailable || aws.ToString(ni.SubnetId) != subnet || aws.ToString(ni.MacAddress) == "" ||
		aws.ToString(ni.PrivateIpAddress) != "10.0.0.4" || len(ni.Groups) != 2 || tags(ni.TagSet)["session"] != "s1" ||
		aws.ToString(ni.AvailabilityZone) != region+"a" || aws.ToString(ni.Description) != "for a test" || aws.ToString(out.ClientToken) == "" {
		t.Fatalf("created = %+v", ni)
	}
	got, err := describeInterfaces(t, c, &awsec2.DescribeNetworkInterfacesInput{NetworkInterfaceIds: []string{id}})
	if err != nil || len(got) != 1 || aws.ToString(got[0].MacAddress) != aws.ToString(ni.MacAddress) ||
		len(got[0].PrivateIpAddresses) != 1 || !aws.ToBool(got[0].PrivateIpAddresses[0].Primary) {
		t.Fatalf("describe = %+v, %v", got, err)
	}

	eip := aws.ToString(allocate(t, c).AllocationId)
	if _, err := associate(t, c, eip, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteNetworkInterface(t.Context(), &awsec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String(id)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LookupNetworkInterface(region, id); ok {
		t.Fatal("the interface is still there")
	}
	if a, ok := s.LookupAddress(region, eip); !ok || a.AssociationID != "" {
		t.Fatalf("deleting the interface left its address %+v, %v; want it allocated and disassociated", a, ok)
	}
	again, err := c.CreateNetworkInterface(t.Context(), &awsec2.CreateNetworkInterfaceInput{SubnetId: aws.String(subnet), PrivateIpAddress: aws.String("10.0.0.4")})
	if err != nil || aws.ToString(again.NetworkInterface.PrivateIpAddress) != "10.0.0.4" {
		t.Fatalf("reusing the freed IP = %v", err)
	}
}

func TestInterfaceErrors(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	ni := createInterface(t, c)
	id := aws.ToString(ni.NetworkInterfaceId)
	if err := s.AttachNetworkInterface(region, id, "i-0123456789abcdef0"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		call func() error
		code string
	}{
		{"describe an unknown interface", func() error {
			_, err := describeInterfaces(t, c, &awsec2.DescribeNetworkInterfacesInput{NetworkInterfaceIds: []string{id, "eni-missing"}})
			return err
		}, "InvalidNetworkInterfaceID.NotFound"},
		{"delete an unknown interface", func() error {
			_, err := c.DeleteNetworkInterface(t.Context(), &awsec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String("eni-missing")})
			return err
		}, "InvalidNetworkInterfaceID.NotFound"},
		{"delete an attached interface", func() error {
			_, err := c.DeleteNetworkInterface(t.Context(), &awsec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String(id)})
			return err
		}, "InvalidNetworkInterface.InUse"},
		{"create with a private IP in use", func() error {
			_, err := c.CreateNetworkInterface(t.Context(), &awsec2.CreateNetworkInterfaceInput{SubnetId: aws.String(subnet), PrivateIpAddress: ni.PrivateIpAddress})
			return err
		}, "InvalidIPAddress.InUse"},
		{"tag an interface as an address", func() error {
			_, err := c.CreateNetworkInterface(t.Context(), &awsec2.CreateNetworkInterfaceInput{SubnetId: aws.String(subnet), TagSpecifications: tagSpec(types.ResourceTypeElasticIp, "k", "v")})
			return err
		}, "InvalidParameterValue"},
		{"page through chosen interfaces", func() error {
			_, err := describeInterfaces(t, c, &awsec2.DescribeNetworkInterfacesInput{NetworkInterfaceIds: []string{id}, MaxResults: aws.Int32(5)})
			return err
		}, "InvalidParameterCombination"},
		{"a page size under 5", func() error {
			_, err := describeInterfaces(t, c, &awsec2.DescribeNetworkInterfacesInput{MaxResults: aws.Int32(4)})
			return err
		}, "InvalidParameterValue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); code(err) != tc.code || status(err) != http.StatusBadRequest {
				t.Fatalf("err = %v, want %s with HTTP 400", err, tc.code)
			}
		})
	}
	got, _ := describeInterfaces(t, c, &awsec2.DescribeNetworkInterfacesInput{NetworkInterfaceIds: []string{id}})
	if got[0].Status != types.NetworkInterfaceStatusInUse || aws.ToString(got[0].Attachment.InstanceId) != "i-0123456789abcdef0" {
		t.Fatalf("attached interface = %+v", got[0])
	}
	if err := s.DetachNetworkInterface(region, id); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteNetworkInterface(t.Context(), &awsec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String(id)}); err != nil {
		t.Fatalf("delete after detaching: %v", err)
	}
	if err := s.AttachNetworkInterface(region, id, "i-1"); !errors.Is(err, ec2.ErrNotFound) {
		t.Fatalf("attaching a deleted interface = %v", err)
	}
}

func TestAddressFilters(t *testing.T) {
	c := newClient(t, ec2.New())
	ids := map[string]string{}
	for _, name := range []string{"web-1", "web-2", "db-1"} {
		ids[name] = aws.ToString(allocate(t, c, "Name", name, "session", "s1").AllocationId)
	}
	ids["untagged"] = aws.ToString(allocate(t, c).AllocationId)
	ni := aws.ToString(createInterface(t, c).NetworkInterfaceId)
	assoc, err := associate(t, c, ids["db-1"], ni, false)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := describeAddresses(t, c, &awsec2.DescribeAddressesInput{AllocationIds: []string{ids["db-1"]}})
	for _, tc := range []struct {
		filters []types.Filter
		want    []string
	}{
		{[]types.Filter{filter("tag:Name", "web-1", "db-1")}, []string{"web-1", "db-1"}},
		{[]types.Filter{filter("tag:Name", "web-*")}, []string{"web-1", "web-2"}},
		{[]types.Filter{filter("tag:Name", "???-1"), filter("tag-key", "session")}, []string{"web-1"}},
		{[]types.Filter{filter("tag-key", "session")}, []string{"web-1", "web-2", "db-1"}},
		{[]types.Filter{filter("allocation-id", ids["untagged"])}, []string{"untagged"}},
		{[]types.Filter{filter("public-ip", aws.ToString(db[0].PublicIp))}, []string{"db-1"}},
		{[]types.Filter{filter("network-interface-id", ni)}, []string{"db-1"}},
		{[]types.Filter{filter("association-id", assoc)}, []string{"db-1"}},
		{[]types.Filter{filter("private-ip-address", aws.ToString(db[0].PrivateIpAddress))}, []string{"db-1"}},
		{[]types.Filter{filter("domain", "vpc"), filter("tag:Name", "nothing")}, nil},
	} {
		got, err := describeAddresses(t, c, &awsec2.DescribeAddressesInput{Filters: tc.filters})
		var names []string
		for _, a := range got {
			for name, id := range ids {
				if id == aws.ToString(a.AllocationId) {
					names = append(names, name)
				}
			}
		}
		slices.Sort(names)
		slices.Sort(tc.want)
		if err != nil || !slices.Equal(names, tc.want) {
			t.Errorf("filters %v = %v, %v; want %v", describe(tc.filters), names, err, tc.want)
		}
	}
}

func describe(fs []types.Filter) string {
	var out []string
	for _, f := range fs {
		out = append(out, fmt.Sprintf("%s=%v", aws.ToString(f.Name), f.Values))
	}
	return fmt.Sprint(out)
}

func TestInterfaceFilters(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	a := aws.ToString(createInterface(t, c, "session", "s1").NetworkInterfaceId)
	b := aws.ToString(createInterface(t, c, "session", "s2").NetworkInterfaceId)
	other, err := c.CreateNetworkInterface(t.Context(), &awsec2.CreateNetworkInterfaceInput{SubnetId: aws.String("subnet-other")})
	if err != nil {
		t.Fatal(err)
	}
	o := aws.ToString(other.NetworkInterface.NetworkInterfaceId)
	if err := s.AttachNetworkInterface(region, b, "i-1"); err != nil {
		t.Fatal(err)
	}
	eip := allocate(t, c)
	if _, err := associate(t, c, aws.ToString(eip.AllocationId), a, false); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		filters []types.Filter
		want    []string
	}{
		{[]types.Filter{filter("tag:session", "s1")}, []string{a}},
		{[]types.Filter{filter("tag:session", "s*")}, []string{a, b}},
		{[]types.Filter{filter("tag-key", "session")}, []string{a, b}},
		{[]types.Filter{filter("network-interface-id", a, o)}, []string{a, o}},
		{[]types.Filter{filter("status", "in-use")}, []string{b}},
		{[]types.Filter{filter("status", "available"), filter("subnet-id", subnet)}, []string{a}},
		{[]types.Filter{filter("subnet-id", "subnet-other")}, []string{o}},
		{[]types.Filter{filter("association.public-ip", aws.ToString(eip.PublicIp))}, []string{a}},
		{[]types.Filter{filter("attachment.instance-id", "i-1")}, []string{b}},
		{[]types.Filter{filter("group-id", "sg-1")}, []string{a, b}},
	} {
		got, err := describeInterfaces(t, c, &awsec2.DescribeNetworkInterfacesInput{Filters: tc.filters})
		var names []string
		for _, ni := range got {
			names = append(names, aws.ToString(ni.NetworkInterfaceId))
		}
		slices.Sort(tc.want)
		if err != nil || !slices.Equal(names, tc.want) {
			t.Errorf("filters %v = %v, %v; want %v", describe(tc.filters), names, err, tc.want)
		}
	}
	if _, err := describeInterfaces(t, c, &awsec2.DescribeNetworkInterfacesInput{Filters: []types.Filter{filter("vpc-id", "vpc-1")}}); code(err) != "InvalidParameterValue" {
		t.Errorf("an unsupported filter = %v, want InvalidParameterValue", err)
	}
}

func TestDescribeInterfacesPages(t *testing.T) {
	c := newClient(t, ec2.New())
	var want []string
	for range 12 {
		want = append(want, aws.ToString(createInterface(t, c).NetworkInterfaceId))
	}
	var got []string
	pages := 0
	p := awsec2.NewDescribeNetworkInterfacesPaginator(c, &awsec2.DescribeNetworkInterfacesInput{MaxResults: aws.Int32(5)})
	for p.HasMorePages() {
		page, err := p.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, ni := range page.NetworkInterfaces {
			got = append(got, aws.ToString(ni.NetworkInterfaceId))
		}
	}
	if pages != 3 || !slices.Equal(got, want) {
		t.Fatalf("%d pages of %v, want 3 of %v", pages, got, want)
	}
}

func TestClientToken(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	in := &awsec2.CreateNetworkInterfaceInput{SubnetId: aws.String(subnet), ClientToken: aws.String("token-1")}
	first, err := c.CreateNetworkInterface(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.CreateNetworkInterface(t.Context(), in)
	if err != nil || aws.ToString(again.NetworkInterface.NetworkInterfaceId) != aws.ToString(first.NetworkInterface.NetworkInterfaceId) ||
		aws.ToString(again.ClientToken) != "token-1" {
		t.Fatalf("repeating the token = %+v, %v; want the first interface", again, err)
	}
	in.Description = aws.String("different")
	if _, err := c.CreateNetworkInterface(t.Context(), in); code(err) != "IdempotentParameterMismatch" {
		t.Fatalf("the token with other parameters = %v, want IdempotentParameterMismatch", err)
	}
	west := clientAt(serve(t, s), "us-west-2", withRetries(1))
	in.Description = nil
	if _, err := west.CreateNetworkInterface(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if got := len(s.NetworkInterfaces()); got != 2 {
		t.Fatalf("%d interfaces, want one per region", got)
	}
}

func TestClientTokenReplaysAcrossALostResponse(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "ec2.CreateNetworkInterface", Nth: 1, Fault: fault.LoseResponse()})
	s := ec2.New(ec2.WithFaults(faults))
	c := newClient(t, s)
	in := &awsec2.CreateNetworkInterfaceInput{SubnetId: aws.String(subnet), ClientToken: aws.String("token-1")}
	if _, err := c.CreateNetworkInterface(t.Context(), in); err == nil || code(err) != "" {
		t.Fatalf("the first create = %v, want a transport error", err)
	}
	out, err := c.CreateNetworkInterface(t.Context(), in)
	if err != nil {
		t.Fatalf("retrying with the token: %v", err)
	}
	got := s.NetworkInterfaces()
	if len(got) != 1 || got[0].NetworkInterfaceID != aws.ToString(out.NetworkInterface.NetworkInterfaceId) {
		t.Fatalf("interfaces = %+v, want only the one the retry returned", got)
	}
	if inj := faults.Injected(); inj[0].Call.Resource != region+"/"+got[0].NetworkInterfaceID {
		t.Fatalf("the fault named %s, want the interface it created", inj[0].Call.Resource)
	}
}

func TestSDKRetriesReuseTheClientToken(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "ec2.CreateNetworkInterface", Times: 2, Fault: fault.FailAfter(http.StatusInternalServerError)})
	s := ec2.New(ec2.WithFaults(faults))
	c := clientAt(serve(t, s), region, func(o *awsec2.Options) { o.RetryMaxAttempts = 3 })
	out, err := c.CreateNetworkInterface(t.Context(), &awsec2.CreateNetworkInterfaceInput{SubnetId: aws.String(subnet)})
	if err != nil {
		t.Fatalf("create with the SDK's retries: %v", err)
	}
	if got := s.NetworkInterfaces(); len(got) != 1 || got[0].NetworkInterfaceID != aws.ToString(out.NetworkInterface.NetworkInterfaceId) {
		t.Fatalf("interfaces = %+v, want one from three attempts", got)
	}
	if got := len(faults.Injected()); got != 2 {
		t.Fatalf("%d faults injected, want 2", got)
	}
}

func TestFaults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fault  fault.Fault
		code   string // error code the caller sees; "-" for a transport error
		status int
		stored bool // whether the allocation took effect anyway
	}{
		{"rejected", fault.Unavailable(), "Unavailable", http.StatusServiceUnavailable, false},
		{"rate limited", fault.RateLimited(), "RequestLimitExceeded", http.StatusTooManyRequests, false},
		{"a chosen code", fault.Reject(http.StatusBadRequest, "InsufficientAddressCapacity"), "InsufficientAddressCapacity", http.StatusBadRequest, false},
		{"response lost", fault.LoseResponse(), "-", 0, true},
		{"error after applying", fault.FailAfter(http.StatusInternalServerError), "InternalError", http.StatusInternalServerError, true},
		{"operation failed", fault.FailOperation(http.StatusInternalServerError), "InternalError", http.StatusInternalServerError, false},
		{"slow", fault.Delay(20 * time.Millisecond), "", http.StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			faults := fault.New().Add(fault.Rule{Method: "ec2.AllocateAddress", Nth: 1, Fault: tc.fault})
			s := ec2.New(ec2.WithFaults(faults))
			c := newClient(t, s)
			start := time.Now()
			_, err := c.AllocateAddress(t.Context(), &awsec2.AllocateAddressInput{})
			if took := time.Since(start); took < tc.fault.Delay {
				t.Fatalf("allocate took %v, want at least %v", took, tc.fault.Delay)
			}
			switch {
			case tc.code == "" && err != nil:
				t.Fatalf("allocate = %v, want success", err)
			case tc.code == "-" && (err == nil || code(err) != ""):
				t.Fatalf("allocate = %v, want a transport error", err)
			case tc.code != "" && tc.code != "-" && (code(err) != tc.code || status(err) != tc.status):
				t.Fatalf("allocate = %v, want %s with HTTP %d", err, tc.code, tc.status)
			}
			if got := len(s.Addresses()); (got == 1) != tc.stored {
				t.Fatalf("%d addresses stored, want stored = %v", got, tc.stored)
			}
			if got := faults.Injected(); len(got) != 1 {
				t.Fatalf("Injected = %v", got)
			}
		})
	}
}

func TestAFaultRuleCanTargetOneResource(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	keep := aws.ToString(allocate(t, c).AllocationId)
	target := aws.ToString(allocate(t, c).AllocationId)
	faults := fault.New().Add(fault.Rule{Method: "ec2.ReleaseAddress", Resource: region + "/" + target, Fault: fault.Unavailable()})
	c = newClient(t, ec2.New(ec2.WithFaults(faults)))
	// A fresh server allocates the same IDs, in the same order.
	allocate(t, c)
	allocate(t, c)
	if _, err := c.ReleaseAddress(t.Context(), &awsec2.ReleaseAddressInput{AllocationId: aws.String(keep)}); err != nil {
		t.Fatalf("releasing another address: %v", err)
	}
	if _, err := c.ReleaseAddress(t.Context(), &awsec2.ReleaseAddressInput{AllocationId: aws.String(target)}); code(err) != "Unavailable" {
		t.Fatalf("releasing the targeted address = %v, want Unavailable", err)
	}
}

func TestDryRun(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	_, err := c.AllocateAddress(t.Context(), &awsec2.AllocateAddressInput{DryRun: aws.Bool(true)})
	if code(err) != "DryRunOperation" || status(err) != http.StatusPreconditionFailed {
		t.Fatalf("a dry run = %v, want DryRunOperation with HTTP 412", err)
	}
	if got := s.Addresses(); len(got) != 0 {
		t.Fatalf("a dry run allocated %v", got)
	}
}

func TestRegionsAreSeparate(t *testing.T) {
	s := ec2.New()
	url := serve(t, s)
	east, west := clientAt(url, "us-east-1", withRetries(1)), clientAt(url, "us-west-2", withRetries(1))
	id := aws.ToString(allocate(t, east).AllocationId)
	if _, err := describeAddresses(t, west, &awsec2.DescribeAddressesInput{AllocationIds: []string{id}}); code(err) != "InvalidAllocationID.NotFound" {
		t.Fatalf("describing it from another region = %v", err)
	}
	westIP := aws.ToString(allocate(t, west).PublicIp)
	if a, ok := s.LookupAddress("us-east-1", id); !ok || a.PublicIP == westIP || a.Region != "us-east-1" {
		t.Fatalf("public IPs must be unique across regions: %+v and %s", a, westIP)
	}
	if _, ok := s.LookupAddress("us-west-2", id); ok {
		t.Fatal("LookupAddress found the address in another region")
	}
}

func TestEndpointFromTheEnvironment(t *testing.T) {
	url := serve(t, ec2.New())
	for _, variable := range []string{"AWS_ENDPOINT_URL_EC2", "AWS_ENDPOINT_URL"} {
		t.Run(variable, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
			t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
			t.Setenv("AWS_ACCESS_KEY_ID", "AKID")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "SECRET")
			t.Setenv("AWS_REGION", "eu-west-1")
			for _, unset := range []string{
				"AWS_ENDPOINT_URL_EC2", "AWS_ENDPOINT_URL", "AWS_IGNORE_CONFIGURED_ENDPOINT_URLS", "AWS_PROFILE",
				"AWS_DEFAULT_PROFILE", "AWS_USE_FIPS_ENDPOINT", "AWS_USE_DUALSTACK_ENDPOINT", "AWS_CA_BUNDLE",
			} {
				t.Setenv(unset, "")
			}
			t.Setenv(variable, url)
			cfg, err := config.LoadDefaultConfig(t.Context(), config.WithHTTPClient(readBodies{awshttp.NewBuildableClient()}))
			if err != nil {
				t.Fatal(err)
			}
			out, err := awsec2.NewFromConfig(cfg).AllocateAddress(t.Context(), &awsec2.AllocateAddressInput{})
			if err != nil || aws.ToString(out.NetworkBorderGroup) != "eu-west-1" {
				t.Fatalf("allocate = %+v, %v", out, err)
			}
		})
	}
}

func TestPutHelpersSetUpState(t *testing.T) {
	s := ec2.New()
	ni, err := s.PutNetworkInterface(region, ec2.NetworkInterface{SubnetID: subnet, InstanceID: "i-1", Tags: map[string]string{"owner": "someone-else"}})
	if err != nil || ni.Status != "in-use" || ni.NetworkInterfaceID == "" || ni.PrivateIPAddress == "" || ni.AttachmentID == "" {
		t.Fatalf("PutNetworkInterface = %+v, %v", ni, err)
	}
	a, err := s.PutAddress(region, ec2.Address{PublicIP: "100.64.9.9", NetworkInterfaceID: ni.NetworkInterfaceID})
	if err != nil || a.AssociationID == "" || a.PrivateIPAddress != ni.PrivateIPAddress || a.Domain != "vpc" {
		t.Fatalf("PutAddress = %+v, %v", a, err)
	}
	c := newClient(t, s)
	got, err := describeAddresses(t, c, &awsec2.DescribeAddressesInput{PublicIps: []string{"100.64.9.9"}})
	if err != nil || aws.ToString(got[0].InstanceId) != "i-1" || aws.ToString(got[0].AssociationId) != a.AssociationID {
		t.Fatalf("describe = %+v, %v", got, err)
	}
	if _, err := c.DeleteNetworkInterface(t.Context(), &awsec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String(ni.NetworkInterfaceID)}); code(err) != "InvalidNetworkInterface.InUse" {
		t.Fatalf("deleting an interface put in use = %v", err)
	}
	for _, bad := range []ec2.Address{
		{PublicIP: "100.64.9.9"},
		{PublicIP: "not-an-ip"},
		{NetworkInterfaceID: "eni-missing"},
		{Domain: "standard"},
		{Tags: map[string]string{"": "x"}},
	} {
		if _, err := s.PutAddress(region, bad); err == nil {
			t.Errorf("PutAddress accepted %+v", bad)
		}
	}
	for _, bad := range []ec2.NetworkInterface{
		{},
		{SubnetID: subnet, PrivateIPAddress: ni.PrivateIPAddress},
		{SubnetID: subnet, Status: "attaching"},
		{SubnetID: subnet, Status: "available", InstanceID: "i-1"},
		{SubnetID: subnet, NetworkInterfaceID: "interface-1"},
	} {
		if _, err := s.PutNetworkInterface(region, bad); err == nil {
			t.Errorf("PutNetworkInterface accepted %+v", bad)
		}
	}
	if err := s.AttachNetworkInterface(region, ni.NetworkInterfaceID, ""); err == nil {
		t.Error("AttachNetworkInterface accepted no instance ID")
	}
}

func TestCreatesDoNotReplacePutResources(t *testing.T) {
	s := ec2.New()
	if _, err := s.PutAddress(region, ec2.Address{AllocationID: "eipalloc-00000000000000001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutNetworkInterface(region, ec2.NetworkInterface{NetworkInterfaceID: "eni-00000000000000003", SubnetID: subnet}); err != nil {
		t.Fatal(err)
	}
	c := newClient(t, s)
	allocate(t, c)
	createInterface(t, c)
	if got := s.Addresses(); len(got) != 2 {
		t.Errorf("addresses = %+v, want the one put and the one allocated", got)
	}
	if got := s.NetworkInterfaces(); len(got) != 2 {
		t.Errorf("interfaces = %+v, want the one put and the one created", got)
	}
}

func TestAssociationIDsStayUnique(t *testing.T) {
	s := ec2.New()
	first, err := s.PutNetworkInterface(region, ec2.NetworkInterface{SubnetID: subnet})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.PutNetworkInterface(region, ec2.NetworkInterface{SubnetID: subnet})
	if err != nil {
		t.Fatal(err)
	}
	put, err := s.PutAddress(region, ec2.Address{NetworkInterfaceID: first.NetworkInterfaceID, AssociationID: "eipassoc-00000000000000003"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutAddress(region, ec2.Address{NetworkInterfaceID: second.NetworkInterfaceID, AssociationID: put.AssociationID}); err == nil {
		t.Error("a second address put with an association ID in use was accepted")
	}
	c := newClient(t, s)
	assoc, err := associate(t, c, aws.ToString(allocate(t, c).AllocationId), second.NetworkInterfaceID, false)
	if err != nil {
		t.Fatal(err)
	}
	if assoc == put.AssociationID {
		t.Errorf("a new association took the ID %s another address holds", put.AssociationID)
	}
}

// reserveInterface is the pattern the stand-in exists to test: create with
// one client token until it succeeds, however the attempts fail.
func reserveInterface(t *testing.T, c *awsec2.Client) (string, error) {
	t.Helper()
	in := &awsec2.CreateNetworkInterfaceInput{SubnetId: aws.String(subnet), ClientToken: aws.String("reserve-1")}
	var last error
	for range 20 {
		out, err := c.CreateNetworkInterface(t.Context(), in)
		if err == nil {
			return aws.ToString(out.NetworkInterface.NetworkInterfaceId), nil
		}
		last = err
	}
	return "", last
}

func TestRetryingWithATokenConvergesUnderRandomFaults(t *testing.T) {
	for seed := range uint64(50) {
		faults := fault.New(fault.Seed(seed)).Random(0.4,
			fault.Unavailable(), fault.LoseResponse(), fault.FailAfter(http.StatusInternalServerError))
		s := ec2.New(ec2.WithFaults(faults))
		c := newClient(t, s)
		id, err := reserveInterface(t, c)
		if err != nil {
			t.Fatalf("seed %d: %v\ninjected: %v", seed, err, faults.Injected())
		}
		if got := s.NetworkInterfaces(); len(got) != 1 || got[0].NetworkInterfaceID != id {
			t.Fatalf("seed %d: interfaces = %+v, want only %s\ninjected: %v", seed, got, id, faults.Injected())
		}
	}
}

func TestConcurrentAllocations(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if _, err := c.AllocateAddress(t.Context(), &awsec2.AllocateAddressInput{}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, a := range s.Addresses() {
		if seen[a.PublicIP] {
			t.Fatalf("IP %s handed out twice", a.PublicIP)
		}
		seen[a.PublicIP] = true
	}
	if len(seen) != 16 {
		t.Fatalf("%d addresses, want 16", len(seen))
	}
}
