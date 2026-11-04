package arm

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

// maxShareSnapshots is how many snapshots a file share holds at most.
const maxShareSnapshots = 200

// shareSnapshot is a snapshot of a file share: the share as it was when taken.
type shareSnapshot struct {
	res *resource
	at  time.Time
}

// expandOf parses the call's $expand, a comma-separated list of the values allowed.
func expandOf(c *call, allowed ...string) (map[string]bool, *apiError) {
	v := c.query("$expand")
	out := map[string]bool{}
	if v == "" {
		return out, nil
	}
	for item := range strings.SplitSeq(v, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		if !slices.Contains(allowed, item) {
			return nil, unsupported("$expand=%s is not supported", v)
		}
		out[item] = true
	}
	return out, nil
}

// snapshotHeader parses the call's x-ms-snapshot header, which is zero when it is not set.
func snapshotHeader(c *call) (time.Time, *apiError) {
	v := c.r.Header.Get("x-ms-snapshot")
	if v == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339Nano, v)
	if err != nil || at.IsZero() {
		return time.Time{}, invalidHeaderValue("x-ms-snapshot", v)
	}
	return at, nil
}

func shareSnapshotNotFound() *apiError {
	return newError(http.StatusNotFound, "ShareSnapshotNotFound", "The specified share snapshot does not exist.")
}

// shareSnapshotAt returns the index of the snapshot of share res taken at at, or -1.
func shareSnapshotAt(res *resource, at time.Time) int {
	return slices.IndexFunc(res.shareSnapshots, func(sn *shareSnapshot) bool { return sn.at.Equal(at) })
}

// addShareSnapshot snapshots the file share res at at, with metadata in
// place of the share's when it is not nil. The caller holds s.mu.
func (s *Server) addShareSnapshot(res *resource, at time.Time, metadata any) (*shareSnapshot, *apiError) {
	if len(res.shareSnapshots) >= maxShareSnapshots {
		return nil, newError(http.StatusConflict, "ShareSnapshotCountExceeded", "The total number of snapshots for the share is over the limit.")
	}
	cp := *res
	cp.props = maps.Clone(res.props)
	if cp.props == nil {
		cp.props = map[string]any{}
	}
	if metadata != nil {
		cp.props["metadata"] = metadata
	}
	cp.shareSnapshots = nil
	sn := &shareSnapshot{res: &cp, at: at}
	i, _ := slices.BinarySearchFunc(res.shareSnapshots, at, func(sn *shareSnapshot, at time.Time) int { return sn.at.Compare(at) })
	res.shareSnapshots = slices.Insert(res.shareSnapshots, i, sn)
	return sn, nil
}

// snapshotShare answers a create of a snapshot of the share the call names.
// The caller holds s.mu.
func (s *Server) snapshotShare(c *call, p *parsed) (*reply, *apiError) {
	res := s.resources[key(shares.id(c))]
	if res == nil {
		return nil, shares.notFound()
	}
	for _, k := range slices.Sorted(maps.Keys(p.props)) {
		if k != "metadata" {
			return nil, unsupported("a share snapshot takes only metadata, not %s", k)
		}
	}
	if len(p.tags) > 0 || len(p.top) > 0 {
		return nil, unsupported("a share snapshot takes only metadata")
	}
	at := s.now().UTC()
	if n := len(res.shareSnapshots); n > 0 && !at.After(res.shareSnapshots[n-1].at) {
		at = res.shareSnapshots[n-1].at.Add(100 * time.Nanosecond)
	}
	sn, err := s.addShareSnapshot(res, at, p.props["metadata"])
	if err != nil {
		return nil, err
	}
	return &reply{status: http.StatusCreated, body: encode(renderShareSnapshot(sn, false))}, nil
}

func renderShareSnapshot(sn *shareSnapshot, stats bool) map[string]any {
	m := renderShare(sn.res)
	props := m["properties"].(map[string]any)
	props["snapshotTime"] = timestamp(sn.at)
	if stats {
		props["shareUsageBytes"] = sn.res.usage
	}
	return m
}

// deleteShare answers a delete of the share res, which is nil when there is
// none, or of one of its snapshots. The caller holds s.mu.
func (s *Server) deleteShare(c *call, acct, res *resource) (*reply, *apiError) {
	at, err := snapshotHeader(c)
	if err != nil {
		return nil, err
	}
	include := c.query("$include")
	switch strings.ToLower(include) {
	case "", "snapshots", "leased-snapshots", "none":
	default:
		return nil, unsupported("$include=%s is not supported", include)
	}
	if res == nil {
		return &reply{status: http.StatusNoContent}, nil
	}
	if !at.IsZero() {
		i := shareSnapshotAt(res, at)
		if i < 0 {
			return &reply{status: http.StatusNoContent}, nil
		}
		res.shareSnapshots = slices.Delete(res.shareSnapshots, i, i+1)
		return &reply{status: http.StatusOK}, nil
	}
	if strings.EqualFold(include, "none") && len(res.shareSnapshots) > 0 {
		return nil, newError(http.StatusConflict, "ShareHasSnapshots", "The share has snapshots and the operation requires no snapshots.")
	}
	delete(s.resources, key(res.id))
	if keep := retentionDays(acct.account.services[fileService.path], "shareDeleteRetentionPolicy"); keep > 0 {
		acct.account.deletedShares = append(acct.account.deletedShares, s.newDeletedChild(res, s.now(), keep))
	}
	return &reply{status: http.StatusOK}, nil
}

// deletedShares returns the account's soft-deleted shares that are still
// kept, dropping the rest. The caller holds s.mu.
func (s *Server) deletedShares(acct *resource) []*deletedChild {
	now := s.now()
	acct.account.deletedShares = slices.DeleteFunc(acct.account.deletedShares, func(d *deletedChild) bool { return d.expired(now) })
	return acct.account.deletedShares
}

func (s *Server) restoreShare(c *call) (*reply, *apiError) {
	body, err := readObject(c.r)
	if err != nil {
		return nil, err
	}
	if err := checkMembers(body, "DeletedShare", "", "deletedShareName", "deletedShareVersion"); err != nil {
		return nil, err
	}
	name, version := str(body, "deletedShareName"), str(body, "deletedShareVersion")
	if name == "" || version == "" {
		return nil, badRequest("deletedShareName and deletedShareVersion are required.")
	}
	if name != c.val("name") {
		return nil, unsupported("restoring a share under another name is not supported")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acct, err := s.writableAccount(c, shareKind)
	if err != nil {
		return nil, err
	}
	if s.resources[key(shares.id(c))] != nil {
		return nil, shares.exists()
	}
	deleted := s.deletedShares(acct)
	i := slices.IndexFunc(deleted, func(d *deletedChild) bool { return d.res.name == name && strings.EqualFold(d.version, version) })
	if i < 0 {
		return nil, shares.notFound()
	}
	res := deleted[i].res
	acct.account.deletedShares = slices.Delete(deleted, i, i+1)
	s.resources[key(res.id)] = res
	return &reply{status: http.StatusOK}, nil
}

// shareOf returns the file share with ID id and the account holding it, or nil. The caller holds s.mu.
func (s *Server) shareOf(id string) (*resource, *resource) {
	res := s.resources[key(id)]
	if res == nil || res.kind != shareKind {
		return nil, nil
	}
	acct := s.accountOf(res)
	if acct == nil || acct.account == nil {
		return nil, nil
	}
	return res, acct
}

// SnapshotShare adds a snapshot taken at at to the file share with ID id, as
// a create of it with $expand=snapshots does. It reports whether the share
// exists and could take the snapshot: it has no other taken at at, and fewer
// than 200.
func (s *Server) SnapshotShare(id string, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, _ := s.shareOf(id)
	if res == nil || at.IsZero() || shareSnapshotAt(res, at) >= 0 {
		return false
	}
	_, err := s.addShareSnapshot(res, at.UTC(), nil)
	return err == nil
}

// SoftDeleteShare deletes the file share with ID id, with its snapshots, as
// if it had been deleted at at while its account kept deleted shares for
// days, whatever its account's shareDeleteRetentionPolicy. It returns the
// deleted share's version and reports whether the share exists and days is
// 1 to 365.
func (s *Server) SoftDeleteShare(id string, at time.Time, days int) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, acct := s.shareOf(id)
	if res == nil || days < 1 || days > 365 {
		return "", false
	}
	delete(s.resources, key(res.id))
	d := s.newDeletedChild(res, at, days)
	acct.account.deletedShares = append(acct.account.deletedShares, d)
	return d.version, true
}
