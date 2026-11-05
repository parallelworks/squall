package storage_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	rawstorage "google.golang.org/api/storage/v1"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/gcp/storage"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// apiErr returns the status, reason and message of a Google API error.
func apiErr(t *testing.T, err error) (int, string, string) {
	t.Helper()
	e, ok := errors.AsType[*googleapi.Error](err)
	if !ok {
		t.Fatalf("error = %v, want a Google API error", err)
	}
	reason := ""
	if len(e.Errors) > 0 {
		reason = e.Errors[0].Reason
	}
	return e.Code, reason, e.Message
}

func wantErr(t *testing.T, what string, err error, code int, reason, message string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s succeeded, want %d %s", what, code, reason)
	}
	gotCode, gotReason, gotMessage := apiErr(t, err)
	if gotCode != code || gotReason != reason || !strings.Contains(gotMessage, message) {
		t.Fatalf("%s = %d %s %q, want %d %s %q", what, gotCode, gotReason, gotMessage, code, reason, message)
	}
}

func writeWith(t *testing.T, c *gcs.Client, bucket, name, data string, attrs gcs.ObjectAttrs) (*gcs.ObjectAttrs, error) {
	t.Helper()
	w := c.Bucket(bucket).Object(name).NewWriter(t.Context())
	w.ObjectAttrs = attrs
	w.Name = name
	if _, err := w.Write([]byte(data)); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return w.Attrs(), nil
}

func overwrite(t *testing.T, c *gcs.Client, bucket, name string) error {
	t.Helper()
	_, err := writeWith(t, c, bucket, name, "again", gcs.ObjectAttrs{})
	return err
}

func TestRetentionPolicyAndBucketLock(t *testing.T) {
	clk := newClock()
	s := storage.New(storage.WithClock(clk.Now))
	c := newClient(t, s)
	b := c.Bucket("retained")
	if err := b.Create(t.Context(), project, &gcs.BucketAttrs{RetentionPolicy: &gcs.RetentionPolicy{RetentionPeriod: time.Hour}}); err != nil {
		t.Fatal(err)
	}
	attrs, err := b.Attrs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if p := attrs.RetentionPolicy; p == nil || p.RetentionPeriod != time.Hour || !p.EffectiveTime.Equal(clk.Now()) || p.IsLocked {
		t.Fatalf("retention policy = %+v", p)
	}
	o := write(t, c, "retained", "kept", "data")
	if !o.RetentionExpirationTime.Equal(o.Created.Add(time.Hour)) {
		t.Fatalf("retention expiration = %v, created %v", o.RetentionExpirationTime, o.Created)
	}
	wantErr(t, "delete a retained object", b.Object("kept").Delete(t.Context()), http.StatusForbidden, "retentionPolicyNotMet",
		"Object 'retained/kept' is subject to bucket's retention policy or object retention and cannot be deleted or overwritten until 2026-03-01T13:00:00.000000Z")
	wantErr(t, "overwrite a retained object", overwrite(t, c, "retained", "kept"), http.StatusForbidden, "retentionPolicyNotMet", "cannot be deleted or overwritten")

	wantErr(t, "lock with a stale metageneration", b.If(gcs.BucketConditions{MetagenerationMatch: 7}).LockRetentionPolicy(t.Context()),
		http.StatusPreconditionFailed, "conditionNotMet", "pre-conditions")
	if err := b.If(gcs.BucketConditions{MetagenerationMatch: attrs.MetaGeneration}).LockRetentionPolicy(t.Context()); err != nil {
		t.Fatalf("lock: %v", err)
	}
	locked, err := b.Attrs(t.Context())
	if err != nil || !locked.RetentionPolicy.IsLocked || locked.MetaGeneration != attrs.MetaGeneration+1 {
		t.Fatalf("locked = %+v, %v", locked, err)
	}
	wantErr(t, "lock again", b.If(gcs.BucketConditions{MetagenerationMatch: locked.MetaGeneration}).LockRetentionPolicy(t.Context()),
		http.StatusBadRequest, "invalid", "Bucket 'retained' does not have an unlocked retention policy.")

	_, err = b.Update(t.Context(), gcs.BucketAttrsToUpdate{RetentionPolicy: &gcs.RetentionPolicy{RetentionPeriod: time.Minute}})
	wantErr(t, "shorten a locked policy", err, http.StatusForbidden, "forbidden",
		"Cannot reduce retention duration of a locked Retention Policy for bucket 'retained'.")
	_, err = b.Update(t.Context(), gcs.BucketAttrsToUpdate{RetentionPolicy: &gcs.RetentionPolicy{}})
	wantErr(t, "remove a locked policy", err, http.StatusForbidden, "retentionPolicyNotMet",
		"Bucket 'retained' has a locked Retention Policy which cannot be removed.")
	longer, err := b.Update(t.Context(), gcs.BucketAttrsToUpdate{RetentionPolicy: &gcs.RetentionPolicy{RetentionPeriod: 2 * time.Hour}})
	if err != nil || longer.RetentionPolicy.RetentionPeriod != 2*time.Hour || !longer.RetentionPolicy.IsLocked {
		t.Fatalf("lengthen a locked policy = %+v, %v", longer, err)
	}

	clk.Advance(90 * time.Minute)
	wantErr(t, "delete before the longer period ends", b.Object("kept").Delete(t.Context()), http.StatusForbidden, "retentionPolicyNotMet", "14:00:00")
	clk.Advance(time.Hour)
	if err := b.Object("kept").Delete(t.Context()); err != nil {
		t.Fatalf("delete after the period: %v", err)
	}
	if err := b.Delete(t.Context()); err != nil {
		t.Fatalf("delete the emptied locked bucket: %v", err)
	}
}

func TestUnlockedRetentionPolicyChanges(t *testing.T) {
	s := storage.New()
	c := newClient(t, s)
	b := c.Bucket("unlocked")
	if err := b.Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Update(t.Context(), gcs.BucketAttrsToUpdate{RetentionPolicy: &gcs.RetentionPolicy{RetentionPeriod: 3_155_760_000 * time.Second}}); err != nil {
		t.Fatalf("the maximum period: %v", err)
	}
	if _, err := b.Update(t.Context(), gcs.BucketAttrsToUpdate{RetentionPolicy: &gcs.RetentionPolicy{}}); err != nil {
		t.Fatal(err)
	}
	for _, period := range []time.Duration{-time.Second, 3_155_760_001 * time.Second} {
		_, err := b.Update(t.Context(), gcs.BucketAttrsToUpdate{RetentionPolicy: &gcs.RetentionPolicy{RetentionPeriod: period}})
		wantErr(t, "an out of range period", err, http.StatusBadRequest, "invalid", "greater than 0 and less than 100 years")
	}
	write(t, c, "unlocked", "k", "v")
	if _, err := b.Update(t.Context(), gcs.BucketAttrsToUpdate{RetentionPolicy: &gcs.RetentionPolicy{RetentionPeriod: time.Hour}}); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "delete an object older than the policy", b.Object("k").Delete(t.Context()), http.StatusForbidden, "retentionPolicyNotMet", "unlocked/k")
	if _, err := b.Update(t.Context(), gcs.BucketAttrsToUpdate{RetentionPolicy: &gcs.RetentionPolicy{}}); err != nil {
		t.Fatalf("remove an unlocked policy: %v", err)
	}
	if attrs, _ := b.Attrs(t.Context()); attrs.RetentionPolicy != nil {
		t.Fatalf("policy after removal = %+v", attrs.RetentionPolicy)
	}
	if err := b.Object("k").Delete(t.Context()); err != nil {
		t.Fatalf("delete after removing the policy: %v", err)
	}
}

func TestObjectHolds(t *testing.T) {
	clk := newClock()
	s := storage.New(storage.WithClock(clk.Now))
	c := newClient(t, s)
	b := c.Bucket("holds")
	if err := b.Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	o, err := writeWith(t, c, "holds", "temp", "x", gcs.ObjectAttrs{TemporaryHold: true})
	if err != nil || !o.TemporaryHold || o.EventBasedHold {
		t.Fatalf("upload with a hold = %+v, %v", o, err)
	}
	wantErr(t, "delete a held object", b.Object("temp").Delete(t.Context()), http.StatusForbidden, "forbidden",
		"Object 'holds/temp' is under active Temporary hold and cannot be deleted, overwritten or archived until hold is removed.")
	wantErr(t, "overwrite a held object", overwrite(t, c, "holds", "temp"), http.StatusForbidden, "forbidden", "Temporary hold")
	released, err := b.Object("temp").Update(t.Context(), gcs.ObjectAttrsToUpdate{TemporaryHold: false, Metadata: map[string]string{"k": "v"}})
	if err != nil || released.TemporaryHold || released.Metageneration != 2 || released.Metadata["k"] != "v" {
		t.Fatalf("release = %+v, %v", released, err)
	}
	if err := b.Object("temp").Delete(t.Context()); err != nil {
		t.Fatalf("delete a released object: %v", err)
	}

	if _, err := b.Update(t.Context(), gcs.BucketAttrsToUpdate{DefaultEventBasedHold: true, RetentionPolicy: &gcs.RetentionPolicy{RetentionPeriod: time.Hour}}); err != nil {
		t.Fatal(err)
	}
	held := write(t, c, "holds", "event", "x")
	if !held.EventBasedHold || !held.RetentionExpirationTime.IsZero() {
		t.Fatalf("an object under a default event-based hold = %+v", held)
	}
	wantErr(t, "delete an object under an event-based hold", b.Object("event").Delete(t.Context()), http.StatusForbidden, "forbidden", "active Event-Based hold")
	clk.Advance(3 * time.Hour)
	released, err = b.Object("event").Update(t.Context(), gcs.ObjectAttrsToUpdate{EventBasedHold: false})
	if err != nil || released.EventBasedHold || !released.RetentionExpirationTime.Equal(clk.Now().Add(time.Hour)) {
		t.Fatalf("release the event-based hold = %+v, %v", released, err)
	}
	wantErr(t, "delete once the hold restarts retention", b.Object("event").Delete(t.Context()), http.StatusForbidden, "retentionPolicyNotMet", "holds/event")
	clk.Advance(time.Hour)
	if err := b.Object("event").Delete(t.Context()); err != nil {
		t.Fatalf("delete after retention: %v", err)
	}
}

func TestHoldsAndRetentionInVersionedBuckets(t *testing.T) {
	s := storage.New()
	c := newClient(t, s)
	b := c.Bucket("versioned-retained")
	if err := b.Create(t.Context(), project, &gcs.BucketAttrs{
		VersioningEnabled: true, RetentionPolicy: &gcs.RetentionPolicy{RetentionPeriod: time.Hour},
	}); err != nil {
		t.Fatal(err)
	}
	first := write(t, c, "versioned-retained", "k", "one")
	write(t, c, "versioned-retained", "k", "two")
	wantErr(t, "delete a retained noncurrent generation", b.Object("k").Generation(first.Generation).Delete(t.Context()),
		http.StatusForbidden, "retentionPolicyNotMet", "versioned-retained/k")
	if _, err := s.SetObjectHolds("versioned-retained", "k", true, false); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "archive a held object", b.Object("k").Delete(t.Context()), http.StatusForbidden, "forbidden", "Temporary hold")
	if _, err := s.SetObjectHolds("versioned-retained", "k", false, false); err != nil {
		t.Fatal(err)
	}
	if err := b.Object("k").Delete(t.Context()); err != nil {
		t.Fatalf("make a retained object noncurrent: %v", err)
	}
}

func TestObjectRetention(t *testing.T) {
	clk := newClock()
	s := storage.New(storage.WithClock(clk.Now))
	c := newClient(t, s)
	b := c.Bucket("object-retention")
	if err := b.SetObjectRetention(true).Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	if attrs, err := b.Attrs(t.Context()); err != nil || attrs.ObjectRetentionMode != "Enabled" {
		t.Fatalf("attrs = %+v, %v", attrs, err)
	}
	until := clk.Now().Add(time.Hour)
	o, err := writeWith(t, c, "object-retention", "k", "x", gcs.ObjectAttrs{Retention: &gcs.ObjectRetention{Mode: "Unlocked", RetainUntil: until}})
	if err != nil || o.Retention == nil || o.Retention.Mode != "Unlocked" || !o.Retention.RetainUntil.Equal(until) {
		t.Fatalf("upload with retention = %+v, %v", o, err)
	}
	wantErr(t, "delete a retained object", b.Object("k").Delete(t.Context()), http.StatusForbidden, "retentionPolicyNotMet", "object-retention/k")
	shorter := gcs.ObjectAttrsToUpdate{Retention: &gcs.ObjectRetention{Mode: "Unlocked", RetainUntil: until.Add(-time.Minute)}}
	_, err = b.Object("k").Update(t.Context(), shorter)
	wantErr(t, "shorten without override", err, http.StatusForbidden, "forbidden", "without overriding unlocked retention intent")
	_, err = b.Object("k").Update(t.Context(), gcs.ObjectAttrsToUpdate{EventBasedHold: true})
	wantErr(t, "an event-based hold beside retention", err, http.StatusBadRequest, "invalid", "event-based hold")
	_, err = b.Object("k").Update(t.Context(), gcs.ObjectAttrsToUpdate{Retention: &gcs.ObjectRetention{Mode: "Locked", RetainUntil: until}})
	wantErr(t, "lock unlocked retention without override", err, http.StatusForbidden, "forbidden", "without overriding unlocked retention intent")
	if _, err := b.Object("k").OverrideUnlockedRetention(true).Update(t.Context(), shorter); err != nil {
		t.Fatalf("shorten with override: %v", err)
	}
	removed, err := b.Object("k").OverrideUnlockedRetention(true).Update(t.Context(), gcs.ObjectAttrsToUpdate{Retention: &gcs.ObjectRetention{}})
	if err != nil || removed.Retention != nil {
		t.Fatalf("remove with override = %+v, %v", removed, err)
	}

	locked := gcs.ObjectAttrsToUpdate{Retention: &gcs.ObjectRetention{Mode: "Locked", RetainUntil: until}}
	if _, err := b.Object("k").Update(t.Context(), locked); err != nil {
		t.Fatalf("lock retention: %v", err)
	}
	_, err = b.Object("k").OverrideUnlockedRetention(true).Update(t.Context(), gcs.ObjectAttrsToUpdate{Retention: &gcs.ObjectRetention{Mode: "Unlocked", RetainUntil: until}})
	wantErr(t, "unlock locked retention", err, http.StatusForbidden, "forbidden", "The locked object retention cannot be removed.")
	extended := gcs.ObjectAttrsToUpdate{Retention: &gcs.ObjectRetention{Mode: "Locked", RetainUntil: until.Add(time.Hour)}}
	if _, err := b.Object("k").Update(t.Context(), extended); err != nil {
		t.Fatalf("extend locked retention: %v", err)
	}
	clk.Advance(90 * time.Minute)
	wantErr(t, "delete before the extended time", b.Object("k").Delete(t.Context()), http.StatusForbidden, "retentionPolicyNotMet", "14:00:00")

	_, err = b.Object("k").Update(t.Context(), gcs.ObjectAttrsToUpdate{Retention: &gcs.ObjectRetention{Mode: "Locked", RetainUntil: clk.Now().Add(-time.Minute)}})
	wantErr(t, "a retain-until time in the past", err, http.StatusBadRequest, "invalid", "Retain until time cannot be in the past.")

	if err := c.Bucket("plain").Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	_, err = writeWith(t, c, "plain", "k", "x", gcs.ObjectAttrs{Retention: &gcs.ObjectRetention{Mode: "Unlocked", RetainUntil: clk.Now().Add(time.Hour)}})
	wantErr(t, "retention where the bucket does not enable it", err, http.StatusForbidden, "forbidden", "storage.objects.setRetention")
}

func TestSoftDeletedObjects(t *testing.T) {
	clk := newClock()
	s := storage.New(storage.WithClock(clk.Now))
	c := newClient(t, s)
	b := c.Bucket("soft")
	if err := b.Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	attrs, err := b.Attrs(t.Context())
	if err != nil || attrs.SoftDeletePolicy == nil || attrs.SoftDeletePolicy.RetentionDuration != 7*24*time.Hour || !attrs.SoftDeletePolicy.EffectiveTime.Equal(clk.Now()) {
		t.Fatalf("soft delete policy = %+v, %v", attrs.SoftDeletePolicy, err)
	}
	first := write(t, c, "soft", "k", "one")
	second := write(t, c, "soft", "k", "two")
	if err := b.Object("k").Delete(t.Context()); err != nil {
		t.Fatal(err)
	}
	if live := objects(t, b.Objects(t.Context(), nil)); len(live) != 0 {
		t.Fatalf("live = %+v", live)
	}
	soft := objects(t, b.Objects(t.Context(), &gcs.Query{SoftDeleted: true}))
	if len(soft) != 2 || soft[0].Generation != first.Generation || soft[1].Generation != second.Generation ||
		!soft[0].SoftDeleteTime.Equal(clk.Now()) || !soft[0].HardDeleteTime.Equal(clk.Now().Add(7*24*time.Hour)) {
		t.Fatalf("soft-deleted = %+v", soft)
	}
	got, err := b.Object("k").Generation(first.Generation).SoftDeleted().Attrs(t.Context())
	if err != nil || got.Size != 3 || got.SoftDeleteTime.IsZero() {
		t.Fatalf("get a soft-deleted generation = %+v, %v", got, err)
	}
	if err := b.Object("k").Generation(first.Generation).Delete(t.Context()); !errors.Is(err, gcs.ErrObjectNotExist) {
		t.Fatalf("hard delete a soft-deleted generation = %v", err)
	}
	if err := b.Delete(t.Context()); err != nil {
		t.Fatalf("delete a bucket holding only soft-deleted objects: %v", err)
	}
	if err := c.Bucket("soft").Create(t.Context(), project, nil); err != nil {
		t.Fatalf("create the name again: %v", err)
	}
	if err := c.Bucket("soft").Delete(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := s.SoftDeletedBuckets(); len(got) != 2 || got[0].Name != "soft" || got[0].HardDeleteTime == "" {
		t.Fatalf("soft-deleted buckets = %+v", got)
	}
	clk.Advance(7 * 24 * time.Hour)
	if got := s.SoftDeletedBuckets(); len(got) != 0 {
		t.Fatalf("soft-deleted buckets after their retention = %+v", got)
	}
}

func TestRestoreObjects(t *testing.T) {
	clk := newClock()
	s := storage.New(storage.WithClock(clk.Now))
	c := newClient(t, s)
	b := c.Bucket("restorable")
	if err := b.Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	first := write(t, c, "restorable", "k", "one")
	second := write(t, c, "restorable", "k", "two")
	restored, err := b.Object("k").Generation(first.Generation).Restore(t.Context(), &gcs.RestoreOptions{})
	if err != nil || restored.Generation <= second.Generation || restored.Size != 3 || restored.Metageneration != 1 || restored.StorageClass != "STANDARD" {
		t.Fatalf("restore = %+v, %v", restored, err)
	}
	soft := s.SoftDeletedObjects("restorable")
	if len(soft) != 2 || soft[0].Generation != first.Generation || soft[1].Generation != second.Generation {
		t.Fatalf("soft-deleted after restore = %+v", soft)
	}
	_, err = b.Object("k").Generation(restored.Generation).Restore(t.Context(), &gcs.RestoreOptions{})
	wantErr(t, "restore a live generation", err, http.StatusPreconditionFailed, "objectNotSoftDeleted", "")
	_, err = b.Object("k").Generation(second.Generation).If(gcs.Conditions{DoesNotExist: true}).Restore(t.Context(), &gcs.RestoreOptions{})
	wantErr(t, "restore over a live object with DoesNotExist", err, http.StatusPreconditionFailed, "conditionNotMet", "")
	if _, err := s.SetObjectHolds("restorable", "k", true, false); err != nil {
		t.Fatal(err)
	}
	_, err = b.Object("k").Generation(second.Generation).Restore(t.Context(), &gcs.RestoreOptions{})
	wantErr(t, "restore over a held object", err, http.StatusForbidden, "forbidden", "Temporary hold")

	if _, err := s.SetObjectHolds("restorable", "k", false, false); err != nil {
		t.Fatal(err)
	}
	if err := b.Object("k").Delete(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err = b.Object("k").Generation(first.Generation).If(gcs.Conditions{GenerationNotMatch: 5}).Restore(t.Context(), &gcs.RestoreOptions{})
	wantErr(t, "restore with ifGenerationNotMatch and no live object", err, http.StatusPreconditionFailed, "conditionNotMet", "")

	clk.Advance(7 * 24 * time.Hour)
	if _, err := b.Object("k").Generation(first.Generation).Restore(t.Context(), &gcs.RestoreOptions{}); !errors.Is(err, gcs.ErrObjectNotExist) {
		t.Fatalf("restore after retention = %v", err)
	}

	_, err = b.Update(t.Context(), gcs.BucketAttrsToUpdate{SoftDeletePolicy: &gcs.SoftDeletePolicy{RetentionDuration: 24 * time.Hour}})
	wantErr(t, "a one-day soft delete policy", err, http.StatusBadRequest, "invalid", "between 7 days and 90 days")
	off, err := b.Update(t.Context(), gcs.BucketAttrsToUpdate{SoftDeletePolicy: &gcs.SoftDeletePolicy{}})
	if err != nil || off.SoftDeletePolicy == nil || off.SoftDeletePolicy.RetentionDuration != 0 {
		t.Fatalf("turn soft delete off = %+v, %v", off, err)
	}
	_, err = b.Objects(t.Context(), &gcs.Query{SoftDeleted: true}).Next()
	wantErr(t, "list soft-deleted objects with soft delete off", err, http.StatusBadRequest, "invalid", "Soft delete policy is required")
}

func TestPutSoftDeletedObject(t *testing.T) {
	s := storage.New()
	if _, err := s.PutBucket(storage.Bucket{Name: "seeded", Project: project}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutObject("seeded", "k", []byte("live")); err != nil {
		t.Fatal(err)
	}
	gone, err := s.PutSoftDeletedObject("seeded", "k", []byte("gone"))
	if err != nil || gone.SoftDeleteTime == "" {
		t.Fatalf("put soft-deleted = %+v, %v", gone, err)
	}
	if got := s.Objects("seeded"); len(got) != 1 || got[0].Size != 4 || got[0].TimeDeleted != "" {
		t.Fatalf("objects = %+v", got)
	}
	restored, err := newClient(t, s).Bucket("seeded").Object("k").Generation(gone.Generation).Restore(t.Context(), &gcs.RestoreOptions{})
	if err != nil || restored.Size != 4 {
		t.Fatalf("restore = %+v, %v", restored, err)
	}
	if _, err := s.PutBucket(storage.Bucket{Name: "keeps-nothing", Project: project, SoftDeletePolicy: &storage.SoftDeletePolicy{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSoftDeletedObject("keeps-nothing", "k", nil); err == nil {
		t.Fatal("a soft-deleted object in a bucket that keeps none was put")
	}
}

func TestSetRetentionHelpers(t *testing.T) {
	clk := newClock()
	s := storage.New(storage.WithClock(clk.Now))
	if _, err := s.PutBucket(storage.Bucket{Name: "helpers", Project: project, ObjectRetention: &storage.BucketObjectRetention{Mode: "Enabled"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutObject("helpers", "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if b, err := s.SetRetentionPolicy("helpers", &storage.RetentionPolicy{RetentionPeriod: 60, IsLocked: true}); err != nil || !b.RetentionPolicy.IsLocked {
		t.Fatalf("set a locked policy = %+v, %v", b, err)
	}
	c := newClient(t, s)
	_, err := c.Bucket("helpers").Update(t.Context(), gcs.BucketAttrsToUpdate{RetentionPolicy: &gcs.RetentionPolicy{}})
	wantErr(t, "remove a policy a helper locked", err, http.StatusForbidden, "retentionPolicyNotMet", "locked")
	if _, err := s.SetRetentionPolicy("helpers", nil); err != nil {
		t.Fatal(err)
	}
	until := clk.Now().Add(time.Hour).Format(time.RFC3339)
	o, err := s.SetObjectRetention("helpers", "k", &storage.Retention{Mode: "Locked", RetainUntilTime: until})
	if err != nil || o.Retention == nil || o.Retention.Mode != "Locked" {
		t.Fatalf("set retention = %+v, %v", o, err)
	}
	wantErr(t, "delete under retention a helper set", c.Bucket("helpers").Object("k").Delete(t.Context()), http.StatusForbidden, "retentionPolicyNotMet", "helpers/k")
	if _, err := s.SetObjectRetention("helpers", "k", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetObjectHolds("helpers", "k", false, true); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "delete under a hold a helper set", c.Bucket("helpers").Object("k").Delete(t.Context()), http.StatusForbidden, "forbidden", "Event-Based")
	if _, err := s.SetObjectHolds("helpers", "missing", true, false); err == nil {
		t.Fatal("held a missing object")
	}
}

func rawService(t *testing.T, url string) *rawstorage.Service {
	t.Helper()
	svc, err := rawstorage.NewService(t.Context(), option.WithEndpoint(url+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// The Go client has no bucket soft delete calls, so this goes through the generated JSON API client.
func TestSoftDeletedBuckets(t *testing.T) {
	s := storage.New()
	url := serve(t, s)
	c, svc := clientAt(t, url), rawService(t, url)
	if err := c.Bucket("gone").Create(t.Context(), project, &gcs.BucketAttrs{Labels: map[string]string{"a": "b"}}); err != nil {
		t.Fatal(err)
	}
	write(t, c, "gone", "k", "v")
	if err := c.Bucket("gone").Object("k").Delete(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.Bucket("gone").Delete(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.Bucket("kept-nothing").Create(t.Context(), project, &gcs.BucketAttrs{SoftDeletePolicy: &gcs.SoftDeletePolicy{}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Bucket("kept-nothing").Delete(t.Context()); err != nil {
		t.Fatal(err)
	}
	if live := names(t, c.Buckets(t.Context(), project)); len(live) != 0 {
		t.Fatalf("live buckets = %v", live)
	}
	list, err := svc.Buckets.List(project).SoftDeleted(true).Context(t.Context()).Do()
	if err != nil || len(list.Items) != 1 || list.Items[0].Name != "gone" || list.Items[0].SoftDeleteTime == "" || list.Items[0].HardDeleteTime == "" {
		t.Fatalf("soft-deleted buckets = %+v, %v", list, err)
	}
	gen := list.Items[0].Generation
	got, err := svc.Buckets.Get("gone").SoftDeleted(true).Generation(gen).Context(t.Context()).Do()
	if err != nil || got.Labels["a"] != "b" {
		t.Fatalf("get a soft-deleted bucket = %+v, %v", got, err)
	}
	_, err = svc.Buckets.Get("gone").SoftDeleted(true).Context(t.Context()).Do()
	wantErr(t, "get a soft-deleted bucket without its generation", err, http.StatusBadRequest, "required", "generation")

	if err := c.Bucket("gone").Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Buckets.Restore("gone", gen).Context(t.Context()).Do()
	wantErr(t, "restore over a live bucket", err, http.StatusConflict, "conflict", "")
	if err := c.Bucket("gone").Delete(t.Context()); err != nil {
		t.Fatal(err)
	}
	restored, err := svc.Buckets.Restore("gone", gen).Context(t.Context()).Do()
	if err != nil || restored.Name != "gone" || restored.Generation != gen || restored.SoftDeleteTime != "" {
		t.Fatalf("restore = %+v, %v", restored, err)
	}
	soft := objects(t, c.Bucket("gone").Objects(t.Context(), &gcs.Query{SoftDeleted: true}))
	if len(soft) != 1 {
		t.Fatalf("soft-deleted objects in the restored bucket = %+v", soft)
	}
	if _, err := c.Bucket("gone").Object("k").Generation(soft[0].Generation).Restore(t.Context(), &gcs.RestoreOptions{}); err != nil {
		t.Fatalf("restore an object in the restored bucket: %v", err)
	}
	_, err = svc.Buckets.Restore("kept-nothing", 1).Context(t.Context()).Do()
	wantErr(t, "restore a bucket that was not kept", err, http.StatusNotFound, "notFound", "")
}

func TestObjectsUpdateReplacesMetadata(t *testing.T) {
	s := storage.New()
	url := serve(t, s)
	c, svc := clientAt(t, url), rawService(t, url)
	if err := c.Bucket("replaced").Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := writeWith(t, c, "replaced", "k", "v", gcs.ObjectAttrs{TemporaryHold: true, Metadata: map[string]string{"a": "1"}}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Objects.Update("replaced", "k", &rawstorage.Object{Metadata: map[string]string{"b": "2"}}).Context(t.Context()).Do()
	if err != nil || got.TemporaryHold || len(got.Metadata) != 1 || got.Metadata["b"] != "2" || got.Metageneration != 2 {
		t.Fatalf("update = %+v, %v", got, err)
	}
	if err := c.Bucket("replaced").Object("k").Delete(t.Context()); err != nil {
		t.Fatalf("delete after an update released the hold: %v", err)
	}
}

func TestRequesterPays(t *testing.T) {
	policy := access.Allow("storage.buckets.create", "storage.buckets.get", "storage.objects.create", serviceUsage)
	s := storage.New(storage.WithPolicy(policy))
	c := newClient(t, s)
	if err := c.Bucket("payer").Create(t.Context(), project, &gcs.BucketAttrs{RequesterPays: true}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Bucket("payer").Attrs(t.Context())
	wantErr(t, "get without a user project", err, http.StatusBadRequest, "required", "Bucket is a requester pays bucket but no user project provided.")
	attrs, err := c.Bucket("payer").UserProject("billed").Attrs(t.Context())
	if err != nil || !attrs.RequesterPays {
		t.Fatalf("get with a user project = %+v, %v", attrs, err)
	}
	if _, err := s.PutBucket(storage.Bucket{Name: "free", Project: project}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bucket("free").Attrs(t.Context()); err != nil {
		t.Fatalf("get a bucket that is not Requester Pays: %v", err)
	}
}

const serviceUsage = "serviceusage.services.use"

func TestSoftDeletedBucketsOutliveTheirObjects(t *testing.T) {
	clk := newClock()
	s := storage.New(storage.WithClock(clk.Now))
	url := serve(t, s)
	c, svc := clientAt(t, url), rawService(t, url)
	for _, name := range []string{"shortened", "turned-off"} {
		if err := c.Bucket(name).Create(t.Context(), project, &gcs.BucketAttrs{SoftDeletePolicy: &gcs.SoftDeletePolicy{RetentionDuration: 90 * 24 * time.Hour}}); err != nil {
			t.Fatal(err)
		}
		write(t, c, name, "k", "v")
		if err := c.Bucket(name).Object("k").Delete(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Bucket("shortened").Update(t.Context(), gcs.BucketAttrsToUpdate{SoftDeletePolicy: &gcs.SoftDeletePolicy{RetentionDuration: 7 * 24 * time.Hour}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bucket("turned-off").Update(t.Context(), gcs.BucketAttrsToUpdate{SoftDeletePolicy: &gcs.SoftDeletePolicy{}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"shortened", "turned-off"} {
		if err := c.Bucket(name).Delete(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	clk.Advance(30 * 24 * time.Hour)
	kept := s.SoftDeletedBuckets()
	want := clk.Now().Add(60 * 24 * time.Hour).Format("2006-01-02T15:04:05.000Z")
	if len(kept) != 2 || kept[0].Name != "shortened" || kept[1].Name != "turned-off" || kept[0].HardDeleteTime != want || kept[1].HardDeleteTime != want {
		t.Fatalf("soft-deleted buckets = %+v, want both until %s", kept, want)
	}
	if _, err := svc.Buckets.Restore("shortened", kept[0].Generation).Context(t.Context()).Do(); err != nil {
		t.Fatal(err)
	}
	soft := objects(t, c.Bucket("shortened").Objects(t.Context(), &gcs.Query{SoftDeleted: true}))
	if len(soft) != 1 {
		t.Fatalf("soft-deleted objects in the restored bucket = %+v", soft)
	}
	if _, err := c.Bucket("shortened").Object("k").Generation(soft[0].Generation).Restore(t.Context(), &gcs.RestoreOptions{}); err != nil {
		t.Fatalf("restore an object that outlived the shortened policy: %v", err)
	}
	if _, err := svc.Buckets.Restore("turned-off", kept[1].Generation).Context(t.Context()).Do(); err != nil {
		t.Fatal(err)
	}
	soft = objects(t, c.Bucket("turned-off").Objects(t.Context(), &gcs.Query{SoftDeleted: true}))
	if len(soft) != 1 {
		t.Fatalf("soft-deleted objects in a restored bucket with soft delete off = %+v", soft)
	}
	if _, err := c.Bucket("turned-off").Object("k").Generation(soft[0].Generation).Restore(t.Context(), &gcs.RestoreOptions{}); err != nil {
		t.Fatalf("restore an object kept after soft delete was turned off: %v", err)
	}
	for _, name := range []string{"shortened", "turned-off"} {
		if err := c.Bucket(name).Object("k").Delete(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := c.Bucket(name).Delete(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	clk.Advance(60 * 24 * time.Hour)
	if kept := s.SoftDeletedBuckets(); len(kept) != 0 {
		t.Fatalf("soft-deleted buckets after their objects expired = %+v", kept)
	}
}

func TestRestoreKeepsHoldsAndRetention(t *testing.T) {
	clk := newClock()
	s := storage.New(storage.WithClock(clk.Now))
	c := newClient(t, s)
	b := c.Bucket("restored-metadata")
	if err := b.SetObjectRetention(true).Create(t.Context(), project, &gcs.BucketAttrs{DefaultEventBasedHold: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := writeWith(t, c, "restored-metadata", "k", "v", gcs.ObjectAttrs{Metadata: map[string]string{"a": "1"}}); err != nil {
		t.Fatal(err)
	}
	until := clk.Now().Add(time.Hour)
	if _, err := b.Object("k").Update(t.Context(), gcs.ObjectAttrsToUpdate{
		EventBasedHold: false, Retention: &gcs.ObjectRetention{Mode: "Unlocked", RetainUntil: until},
	}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Hour)
	if err := b.Object("k").Delete(t.Context()); err != nil {
		t.Fatal(err)
	}
	gen := s.SoftDeletedObjects("restored-metadata")[0].Generation
	restored, err := b.Object("k").Generation(gen).Restore(t.Context(), &gcs.RestoreOptions{})
	if err != nil || restored.EventBasedHold || restored.Metadata["a"] != "1" || restored.Retention == nil || !restored.Retention.RetainUntil.Equal(until) {
		t.Fatalf("restore = %+v, %v", restored, err)
	}
}

func TestLiveBucketGetByGeneration(t *testing.T) {
	s := storage.New()
	url := serve(t, s)
	c, svc := clientAt(t, url), rawService(t, url)
	if err := c.Bucket("live").Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Buckets.Get("live").Context(t.Context()).Do()
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Buckets.Get("live").Generation(got.Generation).Context(t.Context()).Do()
	wantErr(t, "get a generation without softDeleted", err, http.StatusBadRequest, "required", "softDeleted")
}

func TestNullHoldReleases(t *testing.T) {
	s := storage.New()
	url := serve(t, s)
	c, svc := clientAt(t, url), rawService(t, url)
	if err := c.Bucket("nulls").Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := writeWith(t, c, "nulls", "k", "v", gcs.ObjectAttrs{TemporaryHold: true}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Objects.Patch("nulls", "k", &rawstorage.Object{NullFields: []string{"TemporaryHold"}}).Context(t.Context()).Do()
	if err != nil || got.TemporaryHold {
		t.Fatalf("patch a null hold = %+v, %v", got, err)
	}
	if err := c.Bucket("nulls").Object("k").Delete(t.Context()); err != nil {
		t.Fatalf("delete after a null hold: %v", err)
	}
}

func TestSoftDeleteEffectiveTimeFollowsHistory(t *testing.T) {
	clk := newClock()
	c := newClient(t, storage.New(storage.WithClock(clk.Now)))
	b := c.Bucket("history")
	if err := b.Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	day := 24 * time.Hour
	start := clk.Now()
	var marks []time.Time
	for _, step := range []struct {
		keep time.Duration
		want int
	}{
		{30 * day, 1},
		{7 * day, 0},
		{30 * day, 3},
		{14 * day, 3},
		{0, -1},
		{7 * day, 6},
	} {
		clk.Advance(time.Hour)
		marks = append(marks, clk.Now())
		got, err := b.Update(t.Context(), gcs.BucketAttrsToUpdate{SoftDeletePolicy: &gcs.SoftDeletePolicy{RetentionDuration: step.keep}})
		if err != nil {
			t.Fatal(err)
		}
		want := time.Time{}
		switch {
		case step.want == 0:
			want = start
		case step.want > 0:
			want = marks[step.want-1]
		}
		if !got.SoftDeletePolicy.EffectiveTime.Equal(want) {
			t.Fatalf("after %v at %v: effective %v, want %v", step.keep, clk.Now(), got.SoftDeletePolicy.EffectiveTime, want)
		}
	}
}

func TestReadModifyWriteUpdate(t *testing.T) {
	s := storage.New()
	url := serve(t, s)
	c, svc := clientAt(t, url), rawService(t, url)
	if err := c.Bucket("rmw").SetObjectRetention(true).Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := writeWith(t, c, "rmw", "k", "v", gcs.ObjectAttrs{
		Metadata: map[string]string{"a": "1"}, Retention: &gcs.ObjectRetention{Mode: "Unlocked", RetainUntil: time.Now().Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	o, err := svc.Objects.Get("rmw", "k").Context(t.Context()).Do()
	if err != nil {
		t.Fatal(err)
	}
	o.Metadata["b"] = "2"
	got, err := svc.Objects.Update("rmw", "k", o).Context(t.Context()).Do()
	if err != nil || got.Metadata["a"] != "1" || got.Metadata["b"] != "2" || got.Retention == nil || got.Metageneration != 2 {
		t.Fatalf("update with the body a get returned = %+v, %v", got, err)
	}
}

func TestSoftDeletedTimeDeleted(t *testing.T) {
	c := newClient(t, storage.New())
	for _, versioned := range []bool{false, true} {
		name := fmt.Sprintf("deleted-%t", versioned)
		b := c.Bucket(name)
		if err := b.Create(t.Context(), project, &gcs.BucketAttrs{VersioningEnabled: versioned}); err != nil {
			t.Fatal(err)
		}
		o := write(t, c, name, "k", "v")
		if err := b.Object("k").Delete(t.Context()); err != nil {
			t.Fatal(err)
		}
		if versioned {
			if err := b.Object("k").Generation(o.Generation).Delete(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		soft := objects(t, b.Objects(t.Context(), &gcs.Query{SoftDeleted: true}))
		if len(soft) != 1 || soft[0].Deleted.IsZero() != !versioned {
			t.Fatalf("versioned %t: soft-deleted = %+v", versioned, soft)
		}
	}
}

func TestUploadIfGenerationNotMatchNeedsALiveObject(t *testing.T) {
	c := newClient(t, storage.New())
	if err := c.Bucket("not-match").Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	w := c.Bucket("not-match").Object("k").If(gcs.Conditions{GenerationNotMatch: 5}).NewWriter(t.Context())
	_, _ = w.Write([]byte("x"))
	wantErr(t, "an upload with ifGenerationNotMatch and no live object", w.Close(), http.StatusPreconditionFailed, "conditionNotMet", "")
}

func TestBucketReadModifyWrite(t *testing.T) {
	s := storage.New()
	url := serve(t, s)
	c, svc := clientAt(t, url), rawService(t, url)
	if err := c.Bucket("rmw-bucket").Create(t.Context(), project, &gcs.BucketAttrs{
		Location: "us-east1", RetentionPolicy: &gcs.RetentionPolicy{RetentionPeriod: time.Hour},
	}); err != nil {
		t.Fatal(err)
	}
	b, err := svc.Buckets.Get("rmw-bucket").Context(t.Context()).Do()
	if err != nil {
		t.Fatal(err)
	}
	b.Labels = map[string]string{"team": "data"}
	b.SoftDeletePolicy.RetentionDurationSeconds = 0
	b.SoftDeletePolicy.ForceSendFields = []string{"RetentionDurationSeconds"}
	got, err := svc.Buckets.Patch("rmw-bucket", b).Context(t.Context()).Do()
	if err != nil || got.Labels["team"] != "data" || got.SoftDeletePolicy.RetentionDurationSeconds != 0 || got.RetentionPolicy.RetentionPeriod != 3600 {
		t.Fatalf("patch with a get's body = %+v, %v", got, err)
	}
}

func TestPatchRPO(t *testing.T) {
	c := newClient(t, storage.New())
	b := c.Bucket("rpo")
	if err := b.Create(t.Context(), project, &gcs.BucketAttrs{Location: "nam4"}); err != nil {
		t.Fatal(err)
	}
	got, err := b.Update(t.Context(), gcs.BucketAttrsToUpdate{RPO: gcs.RPOAsyncTurbo})
	if err != nil || got.RPO != gcs.RPOAsyncTurbo {
		t.Fatalf("patch rpo = %+v, %v", got, err)
	}
}
