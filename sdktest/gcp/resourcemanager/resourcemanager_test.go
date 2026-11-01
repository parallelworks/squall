package resourcemanager_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	rm "cloud.google.com/go/resourcemanager/apiv3"
	"cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/resourcemanager"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

const (
	project = "my-project"
	number  = 123456789
)

func setup(t *testing.T, opts ...resourcemanager.Option) (*resourcemanager.Server, *rm.ProjectsClient) {
	t.Helper()
	s := resourcemanager.New(append([]resourcemanager.Option{resourcemanager.WithProject(project, number)}, opts...)...)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	c, err := rm.NewProjectsRESTClient(t.Context(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return s, c
}

func code(err error) codes.Code {
	if e, ok := errors.AsType[*apierror.APIError](err); ok {
		return e.GRPCStatus().Code()
	}
	return codes.Unknown
}

func TestGetProjectByIDOrNumber(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s, c := setup(t, resourcemanager.WithClock(func() time.Time { return created }))
	if err := s.PutProject(resourcemanager.Project{Name: "projects/42", ProjectID: "child-project", Parent: "folders/7", DisplayName: "Child", Labels: map[string]string{"env": "test"}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"projects/" + project, "projects/123456789"} {
		p, err := c.GetProject(t.Context(), &resourcemanagerpb.GetProjectRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if p.GetName() != "projects/123456789" || p.GetProjectId() != project || p.GetDisplayName() != project || p.GetState() != resourcemanagerpb.Project_ACTIVE ||
			!p.GetCreateTime().AsTime().Equal(created) || p.GetEtag() == "" || p.GetParent() != "" {
			t.Errorf("get %s = %v", name, p)
		}
	}
	p, err := c.GetProject(t.Context(), &resourcemanagerpb.GetProjectRequest{Name: "projects/child-project"})
	if err != nil {
		t.Fatal(err)
	}
	if p.GetName() != "projects/42" || p.GetParent() != "folders/7" || p.GetDisplayName() != "Child" || p.GetLabels()["env"] != "test" {
		t.Errorf("get child = %v", p)
	}
	if got := s.Projects(); len(got) != 2 || got[0].ProjectID != "child-project" || got[1].ProjectID != project {
		t.Errorf("Projects() = %+v", got)
	}
	if got, ok := s.LookupProject("42"); !ok || got.ProjectID != "child-project" {
		t.Errorf("LookupProject(42) = %+v, %v", got, ok)
	}
}

func TestProjectNotHeldIsPermissionDenied(t *testing.T) {
	_, c := setup(t)
	_, err := c.GetProject(t.Context(), &resourcemanagerpb.GetProjectRequest{Name: "projects/no-such-project"})
	if code(err) != codes.PermissionDenied || sdkerr.GoogleCode(err) != http.StatusForbidden {
		t.Fatalf("get = %v, want 403 PERMISSION_DENIED", err)
	}
	e, _ := errors.AsType[*apierror.APIError](err)
	if e.Reason() != "IAM_PERMISSION_DENIED" || !strings.Contains(err.Error(), "Permission 'resourcemanager.projects.get' denied on resource '//cloudresourcemanager.googleapis.com/projects/no-such-project' (or it may not exist).") {
		t.Errorf("error = %v, reason %q", err, e.Reason())
	}
	if _, err := c.GetIamPolicy(t.Context(), &iampb.GetIamPolicyRequest{Resource: "projects/1"}); code(err) != codes.PermissionDenied {
		t.Errorf("getIamPolicy = %v, want PERMISSION_DENIED", err)
	}
}

func TestPolicyReadModifyWrite(t *testing.T) {
	s, c := setup(t)
	const resource = "projects/" + project
	p, err := c.GetIamPolicy(t.Context(), &iampb.GetIamPolicyRequest{Resource: resource})
	if err != nil {
		t.Fatal(err)
	}
	if p.GetVersion() != 1 || len(p.GetBindings()) != 0 || len(p.GetEtag()) == 0 {
		t.Fatalf("initial policy = %v", p)
	}
	stale := p.GetEtag()
	p.Bindings = append(p.Bindings, &iampb.Binding{Role: "roles/viewer", Members: []string{"user:a@example.com", "serviceAccount:sa@my-project.iam.gserviceaccount.com"}})
	set, err := c.SetIamPolicy(t.Context(), &iampb.SetIamPolicyRequest{Resource: resource, Policy: p})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.GetBindings()) != 1 || slices.Equal(set.GetEtag(), stale) {
		t.Fatalf("set = %v", set)
	}
	p.Bindings = nil
	_, err = c.SetIamPolicy(t.Context(), &iampb.SetIamPolicyRequest{Resource: resource, Policy: p})
	if code(err) != codes.Aborted || sdkerr.GoogleCode(err) != http.StatusConflict {
		t.Fatalf("set with a stale etag = %v, want 409 ABORTED", err)
	}
	stored, ok := s.IamPolicy(project)
	if !ok || len(stored.Bindings) != 1 || stored.Bindings[0].Role != "roles/viewer" || len(stored.Bindings[0].Members) != 2 {
		t.Errorf("IamPolicy() = %+v after a refused set", stored)
	}

	got, err := c.GetIamPolicy(t.Context(), &iampb.GetIamPolicyRequest{Resource: "projects/123456789"})
	if err != nil || !slices.Equal(got.GetEtag(), set.GetEtag()) || got.GetBindings()[0].GetRole() != "roles/viewer" {
		t.Fatalf("get by number = %v, %v", got, err)
	}
	got.Bindings = append(got.Bindings, &iampb.Binding{Role: "roles/storage.objectViewer", Members: []string{"group:g@example.com"}, Condition: &expr.Expr{Title: "until 2030", Expression: "request.time < timestamp('2030-01-01T00:00:00Z')"}})
	if _, err := c.SetIamPolicy(t.Context(), &iampb.SetIamPolicyRequest{Resource: resource, Policy: got}); code(err) != codes.InvalidArgument {
		t.Fatalf("a conditional binding at version 1 = %v, want INVALID_ARGUMENT", err)
	}
	got.Version = 3
	if _, err := c.SetIamPolicy(t.Context(), &iampb.SetIamPolicyRequest{Resource: resource, Policy: got}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetIamPolicy(t.Context(), &iampb.GetIamPolicyRequest{Resource: resource}); code(err) != codes.InvalidArgument {
		t.Errorf("get of a conditional policy at version 1 = %v, want INVALID_ARGUMENT", err)
	}
	v3, err := c.GetIamPolicy(t.Context(), &iampb.GetIamPolicyRequest{Resource: resource, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
	if err != nil || v3.GetVersion() != 3 || v3.GetBindings()[1].GetCondition().GetTitle() != "until 2030" {
		t.Errorf("get at version 3 = %v, %v", v3, err)
	}
}

func TestPermissions(t *testing.T) {
	policy := access.Allow("resourcemanager.projects.get", "resourcemanager.projects.getIamPolicy")
	_, c := setup(t, resourcemanager.WithPolicy(policy))
	const resource = "projects/" + project
	if _, err := c.GetProject(t.Context(), &resourcemanagerpb.GetProjectRequest{Name: resource}); err != nil {
		t.Fatal(err)
	}
	p, err := c.GetIamPolicy(t.Context(), &iampb.GetIamPolicyRequest{Resource: resource})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.SetIamPolicy(t.Context(), &iampb.SetIamPolicyRequest{Resource: resource, Policy: p})
	if code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "'resourcemanager.projects.setIamPolicy'") {
		t.Fatalf("set without setIamPolicy = %v", err)
	}
	got, err := c.TestIamPermissions(t.Context(), &iampb.TestIamPermissionsRequest{Resource: resource, Permissions: []string{
		"resourcemanager.projects.setIamPolicy", "resourcemanager.projects.get", "resourcemanager.projects.getIamPolicy",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"resourcemanager.projects.get", "resourcemanager.projects.getIamPolicy"}; !slices.Equal(got.GetPermissions(), want) {
		t.Errorf("testIamPermissions = %v, want %v", got.GetPermissions(), want)
	}
	if denials := policy.Denials(); len(denials) != 1 || denials[0].Method != "cloudresourcemanager.projects.setIamPolicy" {
		t.Errorf("denials = %+v, want only the setIamPolicy call", denials)
	}
	if _, err := c.TestIamPermissions(t.Context(), &iampb.TestIamPermissionsRequest{Resource: resource, Permissions: []string{"resourcemanager.*"}}); code(err) != codes.InvalidArgument {
		t.Errorf("a wildcard permission = %v, want INVALID_ARGUMENT", err)
	}
}

func TestFaults(t *testing.T) {
	faults := fault.New().
		Add(fault.Rule{Method: "cloudresourcemanager.projects.get", Resource: "projects/" + project, Nth: 1, Fault: fault.Reject(http.StatusServiceUnavailable, "backendError")}).
		Add(fault.Rule{Method: "cloudresourcemanager.projects.getIamPolicy", Nth: 1, Fault: fault.Reject(http.StatusInternalServerError, "")})
	_, c := setup(t, resourcemanager.WithFaults(faults))
	if _, err := c.GetProject(t.Context(), &resourcemanagerpb.GetProjectRequest{Name: "projects/" + project}); err != nil {
		t.Fatalf("get = %v, want the client to retry the 503", err)
	}
	if _, err := c.GetIamPolicy(t.Context(), &iampb.GetIamPolicyRequest{Resource: "projects/" + project}); code(err) != codes.Internal {
		t.Fatalf("getIamPolicy = %v, want INTERNAL", err)
	}
	if got := len(faults.Injected()); got != 2 {
		t.Errorf("%d faults injected, want 2", got)
	}
}
