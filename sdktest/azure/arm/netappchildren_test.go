package arm_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/netapp/armnetapp/v10"

	"github.com/parallelworks/squall/azure/arm"
)

// createNetAppAccount creates the NetApp account acct.
func (c *clients) createNetAppAccount(t *testing.T, acct string) armnetapp.Account {
	t.Helper()
	p, err := c.netapps.BeginCreateOrUpdate(t.Context(), group, acct, armnetapp.Account{Location: new(location)}, nil)
	res, err := wait(t, p, err)
	if err != nil {
		t.Fatal(err)
	}
	return res.Account
}

// accountOf is the ID of the NetApp account that holds the resource id.
func accountOf(id string) string {
	acct, _, _ := strings.Cut(id, "/capacityPools/")
	return acct
}

// poolOf is the ID of the capacity pool that holds the volume id.
func poolOf(id string) string {
	pool, _, _ := strings.Cut(id, "/volumes/")
	return pool
}

func (c *clients) deleteNetAppAccount(t *testing.T, acct string) error {
	t.Helper()
	p, err := c.netapps.BeginDelete(t.Context(), group, acct, nil)
	_, err = wait(t, p, err)
	return err
}

// accountChild is a type of resource a NetApp account holds, served through its client.
type accountChild struct {
	name string
	typ  string
	// tags reports whether the type takes tags.
	tags   bool
	create func(c *clients, acct, name string, tags map[string]*string) (id, gotName string, sd *armnetapp.SystemData, err error)
	get    func(c *clients, acct, name string) (string, error)
	list   func(c *clients, acct string) ([]string, error)
	delete func(c *clients, acct, name string) error
	add    func(s *arm.Server, acct, name string) (string, bool)
}

func pagerNames[T any](t *testing.T, more func() bool, next func() (T, error), names func(T) []string) ([]string, error) {
	t.Helper()
	var out []string
	for more() {
		page, err := next()
		if err != nil {
			return nil, err
		}
		out = append(out, names(page)...)
	}
	return out, nil
}

func accountChildren(t *testing.T) []accountChild {
	t.Helper()
	return []accountChild{
		{
			name: "backup vault", typ: "Microsoft.NetApp/netAppAccounts/backupVaults", tags: true,
			create: func(c *clients, acct, name string, tags map[string]*string) (string, string, *armnetapp.SystemData, error) {
				p, err := c.anf.NewBackupVaultsClient().BeginCreateOrUpdate(t.Context(), group, acct, name, armnetapp.BackupVault{
					Location: new(location), Tags: tags,
				}, nil)
				res, err := wait(t, p, err)
				if err != nil {
					return "", "", nil, err
				}
				if *res.Properties.ProvisioningState != "Succeeded" || len(res.Tags) != len(tags) {
					t.Fatalf("backup vault = %+v", res.BackupVault)
				}
				return *res.ID, *res.Name, res.SystemData, nil
			},
			get: func(c *clients, acct, name string) (string, error) {
				res, err := c.anf.NewBackupVaultsClient().Get(t.Context(), group, acct, name, nil)
				if err != nil {
					return "", err
				}
				return *res.Name, nil
			},
			list: func(c *clients, acct string) ([]string, error) {
				p := c.anf.NewBackupVaultsClient().NewListByNetAppAccountPager(group, acct, nil)
				return pagerNames(t, p.More, func() (armnetapp.BackupVaultsClientListByNetAppAccountResponse, error) {
					return p.NextPage(t.Context())
				},
					func(r armnetapp.BackupVaultsClientListByNetAppAccountResponse) []string {
						var out []string
						for _, v := range r.Value {
							out = append(out, *v.Name)
						}
						return out
					})
			},
			delete: func(c *clients, acct, name string) error {
				p, err := c.anf.NewBackupVaultsClient().BeginDelete(t.Context(), group, acct, name, nil)
				_, err = wait(t, p, err)
				return err
			},
			add: (*arm.Server).AddBackupVault,
		},
		{
			name: "backup policy", typ: "Microsoft.NetApp/netAppAccounts/backupPolicies", tags: true,
			create: func(c *clients, acct, name string, tags map[string]*string) (string, string, *armnetapp.SystemData, error) {
				p, err := c.anf.NewBackupPoliciesClient().BeginCreate(t.Context(), group, acct, name, armnetapp.BackupPolicy{
					Location: new(location), Tags: tags,
					Properties: &armnetapp.BackupPolicyProperties{
						DailyBackupsToKeep: new(int32(10)), WeeklyBackupsToKeep: new(int32(4)), MonthlyBackupsToKeep: new(int32(6)), Enabled: new(true),
					},
				}, nil)
				res, err := wait(t, p, err)
				if err != nil {
					return "", "", nil, err
				}
				props := res.Properties
				if *props.DailyBackupsToKeep != 10 || props.BackupPolicyID == nil || *props.VolumesAssigned != 0 || res.Etag == nil {
					t.Fatalf("backup policy = %+v", res.BackupPolicy)
				}
				return *res.ID, *res.Name, res.SystemData, nil
			},
			get: func(c *clients, acct, name string) (string, error) {
				res, err := c.anf.NewBackupPoliciesClient().Get(t.Context(), group, acct, name, nil)
				if err != nil {
					return "", err
				}
				return *res.Name, nil
			},
			list: func(c *clients, acct string) ([]string, error) {
				p := c.anf.NewBackupPoliciesClient().NewListPager(group, acct, nil)
				return pagerNames(t, p.More, func() (armnetapp.BackupPoliciesClientListResponse, error) { return p.NextPage(t.Context()) },
					func(r armnetapp.BackupPoliciesClientListResponse) []string {
						var out []string
						for _, v := range r.Value {
							out = append(out, *v.Name)
						}
						return out
					})
			},
			delete: func(c *clients, acct, name string) error {
				p, err := c.anf.NewBackupPoliciesClient().BeginDelete(t.Context(), group, acct, name, nil)
				_, err = wait(t, p, err)
				return err
			},
			add: (*arm.Server).AddBackupPolicy,
		},
		{
			name: "snapshot policy", typ: "Microsoft.NetApp/netAppAccounts/snapshotPolicies", tags: true,
			create: func(c *clients, acct, name string, tags map[string]*string) (string, string, *armnetapp.SystemData, error) {
				res, err := c.anf.NewSnapshotPoliciesClient().Create(t.Context(), group, acct, name, armnetapp.SnapshotPolicy{
					Location: new(location), Tags: tags,
					Properties: &armnetapp.SnapshotPolicyProperties{
						Enabled: new(true), HourlySchedule: &armnetapp.HourlySchedule{Minute: new(int32(5)), SnapshotsToKeep: new(int32(2))},
					},
				}, nil)
				if err != nil {
					return "", "", nil, err
				}
				if *res.Properties.ProvisioningState != "Succeeded" || *res.Properties.HourlySchedule.Minute != 5 {
					t.Fatalf("snapshot policy = %+v", res.SnapshotPolicy)
				}
				return *res.ID, *res.Name, res.SystemData, nil
			},
			get: func(c *clients, acct, name string) (string, error) {
				res, err := c.anf.NewSnapshotPoliciesClient().Get(t.Context(), group, acct, name, nil)
				if err != nil {
					return "", err
				}
				return *res.Name, nil
			},
			list: func(c *clients, acct string) ([]string, error) {
				p := c.anf.NewSnapshotPoliciesClient().NewListPager(group, acct, nil)
				return pagerNames(t, p.More, func() (armnetapp.SnapshotPoliciesClientListResponse, error) { return p.NextPage(t.Context()) },
					func(r armnetapp.SnapshotPoliciesClientListResponse) []string {
						var out []string
						for _, v := range r.Value {
							out = append(out, *v.Name)
						}
						return out
					})
			},
			delete: func(c *clients, acct, name string) error {
				p, err := c.anf.NewSnapshotPoliciesClient().BeginDelete(t.Context(), group, acct, name, nil)
				_, err = wait(t, p, err)
				return err
			},
			add: (*arm.Server).AddSnapshotPolicy,
		},
		{
			name: "volume group", typ: "Microsoft.NetApp/netAppAccounts/volumeGroups",
			create: func(c *clients, acct, name string, _ map[string]*string) (string, string, *armnetapp.SystemData, error) {
				p, err := c.anf.NewVolumeGroupsClient().BeginCreate(t.Context(), group, acct, name, armnetapp.VolumeGroupDetails{
					Location: new(location),
					Properties: &armnetapp.VolumeGroupProperties{GroupMetaData: &armnetapp.VolumeGroupMetaData{
						ApplicationType: new(armnetapp.ApplicationTypeORACLE), ApplicationIdentifier: new("OR1"),
					}},
				}, nil)
				res, err := wait(t, p, err)
				if err != nil {
					return "", "", nil, err
				}
				if *res.Properties.GroupMetaData.ApplicationIdentifier != "OR1" || len(res.Properties.Volumes) != 0 {
					t.Fatalf("volume group = %+v", res.VolumeGroupDetails)
				}
				return *res.ID, *res.Name, res.SystemData, nil
			},
			get: func(c *clients, acct, name string) (string, error) {
				res, err := c.anf.NewVolumeGroupsClient().Get(t.Context(), group, acct, name, nil)
				if err != nil {
					return "", err
				}
				return *res.Name, nil
			},
			list: func(c *clients, acct string) ([]string, error) {
				p := c.anf.NewVolumeGroupsClient().NewListByNetAppAccountPager(group, acct, nil)
				return pagerNames(t, p.More, func() (armnetapp.VolumeGroupsClientListByNetAppAccountResponse, error) {
					return p.NextPage(t.Context())
				},
					func(r armnetapp.VolumeGroupsClientListByNetAppAccountResponse) []string {
						var out []string
						for _, v := range r.Value {
							out = append(out, *v.Name)
						}
						return out
					})
			},
			delete: func(c *clients, acct, name string) error {
				p, err := c.anf.NewVolumeGroupsClient().BeginDelete(t.Context(), group, acct, name, nil)
				_, err = wait(t, p, err)
				return err
			},
			add: func(s *arm.Server, acct, name string) (string, bool) { return s.AddVolumeGroup(acct, name) },
		},
	}
}

func TestNetAppAccountChildren(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, k := range accountChildren(t) {
		t.Run(k.name, func(t *testing.T) {
			c := setup(t, arm.WithSettleAfter(1), arm.WithClock(func() time.Time { return created }))
			c.createGroup(t, group, nil)
			if _, err := k.list(c, "acct"); code(err) != "ResourceNotFound" || status(err) != http.StatusNotFound ||
				!strings.Contains(err.Error(), "Microsoft.NetApp/netAppAccounts/acct'") {
				t.Fatalf("listing under a missing account = %v", err)
			}
			if _, err := k.get(c, "acct", "child"); code(err) != "ResourceNotFound" || status(err) != http.StatusNotFound ||
				!strings.Contains(err.Error(), "Microsoft.NetApp/netAppAccounts/acct'") {
				t.Fatalf("a get under a missing account = %v", err)
			}
			if _, _, _, err := k.create(c, "acct", "child", nil); code(err) != "ParentResourceNotFound" || status(err) != http.StatusNotFound {
				t.Fatalf("creating under a missing account = %v", err)
			}
			acct := c.createNetAppAccount(t, "acct")
			if names, err := k.list(c, "acct"); err != nil || len(names) != 0 {
				t.Fatalf("listing an empty account = %v, %v", names, err)
			}
			var tags map[string]*string
			if k.tags {
				tags = map[string]*string{"team": new("storage")}
			}
			id, name, sd, err := k.create(c, "acct", "child", tags)
			if err != nil {
				t.Fatal(err)
			}
			want := "acct/child"
			if k.name == "volume group" {
				want = "child"
			}
			if !strings.EqualFold(id, *acct.ID+"/"+k.typ[strings.LastIndex(k.typ, "/")+1:]+"/child") || name != want {
				t.Fatalf("created %s = %s named %s", k.name, id, name)
			}
			checkNetAppSystemData(t, k.name, sd, created, created)
			if got, err := k.get(c, "acct", "child"); err != nil || got != want {
				t.Fatalf("get = %q, %v", got, err)
			}
			if _, err := k.get(c, "acct", "other"); code(err) != "ResourceNotFound" || status(err) != http.StatusNotFound {
				t.Fatalf("get of a missing %s = %v", k.name, err)
			}
			if _, ok := k.add(c.server, *acct.ID, "added"); !ok {
				t.Fatalf("adding a %s failed", k.name)
			}
			if _, ok := k.add(c.server, *acct.ID, "added"); ok {
				t.Fatalf("added a %s twice", k.name)
			}
			if _, ok := k.add(c.server, *acct.ID+"x", "added"); ok {
				t.Fatalf("added a %s to a missing account", k.name)
			}
			addedName := "acct/added"
			if k.name == "volume group" {
				addedName = "added"
			}
			if names, err := k.list(c, "acct"); err != nil || !slices.Equal(names, []string{addedName, want}) {
				t.Fatalf("listed = %v, %v", names, err)
			}
			if err := c.deleteNetAppAccount(t, "acct"); code(err) != "CannotDeleteResource" || status(err) != http.StatusConflict {
				t.Fatalf("deleting an account that holds a %s = %v", k.name, err)
			}
			for _, n := range []string{"child", "added", "child"} {
				if err := k.delete(c, "acct", n); err != nil {
					t.Fatalf("deleting %s = %v", n, err)
				}
			}
			if _, err := k.get(c, "acct", "child"); code(err) != "ResourceNotFound" {
				t.Fatalf("a deleted %s = %v", k.name, err)
			}
			if err := c.deleteNetAppAccount(t, "acct"); err != nil {
				t.Fatalf("deleting the emptied account = %v", err)
			}
		})
	}
}

func TestNetAppBackupVaultHoldsBackups(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	vol, err := c.createVolume(t, "acct", "pool", "vol")
	if err != nil {
		t.Fatal(err)
	}
	vault, ok := c.server.AddBackupVault(accountOf(*vol.ID), "vault")
	if !ok {
		t.Fatal("AddBackupVault failed")
	}
	backups := c.anf.NewBackupsClient()
	if _, err := backups.NewListByVaultPager(group, "acct", "other", nil).NextPage(t.Context()); code(err) != "ResourceNotFound" {
		t.Fatalf("listing backups of a missing vault = %v", err)
	}
	if page, err := backups.NewListByVaultPager(group, "acct", "vault", nil).NextPage(t.Context()); err != nil || len(page.Value) != 0 {
		t.Fatalf("backups of an empty vault = %+v, %v", page.Value, err)
	}
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	id, ok := c.server.AddBackup(vault, "b1", *vol.ID, at)
	if !ok || id != vault+"/backups/b1" {
		t.Fatalf("AddBackup = %q, %v", id, ok)
	}
	if _, ok := c.server.AddBackup(vault, "b1", *vol.ID, at); ok {
		t.Fatal("AddBackup took a name a backup has")
	}
	if _, ok := c.server.AddBackup(vault+"x", "b2", *vol.ID, at); ok {
		t.Fatal("AddBackup added to a missing vault")
	}
	got, err := backups.Get(t.Context(), group, "acct", "vault", "b1", nil)
	if err != nil {
		t.Fatal(err)
	}
	props := got.Properties
	if *got.Name != "acct/vault/b1" || *got.Type != "Microsoft.NetApp/netAppAccounts/backupVaults/backups" ||
		*props.VolumeResourceID != *vol.ID || !props.CreationDate.Equal(at) || props.BackupID == nil || *props.ProvisioningState != "Succeeded" {
		t.Fatalf("backup = %+v %+v", got.Backup, props)
	}
	checkNetAppSystemData(t, "backup", got.SystemData, at, at)
	other := strings.Replace(*vol.ID, "/volumes/vol", "/volumes/other", 1)
	if _, ok := c.server.AddBackup(vault, "b2", other, at); !ok {
		t.Fatal("AddBackup of another volume failed")
	}
	listed := func(filter *string) ([]string, error) {
		page, err := backups.NewListByVaultPager(group, "acct", "vault", &armnetapp.BackupsClientListByVaultOptions{Filter: filter}).NextPage(t.Context())
		var names []string
		for _, b := range page.Value {
			names = append(names, *b.Name)
		}
		return names, err
	}
	if names, err := listed(nil); err != nil || !slices.Equal(names, []string{"acct/vault/b1", "acct/vault/b2"}) {
		t.Fatalf("backups of the vault = %v, %v", names, err)
	}
	if names, err := listed(vol.ID); err != nil || !slices.Equal(names, []string{"acct/vault/b1"}) {
		t.Fatalf("backups of one volume = %v, %v", names, err)
	}
	if names, err := listed(new(strings.ToUpper(other))); err != nil || !slices.Equal(names, []string{"acct/vault/b2"}) {
		t.Fatalf("backups of a volume named in upper case = %v, %v", names, err)
	}
	for _, f := range []string{"backupType eq 'Manual'", "volumeResourceId eq '" + other + "'", "vol"} {
		if _, err := listed(new(f)); code(err) != "BadRequest" || status(err) != http.StatusBadRequest {
			t.Fatalf("backups listed with $filter %s = %v", f, err)
		}
	}
	b2p, err := backups.BeginDelete(t.Context(), group, "acct", "vault", "b2", nil)
	if _, err = wait(t, b2p, err); err != nil {
		t.Fatal(err)
	}

	vp, err := c.anf.NewBackupVaultsClient().BeginDelete(t.Context(), group, "acct", "vault", nil)
	if _, err = wait(t, vp, err); code(err) != "CannotDeleteResource" || status(err) != http.StatusConflict {
		t.Fatalf("deleting a vault that holds a backup = %v", err)
	}
	dv, err := c.volumes.BeginDelete(t.Context(), group, "acct", "pool", "vol", nil)
	if _, err = wait(t, dv, err); err != nil {
		t.Fatal(err)
	}
	if _, err := backups.Get(t.Context(), group, "acct", "vault", "b1", nil); err != nil {
		t.Fatalf("a backup after its volume's deletion = %v", err)
	}
	bp, err := backups.BeginDelete(t.Context(), group, "acct", "vault", "b1", nil)
	if _, err = wait(t, bp, err); err != nil {
		t.Fatal(err)
	}
	vp, err = c.anf.NewBackupVaultsClient().BeginDelete(t.Context(), group, "acct", "vault", nil)
	if _, err = wait(t, vp, err); err != nil {
		t.Fatalf("deleting an emptied vault = %v", err)
	}
	if _, err := backups.BeginCreate(t.Context(), group, "acct", "vault", "b2", armnetapp.Backup{
		Properties: &armnetapp.BackupProperties{VolumeResourceID: vol.ID},
	}, nil); status(err) != http.StatusNotFound {
		t.Fatalf("creating a backup through the API = %v", err)
	}
}

// protectVolume sets the dataProtection of the volume acct/pool/vol.
func (c *clients) protectVolume(t *testing.T, vol armnetapp.Volume, dp *armnetapp.VolumePropertiesDataProtection) error {
	t.Helper()
	props := *vol.Properties
	props.DataProtection = dp
	p, err := c.volumes.BeginCreateOrUpdate(t.Context(), group, "acct", "pool", "vol", armnetapp.Volume{Location: new(location), Properties: &props}, nil)
	_, err = wait(t, p, err)
	return err
}

func TestNetAppPoliciesInUse(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	vol, err := c.createVolume(t, "acct", "pool", "vol")
	if err != nil {
		t.Fatal(err)
	}
	acctID := accountOf(*vol.ID)
	snapPolicy, _ := c.server.AddSnapshotPolicy(acctID, "snaps")
	backupPolicy, _ := c.server.AddBackupPolicy(acctID, "backups")
	vault, _ := c.server.AddBackupVault(acctID, "vault")
	if err := c.protectVolume(t, vol, &armnetapp.VolumePropertiesDataProtection{
		Snapshot: &armnetapp.VolumeSnapshotProperties{SnapshotPolicyID: new(snapPolicy)},
		Backup:   &armnetapp.VolumeBackupProperties{BackupPolicyID: new(backupPolicy), BackupVaultID: new(vault)},
	}); err != nil {
		t.Fatal(err)
	}
	bp, err := c.anf.NewBackupPoliciesClient().Get(t.Context(), group, "acct", "backups", nil)
	if err != nil || *bp.Properties.VolumesAssigned != 1 {
		t.Fatalf("a backup policy a volume uses = %+v, %v", bp.Properties, err)
	}
	deletes := map[string]func() error{
		"snapshot policy": func() error {
			p, err := c.anf.NewSnapshotPoliciesClient().BeginDelete(t.Context(), group, "acct", "snaps", nil)
			_, err = wait(t, p, err)
			return err
		},
		"backup policy": func() error {
			p, err := c.anf.NewBackupPoliciesClient().BeginDelete(t.Context(), group, "acct", "backups", nil)
			_, err = wait(t, p, err)
			return err
		},
		"backup vault": func() error {
			p, err := c.anf.NewBackupVaultsClient().BeginDelete(t.Context(), group, "acct", "vault", nil)
			_, err = wait(t, p, err)
			return err
		},
	}
	for name, del := range deletes {
		if err := del(); code(err) != "CannotDeleteResource" || status(err) != http.StatusConflict || !strings.Contains(err.Error(), "acct/pool/vol") {
			t.Fatalf("deleting a %s a volume uses = %v", name, err)
		}
	}
	if err := c.protectVolume(t, vol, nil); err != nil {
		t.Fatal(err)
	}
	for name, del := range deletes {
		if err := del(); err != nil {
			t.Fatalf("deleting a %s no volume uses = %v", name, err)
		}
	}
}

func TestNetAppVolumeGroups(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	vol, err := c.createVolume(t, "acct", "pool", "vol")
	if err != nil {
		t.Fatal(err)
	}
	groups := c.anf.NewVolumeGroupsClient()
	create := func(body armnetapp.VolumeGroupDetails) error {
		p, err := groups.BeginCreate(t.Context(), group, "acct", "vg", body, nil)
		_, err = wait(t, p, err)
		return err
	}
	withVolumes := armnetapp.VolumeGroupDetails{Location: new(location), Properties: &armnetapp.VolumeGroupProperties{
		Volumes: []*armnetapp.VolumeGroupVolumeProperties{{Name: new("data"), Properties: vol.Properties}},
	}}
	if err := create(withVolumes); code(err) != "BadRequest" || status(err) != http.StatusBadRequest {
		t.Fatalf("a volume group that creates volumes = %v", err)
	}
	acctID := accountOf(*vol.ID)
	if _, ok := c.server.AddVolumeGroup(acctID, "vg", *vol.ID+"x"); ok {
		t.Fatal("AddVolumeGroup took a missing volume")
	}
	if _, ok := c.server.AddVolumeGroup(acctID+"x", "vg", *vol.ID); ok {
		t.Fatal("AddVolumeGroup took a volume of another account")
	}
	id, ok := c.server.AddVolumeGroup(acctID, "vg", *vol.ID)
	if !ok || id != acctID+"/volumeGroups/vg" {
		t.Fatalf("AddVolumeGroup = %q, %v", id, ok)
	}
	if _, ok := c.server.AddVolumeGroup(acctID, "other", *vol.ID); ok {
		t.Fatal("AddVolumeGroup took a volume another group holds")
	}
	got, err := groups.Get(t.Context(), group, "acct", "vg", nil)
	if err != nil || len(got.Properties.Volumes) != 1 || *got.Properties.Volumes[0].ID != *vol.ID || *got.Properties.GroupMetaData.VolumesCount != 1 ||
		*got.Properties.Volumes[0].Name != "acct/pool/vol" || *got.Properties.Volumes[0].Properties.CreationToken != "vol" {
		t.Fatalf("volume group = %+v, %v", got.VolumeGroupDetails, err)
	}
	v, err := c.volumes.Get(t.Context(), group, "acct", "pool", "vol", nil)
	if err != nil || v.Properties.VolumeGroupName == nil || *v.Properties.VolumeGroupName != "vg" {
		t.Fatalf("a volume in a group = %+v, %v", v.Properties, err)
	}
	if err := create(armnetapp.VolumeGroupDetails{Location: new(location), Properties: &armnetapp.VolumeGroupProperties{}}); err != nil {
		t.Fatalf("creating the group again = %v", err)
	}
	if got, err := groups.Get(t.Context(), group, "acct", "vg", nil); err != nil || len(got.Properties.Volumes) != 1 {
		t.Fatalf("volume group created again = %+v, %v", got.VolumeGroupDetails, err)
	}
	dp, err := groups.BeginDelete(t.Context(), group, "acct", "vg", nil)
	if _, err = wait(t, dp, err); code(err) != "CannotDeleteResource" || status(err) != http.StatusConflict {
		t.Fatalf("deleting a volume group that holds a volume = %v", err)
	}
	vp, err := c.volumes.BeginDelete(t.Context(), group, "acct", "pool", "vol", nil)
	if _, err = wait(t, vp, err); err != nil {
		t.Fatal(err)
	}
	dp, err = groups.BeginDelete(t.Context(), group, "acct", "vg", nil)
	if _, err = wait(t, dp, err); err != nil {
		t.Fatalf("deleting an emptied volume group = %v", err)
	}
}

func cacheBody() armnetapp.Cache {
	return armnetapp.Cache{
		Location: new(location), Tags: map[string]*string{"team": new("storage")},
		Properties: &armnetapp.CacheProperties{
			CacheSubnetResourceID: new(subnet), PeeringSubnetResourceID: new(subnet), FilePath: new("cache1"), Size: new(50 * int64(1) << 30),
			EncryptionKeySource: new(armnetapp.EncryptionKeySourceMicrosoftNetApp),
			OriginClusterInformation: &armnetapp.OriginClusterInformation{
				PeerAddresses: []*string{new("192.0.2.10")}, PeerClusterName: new("origin"), PeerVolumeName: new("vol"), PeerVserverName: new("svm"),
			},
		},
	}
}

func TestNetAppCaches(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	c := setup(t, arm.WithSettleAfter(1), arm.WithClock(func() time.Time { return created }))
	c.createGroup(t, group, nil)
	caches := c.anf.NewCachesClient()
	if _, err := caches.NewListPager(group, "acct", "pool", nil).NextPage(t.Context()); code(err) != "ResourceNotFound" || status(err) != http.StatusNotFound {
		t.Fatalf("listing caches of a missing pool = %v", err)
	}
	vol, err := c.createVolume(t, "acct", "pool", "vol")
	if err != nil {
		t.Fatal(err)
	}
	if page, err := caches.NewListPager(group, "acct", "pool", nil).NextPage(t.Context()); err != nil || len(page.Value) != 0 {
		t.Fatalf("caches of an empty pool = %+v, %v", page.Value, err)
	}
	body := cacheBody()
	body.Properties.FilePath = nil
	p, err := caches.BeginCreateOrUpdate(t.Context(), group, "acct", "pool", "cache", body, nil)
	if _, err = wait(t, p, err); code(err) != "BadRequest" || status(err) != http.StatusBadRequest {
		t.Fatalf("a cache without filePath = %v", err)
	}
	p, err = caches.BeginCreateOrUpdate(t.Context(), group, "acct", "pool", "cache", cacheBody(), nil)
	res, err := wait(t, p, err)
	if err != nil {
		t.Fatal(err)
	}
	cache := res.Cache
	poolID := poolOf(*vol.ID)
	if *cache.ID != poolID+"/caches/cache" || *cache.Name != "acct/pool/cache" || *cache.Type != "Microsoft.NetApp/netAppAccounts/capacityPools/caches" ||
		*cache.Properties.ProvisioningState != armnetapp.CacheProvisioningStateSucceeded || *cache.Tags["team"] != "storage" || cache.Etag == nil ||
		len(cache.Properties.MountTargets) != 1 || *cache.Properties.MountTargets[0].IPAddress == *vol.Properties.MountTargets[0].IPAddress {
		t.Fatalf("cache = %+v %+v", cache, cache.Properties)
	}
	checkNetAppSystemData(t, "cache", cache.SystemData, created, created)
	added, ok := c.server.AddCache(poolID, "added")
	if !ok || added != poolID+"/caches/added" {
		t.Fatalf("AddCache = %q, %v", added, ok)
	}
	if _, ok := c.server.AddCache(poolID+"x", "added"); ok {
		t.Fatal("AddCache added to a missing pool")
	}
	page, err := caches.NewListPager(group, "acct", "pool", nil).NextPage(t.Context())
	if err != nil || len(page.Value) != 2 || *page.Value[0].Name != "acct/pool/added" || *page.Value[1].Name != "acct/pool/cache" {
		t.Fatalf("listed caches = %+v, %v", page.Value, err)
	}
	vp, err := c.volumes.BeginDelete(t.Context(), group, "acct", "pool", "vol", nil)
	if _, err = wait(t, vp, err); err != nil {
		t.Fatal(err)
	}
	pp, err := c.pools.BeginDelete(t.Context(), group, "acct", "pool", nil)
	if _, err = wait(t, pp, err); code(err) != "CannotDeleteResource" || status(err) != http.StatusConflict {
		t.Fatalf("deleting a pool that holds caches = %v", err)
	}
	for _, name := range []string{"cache", "added"} {
		dp, err := caches.BeginDelete(t.Context(), group, "acct", "pool", name, nil)
		if _, err = wait(t, dp, err); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := caches.Get(t.Context(), group, "acct", "pool", "cache", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("a deleted cache = %v", err)
	}
	pp, err = c.pools.BeginDelete(t.Context(), group, "acct", "pool", nil)
	if _, err = wait(t, pp, err); err != nil {
		t.Fatalf("deleting an emptied pool = %v", err)
	}
}

func TestNetAppGroupDeletionTakesAccountChildren(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	vol, err := c.createVolume(t, "acct", "pool", "vol")
	if err != nil {
		t.Fatal(err)
	}
	acctID := accountOf(*vol.ID)
	vault, _ := c.server.AddBackupVault(acctID, "vault")
	snapPolicy, _ := c.server.AddSnapshotPolicy(acctID, "snaps")
	if _, ok := c.server.AddBackup(vault, "b1", *vol.ID, time.Now()); !ok {
		t.Fatal("AddBackup failed")
	}
	if err := c.protectVolume(t, vol, &armnetapp.VolumePropertiesDataProtection{
		Snapshot: &armnetapp.VolumeSnapshotProperties{SnapshotPolicyID: new(snapPolicy)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.server.AddVolumeGroup(acctID, "vg", *vol.ID); !ok {
		t.Fatal("AddVolumeGroup failed")
	}
	if _, ok := c.server.AddBackupPolicy(acctID, "backups"); !ok {
		t.Fatal("AddBackupPolicy failed")
	}
	if _, ok := c.server.AddCache(poolOf(*vol.ID), "cache"); !ok {
		t.Fatal("AddCache failed")
	}
	p, err := c.groups.BeginDelete(t.Context(), group, nil)
	if _, err = wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	if got := c.server.Resources(); len(got) != 0 {
		t.Fatalf("resources after the group's deletion = %+v", got)
	}
}

func TestNetAppAddVolumeGroupLeavesVolumesAlone(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(2))
	c.createGroup(t, group, nil)
	vol, err := c.createVolume(t, "acct", "pool", "vol")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.volumes.BeginDelete(t.Context(), group, "acct", "pool", "vol", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.server.AddVolumeGroup(accountOf(*vol.ID), "vg", *vol.ID); ok {
		t.Fatal("AddVolumeGroup took a volume being deleted")
	}
	got, err := c.volumes.Get(t.Context(), group, "acct", "pool", "vol", nil)
	if err != nil || *got.Properties.ProvisioningState != "Deleting" {
		t.Fatalf("a volume being deleted after AddVolumeGroup = %+v, %v", got.Properties, err)
	}
}

func TestNetAppReadsUnderMissingParents(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	missing := func(what, parent string, err error) {
		t.Helper()
		if code(err) != "ResourceNotFound" || status(err) != http.StatusNotFound || !strings.Contains(err.Error(), "'"+parent+"'") {
			t.Fatalf("%s = %v, want ResourceNotFound naming %s", what, err, parent)
		}
	}
	_, err := c.pools.NewListPager(group, "acct", nil).NextPage(t.Context())
	missing("pools of a missing account", "Microsoft.NetApp/netAppAccounts/acct", err)
	_, err = c.volSnaps.NewListPager(group, "acct", "pool", "vol", nil).NextPage(t.Context())
	missing("snapshots under a missing account", "Microsoft.NetApp/netAppAccounts/acct", err)
	c.createNetAppAccount(t, "acct")
	_, err = c.volumes.NewListPager(group, "acct", "pool", nil).NextPage(t.Context())
	missing("volumes of a missing pool", "Microsoft.NetApp/netAppAccounts/capacityPools/acct/pool", err)
	_, err = c.volumes.Get(t.Context(), group, "acct", "pool", "vol", nil)
	missing("a volume of a missing pool", "Microsoft.NetApp/netAppAccounts/capacityPools/acct/pool", err)
	if _, err := c.createVolume(t, "acct", "pool", "vol"); err != nil {
		t.Fatal(err)
	}
	_, err = c.volSnaps.NewListPager(group, "acct", "pool", "other", nil).NextPage(t.Context())
	missing("snapshots of a missing volume", "Microsoft.NetApp/netAppAccounts/capacityPools/volumes/acct/pool/other", err)
	if page, err := c.pools.NewListPager(group, "acct", nil).NextPage(t.Context()); err != nil || len(page.Value) != 1 {
		t.Fatalf("pools of the account = %+v, %v", page.Value, err)
	}
}
