package storage_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/gcp/storage"
)

func TestPolicyNeedsDeleteToOverwriteAnObject(t *testing.T) {
	policy := access.Allow("storage.buckets.create", "storage.objects.create", "storage.objects.list")
	c := newClient(t, storage.New(storage.WithPolicy(policy)))
	if err := c.Bucket("data").Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	write(t, c, "data", "a.txt", "one")

	w := c.Bucket("data").Object("a.txt").NewWriter(t.Context())
	if _, err := w.Write([]byte("two")); err != nil {
		t.Fatal(err)
	}
	err := w.Close()
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusForbidden {
		t.Fatalf("overwrite = %v, want 403", err)
	}
	if !strings.Contains(gerr.Message, "Permission 'storage.objects.delete' denied") {
		t.Errorf("message = %q, want it to name storage.objects.delete", gerr.Message)
	}
	if got := policy.Unused(); !slices.Equal(got, []string{"storage.objects.list"}) {
		t.Errorf("Unused() = %v, want [storage.objects.list]", got)
	}
}

func TestPolicyRefusesAMethodItDoesNotGrant(t *testing.T) {
	policy := access.Allow("storage.buckets.create")
	c := newClient(t, storage.New(storage.WithPolicy(policy)))
	if err := c.Bucket("data").Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	err := c.Bucket("data").Delete(t.Context())
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusForbidden {
		t.Fatalf("delete = %v, want 403", err)
	}
	denials := policy.Denials()
	if len(denials) != 1 || denials[0].Method != "storage.buckets.delete" || denials[0].Resource != "b/data" {
		t.Errorf("Denials() = %+v", denials)
	}
}
