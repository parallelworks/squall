package arm

import (
	"net/http"
	"testing"
)

func TestAddVolumeGroupRefusesFailedVolumes(t *testing.T) {
	s := New()
	const group = "/subscriptions/s/resourceGroups/rg"
	const acct = group + "/providers/Microsoft.NetApp/netAppAccounts/a"
	const vol = acct + "/capacityPools/p/volumes/v"
	const subnet = group + "/providers/Microsoft.Network/virtualNetworks/n/subnets/s"
	serve(t, s, http.MethodPut, group, `{"location":"eastus"}`)
	serve(t, s, http.MethodPut, acct, `{"location":"eastus"}`)
	serve(t, s, http.MethodPut, acct+"/capacityPools/p", `{"location":"eastus","properties":{"serviceLevel":"Premium","size":4398046511104}}`)
	serve(t, s, http.MethodPut, vol, `{"location":"eastus","properties":{"creationToken":"v","subnetId":"`+subnet+`","usageThreshold":107374182400}}`)
	s.mu.Lock()
	s.resources[key(vol)].state = stateFailed
	s.mu.Unlock()
	if _, ok := s.AddVolumeGroup(acct, "g", vol); ok {
		t.Fatal("AddVolumeGroup took a failed volume")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resources[key(acct+"/volumeGroups/g")] != nil || s.resources[key(vol)].volumeGroup != "" {
		t.Fatal("a failed AddVolumeGroup left a group behind")
	}
}
