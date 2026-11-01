package route53_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsroute53 "github.com/aws/aws-sdk-go-v2/service/route53"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/aws/route53"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

func TestPolicyNeedsDescribeVpcsForAPrivateZone(t *testing.T) {
	policy := access.Allow("route53:CreateHostedZone", "route53:AssociateVPCWithHostedZone")
	c := newClient(t, route53.New(route53.WithPolicy(policy)))
	_, err := c.CreateHostedZone(t.Context(), createInput("example.internal", "ref", vpcA))
	if sdkerr.AWSCode(err) != "AccessDenied" || sdkerr.AWSStatus(err) != http.StatusForbidden || !strings.Contains(err.Error(), "ec2:DescribeVpcs") {
		t.Fatalf("create = %v, want 403 AccessDenied naming ec2:DescribeVpcs", err)
	}
	denials := policy.Denials()
	if len(denials) != 1 || denials[0].Method != "route53.CreateHostedZone" || !slices.Equal(denials[0].Missing, []string{"ec2:DescribeVpcs"}) {
		t.Fatalf("Denials() = %+v", denials)
	}
}

func TestPolicyRefusesAnOperationByItsAction(t *testing.T) {
	policy := access.Allow("route53:CreateHostedZone", "route53:AssociateVPCWithHostedZone", "ec2:DescribeVpcs")
	c := newClient(t, route53.New(route53.WithPolicy(policy)))
	id := createZone(t, c, "example.internal", "ref", vpcA)
	_, err := c.DeleteHostedZone(t.Context(), &awsroute53.DeleteHostedZoneInput{Id: aws.String(id)})
	if sdkerr.AWSCode(err) != "AccessDenied" || !strings.Contains(err.Error(), "route53:DeleteHostedZone") ||
		!strings.Contains(err.Error(), "arn:aws:route53:::hostedzone/Z") {
		t.Fatalf("delete = %v, want AccessDenied for route53:DeleteHostedZone on the zone", err)
	}
	if unused := policy.Unused(); len(unused) != 0 {
		t.Errorf("Unused() = %v", unused)
	}
}
