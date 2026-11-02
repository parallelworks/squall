package serviceusage_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	su "cloud.google.com/go/serviceusage/apiv1"
	"cloud.google.com/go/serviceusage/apiv1/serviceusagepb"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/serviceusage"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

const (
	project  = "my-project"
	number   = 123456789
	bigquery = "bigquery.googleapis.com"
	byID     = "projects/" + project + "/services/" + bigquery
	byNumber = "projects/123456789/services/" + bigquery
)

func setup(t *testing.T, opts ...serviceusage.Option) (*serviceusage.Server, *su.Client) {
	t.Helper()
	s := serviceusage.New(append([]serviceusage.Option{serviceusage.WithProject(project, number)}, opts...)...)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	c, err := su.NewRESTClient(t.Context(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
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

func reason(err error) string {
	if e, ok := errors.AsType[*apierror.APIError](err); ok {
		return e.Reason()
	}
	return ""
}

func enable(t *testing.T, c *su.Client, name string) (*serviceusagepb.Service, error) {
	t.Helper()
	op, err := c.EnableService(t.Context(), &serviceusagepb.EnableServiceRequest{Name: name})
	if err != nil {
		return nil, err
	}
	resp, err := op.Wait(t.Context())
	if err != nil {
		return nil, err
	}
	return resp.GetService(), nil
}

func state(t *testing.T, c *su.Client, name string) serviceusagepb.State {
	t.Helper()
	svc, err := c.GetService(t.Context(), &serviceusagepb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return svc.GetState()
}

func TestEnableAndDisable(t *testing.T) {
	s, c := setup(t)
	svc, err := c.GetService(t.Context(), &serviceusagepb.GetServiceRequest{Name: byID})
	if err != nil {
		t.Fatal(err)
	}
	if svc.GetName() != byNumber || svc.GetParent() != "projects/123456789" || svc.GetState() != serviceusagepb.State_DISABLED ||
		svc.GetConfig().GetName() != bigquery || svc.GetConfig().GetTitle() != "BigQuery API" {
		t.Fatalf("get before enabling = %v", svc)
	}

	got, err := enable(t, c, byID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetName() != byNumber || got.GetState() != serviceusagepb.State_ENABLED {
		t.Fatalf("enable answered %v", got)
	}
	for _, name := range []string{byID, byNumber} {
		if st := state(t, c, name); st != serviceusagepb.State_ENABLED {
			t.Errorf("get %s = %v, want ENABLED", name, st)
		}
	}
	if got := s.EnabledServices("123456789"); !slices.Equal(got, []string{bigquery}) {
		t.Errorf("EnabledServices = %v", got)
	}

	op, err := c.EnableService(t.Context(), &serviceusagepb.EnableServiceRequest{Name: byNumber})
	if err != nil {
		t.Fatalf("enabling an enabled service = %v", err)
	}
	if !op.Done() || op.Name() != "operations/noop.DONE_OPERATION" {
		t.Errorf("enabling an enabled service started %s, done %v", op.Name(), op.Done())
	}

	if _, err := enable(t, c, "projects/"+project+"/services/compute.googleapis.com"); err != nil {
		t.Fatal(err)
	}
	var enabled []string
	it := c.ListServices(t.Context(), &serviceusagepb.ListServicesRequest{Parent: "projects/" + project, Filter: "state:ENABLED"})
	for {
		svc, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		enabled = append(enabled, svc.GetName())
	}
	if want := []string{byNumber, "projects/123456789/services/compute.googleapis.com"}; !slices.Equal(enabled, want) {
		t.Errorf("list enabled = %v, want %v", enabled, want)
	}

	dop, err := c.DisableService(t.Context(), &serviceusagepb.DisableServiceRequest{Name: byID, DisableDependentServices: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := dop.Wait(t.Context()); err != nil || resp.GetService().GetState() != serviceusagepb.State_DISABLED {
		t.Fatalf("disable = %v, %v", resp, err)
	}
	if st := state(t, c, byID); st != serviceusagepb.State_DISABLED {
		t.Errorf("get after disabling = %v", st)
	}
	_, err = c.DisableService(t.Context(), &serviceusagepb.DisableServiceRequest{Name: byID})
	if herr, ok := errors.AsType[*googleapi.Error](err); !ok || herr.Code != http.StatusBadRequest || !strings.Contains(herr.Body, `"status":"FAILED_PRECONDITION"`) {
		t.Errorf("disabling a disabled service = %v, want 400 FAILED_PRECONDITION", err)
	}
}

func TestUnknownProjectsAndServicesArePermissionDenied(t *testing.T) {
	_, c := setup(t)
	_, err := enable(t, c, "projects/other-project/services/"+bigquery)
	if code(err) != codes.PermissionDenied || sdkerr.GoogleCode(err) != http.StatusForbidden || reason(err) != "AUTH_PERMISSION_DENIED" {
		t.Errorf("enable in an unknown project = %v, want 403 PERMISSION_DENIED", err)
	}
	if _, err := c.GetService(t.Context(), &serviceusagepb.GetServiceRequest{Name: "projects/42/services/" + bigquery}); code(err) != codes.PermissionDenied {
		t.Errorf("get in an unknown project = %v, want PERMISSION_DENIED", err)
	}
	_, err = enable(t, c, "projects/"+project+"/services/made-up.googleapis.com")
	if code(err) != codes.PermissionDenied || reason(err) != "SERVICE_CONFIG_NOT_FOUND_OR_PERMISSION_DENIED" ||
		!strings.Contains(err.Error(), "Not found or permission denied for service(s): made-up.googleapis.com.") {
		t.Errorf("enable an unknown service = %v, want 403 SERVICE_CONFIG_NOT_FOUND_OR_PERMISSION_DENIED", err)
	}
}

func TestAddedServiceCanBeEnabled(t *testing.T) {
	s, c := setup(t, serviceusage.WithService("example.googleapis.com", "Example API"))
	if _, err := enable(t, c, "projects/"+project+"/services/example.googleapis.com"); err != nil {
		t.Fatal(err)
	}
	if svc, ok := s.LookupService(project, "example.googleapis.com"); !ok || svc.State != "ENABLED" || svc.Config.Title != "Example API" {
		t.Errorf("LookupService = %+v, %v", svc, ok)
	}
}

func TestFailedEnableOperation(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "serviceusage.services.enable", Nth: 1, Fault: fault.FailOperation(http.StatusInternalServerError)})
	s, c := setup(t, serviceusage.WithFaults(faults))
	op, err := c.EnableService(t.Context(), &serviceusagepb.EnableServiceRequest{Name: byID})
	if err != nil {
		t.Fatalf("enable = %v, want an operation", err)
	}
	if _, err := op.Wait(t.Context()); code(err) != codes.Internal {
		t.Fatalf("wait = %v, want INTERNAL", err)
	}
	if got := s.EnabledServices(project); len(got) != 0 {
		t.Fatalf("a failed enable enabled %v", got)
	}
	if _, err := enable(t, c, byID); err != nil {
		t.Fatalf("retrying the enable = %v", err)
	}
	if st := state(t, c, byID); st != serviceusagepb.State_ENABLED {
		t.Errorf("get after the retry = %v", st)
	}
}

func TestLostEnableResponseConverges(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "serviceusage.services.enable", Nth: 1, Fault: fault.LoseResponse()})
	s, c := setup(t, serviceusage.WithFaults(faults))
	if _, err := c.EnableService(t.Context(), &serviceusagepb.EnableServiceRequest{Name: byID}); err == nil {
		t.Fatal("enable with a lost response succeeded")
	}
	if got := s.EnabledServices(project); !slices.Equal(got, []string{bigquery}) {
		t.Fatalf("EnabledServices after a lost response = %v", got)
	}
	if _, err := enable(t, c, byID); err != nil {
		t.Fatalf("retrying the enable = %v", err)
	}
}

func TestPolicyRefusesEnable(t *testing.T) {
	policy := access.Allow("serviceusage.services.get")
	s, c := setup(t, serviceusage.WithPolicy(policy))
	_, err := enable(t, c, byID)
	if code(err) != codes.PermissionDenied || reason(err) != "AUTH_PERMISSION_DENIED" || !strings.Contains(err.Error(), "Permission denied to enable service ["+bigquery+"]") {
		t.Fatalf("enable without permission = %v", err)
	}
	if got := s.EnabledServices(project); len(got) != 0 {
		t.Fatalf("a refused enable enabled %v", got)
	}
	if st := state(t, c, byID); st != serviceusagepb.State_DISABLED {
		t.Errorf("get = %v", st)
	}
	if d := policy.Denials(); len(d) != 1 || !slices.Equal(d[0].Missing, []string{"serviceusage.services.enable"}) {
		t.Errorf("denials = %+v", d)
	}
}
