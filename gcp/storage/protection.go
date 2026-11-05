package storage

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Retention is an object's own retention, with Mode "Unlocked" or "Locked".
type Retention struct {
	Mode            string `json:"mode"`
	RetainUntilTime string `json:"retainUntilTime"`
}

const untilFormat = "2006-01-02T15:04:05.000000Z07:00"

func validRetentionPeriod(seconds int64) bool { return seconds > 0 && seconds <= maxRetentionPeriod }

func validSoftDelete(seconds int64) bool {
	return seconds == 0 || seconds >= minSoftDelete && seconds <= maxSoftDelete
}

func badRetentionPeriod() *apiError {
	return invalid("Retention policy must have a retention period greater than 0 and less than 100 years.")
}

func badSoftDelete() *apiError {
	return invalid("Soft delete policy must have a retention duration between 7 days and 90 days.")
}

// nextRetentionPolicy is b's retention policy after a patch sets it to in, or removes it. The caller holds s.mu.
func (s *Server) nextRetentionPolicy(b *bucket, in *retentionPolicyInput, removed bool) (*RetentionPolicy, *apiError) {
	old := b.RetentionPolicy
	locked := old != nil && old.IsLocked
	if removed {
		if locked {
			return nil, &apiError{
				Status: http.StatusForbidden, Reason: "retentionPolicyNotMet",
				Message: fmt.Sprintf("Bucket '%s' has a locked Retention Policy which cannot be removed.", b.Name),
			}
		}
		return nil, nil
	}
	if in == nil {
		return old, nil
	}
	if in.RetentionPeriod == nil || !validRetentionPeriod(int64(*in.RetentionPeriod)) {
		return nil, badRetentionPeriod()
	}
	period := int64(*in.RetentionPeriod)
	if locked && period < old.RetentionPeriod {
		return nil, &apiError{
			Status: http.StatusForbidden, Reason: "forbidden",
			Message: fmt.Sprintf("Cannot reduce retention duration of a locked Retention Policy for bucket '%s'.", b.Name),
		}
	}
	if old != nil && old.RetentionPeriod == period {
		return new(*old), nil
	}
	_, now := s.timestamp()
	return &RetentionPolicy{RetentionPeriod: period, EffectiveTime: now, IsLocked: locked}, nil
}

// nextSoftDeletePolicy is b's soft delete policy after a patch sets it to in. The caller holds s.mu.
func (s *Server) nextSoftDeletePolicy(b *bucket, in *softDeletePolicyInput) (*SoftDeletePolicy, *apiError) {
	if in == nil {
		return b.SoftDeletePolicy, nil
	}
	if in.RetentionDurationSeconds == nil || !validSoftDelete(int64(*in.RetentionDurationSeconds)) {
		return nil, badSoftDelete()
	}
	seconds := int64(*in.RetentionDurationSeconds)
	if old := b.SoftDeletePolicy; old != nil && old.RetentionDurationSeconds == seconds {
		return new(*old), nil
	}
	p := &SoftDeletePolicy{RetentionDurationSeconds: seconds}
	if seconds > 0 {
		p.EffectiveTime = effectiveSince(b.softDeleteHistory, seconds, s.now()).UTC().Format(timeFormat)
	}
	return p, nil
}

// policyStep is a soft delete duration and when it took effect.
type policyStep struct {
	seconds int64
	since   time.Time
}

// effectiveSince is when the latest unbroken run of durations at least seconds began, ending with one set at now.
func effectiveSince(history []policyStep, seconds int64, now time.Time) time.Time {
	since := now
	for i := len(history) - 1; i >= 0 && history[i].seconds >= seconds; i-- {
		since = history[i].since
	}
	return since
}

// recordSoftDelete adds b's soft delete duration to its history if it changed. The caller holds s.mu.
func (s *Server) recordSoftDelete(b *bucket) {
	seconds := b.SoftDeletePolicy.RetentionDurationSeconds
	if n := len(b.softDeleteHistory); n == 0 || b.softDeleteHistory[n-1].seconds != seconds {
		b.softDeleteHistory = append(b.softDeleteHistory, policyStep{seconds: seconds, since: s.now()})
	}
}

func (s *Server) lockRetentionPolicy(r *http.Request) (any, *apiError) {
	metageneration, ok, err := int64Param(r, "ifMetagenerationMatch")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, required("ifMetagenerationMatch")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(r.PathValue("bucket"), "storage.buckets.update")
	if err != nil {
		return nil, err
	}
	if metageneration != b.Metageneration {
		return nil, conditionNotMet()
	}
	if b.RetentionPolicy == nil || b.RetentionPolicy.IsLocked {
		return nil, invalid("Bucket '%s' does not have an unlocked retention policy.", b.Name)
	}
	b.RetentionPolicy.IsLocked = true
	s.touch(b)
	return b.clone(), nil
}

func isSet(hold *bool) bool { return hold != nil && *hold }

// retainedUntil is when o may leave b: the later of the end of b's retention policy and o's own retention.
func (b *bucket) retainedUntil(o *Object) time.Time {
	var until time.Time
	if p := b.RetentionPolicy; p != nil && !isSet(o.EventBasedHold) {
		until = o.retentionStart.Add(time.Duration(p.RetentionPeriod) * time.Second)
	}
	if o.Retention != nil {
		if t, err := time.Parse(time.RFC3339Nano, o.Retention.RetainUntilTime); err == nil && t.After(until) {
			until = t
		}
	}
	return until
}

// guard returns why o cannot be deleted or replaced, or with archive, made noncurrent.
func (s *Server) guard(b *bucket, o *Object, archive bool) *apiError {
	kind := ""
	switch {
	case isSet(o.TemporaryHold):
		kind = "Temporary"
	case isSet(o.EventBasedHold):
		kind = "Event-Based"
	}
	if kind != "" {
		return &apiError{
			Status: http.StatusForbidden, Reason: "forbidden",
			Message: fmt.Sprintf("Object '%s/%s' is under active %s hold and cannot be deleted, overwritten or archived until hold is removed.", b.Name, o.Name, kind),
		}
	}
	if archive {
		return nil
	}
	if until := b.retainedUntil(o); s.now().Before(until) {
		return &apiError{
			Status: http.StatusForbidden, Reason: "retentionPolicyNotMet",
			Message: fmt.Sprintf("Object '%s/%s' is subject to bucket's retention policy or object retention and cannot be deleted or overwritten until %s",
				b.Name, o.Name, until.UTC().Format(untilFormat)),
		}
	}
	return nil
}

func retentionConflict() *apiError {
	return invalid("An object cannot have both an event-based hold and a retention configuration.")
}

// checkRetention returns next as stored if an object with retention old may take it; nil removes it.
func (s *Server) checkRetention(b *bucket, old, next *Retention, override, eventHold bool) (*Retention, *apiError) {
	var until time.Time
	if next != nil {
		if b.ObjectRetention == nil {
			return nil, &apiError{
				Status: http.StatusForbidden, Reason: "forbidden",
				Message: "The caller does not have storage.objects.setRetention access to the Google Cloud Storage object.",
			}
		}
		switch {
		case next.Mode == "":
			return nil, invalid("Missing retention mode. Both mode and retain until time are required.")
		case next.RetainUntilTime == "":
			return nil, invalid("Missing retain until time. Both mode and retain until time are required.")
		case next.Mode != "Unlocked" && next.Mode != "Locked":
			return nil, invalid("Invalid value for: %s is not a valid value", next.Mode)
		}
		t, err := time.Parse(time.RFC3339Nano, next.RetainUntilTime)
		if err != nil {
			return nil, invalid("Invalid value for: %s is not a valid value", next.RetainUntilTime)
		}
		if old != nil && !eventHold && old.Mode == next.Mode && old.RetainUntilTime == t.UTC().Format(timeFormat) {
			return old, nil
		}
		if t.Before(s.now()) {
			return nil, invalid("Retain until time cannot be in the past.")
		}
		if eventHold {
			return nil, retentionConflict()
		}
		until = t
		next = &Retention{Mode: next.Mode, RetainUntilTime: t.UTC().Format(timeFormat)}
	}
	if old != nil {
		oldUntil, _ := time.Parse(time.RFC3339Nano, old.RetainUntilTime)
		shortened := next == nil || until.Before(oldUntil)
		switch {
		case old.Mode == "Locked" && (shortened || next.Mode != "Locked"):
			return nil, &apiError{
				Status: http.StatusForbidden, Reason: "forbidden",
				Message: "The locked object retention cannot be removed. Its retention mode cannot be changed and its retention period cannot be shortened.",
			}
		case old.Mode != "Locked" && (shortened || next.Mode == "Locked") && !override:
			return nil, &apiError{
				Status: http.StatusForbidden, Reason: "forbidden",
				Message: "The unlocked object retention cannot be removed and its retention period cannot be shortened without overriding unlocked retention intent and permission.",
			}
		}
	}
	return next, nil
}

// SetRetentionPolicy sets or, with nil, removes a bucket's retention policy, locked or not, without a patch's checks.
func (s *Server) SetRetentionPolicy(bucketName string, p *RetentionPolicy) (Bucket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucketName]
	if !ok {
		return Bucket{}, notFoundBucket()
	}
	if p != nil {
		if !validRetentionPeriod(p.RetentionPeriod) {
			return Bucket{}, badRetentionPeriod()
		}
		_, now := s.timestamp()
		p = &RetentionPolicy{RetentionPeriod: p.RetentionPeriod, EffectiveTime: cmp.Or(p.EffectiveTime, now), IsLocked: p.IsLocked}
	}
	b.RetentionPolicy = p
	s.touch(b)
	return b.clone(), nil
}

// SetObjectHolds sets the holds on a live object; releasing an event-based hold restarts its retention period.
func (s *Server) SetObjectHolds(bucketName, name string, temporary, eventBased bool) (Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, o, err := s.liveForHelper(bucketName, name)
	if err != nil {
		return Object{}, err
	}
	if eventBased && o.Retention != nil {
		return Object{}, retentionConflict()
	}
	s.setHolds(o, &temporary, &eventBased)
	s.touchObject(o)
	return s.view(b, o), nil
}

// SetObjectRetention sets or, with nil, removes a live object's retention without a patch's checks.
func (s *Server) SetObjectRetention(bucketName, name string, r *Retention) (Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, o, err := s.liveForHelper(bucketName, name)
	if err != nil {
		return Object{}, err
	}
	if r != nil {
		t, err := time.Parse(time.RFC3339Nano, r.RetainUntilTime)
		if err != nil || r.Mode != "Unlocked" && r.Mode != "Locked" {
			return Object{}, fmt.Errorf("squall: invalid retention %+v", *r)
		}
		r = &Retention{Mode: r.Mode, RetainUntilTime: t.UTC().Format(timeFormat)}
	}
	o.Retention = r
	s.touchObject(o)
	return s.view(b, o), nil
}

// liveForHelper returns the named bucket and name's live generation in it. The caller holds s.mu.
func (s *Server) liveForHelper(bucketName, name string) (*bucket, *Object, error) {
	b, ok := s.buckets[bucketName]
	if !ok {
		return nil, nil, notFoundBucket()
	}
	o := b.liveObject(name)
	if o == nil {
		return nil, nil, errors.New("squall: " + noSuchObject(bucketName, name).Message)
	}
	return b, o, nil
}
