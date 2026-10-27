package arm_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/costmanagement/armcostmanagement/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
)

const (
	exportsNamespace = "Microsoft.CostManagementExports"
	subscriptionID   = "/subscriptions/" + subscription
	exportName       = "daily-usage"
)

var exportFrom = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func exportBody(account string) armcostmanagement.Export {
	return armcostmanagement.Export{Properties: &armcostmanagement.ExportProperties{
		Definition: &armcostmanagement.ExportDefinition{
			Type: new(armcostmanagement.ExportTypeUsage), Timeframe: new(armcostmanagement.TimeframeTypeMonthToDate),
		},
		DeliveryInfo: &armcostmanagement.ExportDeliveryInfo{Destination: &armcostmanagement.ExportDeliveryDestination{
			ResourceID: new(account), Container: new("exports"), RootFolderPath: new("usage"),
		}},
		Format: new(armcostmanagement.FormatTypeCSV),
		Schedule: &armcostmanagement.ExportSchedule{
			Recurrence: new(armcostmanagement.RecurrenceTypeDaily),
			RecurrencePeriod: &armcostmanagement.ExportRecurrencePeriod{
				From: new(exportFrom), To: new(exportFrom.AddDate(20, 0, 0)),
			},
			Status: new(armcostmanagement.StatusTypeActive),
		},
	}}
}

// ensureRegistered registers the namespace unless it is, without waiting for the registration to finish.
func (c *clients) ensureRegistered(t *testing.T, namespace string) {
	t.Helper()
	got, err := c.providers.Get(t.Context(), namespace, nil)
	if err != nil {
		t.Fatal(err)
	}
	if *got.RegistrationState == "Registered" {
		return
	}
	if _, err := c.providers.Register(t.Context(), namespace, nil); err != nil {
		t.Fatal(err)
	}
}

// putExport creates the export, or updates it with the eTag a get returns.
func (c *clients) putExport(t *testing.T, export armcostmanagement.Export) (armcostmanagement.Export, error) {
	t.Helper()
	got, err := c.exports.Get(t.Context(), subscriptionID, exportName, nil)
	switch {
	case status(err) == http.StatusNotFound:
	case err != nil:
		t.Fatal(err)
	default:
		export.ETag = got.ETag
	}
	res, err := c.exports.CreateOrUpdate(t.Context(), subscriptionID, exportName, export, nil)
	return res.Export, err
}

func (c *clients) exportDestination(t *testing.T) string {
	t.Helper()
	c.createGroup(t, group, nil)
	account := c.createAccount(t, group, "costexports")
	if _, err := c.containers.Create(t.Context(), group, "costexports", "exports", armstorage.BlobContainer{}, nil); err != nil {
		t.Fatal(err)
	}
	return *account.ID
}

func TestCostExportFlow(t *testing.T) {
	c := setup(t, arm.WithUnregisteredProviders(exportsNamespace))
	c.ensureRegistered(t, exportsNamespace)
	if got := c.server.ProviderState(subscription, exportsNamespace); got != "Registered" {
		t.Fatalf("ProviderState = %s, want Registered with WithSettleAfter(0)", got)
	}
	account := c.exportDestination(t)

	created, err := c.putExport(t, exportBody(account))
	if err != nil {
		t.Fatal(err)
	}
	if *created.ID != subscriptionID+"/providers/Microsoft.CostManagement/exports/"+exportName || *created.Name != exportName ||
		*created.Type != "Microsoft.CostManagement/exports" || created.ETag == nil ||
		*created.Properties.DeliveryInfo.Destination.ResourceID != account || !created.Properties.Schedule.RecurrencePeriod.From.Equal(exportFrom) {
		t.Fatalf("create = %+v", created)
	}
	updated, err := c.putExport(t, exportBody(account))
	if err != nil {
		t.Fatal(err)
	}
	if *updated.ETag == *created.ETag {
		t.Errorf("update kept eTag %s", *updated.ETag)
	}
	want := arm.Export{
		ID: *created.ID, Scope: subscriptionID, Name: exportName, DefinitionType: "Usage", Timeframe: "MonthToDate",
		Destination: account, Container: "exports", RootFolderPath: "usage", Format: "Csv",
		Recurrence: "Daily", RecurrenceFrom: exportFrom, RecurrenceTo: exportFrom.AddDate(20, 0, 0), Status: "Active", ETag: string(*updated.ETag),
	}
	if got := c.server.Exports(); len(got) != 1 || got[0] != want {
		t.Fatalf("Exports() = %+v, want %+v", got, want)
	}

	if _, err := c.exports.Delete(t.Context(), subscriptionID, exportName, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.exports.Delete(t.Context(), subscriptionID, exportName, nil); status(err) != http.StatusNotFound || code(err) != "404" {
		t.Fatalf("second delete = %v, want 404", err)
	}
	p, err := c.groups.BeginDelete(t.Context(), group, nil)
	if _, err := wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	regs := c.server.ProviderRegistrations()
	if i := slices.IndexFunc(regs, func(r arm.ProviderRegistration) bool { return r.Namespace == exportsNamespace }); i < 0 || regs[i].Registrations != 1 {
		t.Errorf("ProviderRegistrations() = %+v, want one register of %s", regs, exportsNamespace)
	}
}

func TestCostExportOutlivesItsDestinationGroup(t *testing.T) {
	c := setup(t)
	account := c.exportDestination(t)
	if _, err := c.putExport(t, exportBody(account)); err != nil {
		t.Fatal(err)
	}
	p, err := c.groups.BeginDelete(t.Context(), group, nil)
	if _, err := wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	if got, err := c.exports.Get(t.Context(), subscriptionID, exportName, nil); err != nil || *got.Properties.DeliveryInfo.Destination.ResourceID != account {
		t.Fatalf("get after deleting the group = %v %+v", err, got)
	}
}

func TestCostExportUpdateNeedsTheCurrentETag(t *testing.T) {
	c := setup(t)
	account := "/subscriptions/" + subscription + "/resourceGroups/elsewhere/providers/Microsoft.Storage/storageAccounts/unchecked"
	created, err := c.exports.CreateOrUpdate(t.Context(), subscriptionID, exportName, exportBody(account), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.exports.CreateOrUpdate(t.Context(), subscriptionID, exportName, exportBody(account), nil)
	if status(err) != http.StatusBadRequest || code(err) != "400" || !strings.Contains(err.Error(), "Etag value required for update operation of report: "+exportName) {
		t.Fatalf("update without an eTag = %v", err)
	}
	update := exportBody(account)
	update.ETag = created.ETag
	if _, err := c.exports.CreateOrUpdate(t.Context(), subscriptionID, exportName, update, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.exports.CreateOrUpdate(t.Context(), subscriptionID, exportName, update, nil); status(err) != http.StatusPreconditionFailed {
		t.Fatalf("update with a stale eTag = %v, want 412", err)
	}
}

func TestProviderRegistrationSettles(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(1), arm.WithUnregisteredProviders(exportsNamespace))
	got, err := c.providers.Get(t.Context(), exportsNamespace, nil)
	if err != nil || *got.RegistrationState != "NotRegistered" {
		t.Fatalf("get = %v %+v", err, got)
	}
	registered, err := c.providers.Register(t.Context(), exportsNamespace, nil)
	if err != nil || *registered.RegistrationState != "Registering" {
		t.Fatalf("register = %v %+v", err, registered)
	}
	if _, err := c.exports.CreateOrUpdate(t.Context(), subscriptionID, exportName, exportBody("unchecked"), nil); status(err) != http.StatusServiceUnavailable {
		t.Fatalf("create while registering = %v, want 503", err)
	}
	if got, err := c.providers.Get(t.Context(), exportsNamespace, nil); err != nil || *got.RegistrationState != "Registered" {
		t.Fatalf("get after the refused create = %v %+v, want Registered", err, got)
	}
	if _, err := c.exports.CreateOrUpdate(t.Context(), subscriptionID, exportName, exportBody("unchecked"), nil); err != nil {
		t.Fatal(err)
	}

	c = setup(t, arm.WithSettleAfter(-1), arm.WithUnregisteredProviders(exportsNamespace))
	c.ensureRegistered(t, exportsNamespace)
	if got := c.server.ProviderState(subscription, exportsNamespace); got != "Registering" {
		t.Fatalf("ProviderState = %s, want Registering until Settle", got)
	}
	c.server.Settle()
	if got := c.server.ProviderState(subscription, exportsNamespace); got != "Registered" {
		t.Fatalf("ProviderState after Settle = %s", got)
	}
}

func TestCostExportPermissions(t *testing.T) {
	policy := access.Allow("Microsoft.Resources/subscriptions/providers/read", "Microsoft.CostManagement/exports/read")
	c := setup(t, arm.WithPolicy(policy), arm.WithUnregisteredProviders(exportsNamespace))
	_, err := c.providers.Register(t.Context(), exportsNamespace, nil)
	if code(err) != "AuthorizationFailed" || !strings.Contains(err.Error(), "'"+exportsNamespace+"/register/action'") {
		t.Fatalf("register = %v", err)
	}
	if _, err := c.exports.Get(t.Context(), subscriptionID, exportName, nil); code(err) != "404" {
		t.Fatalf("get = %v, want 404", err)
	}
	_, err = c.exports.CreateOrUpdate(t.Context(), subscriptionID, exportName, exportBody("unchecked"), nil)
	if code(err) != "AuthorizationFailed" || !strings.Contains(err.Error(), "'Microsoft.CostManagement/exports/write'") {
		t.Fatalf("create = %v", err)
	}
	_, err = c.exports.Delete(t.Context(), subscriptionID, exportName, nil)
	if code(err) != "AuthorizationFailed" || !strings.Contains(err.Error(), "'Microsoft.CostManagement/exports/delete'") {
		t.Fatalf("delete = %v", err)
	}
}
