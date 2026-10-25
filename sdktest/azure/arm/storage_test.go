package arm_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
	"github.com/parallelworks/squall/fault"
)

func accountParams() armstorage.AccountCreateParameters {
	return armstorage.AccountCreateParameters{
		SKU:      &armstorage.SKU{Name: new(armstorage.SKUNameStandardLRS)},
		Kind:     new(armstorage.KindStorageV2),
		Location: new(location),
		Tags:     map[string]*string{"session-id": new("s-1")},
	}
}

func (c *clients) createAccount(t *testing.T, group, name string) armstorage.Account {
	t.Helper()
	p, err := c.accounts.BeginCreate(t.Context(), group, name, accountParams(), nil)
	res, err := wait(t, p, err)
	if err != nil {
		t.Fatal(err)
	}
	return res.Account
}

func (c *clients) nameAvailable(t *testing.T, name string) armstorage.CheckNameAvailabilityResult {
	t.Helper()
	res, err := c.accounts.CheckNameAvailability(t.Context(), armstorage.AccountCheckNameAvailabilityParameters{
		Name: new(name), Type: new("Microsoft.Storage/storageAccounts"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.CheckNameAvailabilityResult
}

func TestStorageAccounts(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	if got := c.nameAvailable(t, "bucket1"); !*got.NameAvailable {
		t.Fatalf("a free name = %+v", got)
	}
	if got := c.nameAvailable(t, "Bucket_1"); *got.NameAvailable || *got.Reason != armstorage.ReasonAccountNameInvalid {
		t.Fatalf("an invalid name = %+v", got)
	}
	if _, err := c.accounts.GetProperties(t.Context(), group, "bucket1", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("get of a missing account = %v", err)
	}
	acct := c.createAccount(t, group, "bucket1")
	if *acct.Name != "bucket1" || *acct.Properties.ProvisioningState != armstorage.ProvisioningStateSucceeded ||
		*acct.Properties.PrimaryEndpoints.Blob != "https://bucket1.blob.core.windows.net/" || *acct.SKU.Tier != armstorage.SKUTierStandard ||
		*acct.Kind != armstorage.KindStorageV2 || *acct.Tags["session-id"] != "s-1" {
		t.Fatalf("account = %+v", acct)
	}
	if got := c.nameAvailable(t, "bucket1"); *got.NameAvailable || *got.Reason != armstorage.ReasonAlreadyExists {
		t.Fatalf("a taken name = %+v", got)
	}
	c.createGroup(t, "rg-2", nil)
	p, err := c.accounts.BeginCreate(t.Context(), "rg-2", "bucket1", accountParams(), nil)
	if _, err = wait(t, p, err); code(err) != "StorageAccountInAnotherResourceGroup" || status(err) != http.StatusConflict {
		t.Fatalf("the name in another group = %v", err)
	}
	p, err = c.accounts.BeginCreate(t.Context(), group, "Bucket_1", accountParams(), nil)
	if _, err = wait(t, p, err); code(err) != "AccountNameInvalid" {
		t.Fatalf("an invalid name = %v", err)
	}
	keys, err := c.accounts.ListKeys(t.Context(), group, "bucket1", nil)
	if err != nil || len(keys.Keys) != 2 || *keys.Keys[0].KeyName != "key1" || *keys.Keys[0].Value == "" {
		t.Fatalf("keys = %+v, %v", keys, err)
	}

	cors := armstorage.BlobServiceProperties{BlobServiceProperties: &armstorage.BlobServicePropertiesProperties{
		Cors: &armstorage.CorsRules{CorsRules: []*armstorage.CorsRule{{
			AllowedHeaders: []*string{new("*")}, AllowedOrigins: []*string{new("*")}, ExposedHeaders: []*string{new("Content-Disposition")},
			AllowedMethods:  []*armstorage.CorsRuleAllowedMethodsItem{new(armstorage.CorsRuleAllowedMethodsItemGET)},
			MaxAgeInSeconds: new(int32(3600)),
		}}},
	}}
	if _, err := c.blob.SetServiceProperties(t.Context(), group, "bucket1", cors, nil); err != nil {
		t.Fatal(err)
	}
	versioning := armstorage.BlobServiceProperties{BlobServiceProperties: &armstorage.BlobServicePropertiesProperties{IsVersioningEnabled: new(true)}}
	if _, err := c.blob.SetServiceProperties(t.Context(), group, "bucket1", versioning, nil); err != nil {
		t.Fatal(err)
	}
	svc, err := c.blob.GetServiceProperties(t.Context(), group, "bucket1", nil)
	if err != nil || len(svc.BlobServiceProperties.BlobServiceProperties.Cors.CorsRules) != 1 ||
		!*svc.BlobServiceProperties.BlobServiceProperties.IsVersioningEnabled {
		t.Fatalf("blob service keeps what each set changed = %+v, %v", svc, err)
	}
	if _, err := c.blob.SetServiceProperties(t.Context(), group, "missing1", cors, nil); code(err) != "ParentResourceNotFound" {
		t.Fatalf("blob service of a missing account = %v", err)
	}

	if _, err := c.containers.Get(t.Context(), group, "bucket1", "data", nil); code(err) != "ContainerNotFound" || status(err) != http.StatusNotFound {
		t.Fatalf("get of a missing container = %v", err)
	}
	made, err := c.containers.Create(t.Context(), group, "bucket1", "data", armstorage.BlobContainer{}, nil)
	if err != nil || *made.Name != "data" || *made.ContainerProperties.PublicAccess != armstorage.PublicAccessNone || made.Etag == nil {
		t.Fatalf("container = %+v, %v", made, err)
	}
	if _, err := c.containers.Create(t.Context(), group, "bucket1", "data", armstorage.BlobContainer{}, nil); code(err) != "ContainerAlreadyExists" {
		t.Fatalf("creating a container twice = %v", err)
	}
	if got, err := c.containers.Get(t.Context(), group, "bucket1", "data", nil); err != nil || *got.ID != *acct.ID+"/blobServices/default/containers/data" {
		t.Fatalf("get container = %+v, %v", got, err)
	}

	gp, err := c.groups.BeginDelete(t.Context(), group, nil)
	if _, err = wait(t, gp, err); err != nil {
		t.Fatal(err)
	}
	if got := c.nameAvailable(t, "bucket1"); !*got.NameAvailable {
		t.Fatalf("the name once its group is deleted = %+v", got)
	}
	if left := c.server.Resources(); len(left) != 0 {
		t.Fatalf("left after deleting the group = %+v", left)
	}
}

func TestStorageAccountCreateIsPolledByItsLocation(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(1))
	c.createGroup(t, group, nil)
	p, err := c.accounts.BeginCreate(t.Context(), group, "bucket1", accountParams(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.accounts.GetProperties(t.Context(), group, "bucket1", nil)
	if err != nil || *got.Properties.ProvisioningState != armstorage.ProvisioningStateCreating {
		t.Fatalf("the account while it is created = %+v, %v", got, err)
	}
	res, err := p.PollUntilDone(t.Context(), nil)
	if err != nil || *res.Name != "bucket1" || *res.Properties.ProvisioningState != armstorage.ProvisioningStateSucceeded {
		t.Fatalf("the operation's result = %+v, %v", res, err)
	}
	if !slices.ContainsFunc(*c.requests, func(r string) bool {
		return strings.HasPrefix(r, "GET /subscriptions/"+subscription+"/providers/Microsoft.Storage/operations/")
	}) {
		t.Fatalf("requests = %v", *c.requests)
	}
}

func TestStorageAccountCreateThatFails(t *testing.T) {
	faults := fault.New().Add(fault.Rule{
		Method: "Microsoft.Storage/storageAccounts/write", Nth: 1,
		Fault: fault.Fault{Mode: fault.ModeFailOperation, Status: http.StatusConflict, Reason: "StorageAccountAlreadyTaken"},
	})
	c := setup(t, arm.WithFaults(faults))
	c.createGroup(t, group, nil)
	p, err := c.accounts.BeginCreate(t.Context(), group, "bucket1", accountParams(), nil)
	if _, err = wait(t, p, err); code(err) != "StorageAccountAlreadyTaken" {
		t.Fatalf("a failed create = %v", err)
	}
	if _, err := c.accounts.GetProperties(t.Context(), group, "bucket1", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("the account after a failed create = %v", err)
	}
}

func TestStorageOperationPollNeedsOperationsRead(t *testing.T) {
	policy := access.Allow("Microsoft.Resources/subscriptions/resourceGroups/write", "Microsoft.Storage/storageAccounts/write")
	c := setup(t, arm.WithSettleAfter(1), arm.WithPolicy(policy))
	c.createGroup(t, group, nil)
	p, err := c.accounts.BeginCreate(t.Context(), group, "bucket1", accountParams(), nil)
	if _, err = wait(t, p, err); code(err) != "AuthorizationFailed" || !strings.Contains(err.Error(), "Microsoft.Storage/operations/read") {
		t.Fatalf("polling without Microsoft.Storage/operations/read = %v", err)
	}
}
