package compute_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	gcompute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/gcp/compute"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

func acceleratorsClient(t *testing.T, c *clients) *gcompute.AcceleratorTypesClient {
	t.Helper()
	client, err := gcompute.NewAcceleratorTypesRESTClient(t.Context(), option.WithEndpoint(c.url), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestAcceleratorTypes(t *testing.T) {
	c := setup(t,
		compute.WithZones("asia-east1", "asia-east1-a"),
		compute.WithAcceleratorTypes("us-east1-c", "nvidia-tesla-t4", "nvidia-tesla-p100"),
		compute.WithAcceleratorTypes("us-east1-d"),
	)
	accel := acceleratorsClient(t, c)

	aggregated := func(filter string) map[string][]string {
		req := &computepb.AggregatedListAcceleratorTypesRequest{Project: project}
		if filter != "" {
			req.Filter = new(filter)
		}
		it := accel.AggregatedList(t.Context(), req)
		out := map[string][]string{}
		for {
			pair, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return out
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range pair.Value.GetAcceleratorTypes() {
				out[pair.Key] = append(out[pair.Key], a.GetName())
			}
		}
	}
	all := aggregated("")
	if fmt.Sprint(all["zones/us-east1-b"]) != "[nvidia-l4 nvidia-tesla-t4]" || fmt.Sprint(all["zones/asia-east1-a"]) != "[nvidia-l4 nvidia-tesla-t4]" ||
		fmt.Sprint(all["zones/us-east1-c"]) != "[nvidia-tesla-p100 nvidia-tesla-t4]" {
		t.Errorf("aggregatedList = %v", all)
	}
	if _, ok := all["zones/us-east1-d"]; ok {
		t.Errorf("aggregatedList lists a zone that offers none: %v", all)
	}
	if got := fmt.Sprint(aggregated("name=nvidia-tesla-p100")); got != "map[zones/us-east1-c:[nvidia-tesla-p100]]" {
		t.Errorf("aggregatedList filtered = %s", got)
	}

	a, err := accel.Get(t.Context(), &computepb.GetAcceleratorTypeRequest{Project: project, Zone: "us-east1-c", AcceleratorType: "nvidia-tesla-p100"})
	if err != nil {
		t.Fatal(err)
	}
	if a.GetDescription() != "NVIDIA Tesla P100" || a.GetMaximumCardsPerInstance() != 4 || a.GetId() == 0 ||
		a.GetZone() != "https://www.googleapis.com/compute/v1/projects/p/zones/us-east1-c" ||
		a.GetSelfLink() != "https://www.googleapis.com/compute/v1/projects/p/zones/us-east1-c/acceleratorTypes/nvidia-tesla-p100" {
		t.Errorf("accelerator type = %v", a)
	}
	_, err = accel.Get(t.Context(), &computepb.GetAcceleratorTypeRequest{Project: project, Zone: "us-east1-b", AcceleratorType: "nvidia-tesla-p100"})
	if sdkerr.GoogleCode(err) != http.StatusNotFound {
		t.Errorf("get of a type the zone does not offer = %v, want HTTP 404", err)
	}

	list := func(zone string) ([]string, error) {
		it := accel.List(t.Context(), &computepb.ListAcceleratorTypesRequest{Project: project, Zone: zone})
		var out []string
		for {
			a, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return out, nil
			}
			if err != nil {
				return out, err
			}
			out = append(out, a.GetName())
		}
	}
	if got, err := list("us-east1-c"); err != nil || fmt.Sprint(got) != "[nvidia-tesla-p100 nvidia-tesla-t4]" {
		t.Errorf("list us-east1-c = %v, %v", got, err)
	}
	if got, err := list("us-east1-d"); err != nil || len(got) != 0 {
		t.Errorf("list of a zone that offers none = %v, %v", got, err)
	}
	if _, err := list("us-east1-z"); sdkerr.GoogleCode(err) != http.StatusBadRequest {
		t.Errorf("list of an unknown zone = %v, want HTTP 400", err)
	}
}

func TestAcceleratorTypesNeedTheirPermissions(t *testing.T) {
	policy := access.Allow("compute.acceleratorTypes.list")
	c := setup(t, compute.WithPolicy(policy))
	accel := acceleratorsClient(t, c)
	if _, err := accel.AggregatedList(t.Context(), &computepb.AggregatedListAcceleratorTypesRequest{Project: project}).Next(); err != nil {
		t.Errorf("aggregatedList = %v", err)
	}
	_, err := accel.Get(t.Context(), &computepb.GetAcceleratorTypeRequest{Project: project, Zone: zone, AcceleratorType: "nvidia-tesla-t4"})
	if sdkerr.GoogleCode(err) != http.StatusForbidden {
		t.Errorf("get without compute.acceleratorTypes.get = %v, want HTTP 403", err)
	}
}
