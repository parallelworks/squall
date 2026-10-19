package arm_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azarm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v8"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources/v3"

	"github.com/parallelworks/squall/azure/arm"
	"github.com/parallelworks/squall/fault"
)

const (
	subscription = "00000000-0000-0000-0000-000000000001"
	location     = "eastus"
	group        = "rg-1"
	subnet       = "/subscriptions/" + subscription + "/resourceGroups/net/providers/Microsoft.Network/virtualNetworks/vnet/subnets/default"
	vnet         = "/subscriptions/" + subscription + "/resourceGroups/net/providers/Microsoft.Network/virtualNetworks/vnet"
)

type credential struct{}

func (credential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

type clients struct {
	server   *arm.Server
	groups   *armresources.ResourceGroupsClient
	vms      *armcompute.VirtualMachinesClient
	sets     *armcompute.VirtualMachineScaleSetsClient
	setVMs   *armcompute.VirtualMachineScaleSetVMsClient
	skus     *armcompute.ResourceSKUsClient
	nics     *armnetwork.InterfacesClient
	pips     *armnetwork.PublicIPAddressesClient
	nsgs     *armnetwork.SecurityGroupsClient
	zones    *armprivatedns.PrivateZonesClient
	links    *armprivatedns.VirtualNetworkLinksClient
	records  *armprivatedns.RecordSetsClient
	requests *[]string
}

// setup serves a new server and returns clients for it that do not retry,
// so each call is one request.
func setup(t *testing.T, opts ...arm.Option) *clients {
	t.Helper()
	return setupWith(t, policy.RetryOptions{MaxRetries: -1}, opts...)
}

// setupRetrying is setup with clients that retry quickly.
func setupRetrying(t *testing.T, opts ...arm.Option) *clients {
	t.Helper()
	return setupWith(t, policy.RetryOptions{MaxRetries: 3, RetryDelay: time.Millisecond, MaxRetryDelay: 5 * time.Millisecond}, opts...)
}

func setupWith(t *testing.T, retry policy.RetryOptions, opts ...arm.Option) *clients {
	t.Helper()
	s := arm.New(opts...)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	requests := &[]string{}
	options := &azarm.ClientOptions{
		Cloud: cloud.Configuration{Services: map[cloud.ServiceName]cloud.ServiceConfiguration{
			cloud.ResourceManager: {Endpoint: srv.URL, Audience: "https://management.azure.com"},
		}},
		Retry:                           retry,
		PerCallPolicies:                 []policy.Policy{recorder{requests}},
		InsecureAllowCredentialWithHTTP: true,
	}
	compute, err := armcompute.NewClientFactory(subscription, credential{}, options)
	if err != nil {
		t.Fatal(err)
	}
	network, err := armnetwork.NewClientFactory(subscription, credential{}, options)
	if err != nil {
		t.Fatal(err)
	}
	dns, err := armprivatedns.NewClientFactory(subscription, credential{}, options)
	if err != nil {
		t.Fatal(err)
	}
	resources, err := armresources.NewClientFactory(subscription, credential{}, options)
	if err != nil {
		t.Fatal(err)
	}
	return &clients{
		server: s, groups: resources.NewResourceGroupsClient(),
		vms: compute.NewVirtualMachinesClient(), sets: compute.NewVirtualMachineScaleSetsClient(),
		setVMs: compute.NewVirtualMachineScaleSetVMsClient(), skus: compute.NewResourceSKUsClient(),
		nics: network.NewInterfacesClient(), pips: network.NewPublicIPAddressesClient(), nsgs: network.NewSecurityGroupsClient(),
		zones: dns.NewPrivateZonesClient(), links: dns.NewVirtualNetworkLinksClient(), records: dns.NewRecordSetsClient(),
		requests: requests,
	}
}

// recorder notes the method and path of every call a client makes, once per call.
type recorder struct{ requests *[]string }

func (r recorder) Do(req *policy.Request) (*http.Response, error) {
	*r.requests = append(*r.requests, req.Raw().Method+" "+req.Raw().URL.Path)
	return req.Next()
}

// polls counts the calls that polled an operation.
func (c *clients) polls() int {
	n := 0
	for _, r := range *c.requests {
		if r := strings.ToLower(r); strings.Contains(r, "/operations/") || strings.Contains(r, "operationresults/") ||
			strings.Contains(r, "operationstatuses/") {
			n++
		}
	}
	return n
}

// pollOnce polls p once and checks that its operation is still in progress.
func pollOnce[T any](t *testing.T, p *runtime.Poller[T]) {
	t.Helper()
	resp, err := p.Poll(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if p.Done() {
		t.Fatal("the operation finished")
	}
}

func code(err error) string {
	if e, ok := errors.AsType[*azcore.ResponseError](err); ok {
		return e.ErrorCode
	}
	return ""
}

func status(err error) int {
	if e, ok := errors.AsType[*azcore.ResponseError](err); ok {
		return e.StatusCode
	}
	return 0
}

// wait polls the poller a Begin call returned to the end.
func wait[T any](t *testing.T, p *runtime.Poller[T], err error) (T, error) {
	t.Helper()
	if err != nil {
		var zero T
		return zero, err
	}
	return p.PollUntilDone(t.Context(), nil)
}

func (c *clients) createGroup(t *testing.T, name string, tags map[string]*string) {
	t.Helper()
	if _, err := c.groups.CreateOrUpdate(t.Context(), name, armresources.ResourceGroup{Location: new(location), Tags: tags}, nil); err != nil {
		t.Fatal(err)
	}
}

func (c *clients) listGroups(t *testing.T, filter string) []string {
	t.Helper()
	var opts *armresources.ResourceGroupsClientListOptions
	if filter != "" {
		opts = &armresources.ResourceGroupsClientListOptions{Filter: new(filter)}
	}
	var names []string
	pager := c.groups.NewListPager(opts)
	for pager.More() {
		page, err := pager.NextPage(t.Context())
		if err != nil {
			t.Fatalf("list %q: %v", filter, err)
		}
		for _, g := range page.Value {
			names = append(names, *g.Name)
		}
	}
	return names
}

func TestResourceGroups(t *testing.T) {
	c := setup(t)
	if _, err := c.groups.Get(t.Context(), "nope", nil); code(err) != "ResourceGroupNotFound" || status(err) != http.StatusNotFound {
		t.Fatalf("get of a missing group = %v", err)
	}
	created, err := c.groups.CreateOrUpdate(t.Context(), "Session-A", armresources.ResourceGroup{
		Location: new("East US"), Tags: map[string]*string{"session": new("a"), "owner": new("x")},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := created.ResourceGroup
	if *g.ID != "/subscriptions/"+subscription+"/resourceGroups/Session-A" || *g.Location != location ||
		*g.Properties.ProvisioningState != "Succeeded" || *g.Tags["session"] != "a" || *g.Type != "Microsoft.Resources/resourceGroups" {
		t.Fatalf("created = %+v", g)
	}
	got, err := c.groups.Get(t.Context(), "session-a", nil)
	if err != nil || *got.Name != "Session-A" {
		t.Fatalf("get without regard to case = %+v, %v", got, err)
	}
	if exists, err := c.groups.CheckExistence(t.Context(), "session-a", nil); err != nil || !exists.Success {
		t.Fatalf("check existence = %+v, %v", exists, err)
	}
	if exists, err := c.groups.CheckExistence(t.Context(), "nope", nil); err != nil || exists.Success {
		t.Fatalf("check existence of a missing group = %+v, %v", exists, err)
	}
	_, err = c.groups.CreateOrUpdate(t.Context(), "Session-A", armresources.ResourceGroup{Location: new("westus")}, nil)
	if code(err) != "InvalidResourceGroupLocation" || status(err) != http.StatusConflict {
		t.Fatalf("moving a group = %v", err)
	}
	if _, err := c.groups.CreateOrUpdate(t.Context(), "Session-A", armresources.ResourceGroup{}, nil); code(err) != "LocationRequired" {
		t.Fatalf("a group with no location = %v", err)
	}
	updated, err := c.groups.CreateOrUpdate(t.Context(), "Session-A", armresources.ResourceGroup{
		Location: new(location), Tags: map[string]*string{"session": new("b")},
	}, nil)
	if err != nil || len(updated.Tags) != 1 || *updated.Tags["session"] != "b" {
		t.Fatalf("update replaces tags = %+v, %v", updated.Tags, err)
	}
	if got := c.server.ResourceGroups(); len(got) != 1 || got[0].Tags["session"] != "b" {
		t.Fatalf("ResourceGroups = %+v", got)
	}
}

func TestResourceGroupTagFilters(t *testing.T) {
	c := setup(t)
	c.createGroup(t, "a", map[string]*string{"Session": new("s1")})
	c.createGroup(t, "b", map[string]*string{"session": new("s2")})
	c.createGroup(t, "c", map[string]*string{"session": new("it's")})
	c.createGroup(t, "d", nil)
	for filter, want := range map[string][]string{
		"":                     {"a", "b", "c", "d"},
		"tagName eq 'session'": {"a", "b", "c"},
		"tagname eq 'SESSION' and tagvalue eq 's1'":    {"a"},
		"tagName eq 'session' and tagValue eq 'S1'":    nil,
		"tagName eq 'session' and tagValue eq 'it''s'": {"c"},
	} {
		if got := c.listGroups(t, filter); !slices.Equal(got, want) {
			t.Errorf("list %q = %v, want %v", filter, got, want)
		}
	}
	for _, filter := range []string{"startswith(tagName, 'se')", "tagValue eq 's1'", "tagName eq 'session' or tagValue eq 'x'", "tagName eq 'x"} {
		_, err := c.groups.NewListPager(&armresources.ResourceGroupsClientListOptions{Filter: new(filter)}).NextPage(t.Context())
		if status(err) != http.StatusBadRequest {
			t.Errorf("list %q = %v, want 400", filter, err)
		}
	}
	page, err := c.groups.NewListPager(&armresources.ResourceGroupsClientListOptions{Top: new(int32(2))}).NextPage(t.Context())
	if err != nil || len(page.Value) != 2 {
		t.Errorf("list with $top = %d, %v", len(page.Value), err)
	}
}

func TestDeleteResourceGroupCascades(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.createGroup(t, "other", nil)
	c.createNIC(t, "other", "keep", nil)
	c.createVM(t, "vm-1", c.createNIC(t, group, "nic-1", nil))
	c.createScaleSet(t, "set", 2)
	c.createZone(t, "example.internal")
	c.putRecord(t, "example.internal", "www", "10.1.0.4")
	p, err := c.groups.BeginDelete(t.Context(), group, nil)
	if _, err := wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	if c.polls() != 0 {
		t.Errorf("polled %d times, want 0 for a deletion that finished at once", c.polls())
	}
	if _, err := c.groups.Get(t.Context(), group, nil); code(err) != "ResourceGroupNotFound" {
		t.Fatalf("get after delete = %v", err)
	}
	got := c.server.Resources()
	if len(got) != 1 || got[0].ID != "/subscriptions/"+subscription+"/resourceGroups/other/providers/Microsoft.Network/networkInterfaces/keep" {
		t.Fatalf("resources after delete = %+v", got)
	}
	if _, err = c.groups.BeginDelete(t.Context(), group, nil); code(err) != "ResourceGroupNotFound" {
		t.Fatalf("deleting it again = %v", err)
	}
	// Its addresses are free again.
	c.createGroup(t, group, nil)
	static := nicBody(nil, nil)
	static.Properties.IPConfigurations[0].Properties.PrivateIPAllocationMethod = new(armnetwork.IPAllocationMethodStatic)
	static.Properties.IPConfigurations[0].Properties.PrivateIPAddress = new("10.0.0.5")
	np, err := c.nics.BeginCreateOrUpdate(t.Context(), group, "nic-2", static, nil)
	if _, err = wait(t, np, err); err != nil {
		t.Errorf("taking the deleted NIC's address = %v", err)
	}
}

func TestDeleteResourceGroupWhileSettling(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(-1))
	c.createGroup(t, group, nil)
	c.server.Settle()
	nic := c.createNIC(t, group, "nic-1", nil)
	if *nic.Properties.ProvisioningState != "Creating" {
		t.Fatalf("a NIC in progress = %s", *nic.Properties.ProvisioningState)
	}
	poller, err := c.groups.BeginDelete(t.Context(), group, nil)
	if err != nil {
		t.Fatal(err)
	}
	pollOnce(t, poller)
	if g, err := c.groups.Get(t.Context(), group, nil); err != nil || *g.Properties.ProvisioningState != "Deleting" {
		t.Fatalf("group being deleted = %+v, %v", g, err)
	}
	if _, err := c.groups.CreateOrUpdate(t.Context(), group, armresources.ResourceGroup{Location: new(location)}, nil); code(err) != "ResourceGroupBeingDeleted" || status(err) != http.StatusConflict {
		t.Fatalf("recreating a group being deleted = %v", err)
	}
	if _, err := c.nics.BeginCreateOrUpdate(t.Context(), group, "nic-2", nicBody(nil, nil), nil); code(err) != "ResourceGroupBeingDeleted" {
		t.Fatalf("creating in a group being deleted = %v", err)
	}
	again, err := c.groups.BeginDelete(t.Context(), group, nil)
	if err != nil || again.Done() {
		t.Fatalf("deleting it again = done %v, %v", again.Done(), err)
	}
	c.server.Settle()
	if _, err := poller.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if got := c.server.Resources(); len(got) != 0 {
		t.Fatalf("resources = %+v", got)
	}
}

func TestDeleteResourceGroupFailedOperation(t *testing.T) {
	faults := fault.New().Add(fault.Rule{
		Method: "Microsoft.Resources/subscriptions/resourceGroups/delete", Nth: 1, Fault: fault.FailOperation(http.StatusConflict),
	})
	c := setup(t, arm.WithFaults(faults))
	c.createGroup(t, group, nil)
	p, err := c.groups.BeginDelete(t.Context(), group, nil)
	_, err = wait(t, p, err)
	if status(err) != http.StatusConflict || code(err) != "Conflict" {
		t.Fatalf("failed deletion = %v", err)
	}
	if got := c.server.ResourceGroups(); len(got) != 1 || got[0].ProvisioningState != "Succeeded" {
		t.Fatalf("a failed deletion took effect: %+v", got)
	}
}

func TestResourceSKUsListNone(t *testing.T) {
	c := setup(t)
	page, err := c.skus.NewListPager(&armcompute.ResourceSKUsClientListOptions{Filter: new("location eq 'eastus'")}).NextPage(t.Context())
	if err != nil || len(page.Value) != 0 {
		t.Fatalf("skus = %v, %v", page.Value, err)
	}
}
