package arm

import (
	"cmp"
	"net/http"
	"slices"
	"strings"
	"time"
)

var (
	backupVaultKind = &kind{
		typ: "Microsoft.NetApp/netAppAccounts/backupVaults", plural: "backupVaults", typeName: "BackupVault",
		readOnly: []string{"provisioningState"},
		located:  true,
	}
	netappBackupKind = &kind{
		typ: "Microsoft.NetApp/netAppAccounts/backupVaults/backups", plural: "backups", typeName: "Backup",
	}
	backupPolicyKind = &kind{
		typ: "Microsoft.NetApp/netAppAccounts/backupPolicies", plural: "backupPolicies", typeName: "BackupPolicy",
		props:    []string{"dailyBackupsToKeep", "enabled", "monthlyBackupsToKeep", "weeklyBackupsToKeep"},
		readOnly: []string{"backupPolicyId", "provisioningState", "volumeBackups", "volumesAssigned"},
		located:  true, etag: weakETag,
	}
	snapshotPolicyKind = &kind{
		typ: "Microsoft.NetApp/netAppAccounts/snapshotPolicies", plural: "snapshotPolicies", typeName: "SnapshotPolicy",
		props:    []string{"dailySchedule", "enabled", "hourlySchedule", "monthlySchedule", "weeklySchedule"},
		readOnly: []string{"provisioningState"},
		located:  true, etag: weakETag,
	}
	volumeGroupKind = &kind{
		typ: "Microsoft.NetApp/netAppAccounts/volumeGroups", plural: "volumeGroups", typeName: "VolumeGroupDetails",
		props:    []string{"groupMetaData", "volumes"},
		readOnly: []string{"provisioningState"},
		located:  true,
	}
	cacheKind = &kind{
		typ: "Microsoft.NetApp/netAppAccounts/capacityPools/caches", plural: "caches", typeName: "Cache",
		top: []string{"zones"},
		props: []string{
			"cacheSubnetResourceId", "cifsChangeNotifications", "encryptionKeySource", "exportPolicy", "filePath",
			"globalFileLocking", "kerberos", "keyVaultPrivateEndpointResourceId", "ldap", "ldapServerType",
			"originClusterInformation", "peeringSubnetResourceId", "protocolTypes", "size", "smbSettings",
			"throughputMibps", "writeBack",
		},
		readOnly: []string{
			"actualThroughputMibps", "cacheState", "encryption", "fileAccessLogs", "language", "maximumNumberOfFiles",
			"mountTargets", "provisioningState",
		},
		located: true, etag: weakETag,
	}
)

const (
	backupVaultPath    = netappAccount + "/backupVaults/{vault}"
	netappBackupPath   = backupVaultPath + "/backups/{backup}"
	backupPolicyPath   = netappAccount + "/backupPolicies/{policy}"
	snapshotPolicyPath = netappAccount + "/snapshotPolicies/{policy}"
	volumeGroupPath    = netappAccount + "/volumeGroups/{volumeGroup}"
	cachePath          = netappPool + "/caches/{cache}"
)

func backupVaultID(c *call) string  { return netappAccountID(c) + "/backupVaults/" + c.val("vault") }
func netappBackupID(c *call) string { return backupVaultID(c) + "/backups/" + c.val("backup") }
func backupPolicyID(c *call) string { return netappAccountID(c) + "/backupPolicies/" + c.val("policy") }

func snapshotPolicyID(c *call) string {
	return netappAccountID(c) + "/snapshotPolicies/" + c.val("policy")
}

func volumeGroupID(c *call) string {
	return netappAccountID(c) + "/volumeGroups/" + c.val("volumeGroup")
}
func cacheID(c *call) string { return poolID(c) + "/caches/" + c.val("cache") }

// netappIDName is the name NetApp gives the resource of kind k with ID id.
// A volume group is named on its own, not after its account.
func netappIDName(k *kind, id string) string {
	if k == volumeGroupKind {
		return id[strings.LastIndex(id, "/")+1:]
	}
	return netappName(id)
}

func netappResourceName(res *resource) string { return netappIDName(res.kind, res.id) }

// netappParentMissing fails a read under the level l whose parent does not
// exist with 404 ResourceNotFound naming the topmost missing ancestor, as a
// read under a missing virtual network names it. The caller holds s.mu.
func (s *Server) netappParentMissing(c *call, l *netappLevel) *apiError {
	if l.parent == nil {
		return nil
	}
	parent := l.parent(c)
	if s.resources[key(parent)] != nil {
		return nil
	}
	prefix, rest, _ := strings.Cut(parent, "/providers/")
	parts := strings.Split(rest, "/")
	id, types := prefix+"/providers/"+parts[0], []string{parts[0]}
	for i := 1; i+1 < len(parts); i += 2 {
		id += "/" + parts[i] + "/" + parts[i+1]
		types = append(types, parts[i])
		if s.resources[key(id)] == nil {
			break
		}
	}
	return resourceNotFound(strings.Join(types, "/"), netappName(id), c.val("rg"))
}

// backupVolumeFilter reads which volume a backups list names, if any: its
// $filter, or its volumeResourceId parameter, is the volume's resource ID.
func backupVolumeFilter(c *call) (string, *apiError) {
	q := c.r.URL.Query()
	var ids []string
	for _, name := range []string{"$filter", "volumeResourceId"} {
		vals := q[name]
		if len(vals) > 1 {
			return "", unsupported("%s is given %d times", name, len(vals))
		}
		if len(vals) == 0 || strings.TrimSpace(vals[0]) == "" {
			continue
		}
		id := strings.TrimSpace(vals[0])
		if !isVolumeID(id) {
			return "", unsupported("the backups %s '%s' is not supported; give a NetApp volume's resource ID", name, id)
		}
		ids = append(ids, id)
	}
	if len(ids) == 2 && key(ids[0]) != key(ids[1]) {
		return "", unsupported("$filter '%s' and volumeResourceId '%s' name different volumes", ids[0], ids[1])
	}
	if len(ids) == 0 {
		return "", nil
	}
	return ids[0], nil
}

// isVolumeID reports whether id is a NetApp volume's resource ID, and nothing more.
func isVolumeID(id string) bool {
	want := []string{"subscriptions", "", "resourceGroups", "", "providers", "Microsoft.NetApp", "netAppAccounts", "", "capacityPools", "", "volumes", ""}
	parts := strings.Split(id, "/")
	if len(parts) != len(want)+1 || parts[0] != "" {
		return false
	}
	for i, w := range want {
		part := parts[i+1]
		if w == "" && (part == "" || strings.ContainsAny(part, " '\"")) || w != "" && !strings.EqualFold(part, w) {
			return false
		}
	}
	return true
}

// prepareVolumeGroup accepts a volume group that lists no volumes; creating
// volumes with their group is not modeled. The caller holds s.mu.
func (s *Server) prepareVolumeGroup(res *resource, p *parsed) (func(), *apiError) {
	if p.tags != nil {
		return nil, unsupported("a volume group takes no tags")
	}
	if v := p.props["volumes"]; v != nil {
		if vols, ok := v.([]any); !ok || len(vols) > 0 {
			return nil, unsupported("volume groups that create volumes are not modeled")
		}
	}
	delete(p.props, "volumes")
	return func() { s.update(res, p) }, nil
}

// prepareCache validates p as the new body of the cache res, and returns how
// to apply it. A new cache takes an address in its cache subnet. The caller
// holds s.mu.
func (s *Server) prepareCache(res *resource, p *parsed) (func(), *apiError) {
	subnet := str(p.props, "cacheSubnetResourceId")
	if subnet == "" || str(p.props, "peeringSubnetResourceId") == "" || str(p.props, "filePath") == "" ||
		str(p.props, "encryptionKeySource") == "" || p.props["size"] == nil || p.props["originClusterInformation"] == nil {
		return nil, unsupported("a cache needs cacheSubnetResourceId, encryptionKeySource, filePath, originClusterInformation, peeringSubnetResourceId and size")
	}
	return s.holdAddress(res, p, subnet, "cacheSubnetResourceId")
}

// volumesNaming returns the NetApp volumes whose dataProtection names the
// resource id as section.field, such as backup.backupPolicyId. The caller
// holds s.mu.
func (s *Server) volumesNaming(id, section, field string) []*resource {
	var out []*resource
	for _, res := range s.resources {
		if res.kind != volumeKind {
			continue
		}
		dp, _ := res.props["dataProtection"].(map[string]any)
		sec, _ := dp[section].(map[string]any)
		if ref := str(sec, field); ref != "" && key(ref) == key(id) {
			out = append(out, res)
		}
	}
	slices.SortFunc(out, func(a, b *resource) int { return cmp.Compare(key(a.id), key(b.id)) })
	return out
}

// groupVolumes returns the volumes in the volume group g, ordered by ID. The caller holds s.mu.
func (s *Server) groupVolumes(g *resource) []*resource {
	var out []*resource
	for _, res := range s.resources {
		if res.kind == volumeKind && res.volumeGroup != "" && key(res.volumeGroup) == key(g.id) {
			out = append(out, res)
		}
	}
	slices.SortFunc(out, func(a, b *resource) int { return cmp.Compare(key(a.id), key(b.id)) })
	return out
}

func (s *Server) renderGroupVolumes(g *resource) []any {
	out := []any{}
	for _, vol := range s.groupVolumes(g) {
		m := s.renderNetApp(vol)
		delete(m, "systemData")
		delete(m, "location")
		delete(m, "etag")
		out = append(out, m)
	}
	return out
}

// renderNetAppListed renders res as a list shows it: a volume group without its volumes or systemData.
func (s *Server) renderNetAppListed(res *resource) map[string]any {
	m := s.renderNetApp(res)
	if res.kind == volumeGroupKind {
		delete(m, "systemData")
		delete(m["properties"].(map[string]any), "volumes")
	}
	return m
}

// netappChildInUse fails the deletion of a backup vault that holds backups
// or that a volume backs up to, a policy a volume uses, or a volume group
// that holds volumes. The caller holds s.mu.
func (s *Server) netappChildInUse(res *resource) *apiError {
	if err := s.nestedInUse(res); err != nil {
		return err
	}
	var users []*resource
	switch res.kind {
	case backupVaultKind:
		users = s.volumesNaming(res.id, "backup", "backupVaultId")
	case backupPolicyKind:
		users = s.volumesNaming(res.id, "backup", "backupPolicyId")
	case snapshotPolicyKind:
		users = s.volumesNaming(res.id, "snapshot", "snapshotPolicyId")
	case volumeGroupKind:
		users = s.groupVolumes(res)
	}
	if len(users) == 0 {
		return nil
	}
	names := make([]string, 0, len(users))
	for _, u := range users {
		names = append(names, netappName(u.id))
	}
	return newError(http.StatusConflict, "CannotDeleteResource",
		"Can not delete resource '%s' while volumes use it: %s.", netappResourceName(res), strings.Join(names, ", "))
}

// addNetAppChild adds the finished resource of kind k named name under the
// NetApp resource parent of kind pk, created at at, as a finished create of
// it does. It returns nil unless the parent exists, once any create of it in
// progress finishes, has not failed and is not being deleted, and holds no
// resource of that name. The caller holds s.mu.
func (s *Server) addNetAppChild(parentID string, pk, k *kind, name string, at time.Time) *resource {
	if parent := s.resources[key(parentID)]; parent == nil || parent.kind != pk || parent.state == stateDeleting {
		return nil
	}
	parent := s.settled(parentID)
	if parent == nil || parent.state == stateFailed || name == "" || strings.Contains(name, "/") || at.IsZero() {
		return nil
	}
	id := parent.id + "/" + k.plural + "/" + name
	if s.resources[key(id)] != nil {
		return nil
	}
	at = at.UTC()
	res := &resource{
		kind: k, id: id, sub: parent.sub, group: parent.group, loc: parent.loc,
		top: map[string]any{}, props: map[string]any{}, state: stateSucceeded, created: at, modified: at, guid: s.newUUID(),
	}
	res.name = netappResourceName(res)
	s.touch(res)
	s.resources[key(id)] = res
	return res
}

func (s *Server) addAccountChild(accountID string, k *kind, name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := s.addNetAppChild(accountID, netappAccountKind, k, name, s.now())
	if res == nil {
		return "", false
	}
	return res.id, true
}

// AddBackupVault adds the backup vault name to the NetApp account with ID id,
// as a finished create of it does. It returns the vault's ID and reports
// whether the account exists, once any create of it in progress finishes,
// has not failed and is not being deleted, and has no vault of that name.
func (s *Server) AddBackupVault(id, name string) (string, bool) {
	return s.addAccountChild(id, backupVaultKind, name)
}

// AddBackupPolicy adds the backup policy name to the NetApp account with ID
// id, as AddBackupVault adds a vault.
func (s *Server) AddBackupPolicy(id, name string) (string, bool) {
	return s.addAccountChild(id, backupPolicyKind, name)
}

// AddSnapshotPolicy adds the snapshot policy name to the NetApp account with
// ID id, as AddBackupVault adds a vault.
func (s *Server) AddSnapshotPolicy(id, name string) (string, bool) {
	return s.addAccountChild(id, snapshotPolicyKind, name)
}

// AddBackup adds the backup name of the NetApp volume with ID volume, taken
// at at, to the backup vault with ID vault, as a finished backup does. The
// volume need not exist, as backups outlive their volumes. It returns the
// backup's ID and reports whether the vault exists, once any create of it in
// progress finishes, has not failed and is not being deleted, and has no
// backup of that name.
func (s *Server) AddBackup(vault, name, volume string, at time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if volume == "" {
		return "", false
	}
	res := s.addNetAppChild(vault, backupVaultKind, netappBackupKind, name, at)
	if res == nil {
		return "", false
	}
	res.props["volumeResourceId"] = volume
	return res.id, true
}

// AddVolumeGroup adds the volume group name, holding the NetApp volumes with
// IDs volumes, to the NetApp account with ID id, as a finished create of it
// does. It returns the group's ID and reports whether the account exists, has
// not failed, is not being deleted and has no volume group of that name, and
// whether each volume exists in the account, has not failed, is not being
// deleted and is in no other group. It checks all of that before finishing
// any change of the account or volumes in progress, so a refusal leaves them
// as they were.
func (s *Server) AddVolumeGroup(id, name string, volumes ...string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acct := s.resources[key(id)]
	if acct == nil || acct.kind != netappAccountKind || acct.state == stateDeleting || acct.state == stateFailed ||
		name == "" || strings.Contains(name, "/") || s.resources[key(acct.id+"/volumeGroups/"+name)] != nil {
		return "", false
	}
	seen := map[string]bool{}
	for _, v := range volumes {
		vol := s.resources[key(v)]
		if vol == nil || vol.kind != volumeKind || vol.state == stateDeleting || vol.state == stateFailed || vol.volumeGroup != "" ||
			!strings.HasPrefix(key(vol.id), key(acct.id)+"/") || seen[key(v)] {
			return "", false
		}
		seen[key(v)] = true
	}
	res := s.addNetAppChild(id, netappAccountKind, volumeGroupKind, name, s.now())
	if res == nil {
		return "", false
	}
	for _, v := range volumes {
		if vol := s.settled(v); vol != nil {
			vol.volumeGroup = res.id
		}
	}
	return res.id, true
}

// AddCache adds the cache name to the NetApp capacity pool with ID id, as a
// finished create of it does, without a mount target. It returns the cache's
// ID and reports whether the pool exists, once any create of it in progress
// finishes, has not failed and is not being deleted, and has no cache of that
// name.
func (s *Server) AddCache(id, name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := s.addNetAppChild(id, poolKind, cacheKind, name, s.now())
	if res == nil {
		return "", false
	}
	return res.id, true
}
