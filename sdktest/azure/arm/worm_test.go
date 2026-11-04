package arm_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"
)

func TestLegalHoldProtectsBlobsAndTheAccount(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.storageWith(t, "held1", nil)
	client := c.blobs(t, "held1")
	upload(t, client, "a", "x")
	hold, err := c.containers.SetLegalHold(t.Context(), group, "held1", "data", armstorage.LegalHold{Tags: []*string{new("Case1")}}, nil)
	if err != nil || !*hold.HasLegalHold || *hold.Tags[0] != "case1" {
		t.Fatalf("set legal hold = %+v, %v", hold, err)
	}
	got, err := c.containers.Get(t.Context(), group, "held1", "data", nil)
	if err != nil || !*got.ContainerProperties.HasLegalHold || *got.ContainerProperties.LegalHold.Tags[0].Tag != "case1" {
		t.Fatalf("container = %+v, %v", got, err)
	}

	upload(t, client, "new", "a new blob is allowed")
	if _, err := client.UploadBuffer(t.Context(), "data", "a", []byte("y"), nil); code(err) != "BlobImmutableDueToLegalHold" || status(err) != http.StatusConflict {
		t.Fatalf("overwriting a held blob = %v", err)
	}
	if _, err := client.DeleteBlob(t.Context(), "data", "a", nil); code(err) != "BlobImmutableDueToLegalHold" {
		t.Fatalf("deleting a held blob = %v", err)
	}
	if _, err := c.containers.Delete(t.Context(), group, "held1", "data", nil); code(err) != "ContainerHasLegalHold" {
		t.Fatalf("deleting a held container that holds blobs = %v", err)
	}
	if _, err := c.accounts.Delete(t.Context(), group, "held1", nil); code(err) != "AccountProtectedFromDeletion" || status(err) != http.StatusConflict ||
		!strings.Contains(err.Error(), "The container(s) data have a legal hold.") {
		t.Fatalf("deleting the account = %v", err)
	}
	gp, err := c.groups.BeginDelete(t.Context(), group, nil)
	if _, err = wait(t, gp, err); code(err) != "ResourceGroupDeletionBlocked" || !strings.Contains(err.Error(), "AccountProtectedFromDeletion") ||
		strings.Contains(err.Error(), "blobServices") {
		t.Fatalf("deleting the group = %v", err)
	}
	if left := c.server.Blobs(); len(left) != 2 {
		t.Fatalf("blobs after the blocked deletions = %+v", left)
	}

	if _, err := c.containers.ClearLegalHold(t.Context(), group, "held1", "data", armstorage.LegalHold{Tags: []*string{new("case1")}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.accounts.Delete(t.Context(), group, "held1", nil); err != nil {
		t.Fatalf("deleting the account once the hold is cleared = %v", err)
	}
}

func TestLockedPolicyProtectsTheAccount(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.storageWith(t, "worm1", nil)
	client := c.blobs(t, "worm1")
	upload(t, client, "a", "x")
	policy := func(days int32) *armstorage.BlobContainersClientCreateOrUpdateImmutabilityPolicyOptions {
		return &armstorage.BlobContainersClientCreateOrUpdateImmutabilityPolicyOptions{Parameters: &armstorage.ImmutabilityPolicy{
			Properties: &armstorage.ImmutabilityPolicyProperty{ImmutabilityPeriodSinceCreationInDays: new(days)},
		}}
	}
	made, err := c.containers.CreateOrUpdateImmutabilityPolicy(t.Context(), group, "worm1", "data", policy(1))
	if err != nil || *made.Properties.State != armstorage.ImmutabilityPolicyStateUnlocked || made.Etag == nil {
		t.Fatalf("policy = %+v, %v", made, err)
	}
	if _, err := client.DeleteBlob(t.Context(), "data", "a", nil); code(err) != "BlobImmutableDueToPolicy" {
		t.Fatalf("deleting a blob the policy keeps = %v", err)
	}
	if _, err := c.containers.LockImmutabilityPolicy(t.Context(), group, "worm1", "data", `"stale"`, nil); status(err) != http.StatusPreconditionFailed {
		t.Fatalf("locking with a stale ETag = %v", err)
	}
	locked, err := c.containers.LockImmutabilityPolicy(t.Context(), group, "worm1", "data", *made.Etag, nil)
	if err != nil || *locked.Properties.State != armstorage.ImmutabilityPolicyStateLocked {
		t.Fatalf("lock = %+v, %v", locked, err)
	}
	if _, err := c.containers.CreateOrUpdateImmutabilityPolicy(t.Context(), group, "worm1", "data", policy(0)); code(err) != "InvalidParameter" {
		t.Fatalf("a zero-day policy = %v", err)
	}
	if _, err := c.containers.CreateOrUpdateImmutabilityPolicy(t.Context(), group, "worm1", "data", policy(2)); code(err) != "ContainerImmutabilityPolicyLocked" {
		t.Fatalf("changing a locked policy = %v", err)
	}
	got, err := c.containers.Get(t.Context(), group, "worm1", "data", nil)
	if err != nil || !*got.ContainerProperties.HasImmutabilityPolicy ||
		*got.ContainerProperties.ImmutabilityPolicy.Properties.State != armstorage.ImmutabilityPolicyStateLocked ||
		len(got.ContainerProperties.ImmutabilityPolicy.UpdateHistory) != 2 {
		t.Fatalf("container = %+v, %v", got, err)
	}
	if _, err := client.DeleteContainer(t.Context(), "data", nil); code(err) != "ContainerImmutabilityPolicyLocked" {
		t.Fatalf("deleting the container through the Blob service = %v", err)
	}
	if _, err := c.accounts.Delete(t.Context(), group, "worm1", nil); code(err) != "AccountProtectedFromDeletion" ||
		!strings.Contains(err.Error(), "worm1 is protected from deletion.The container(s) data have a locked immutability policy.") {
		t.Fatalf("deleting the account = %v", err)
	}
}

func TestContainersSoftDeletedThroughARM(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.storageWith(t, "keep1", &armstorage.BlobServicePropertiesProperties{
		ContainerDeleteRetentionPolicy: &armstorage.DeleteRetentionPolicy{Enabled: new(true), Days: new(int32(5))},
	})
	upload(t, c.blobs(t, "keep1"), "a", "x")
	if _, err := c.containers.Create(t.Context(), group, "keep1", "other", armstorage.BlobContainer{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.containers.Delete(t.Context(), group, "keep1", "data", nil); err != nil {
		t.Fatal(err)
	}
	list := func(opts *armstorage.BlobContainersClientListOptions) []*armstorage.ListContainerItem {
		var out []*armstorage.ListContainerItem
		pager := c.containers.NewListPager(group, "keep1", opts)
		for pager.More() {
			page, err := pager.NextPage(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, page.Value...)
		}
		return out
	}
	if got := list(nil); len(got) != 1 || *got[0].Name != "other" {
		t.Fatalf("containers = %+v", got)
	}
	all := list(&armstorage.BlobContainersClientListOptions{Include: new(armstorage.ListContainersIncludeDeleted)})
	if len(all) != 2 || *all[0].Name != "data" || !*all[0].Properties.Deleted || *all[0].Properties.RemainingRetentionDays != 5 ||
		all[0].Properties.DeletedTime == nil || *all[0].Properties.Version == "" || *all[1].Properties.Deleted {
		t.Fatalf("containers with deleted = %s", dump(all))
	}
	if got := list(&armstorage.BlobContainersClientListOptions{Include: new(armstorage.ListContainersIncludeDeleted), Filter: new("oth")}); len(got) != 1 {
		t.Fatalf("filtered = %s", dump(got))
	}
}

func dump(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestQueuesAndTables(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.createAccount(t, group, "msgs1")
	if _, err := c.queueSvc.SetServiceProperties(t.Context(), group, "msgs1", armstorage.QueueServiceProperties{
		QueueServiceProperties: &armstorage.QueueServicePropertiesProperties{Cors: &armstorage.CorsRules{}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"jobs", "events"} {
		q, err := c.queues.Create(t.Context(), group, "msgs1", name, armstorage.Queue{QueueProperties: &armstorage.QueueProperties{
			Metadata: map[string]*string{"owner": new("tests")},
		}}, nil)
		if err != nil || *q.Name != name {
			t.Fatalf("queue = %+v, %v", q, err)
		}
		tb, err := c.tables.Create(t.Context(), group, "msgs1", name+"Table", nil)
		if err != nil || *tb.TableProperties.TableName != name+"Table" {
			t.Fatalf("table = %+v, %v", tb, err)
		}
	}
	if _, err := c.queues.Create(t.Context(), group, "msgs1", "jobs", armstorage.Queue{}, nil); err != nil {
		t.Fatalf("creating a queue again = %v", err)
	}
	var queues, tables []string
	for pager := c.queues.NewListPager(group, "msgs1", nil); pager.More(); {
		page, err := pager.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range page.Value {
			queues = append(queues, *q.Name)
		}
	}
	for pager := c.tables.NewListPager(group, "msgs1", nil); pager.More(); {
		page, err := pager.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, tb := range page.Value {
			tables = append(tables, *tb.Name)
		}
	}
	if !slices.Equal(queues, []string{"events", "jobs"}) || !slices.Equal(tables, []string{"eventsTable", "jobsTable"}) {
		t.Fatalf("queues %v, tables %v", queues, tables)
	}
	if got, err := c.queues.Get(t.Context(), group, "msgs1", "jobs", nil); err != nil || *got.QueueProperties.ApproximateMessageCount != 0 {
		t.Fatalf("get queue = %+v, %v", got, err)
	}
	if _, err := c.queues.Delete(t.Context(), group, "msgs1", "jobs", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.queues.Get(t.Context(), group, "msgs1", "jobs", nil); code(err) != "QueueNotFound" {
		t.Fatalf("get deleted queue = %v", err)
	}
	if _, err := c.tables.Delete(t.Context(), group, "msgs1", "jobsTable", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.tables.Get(t.Context(), group, "msgs1", "jobsTable", nil); code(err) != "TableNotFound" {
		t.Fatalf("get deleted table = %v", err)
	}
}

func TestShareUsage(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.createAccount(t, group, "files1")
	share, err := c.shares.Create(t.Context(), group, "files1", "home", armstorage.FileShare{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.server.SetShareUsage(*share.ID, 1234) {
		t.Fatal("SetShareUsage found no share")
	}
	got, err := c.shares.Get(t.Context(), group, "files1", "home", &armstorage.FileSharesClientGetOptions{Expand: new("stats")})
	if err != nil || *got.FileShareProperties.ShareUsageBytes != 1234 {
		t.Fatalf("share with stats = %+v, %v", got, err)
	}
	if got, err := c.shares.Get(t.Context(), group, "files1", "home", nil); err != nil || got.FileShareProperties.ShareUsageBytes != nil {
		t.Fatalf("share without stats = %+v, %v", got, err)
	}
}

func TestVersionLevelImmutability(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	params := accountParams()
	params.Properties = &armstorage.AccountPropertiesCreateParameters{
		ImmutableStorageWithVersioning: &armstorage.ImmutableStorageAccount{Enabled: new(true)},
	}
	p, err := c.accounts.BeginCreate(t.Context(), group, "vlw1", params, nil)
	if _, err := wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	svc, err := c.blob.GetServiceProperties(t.Context(), group, "vlw1", nil)
	if err != nil || svc.BlobServiceProperties.BlobServiceProperties.IsVersioningEnabled == nil || !*svc.BlobServiceProperties.BlobServiceProperties.IsVersioningEnabled {
		t.Fatalf("blob service of a version-level immutable account = %+v, %v", svc, err)
	}
	if _, err := c.containers.Create(t.Context(), group, "vlw1", "data", armstorage.BlobContainer{}, nil); err != nil {
		t.Fatal(err)
	}
	client := c.blobs(t, "vlw1")
	made := upload(t, client, "a", "x")
	if _, err := client.DeleteBlob(t.Context(), "data", "a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DeleteContainer(t.Context(), "data", nil); code(err) != "ContainerImmutableStorageWithVersioningEnabled" || status(err) != http.StatusConflict {
		t.Fatalf("deleting the container through the Blob service = %v", err)
	}
	if _, err := c.containers.Delete(t.Context(), group, "vlw1", "data", nil); code(err) != "ContainerImmutableStorageWithVersioningEnabled" {
		t.Fatalf("deleting the container through ARM while it keeps a version = %v", err)
	}
	if _, err := c.accounts.Delete(t.Context(), group, "vlw1", nil); code(err) != "AccountProtectedFromDeletion" {
		t.Fatalf("deleting the account = %v", err)
	}
	params.Properties.ImmutableStorageWithVersioning.Enabled = new(false)
	p, err = c.accounts.BeginCreate(t.Context(), group, "vlw1", params, nil)
	if _, err := wait(t, p, err); code(err) != "PropertyIsImmutable" || status(err) != http.StatusBadRequest {
		t.Fatalf("disabling version-level immutability = %v", err)
	}

	old, err := client.ServiceClient().NewContainerClient("data").NewBlobClient("a").WithVersionID(*made.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Delete(t.Context(), nil); err != nil {
		t.Fatalf("deleting the previous version = %v", err)
	}
	if _, err := c.containers.Delete(t.Context(), group, "vlw1", "data", nil); err != nil {
		t.Fatalf("deleting the empty container through ARM = %v", err)
	}
	if _, err := c.accounts.Delete(t.Context(), group, "vlw1", nil); err != nil {
		t.Fatalf("deleting the account once it has no container = %v", err)
	}
}
