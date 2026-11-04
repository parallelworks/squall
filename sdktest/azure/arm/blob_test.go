package arm_test

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/sdktest/internal/sdkhttp"
)

// blobs returns an azblob client for account that does not retry, so each call is one request.
func (c *clients) blobs(t *testing.T, account string) *azblob.Client {
	t.Helper()
	client, err := azblob.NewClient(c.blobURL+"/"+account, credential{}, &azblob.ClientOptions{
		Retry: policy.RetryOptions{MaxRetries: -1}, Transport: sdkhttp.CopyBodies(http.DefaultClient), InsecureAllowCredentialWithHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// storageWith makes a storage account with a container named data and sets its blob service properties to props.
func (c *clients) storageWith(t *testing.T, name string, props *armstorage.BlobServicePropertiesProperties) {
	t.Helper()
	c.createAccount(t, group, name)
	if props != nil {
		if _, err := c.blob.SetServiceProperties(t.Context(), group, name, armstorage.BlobServiceProperties{BlobServiceProperties: props}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.containers.Create(t.Context(), group, name, "data", armstorage.BlobContainer{}, nil); err != nil {
		t.Fatal(err)
	}
}

func upload(t *testing.T, client *azblob.Client, name, data string) azblob.UploadBufferResponse {
	t.Helper()
	res, err := client.UploadBuffer(t.Context(), "data", name, []byte(data), nil)
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	return res
}

// listed lists the container data through every page.
func listed(t *testing.T, client *azblob.Client, opts *azblob.ListBlobsFlatOptions) []*container.BlobItem {
	t.Helper()
	var out []*container.BlobItem
	pager := client.NewListBlobsFlatPager("data", opts)
	for pager.More() {
		page, err := pager.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, page.Segment.BlobItems...)
	}
	return out
}

// itemNames describes each listed item as its name and what it is, such as "a version current".
func itemNames(items []*container.BlobItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		s := *it.Name
		if it.Snapshot != nil {
			s += " snapshot"
		}
		if it.VersionID != nil {
			s += " version"
		}
		if it.IsCurrentVersion != nil && *it.IsCurrentVersion {
			s += " current"
		}
		if it.Deleted != nil && *it.Deleted {
			s += " deleted"
		}
		if it.HasVersionsOnly != nil && *it.HasVersionsOnly {
			s += " versions-only"
		}
		out = append(out, s)
	}
	return out
}

func TestBlobsListInPages(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.storageWith(t, "store1", nil)
	client := c.blobs(t, "store1")
	for _, name := range []string{"b/2", "a", "b/1", "c/x/y", "B"} {
		upload(t, client, name, "data-"+name)
	}
	items := listed(t, client, &azblob.ListBlobsFlatOptions{MaxResults: new(int32(2))})
	if got, want := itemNames(items), []string{"B", "a", "b/1", "b/2", "c/x/y"}; !slices.Equal(got, want) {
		t.Fatalf("listed in pages of 2 = %v, want %v", got, want)
	}
	p := items[1].Properties
	if *p.ContentLength != 6 || *p.BlobType != blob.BlobTypeBlockBlob || p.ETag == nil || p.LastModified == nil || p.CreationTime == nil ||
		len(p.ContentMD5) != 16 || *p.ContentType != "application/octet-stream" || *p.LeaseState != "available" {
		t.Fatalf("a's properties = %+v", p)
	}
	if got := itemNames(listed(t, client, &azblob.ListBlobsFlatOptions{Prefix: new("b/")})); !slices.Equal(got, []string{"b/1", "b/2"}) {
		t.Fatalf("prefix b/ = %v", got)
	}

	pager := client.ServiceClient().NewContainerClient("data").NewListBlobsHierarchyPager("/", &container.ListBlobsHierarchyOptions{MaxResults: new(int32(2))})
	var blobs, prefixes []string
	for pager.More() {
		page, err := pager.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range page.Segment.BlobItems {
			blobs = append(blobs, *b.Name)
		}
		for _, p := range page.Segment.BlobPrefixes {
			prefixes = append(prefixes, *p.Name)
		}
	}
	if !slices.Equal(blobs, []string{"B", "a"}) || !slices.Equal(prefixes, []string{"b/", "c/"}) {
		t.Fatalf("by / = blobs %v, prefixes %v", blobs, prefixes)
	}

	if _, err := client.UploadBuffer(t.Context(), "data", "meta", []byte("x"), &azblob.UploadBufferOptions{
		Metadata: map[string]*string{"owner": new("tests")},
	}); err != nil {
		t.Fatal(err)
	}
	withMeta := listed(t, client, &azblob.ListBlobsFlatOptions{Prefix: new("meta"), Include: azblob.ListBlobsInclude{Metadata: true}})
	if len(withMeta) != 1 || *withMeta[0].Metadata["owner"] != "tests" {
		t.Fatalf("with metadata = %+v", withMeta)
	}
}

func TestBlobsVersions(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.storageWith(t, "versioned1", &armstorage.BlobServicePropertiesProperties{IsVersioningEnabled: new(true)})
	client := c.blobs(t, "versioned1")
	first := upload(t, client, "a", "one")
	second := upload(t, client, "a", "two")
	if first.VersionID == nil || second.VersionID == nil || *first.VersionID >= *second.VersionID {
		t.Fatalf("version IDs = %v, %v", first.VersionID, second.VersionID)
	}
	if got := itemNames(listed(t, client, nil)); !slices.Equal(got, []string{"a version current"}) {
		t.Fatalf("listing = %v", got)
	}
	all := listed(t, client, &azblob.ListBlobsFlatOptions{Include: azblob.ListBlobsInclude{Versions: true}})
	if got := itemNames(all); !slices.Equal(got, []string{"a version", "a version current"}) || *all[0].VersionID != *first.VersionID {
		t.Fatalf("with versions = %v", got)
	}

	if _, err := client.DeleteBlob(t.Context(), "data", "a", nil); err != nil {
		t.Fatal(err)
	}
	if got := listed(t, client, nil); len(got) != 0 {
		t.Fatalf("listing after deleting a versioned blob = %v", itemNames(got))
	}
	if got := itemNames(listed(t, client, &azblob.ListBlobsFlatOptions{Include: azblob.ListBlobsInclude{Versions: true}})); !slices.Equal(got, []string{"a version", "a version"}) {
		t.Fatalf("versions after deleting = %v", got)
	}
	if got := itemNames(listed(t, client, &azblob.ListBlobsFlatOptions{Include: azblob.ListBlobsInclude{DeletedWithVersions: true}})); !slices.Equal(got, []string{"a deleted versions-only"}) {
		t.Fatalf("deleted with versions = %v", got)
	}

	old, err := client.ServiceClient().NewContainerClient("data").NewBlobClient("a").WithVersionID(*first.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	props, err := old.GetProperties(t.Context(), nil)
	if err != nil || *props.ContentLength != 3 || *props.VersionID != *first.VersionID || *props.IsCurrentVersion {
		t.Fatalf("an old version's properties = %+v, %v", props, err)
	}
	if _, err := old.Delete(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if got := itemNames(listed(t, client, &azblob.ListBlobsFlatOptions{Include: azblob.ListBlobsInclude{Versions: true}})); !slices.Equal(got, []string{"a version"}) {
		t.Fatalf("versions after deleting one without soft delete = %v", got)
	}
	if _, err := old.Delete(t.Context(), nil); code(err) != "BlobNotFound" {
		t.Fatalf("deleting a deleted version = %v", err)
	}
}

func TestBlobsSoftDelete(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.storageWith(t, "soft1", &armstorage.BlobServicePropertiesProperties{
		DeleteRetentionPolicy: &armstorage.DeleteRetentionPolicy{Enabled: new(true), Days: new(int32(7))},
	})
	client := c.blobs(t, "soft1")
	upload(t, client, "a", "one")
	upload(t, client, "a", "two")
	if got := itemNames(listed(t, client, &azblob.ListBlobsFlatOptions{Include: azblob.ListBlobsInclude{Snapshots: true, Deleted: true}})); !slices.Equal(got, []string{"a snapshot deleted", "a"}) {
		t.Fatalf("an overwrite keeps a soft-deleted snapshot = %v", got)
	}
	if got := itemNames(listed(t, client, &azblob.ListBlobsFlatOptions{Include: azblob.ListBlobsInclude{Snapshots: true}})); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("snapshots without deleted = %v", got)
	}

	blobClient := client.ServiceClient().NewContainerClient("data").NewBlobClient("a")
	snap, err := blobClient.CreateSnapshot(t.Context(), nil)
	if err != nil || snap.Snapshot == nil {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
	if _, err := client.DeleteBlob(t.Context(), "data", "a", nil); code(err) != "SnapshotsPresent" || status(err) != http.StatusConflict {
		t.Fatalf("deleting a blob with snapshots = %v", err)
	}
	if _, err := client.DeleteBlob(t.Context(), "data", "a", &azblob.DeleteBlobOptions{DeleteSnapshots: new(azblob.DeleteSnapshotsOptionTypeInclude)}); err != nil {
		t.Fatal(err)
	}
	if got := listed(t, client, &azblob.ListBlobsFlatOptions{Include: azblob.ListBlobsInclude{Snapshots: true}}); len(got) != 0 {
		t.Fatalf("listing after the delete = %v", itemNames(got))
	}
	deleted := listed(t, client, &azblob.ListBlobsFlatOptions{Include: azblob.ListBlobsInclude{Snapshots: true, Deleted: true}})
	if got := itemNames(deleted); !slices.Equal(got, []string{"a snapshot deleted", "a snapshot deleted", "a deleted"}) {
		t.Fatalf("soft-deleted = %v", got)
	}
	if p := deleted[2].Properties; p.DeletedTime == nil || *p.RemainingRetentionDays != 7 || p.LeaseState != nil {
		t.Fatalf("a soft-deleted blob's properties = %+v", p)
	}
	if _, err := blobClient.GetProperties(t.Context(), nil); code(err) != "BlobNotFound" || status(err) != http.StatusNotFound {
		t.Fatalf("properties of a soft-deleted blob = %v", err)
	}
	if left := c.server.Blobs(); len(left) != 3 || slices.ContainsFunc(left, func(b arm.Blob) bool { return !b.Deleted }) {
		t.Fatalf("Blobs = %+v", left)
	}
}

func TestBlobsUncommitted(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.storageWith(t, "blocks1", nil)
	client := c.blobs(t, "blocks1")
	bb := client.ServiceClient().NewContainerClient("data").NewBlockBlobClient("big")
	id := base64.StdEncoding.EncodeToString([]byte("block-0"))
	if _, err := bb.StageBlock(t.Context(), id, streaming("hello"), nil); err != nil {
		t.Fatal(err)
	}
	if got := listed(t, client, nil); len(got) != 0 {
		t.Fatalf("uncommitted listed by default = %v", itemNames(got))
	}
	if got := itemNames(listed(t, client, &azblob.ListBlobsFlatOptions{Include: azblob.ListBlobsInclude{UncommittedBlobs: true}})); !slices.Equal(got, []string{"big"}) {
		t.Fatalf("with uncommitted blobs = %v", got)
	}
	if _, err := bb.CommitBlockList(t.Context(), []string{base64.StdEncoding.EncodeToString([]byte("block-x"))}, nil); code(err) != "InvalidBlockList" {
		t.Fatalf("committing a block never staged = %v", err)
	}
	if _, err := bb.CommitBlockList(t.Context(), []string{id}, nil); err != nil {
		t.Fatal(err)
	}
	got := listed(t, client, nil)
	if len(got) != 1 || *got[0].Properties.ContentLength != 5 {
		t.Fatalf("committed = %v", itemNames(got))
	}
	stream, err := client.UploadStream(t.Context(), "data", "streamed", bytes.NewReader(make([]byte, 3<<20)), &azblob.UploadStreamOptions{BlockSize: 1 << 20})
	if err != nil || stream.ETag == nil {
		t.Fatalf("upload stream = %+v, %v", stream, err)
	}
	if got := listed(t, client, &azblob.ListBlobsFlatOptions{Prefix: new("streamed")}); len(got) != 1 || *got[0].Properties.ContentLength != 3<<20 {
		t.Fatalf("streamed = %v", itemNames(got))
	}
}

type readSeekCloser struct{ *bytes.Reader }

func (readSeekCloser) Close() error { return nil }

func streaming(s string) readSeekCloser { return readSeekCloser{bytes.NewReader([]byte(s))} }

func TestBlobsAreGoneWithTheirContainerOrAccount(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.storageWith(t, "gone1", nil)
	client := c.blobs(t, "gone1")
	if _, err := client.UploadBuffer(t.Context(), "missing", "a", []byte("x"), nil); code(err) != "ContainerNotFound" || status(err) != http.StatusNotFound {
		t.Fatalf("upload to a missing container = %v", err)
	}
	pager := client.NewListBlobsFlatPager("missing", nil)
	if _, err := pager.NextPage(t.Context()); code(err) != "ContainerNotFound" {
		t.Fatalf("list of a missing container = %v", err)
	}
	upload(t, client, "a", "x")
	if _, err := c.containers.Delete(t.Context(), group, "gone1", "data", nil); err != nil {
		t.Fatal(err)
	}
	if left := c.server.Blobs(); len(left) != 0 {
		t.Fatalf("blobs after deleting their container = %+v", left)
	}
	if _, err := c.containers.Create(t.Context(), group, "gone1", "data", armstorage.BlobContainer{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := listed(t, client, nil); len(got) != 0 {
		t.Fatalf("a new container of the same name = %v", itemNames(got))
	}
	upload(t, client, "b", "x")
	if _, err := c.accounts.Delete(t.Context(), group, "gone1", nil); err != nil {
		t.Fatal(err)
	}
	if left := c.server.Blobs(); len(left) != 0 {
		t.Fatalf("blobs after deleting their account = %+v", left)
	}
	pager = client.NewListBlobsFlatPager("data", nil)
	if _, err := pager.NextPage(t.Context()); code(err) != "ResourceNotFound" || status(err) != http.StatusNotFound {
		t.Fatalf("list in a deleted account = %v", err)
	}
	if _, err := c.blobs(t, "never1").CreateContainer(t.Context(), "data", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("a container in an account that never existed = %v", err)
	}
}

func TestBlobContainers(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.storageWith(t, "boxes1", &armstorage.BlobServicePropertiesProperties{
		ContainerDeleteRetentionPolicy: &armstorage.DeleteRetentionPolicy{Enabled: new(true), Days: new(int32(3))},
	})
	client := c.blobs(t, "boxes1")
	if _, err := client.CreateContainer(t.Context(), "logs", &azblob.CreateContainerOptions{Metadata: map[string]*string{"team": new("a")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateContainer(t.Context(), "logs", nil); code(err) != "ContainerAlreadyExists" || status(err) != http.StatusConflict {
		t.Fatalf("creating a container twice = %v", err)
	}
	if _, err := client.CreateContainer(t.Context(), "Bad_Name", nil); code(err) != "InvalidResourceName" {
		t.Fatalf("an invalid name = %v", err)
	}
	if got, err := c.containers.Get(t.Context(), group, "boxes1", "logs", nil); err != nil || *got.ContainerProperties.Metadata["team"] != "a" {
		t.Fatalf("the container through ARM = %+v, %v", got, err)
	}
	props, err := client.ServiceClient().NewContainerClient("logs").GetProperties(t.Context(), nil)
	if err != nil || props.ETag == nil || *props.Metadata["Team"] != "a" || *props.HasLegalHold {
		t.Fatalf("container properties = %+v, %v", props, err)
	}
	upload(t, client, "kept", "x")
	if _, err := client.DeleteContainer(t.Context(), "data", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DeleteContainer(t.Context(), "data", nil); code(err) != "ContainerNotFound" {
		t.Fatalf("deleting a deleted container = %v", err)
	}
	if _, err := c.containers.Get(t.Context(), group, "boxes1", "data", nil); code(err) != "ContainerNotFound" {
		t.Fatalf("a deleted container through ARM = %v", err)
	}

	list := func(opts *azblob.ListContainersOptions) []string {
		var out []string
		pager := client.NewListContainersPager(opts)
		for pager.More() {
			page, err := pager.NextPage(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, it := range page.ContainerItems {
				s := *it.Name
				if it.Deleted != nil && *it.Deleted {
					s += " deleted " + strings.Repeat("v", min(len(*it.Version), 1))
				}
				out = append(out, s)
			}
		}
		return out
	}
	if got := list(&azblob.ListContainersOptions{MaxResults: new(int32(1))}); !slices.Equal(got, []string{"logs"}) {
		t.Fatalf("containers = %v", got)
	}
	if got := list(&azblob.ListContainersOptions{Include: azblob.ListContainersInclude{Deleted: true}, MaxResults: new(int32(1))}); !slices.Equal(got, []string{"data deleted v", "logs"}) {
		t.Fatalf("containers with deleted = %v", got)
	}
}

func TestBlobPermissions(t *testing.T) {
	policy := access.Allow("Microsoft.Resources/*", "Microsoft.Storage/storageAccounts/write", "Microsoft.Storage/storageAccounts/blobServices/containers/write",
		"Microsoft.Storage/storageAccounts/blobServices/containers/blobs/read", "Microsoft.Storage/storageAccounts/blobServices/containers/blobs/add/action")
	c := setup(t, arm.WithPolicy(policy))
	c.createGroup(t, group, nil)
	c.storageWith(t, "locked1", nil)
	client := c.blobs(t, "locked1")
	upload(t, client, "a", "x")
	if _, err := client.UploadBuffer(t.Context(), "data", "a", []byte("y"), nil); code(err) != "AuthorizationPermissionMismatch" || status(err) != http.StatusForbidden {
		t.Fatalf("overwriting with add/action only = %v", err)
	}
	if got := listed(t, client, nil); len(got) != 1 {
		t.Fatalf("listing with blobs/read = %v", itemNames(got))
	}
	if _, err := client.DeleteBlob(t.Context(), "data", "a", nil); code(err) != "AuthorizationPermissionMismatch" {
		t.Fatalf("deleting without blobs/delete = %v", err)
	}
	denied := policy.Denials()
	if len(denied) != 2 || denied[0].Method != "PutBlob" || denied[0].Resource != "locked1/data/a" ||
		!slices.Equal(denied[0].Missing, []string{"Microsoft.Storage/storageAccounts/blobServices/containers/blobs/write"}) ||
		!slices.Equal(denied[1].Missing, []string{"Microsoft.Storage/storageAccounts/blobServices/containers/blobs/delete"}) {
		t.Fatalf("denials = %+v", denied)
	}
}

func TestBlobNetworkRules(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	params := accountParams()
	params.Properties = &armstorage.AccountPropertiesCreateParameters{NetworkRuleSet: &armstorage.NetworkRuleSet{DefaultAction: new(armstorage.DefaultActionDeny)}}
	p, err := c.accounts.BeginCreate(t.Context(), group, "walled1", params, nil)
	if _, err = wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	client := c.blobs(t, "walled1")
	if _, err := client.CreateContainer(t.Context(), "data", nil); code(err) != "AuthorizationFailure" || status(err) != http.StatusForbidden {
		t.Fatalf("a call the firewall denies = %v", err)
	}
	params.Properties.NetworkRuleSet.IPRules = []*armstorage.IPRule{{IPAddressOrRange: new("127.0.0.0/8"), Action: new("Allow")}}
	p, err = c.accounts.BeginCreate(t.Context(), group, "walled1", params, nil)
	if _, err = wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateContainer(t.Context(), "data", nil); err != nil {
		t.Fatalf("a call an IP rule allows = %v", err)
	}
}

func TestBlobFaults(t *testing.T) {
	faults := fault.New().
		Add(fault.Rule{Method: "PutBlob", Resource: "faulty1/data/lost", Nth: 1, Fault: fault.LoseResponse()}).
		Add(fault.Rule{Method: "ListBlobs", Nth: 1, Fault: fault.Unavailable()}).
		Add(fault.Rule{Method: "DeleteBlob", Nth: 1, Fault: fault.FailAfter(http.StatusInternalServerError)})
	c := setup(t, arm.WithFaults(faults))
	c.createGroup(t, group, nil)
	c.storageWith(t, "faulty1", nil)
	client := c.blobs(t, "faulty1")
	if _, err := client.UploadBuffer(t.Context(), "data", "lost", []byte("x"), nil); err == nil {
		t.Fatal("an upload whose response was lost succeeded")
	}
	if left := c.server.Blobs(); len(left) != 1 || left[0].Name != "lost" {
		t.Fatalf("blobs after a lost response = %+v", left)
	}
	pager := client.NewListBlobsFlatPager("data", nil)
	if _, err := pager.NextPage(t.Context()); code(err) != "ServerBusy" || status(err) != http.StatusServiceUnavailable {
		t.Fatalf("an unavailable list = %v", err)
	}
	if _, err := client.DeleteBlob(t.Context(), "data", "lost", nil); code(err) != "InternalError" {
		t.Fatalf("a delete that fails after it took effect = %v", err)
	}
	if left := c.server.Blobs(); len(left) != 0 {
		t.Fatalf("blobs after the delete = %+v", left)
	}
}
