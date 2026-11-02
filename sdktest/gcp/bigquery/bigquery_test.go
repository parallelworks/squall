package bigquery_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	bq "cloud.google.com/go/bigquery"
	su "cloud.google.com/go/serviceusage/apiv1"
	"cloud.google.com/go/serviceusage/apiv1/serviceusagepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/bigquery"
	"github.com/parallelworks/squall/gcp/serviceusage"
)

const project = "my-project"

func setup(t *testing.T, opts ...bigquery.Option) (*bigquery.Server, *bq.Client) {
	t.Helper()
	s := bigquery.New(opts...)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	c, err := bq.NewClient(t.Context(), project, option.WithEndpoint(srv.URL+"/bigquery/v2/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return s, c
}

// apiError returns err's status and first reason, or 0 and "".
func apiError(err error) (int, string) {
	e, ok := errors.AsType[*googleapi.Error](err)
	if !ok {
		return 0, ""
	}
	if len(e.Errors) == 0 {
		return e.Code, ""
	}
	return e.Code, e.Errors[0].Reason
}

func TestDatasetLifecycle(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s, c := setup(t, bigquery.WithClock(func() time.Time { return created }))
	ds := c.Dataset("analytics")
	if err := ds.Create(t.Context(), &bq.DatasetMetadata{Name: "Analytics", Description: "Usage data", Labels: map[string]string{"env": "test", "team": "data"}}); err != nil {
		t.Fatal(err)
	}
	md, err := ds.Metadata(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if md.FullID != project+":analytics" || md.Name != "Analytics" || md.Description != "Usage data" || md.Location != "US" ||
		md.Labels["env"] != "test" || md.ETag == "" || !md.CreationTime.Equal(created) || !md.LastModifiedTime.Equal(created) {
		t.Fatalf("metadata = %+v", md)
	}
	got, ok := s.LookupDataset(project, "analytics")
	if !ok || got.SelfLink != "https://bigquery.googleapis.com/bigquery/v2/projects/my-project/datasets/analytics" || got.DatasetReference.ProjectID != project {
		t.Fatalf("LookupDataset = %+v, %v", got, ok)
	}

	if err := c.DatasetInProject(project, "eu_data").Create(t.Context(), &bq.DatasetMetadata{Location: "EU"}); err != nil {
		t.Fatal(err)
	}
	if got := s.Datasets(); len(got) != 2 || got[0].ID != project+":analytics" || got[1].Location != "EU" {
		t.Fatalf("Datasets() = %+v", got)
	}

	var update bq.DatasetMetadataToUpdate
	update.Description = "Daily usage"
	update.SetLabel("env", "prod")
	update.DeleteLabel("team")
	updated, err := ds.Update(t.Context(), update, md.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Description != "Daily usage" || updated.Name != "Analytics" || len(updated.Labels) != 1 || updated.Labels["env"] != "prod" || updated.ETag == md.ETag {
		t.Fatalf("update = %+v", updated)
	}
	if _, err := ds.Update(t.Context(), update, md.ETag); func() int { code, _ := apiError(err); return code }() != http.StatusPreconditionFailed {
		t.Errorf("update with a stale etag = %v, want 412", err)
	}

	var ids []string
	it := c.Datasets(t.Context())
	for {
		d, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, d.DatasetID)
	}
	if !slices.Equal(ids, []string{"analytics", "eu_data"}) {
		t.Errorf("list = %v", ids)
	}

	if err := ds.Delete(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err = ds.Metadata(t.Context())
	if code, reason := apiError(err); code != http.StatusNotFound || reason != "notFound" || !strings.Contains(err.Error(), "Not found: Dataset my-project:analytics") {
		t.Fatalf("metadata after delete = %v", err)
	}
	if code, _ := apiError(ds.Delete(t.Context())); code != http.StatusNotFound {
		t.Errorf("deleting a deleted dataset = %v, want 404", code)
	}
}

func TestDatasetsTheAPIRefuses(t *testing.T) {
	_, c := setup(t)
	if err := c.Dataset("dup").Create(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	err := c.Dataset("dup").Create(t.Context(), nil)
	if code, reason := apiError(err); code != http.StatusConflict || reason != "duplicate" || !strings.Contains(err.Error(), "Already Exists: Dataset my-project:dup") {
		t.Errorf("a second create = %v, want 409 duplicate", err)
	}
	for _, tc := range []struct {
		name, id string
		md       *bq.DatasetMetadata
	}{
		{"a hyphen", "bad-id", nil},
		{"a long ID", strings.Repeat("a", 1025), nil},
		{"an upper case label key", "labels_a", &bq.DatasetMetadata{Labels: map[string]string{"Env": "x"}}},
		{"a label key starting with a digit", "labels_b", &bq.DatasetMetadata{Labels: map[string]string{"1env": "x"}}},
		{"an upper case label value", "labels_c", &bq.DatasetMetadata{Labels: map[string]string{"env": "X"}}},
		{"a long label value", "labels_d", &bq.DatasetMetadata{Labels: map[string]string{"env": strings.Repeat("x", 64)}}},
		{"an access entry", "access", &bq.DatasetMetadata{Access: []*bq.AccessEntry{{Role: bq.ReaderRole, EntityType: bq.SpecialGroupEntity, Entity: "projectReaders"}}}},
	} {
		if code, reason := apiError(c.Dataset(tc.id).Create(t.Context(), tc.md)); code != http.StatusBadRequest || reason != "invalid" {
			t.Errorf("create with %s = %d %s, want 400 invalid", tc.name, code, reason)
		}
	}
	if err := c.Dataset(strings.Repeat("a", 1024)).Create(t.Context(), &bq.DatasetMetadata{Labels: map[string]string{"env": "", "ü_key": "ok"}}); err != nil {
		t.Errorf("create with a 1024 character ID and an empty label value = %v", err)
	}
}

func TestDeletingADatasetWithTables(t *testing.T) {
	s, c := setup(t)
	ds := c.Dataset("full")
	if err := ds.Create(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTable(project, "full", "events"); err != nil {
		t.Fatal(err)
	}
	err := ds.Delete(t.Context())
	if code, reason := apiError(err); code != http.StatusBadRequest || reason != "resourceInUse" || !strings.Contains(err.Error(), "Dataset my-project:full is still in use") {
		t.Fatalf("delete = %v, want 400 resourceInUse", err)
	}
	if err := ds.DeleteWithContents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LookupDataset(project, "full"); ok {
		t.Error("the dataset is still there")
	}
}

func TestLostInsertResponse(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "bigquery.datasets.insert", Resource: "projects/my-project/datasets/staging", Nth: 1, Fault: fault.LoseResponse()})
	s, c := setup(t, bigquery.WithFaults(faults))
	ds := c.Dataset("staging")
	if err := ds.Create(t.Context(), nil); err == nil {
		t.Fatal("create with a lost response succeeded")
	}
	if _, err := ds.Metadata(t.Context()); err != nil {
		t.Fatalf("the dataset whose create lost its response = %v", err)
	}
	if code, _ := apiError(ds.Create(t.Context(), nil)); code != http.StatusConflict {
		t.Errorf("retrying the create = %d, want 409", code)
	}
	if len(s.Datasets()) != 1 || len(faults.Injected()) != 1 {
		t.Errorf("datasets %v, faults %v", s.Datasets(), faults.Injected())
	}
}

func TestPolicyRefusals(t *testing.T) {
	policy := access.Allow("bigquery.datasets.get", "bigquery.datasets.delete")
	s, c := setup(t, bigquery.WithPolicy(policy))
	err := c.Dataset("denied").Create(t.Context(), nil)
	if code, reason := apiError(err); code != http.StatusForbidden || reason != "accessDenied" || !strings.Contains(err.Error(), "bigquery.datasets.create") {
		t.Fatalf("create without permission = %v", err)
	}
	if len(s.Datasets()) != 0 {
		t.Fatal("a refused create made a dataset")
	}
	if d := policy.Denials(); len(d) != 1 || !slices.Equal(d[0].Missing, []string{"bigquery.datasets.create"}) {
		t.Errorf("denials = %+v", d)
	}
	if code, _ := apiError(func() error { _, err := c.Dataset("denied").Metadata(t.Context()); return err }()); code != http.StatusNotFound {
		t.Errorf("get with permission = %d, want 404", code)
	}
}

func TestDeleteWithContentsNeedsTablesDelete(t *testing.T) {
	policy := access.Allow("bigquery.datasets.create", "bigquery.datasets.delete")
	s, c := setup(t, bigquery.WithPolicy(policy))
	ds := c.Dataset("full")
	if err := ds.Create(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTable(project, "full", "events"); err != nil {
		t.Fatal(err)
	}
	err := ds.DeleteWithContents(t.Context())
	if code, reason := apiError(err); code != http.StatusForbidden || reason != "accessDenied" || !strings.Contains(err.Error(), "bigquery.tables.delete") {
		t.Fatalf("delete with contents without bigquery.tables.delete = %v", err)
	}
	if _, ok := s.LookupDataset(project, "full"); !ok {
		t.Fatal("a refused delete removed the dataset")
	}
}

func TestDatasetsNeedTheAPIEnabled(t *testing.T) {
	usage := serviceusage.New(serviceusage.WithProject(project, 123456789))
	usrv := httptest.NewServer(usage)
	t.Cleanup(usrv.Close)
	_, c := setup(t, bigquery.WithServiceUsage(usage))
	err := c.Dataset("early").Create(t.Context(), nil)
	if code, reason := apiError(err); code != http.StatusForbidden || reason != "accessNotConfigured" || !strings.Contains(err.Error(), "BigQuery API has not been used in project 123456789 before or it is disabled.") {
		t.Fatalf("create before enabling the API = %v", err)
	}
	uc, err := su.NewRESTClient(t.Context(), option.WithEndpoint(usrv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { uc.Close() })
	op, err := uc.EnableService(t.Context(), &serviceusagepb.EnableServiceRequest{Name: "projects/" + project + "/services/bigquery.googleapis.com"})
	if err == nil {
		_, err = op.Wait(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Dataset("early").Create(t.Context(), nil); err != nil {
		t.Fatalf("create after enabling the API = %v", err)
	}
}
