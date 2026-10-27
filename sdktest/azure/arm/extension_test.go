package arm_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v8"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
)

const scriptExtension = "CustomScriptExtension"

func (c *clients) createWindowsVM(t *testing.T, name string) armcompute.VirtualMachine {
	t.Helper()
	body := vmBody(c.createNIC(t, group, name+"-nic", nil).ID)
	body.Properties.StorageProfile.ImageReference = &armcompute.ImageReference{
		Publisher: new("MicrosoftWindowsServer"), Offer: new("WindowsServer"), SKU: new("2022-datacenter"), Version: new("latest"),
	}
	body.Properties.OSProfile.WindowsConfiguration = &armcompute.WindowsConfiguration{}
	p, err := c.vms.BeginCreateOrUpdate(t.Context(), group, name, body, nil)
	res, err := wait(t, p, err)
	if err != nil {
		t.Fatal(err)
	}
	return res.VirtualMachine
}

func scriptBody(command string) armcompute.VirtualMachineExtension {
	return armcompute.VirtualMachineExtension{
		Location: new(location),
		Properties: &armcompute.VirtualMachineExtensionProperties{
			Publisher: new("Microsoft.Compute"), Type: new(scriptExtension), TypeHandlerVersion: new("1.10"),
			Settings:          map[string]any{"commandToExecute": command},
			ProtectedSettings: map[string]any{"storageAccountKey": "secret"},
		},
	}
}

// sendScript sends the extension without waiting on its poller, as a client that checks it later does.
func (c *clients) sendScript(t *testing.T, vm, command string) {
	t.Helper()
	if _, err := c.extensions.BeginCreateOrUpdate(t.Context(), group, vm, scriptExtension, scriptBody(command), nil); err != nil {
		t.Fatal(err)
	}
}

func TestVMExtensions(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	vm := c.createWindowsVM(t, "win")
	if _, err := c.extensions.Get(t.Context(), group, "win", scriptExtension, nil); code(err) != "ResourceNotFound" || status(err) != http.StatusNotFound {
		t.Fatalf("get before the extension is sent = %v", err)
	}
	c.sendScript(t, "win", "powershell -c hostname")
	got, err := c.extensions.Get(t.Context(), group, "WIN", strings.ToLower(scriptExtension), nil)
	if err != nil {
		t.Fatal(err)
	}
	props := got.Properties
	if *got.ID != *vm.ID+"/extensions/"+scriptExtension || *got.Name != scriptExtension || *got.Type != "Microsoft.Compute/virtualMachines/extensions" ||
		*got.Location != location || *props.ProvisioningState != "Succeeded" || *props.Publisher != "Microsoft.Compute" ||
		*props.Type != scriptExtension || *props.TypeHandlerVersion != "1.10" || props.ProtectedSettings != nil ||
		props.Settings.(map[string]any)["commandToExecute"] != "powershell -c hostname" {
		t.Fatalf("extension = %+v", got)
	}
	if c.polls() != 0 {
		t.Errorf("polled %d times, want 0", c.polls())
	}

	p, err := c.extensions.BeginCreateOrUpdate(t.Context(), group, "win", scriptExtension, scriptBody("whoami"), nil)
	if res, err := wait(t, p, err); err != nil || *res.Properties.ProvisioningState != "Succeeded" {
		t.Fatalf("resend = %+v, %v", res, err)
	}
	exts := c.server.VMExtensions()
	var settings, protected map[string]string
	if len(exts) != 1 || json.Unmarshal(exts[0].Settings, &settings) != nil || json.Unmarshal(exts[0].ProtectedSettings, &protected) != nil {
		t.Fatalf("VMExtensions = %+v", exts)
	}
	if e := exts[0]; e.VirtualMachine != *vm.ID || e.Name != scriptExtension || e.Type != scriptExtension || e.TypeHandlerVersion != "1.10" ||
		settings["commandToExecute"] != "whoami" || protected["storageAccountKey"] != "secret" {
		t.Fatalf("VMExtensions = %+v, settings %v", exts, settings)
	}

	page, err := c.extensions.List(t.Context(), group, "win", nil)
	if err != nil || len(page.Value) != 1 || *page.Value[0].Name != scriptExtension {
		t.Fatalf("list = %+v, %v", page.Value, err)
	}
	d, err := c.extensions.BeginDelete(t.Context(), group, "win", scriptExtension, nil)
	if _, err = wait(t, d, err); err != nil {
		t.Fatal(err)
	}
	if _, err := c.extensions.Get(t.Context(), group, "win", scriptExtension, nil); code(err) != "ResourceNotFound" {
		t.Fatalf("get after delete = %v", err)
	}
	d, err = c.extensions.BeginDelete(t.Context(), group, "win", scriptExtension, nil)
	if _, err = wait(t, d, err); err != nil {
		t.Fatalf("deleting it again = %v", err)
	}
	if page, err := c.extensions.List(t.Context(), group, "win", nil); err != nil || len(page.Value) != 0 {
		t.Fatalf("list after delete = %+v, %v", page.Value, err)
	}
}

func TestVMExtensionsWithSettle(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(1))
	c.createGroup(t, group, nil)
	c.createWindowsVM(t, "win")
	c.sendScript(t, "win", "hostname")
	if got, err := c.extensions.Get(t.Context(), group, "win", scriptExtension, nil); err != nil || *got.Properties.ProvisioningState != "Creating" {
		t.Fatalf("extension in progress = %+v, %v", got.Properties, err)
	}
	if got, err := c.extensions.Get(t.Context(), group, "win", scriptExtension, nil); err != nil || *got.Properties.ProvisioningState != "Succeeded" {
		t.Fatalf("extension done = %+v, %v", got.Properties, err)
	}
	p, err := c.extensions.BeginCreateOrUpdate(t.Context(), group, "win", scriptExtension, scriptBody("whoami"), nil)
	if err != nil || p.Done() {
		t.Fatalf("update = done %v, %v", p.Done(), err)
	}
	if got := c.server.VMExtensions(); got[0].ProvisioningState != "Updating" {
		t.Fatalf("extension being updated = %+v", got)
	}
	if _, err := p.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	d, err := c.extensions.BeginDelete(t.Context(), group, "win", scriptExtension, nil)
	if err != nil || d.Done() {
		t.Fatalf("delete = done %v, %v", d.Done(), err)
	}
	if got, err := c.extensions.Get(t.Context(), group, "win", scriptExtension, nil); err != nil || *got.Properties.ProvisioningState != "Deleting" {
		t.Fatalf("extension being deleted = %+v, %v", got.Properties, err)
	}
	if _, err := d.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if got := c.server.VMExtensions(); len(got) != 0 {
		t.Fatalf("VMExtensions = %+v", got)
	}
}

func TestVMExtensionsOfAMissingVM(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	_, err := c.extensions.BeginCreateOrUpdate(t.Context(), group, "nope", scriptExtension, scriptBody("hostname"), nil)
	if code(err) != "ParentResourceNotFound" || status(err) != http.StatusNotFound || !strings.Contains(err.Error(), "Parent resource 'nope' not found") {
		t.Fatalf("create = %v", err)
	}
	if _, err := c.extensions.Get(t.Context(), group, "nope", scriptExtension, nil); code(err) != "ResourceNotFound" || status(err) != http.StatusNotFound {
		t.Fatalf("get = %v", err)
	}
	if _, err := c.extensions.List(t.Context(), group, "nope", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("list = %v", err)
	}
	if _, err := c.extensions.Get(t.Context(), "other", "nope", scriptExtension, nil); code(err) != "ResourceGroupNotFound" {
		t.Fatalf("get in a missing group = %v", err)
	}
	if got := c.server.VMExtensions(); len(got) != 0 {
		t.Fatalf("VMExtensions = %+v", got)
	}
}

func TestDeletingAVMDeletesItsExtensions(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.createWindowsVM(t, "win")
	c.createWindowsVM(t, "keep")
	c.sendScript(t, "win", "hostname")
	c.sendScript(t, "keep", "hostname")
	d, err := c.vms.BeginDelete(t.Context(), group, "win", nil)
	if _, err = wait(t, d, err); err != nil {
		t.Fatal(err)
	}
	if got := c.server.VMExtensions(); len(got) != 1 || !strings.HasSuffix(got[0].VirtualMachine, "/keep") {
		t.Fatalf("VMExtensions = %+v", got)
	}
	c.createWindowsVM(t, "win")
	if _, err := c.extensions.Get(t.Context(), group, "win", scriptExtension, nil); code(err) != "ResourceNotFound" {
		t.Fatalf("get on a new VM with the old name = %v", err)
	}
	g, err := c.groups.BeginDelete(t.Context(), group, nil)
	if _, err = wait(t, g, err); err != nil {
		t.Fatal(err)
	}
	if got := c.server.VMExtensions(); len(got) != 0 {
		t.Fatalf("VMExtensions after deleting the group = %+v", got)
	}
}

func TestPolicyChecksVMExtensionActions(t *testing.T) {
	policy := access.Allow(
		"Microsoft.Resources/subscriptions/resourceGroups/write",
		"Microsoft.Network/networkInterfaces/write",
		"Microsoft.Network/virtualNetworks/subnets/join/action",
		"Microsoft.Network/networkInterfaces/join/action",
		"Microsoft.Compute/virtualMachines/write",
		"Microsoft.Compute/virtualMachines/extensions/read",
	)
	c := setup(t, arm.WithPolicy(policy))
	c.createGroup(t, group, nil)
	c.createWindowsVM(t, "win")
	_, err := c.extensions.BeginCreateOrUpdate(t.Context(), group, "win", scriptExtension, scriptBody("hostname"), nil)
	if code(err) != "AuthorizationFailed" || status(err) != http.StatusForbidden ||
		!strings.Contains(err.Error(), "'Microsoft.Compute/virtualMachines/extensions/write'") {
		t.Fatalf("create without extensions/write = %v", err)
	}
	if _, err := c.extensions.Get(t.Context(), group, "win", scriptExtension, nil); code(err) != "ResourceNotFound" {
		t.Fatalf("get with extensions/read = %v", err)
	}
	_, err = c.extensions.BeginDelete(t.Context(), group, "win", scriptExtension, nil)
	if code(err) != "AuthorizationFailed" || !strings.Contains(err.Error(), "'Microsoft.Compute/virtualMachines/extensions/delete'") {
		t.Fatalf("delete without extensions/delete = %v", err)
	}
	if got := c.server.VMExtensions(); len(got) != 0 {
		t.Fatalf("a refused create took effect: %+v", got)
	}
}
