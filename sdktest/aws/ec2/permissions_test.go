package ec2_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/aws/ec2"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

func TestPolicyNeedsCreateTagsToTagOnCreate(t *testing.T) {
	policy := access.Allow("ec2:AllocateAddress", "ec2:DescribeAddresses")
	c := newClient(t, ec2.New(ec2.WithPolicy(policy)))

	_, err := c.AllocateAddress(t.Context(), &awsec2.AllocateAddressInput{
		Domain: types.DomainTypeVpc, TagSpecifications: tagSpec(types.ResourceTypeElasticIp, "session", "s"),
	})
	if sdkerr.AWSCode(err) != "UnauthorizedOperation" || sdkerr.AWSStatus(err) != http.StatusForbidden {
		t.Fatalf("allocate = %v, want 403 UnauthorizedOperation", err)
	}
	if !strings.Contains(err.Error(), "ec2:CreateTags") {
		t.Errorf("error = %v, want it to name ec2:CreateTags", err)
	}
	addresses, err := describeAddresses(t, c, &awsec2.DescribeAddressesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(addresses) != 0 {
		t.Errorf("a refused allocate allocated %d addresses", len(addresses))
	}

	if _, err := c.AllocateAddress(t.Context(), &awsec2.AllocateAddressInput{Domain: types.DomainTypeVpc}); err != nil {
		t.Fatalf("untagged allocate: %v", err)
	}
	if got := policy.Unused(); len(got) != 0 {
		t.Errorf("Unused() = %v, want none", got)
	}
	denials := policy.Denials()
	if len(denials) != 1 || denials[0].Method != "ec2.AllocateAddress" || !slices.Equal(denials[0].Missing, []string{"ec2:CreateTags"}) {
		t.Errorf("Denials() = %+v", denials)
	}
}
