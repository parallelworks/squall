package graph_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	abstractions "github.com/microsoft/kiota-abstractions-go"
	msgraph "github.com/microsoftgraph/msgraph-sdk-go"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
	"github.com/microsoftgraph/msgraph-sdk-go/models/odataerrors"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/graph"
	"github.com/parallelworks/squall/fault"
)

// bearer authenticates every request with a fixed token.
type bearer struct{}

func (bearer) AuthenticateRequest(_ context.Context, r *abstractions.RequestInformation, _ map[string]any) error {
	r.Headers.Add("Authorization", "Bearer token")
	return nil
}

func setup(t *testing.T, opts ...graph.Option) (*graph.Server, *msgraph.GraphServiceClient) {
	t.Helper()
	s := graph.New(opts...)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	adapter, err := msgraph.NewGraphRequestAdapter(bearer{})
	if err != nil {
		t.Fatal(err)
	}
	adapter.SetBaseUrl(srv.URL + "/v1.0")
	return s, msgraph.NewGraphServiceClient(adapter)
}

func newUser(upn, password string) models.Userable {
	profile := models.NewPasswordProfile()
	profile.SetPassword(new(password))
	profile.SetForceChangePasswordNextSignIn(new(false))
	u := models.NewUser()
	u.SetAccountEnabled(new(true))
	u.SetDisplayName(new("User One"))
	u.SetMailNickname(new("user1"))
	u.SetUserPrincipalName(new(upn))
	u.SetPasswordProfile(profile)
	return u
}

func odata(err error) (int, string, string) {
	if e, ok := errors.AsType[*odataerrors.ODataError](err); ok {
		main := e.GetErrorEscaped()
		return e.ResponseStatusCode, *main.GetCode(), *main.GetMessage()
	}
	return 0, "", ""
}

func TestDomains(t *testing.T) {
	_, client := setup(t, graph.WithDomains(
		graph.Domain{ID: "contoso.onmicrosoft.com", IsVerified: true},
		graph.Domain{ID: "contoso.com", IsDefault: true, IsVerified: true},
	))
	resp, err := client.Domains().Get(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var defaults, initial []string
	for _, d := range resp.GetValue() {
		if *d.GetIsDefault() {
			defaults = append(defaults, *d.GetId())
		}
		if *d.GetIsInitial() {
			initial = append(initial, *d.GetId())
		}
	}
	if len(resp.GetValue()) != 2 || len(defaults) != 1 || defaults[0] != "contoso.com" {
		t.Fatalf("default domains = %v of %d", defaults, len(resp.GetValue()))
	}
	if len(initial) != 1 || initial[0] != "contoso.onmicrosoft.com" {
		t.Fatalf("initial domains = %v", initial)
	}
}

func TestUserLifecycle(t *testing.T) {
	s, client := setup(t)
	created, err := client.Users().Post(t.Context(), newUser("user1@squall.onmicrosoft.com", "Passw0rd!"), nil)
	if err != nil {
		t.Fatal(err)
	}
	id := *created.GetId()
	got, err := client.Users().ByUserId(id).Get(t.Context(), nil)
	if err != nil || *got.GetUserPrincipalName() != "user1@squall.onmicrosoft.com" || *got.GetDisplayName() != "User One" {
		t.Fatalf("user = %v, %v", got, err)
	}
	byName, err := client.Users().ByUserId("USER1@squall.onmicrosoft.com").Get(t.Context(), nil)
	if err != nil || *byName.GetId() != id {
		t.Fatalf("user by principal name = %v, %v", byName, err)
	}
	if _, err := client.Users().Post(t.Context(), newUser("user1@squall.onmicrosoft.com", "Passw0rd!"), nil); func() bool {
		status, code, msg := odata(err)
		return status != 400 || code != "Request_BadRequest" || msg != "Another object with the same value for property userPrincipalName already exists."
	}() {
		t.Fatalf("a taken principal name = %v", err)
	}
	if err := client.Users().ByUserId(id).Delete(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Users().ByUserId(id).Get(t.Context(), nil); func() bool {
		status, code, _ := odata(err)
		return status != 404 || code != "Request_ResourceNotFound"
	}() {
		t.Fatalf("a deleted user = %v", err)
	}
	if err := client.Users().ByUserId(id).Delete(t.Context(), nil); err == nil {
		t.Fatal("deleting a deleted user succeeded")
	}
	if deleted := s.DeletedUsers(); len(deleted) != 1 || deleted[0].ID != id || deleted[0].Deleted.IsZero() || len(s.Users()) != 0 {
		t.Fatalf("deleted users = %+v, users = %+v", deleted, s.Users())
	}
}

func TestPurgeDeletedUser(t *testing.T) {
	s, client := setup(t)
	created, err := client.Users().Post(t.Context(), newUser("user1@squall.onmicrosoft.com", "Passw0rd!"), nil)
	if err != nil {
		t.Fatal(err)
	}
	id := *created.GetId()
	err = client.Directory().DeletedItems().ByDirectoryObjectId(id).Delete(t.Context(), nil)
	if status, code, _ := odata(err); status != 404 || code != "Request_ResourceNotFound" {
		t.Fatalf("purging a user that is not deleted = %v", err)
	}
	if err := client.Users().ByUserId(id).Delete(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Directory().DeletedItems().ByDirectoryObjectId(id).Delete(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if purged := s.PurgedUsers(); len(purged) != 1 || purged[0].ID != id || purged[0].Purged.IsZero() || len(s.DeletedUsers()) != 0 {
		t.Fatalf("purged users = %+v, deleted users = %+v", purged, s.DeletedUsers())
	}
	err = client.Directory().DeletedItems().ByDirectoryObjectId(id).Delete(t.Context(), nil)
	if status, code, _ := odata(err); status != 404 || code != "Request_ResourceNotFound" {
		t.Fatalf("purging a purged user = %v", err)
	}
}

func TestPurgeNeedsDeleteRestore(t *testing.T) {
	policy := access.Allow("User.Create", "User.ReadWrite.All", "Directory.ReadWrite.All")
	s, client := setup(t, graph.WithPolicy(policy))
	created, err := client.Users().Post(t.Context(), newUser("user1@squall.onmicrosoft.com", "Passw0rd!"), nil)
	if err != nil {
		t.Fatal(err)
	}
	id := *created.GetId()
	if err := client.Users().ByUserId(id).Delete(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	err = client.Directory().DeletedItems().ByDirectoryObjectId(id).Delete(t.Context(), nil)
	if status, code, msg := odata(err); status != 403 || code != "Authorization_RequestDenied" || msg != "Insufficient privileges to complete the operation." {
		t.Fatalf("purging without User.DeleteRestore.All = %d %s %s", status, code, msg)
	}
	if denials := policy.Denials(); len(denials) != 1 || denials[0].Missing[0] != "User.DeleteRestore.All" {
		t.Fatalf("denials = %+v", denials)
	}
	if len(s.DeletedUsers()) != 1 || len(s.PurgedUsers()) != 0 {
		t.Fatalf("a denied purge changed the directory: deleted %+v, purged %+v", s.DeletedUsers(), s.PurgedUsers())
	}
}

func TestOrganization(t *testing.T) {
	_, client := setup(t)
	resp, err := client.Organization().Get(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetValue()) != 1 {
		t.Fatalf("organizations = %d", len(resp.GetValue()))
	}
	org := resp.GetValue()[0]
	if *org.GetId() != "00000000-0000-0000-0000-000000000001" || *org.GetCountryLetterCode() != "US" || *org.GetDefaultUsageLocation() != "US" ||
		len(org.GetVerifiedDomains()) != 1 || *org.GetVerifiedDomains()[0].GetName() != "squall.onmicrosoft.com" {
		t.Fatalf("default organization = %s %s %s %d", *org.GetId(), *org.GetCountryLetterCode(), *org.GetDefaultUsageLocation(), len(org.GetVerifiedDomains()))
	}

	policy := access.Allow("Organization.Read.All")
	_, client = setup(t, graph.WithPolicy(policy), graph.WithOrganization(graph.Organization{
		ID: "11111111-1111-1111-1111-111111111111", DisplayName: "Contoso", CountryLetterCode: "NL", DefaultUsageLocation: "DE",
	}), graph.WithDomains(
		graph.Domain{ID: "contoso.onmicrosoft.com", IsVerified: true},
		graph.Domain{ID: "contoso.com", IsDefault: true, IsVerified: true},
		graph.Domain{ID: "pending.contoso.com"},
	))
	if resp, err = client.Organization().Get(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	org = resp.GetValue()[0]
	if *org.GetId() != "11111111-1111-1111-1111-111111111111" || *org.GetDisplayName() != "Contoso" || *org.GetCountryLetterCode() != "NL" ||
		*org.GetDefaultUsageLocation() != "DE" || len(org.GetVerifiedDomains()) != 2 {
		t.Fatalf("organization = %s %s %s %s %d", *org.GetId(), *org.GetDisplayName(), *org.GetCountryLetterCode(), *org.GetDefaultUsageLocation(),
			len(org.GetVerifiedDomains()))
	}
	for _, d := range org.GetVerifiedDomains() {
		if initial := *d.GetName() == "contoso.onmicrosoft.com"; *d.GetIsInitial() != initial || *d.GetIsDefault() == initial {
			t.Errorf("verified domain %s: isInitial %v, isDefault %v", *d.GetName(), *d.GetIsInitial(), *d.GetIsDefault())
		}
	}
	if used := policy.Used(); len(used) != 1 || used[0] != "Organization.Read.All" {
		t.Fatalf("used = %v", used)
	}
	_, client = setup(t, graph.WithPolicy(access.Allow("User.Read.All")))
	if _, err := client.Organization().Get(t.Context(), nil); func() bool { status, _, _ := odata(err); return status != 403 }() {
		t.Fatalf("reading the organization without a permission = %v", err)
	}
}

func TestNewUsersAreChecked(t *testing.T) {
	_, client := setup(t)
	for _, tc := range []struct{ upn, password, message string }{
		{"user+1@squall.onmicrosoft.com", "Passw0rd!", "Property userPrincipalName is invalid."},
		{"user1@example.com", "Passw0rd!", "The domain portion of the userPrincipalName property is invalid. You must use one of the verified domain names in your organization."},
		{"user1@squall.onmicrosoft.com", "password", "The specified password does not comply with password complexity requirements. Please provide a different password."},
		{"user1@squall.onmicrosoft.com", "Pw0!", "The specified password does not comply with password complexity requirements. Please provide a different password."},
	} {
		_, err := client.Users().Post(t.Context(), newUser(tc.upn, tc.password), nil)
		if status, _, msg := odata(err); status != 400 || msg != tc.message {
			t.Errorf("%s with %q = %d %q", tc.upn, tc.password, status, msg)
		}
	}
}

func TestPolicy(t *testing.T) {
	policy := access.Allow("User.Create", "Domain.Read.All")
	_, client := setup(t, graph.WithPolicy(policy))
	if _, err := client.Domains().Get(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	created, err := client.Users().Post(t.Context(), newUser("user1@squall.onmicrosoft.com", "Passw0rd!"), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = client.Users().ByUserId(*created.GetId()).Delete(t.Context(), nil)
	if status, code, msg := odata(err); status != 403 || code != "Authorization_RequestDenied" || msg != "Insufficient privileges to complete the operation." {
		t.Fatalf("deleting without User.ReadWrite.All = %d %s %s", status, code, msg)
	}
	if denials := policy.Denials(); len(denials) != 1 || denials[0].Missing[0] != "User.ReadWrite.All" {
		t.Fatalf("denials = %+v", denials)
	}
}

func TestFaults(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "graph.users.create", Fault: fault.FailAfter(500)})
	s, client := setup(t, graph.WithFaults(faults))
	_, err := client.Users().Post(t.Context(), newUser("user1@squall.onmicrosoft.com", "Passw0rd!"), nil)
	if err == nil {
		t.Fatal("a create that failed after taking effect succeeded")
	}
	if len(s.Users()) != 1 {
		t.Fatalf("users = %+v", s.Users())
	}
}
