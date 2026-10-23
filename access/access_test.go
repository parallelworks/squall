package access

import (
	"sync"
	"testing"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		grant, need string
		want        bool
	}{
		{"compute.instances.create", "compute.instances.create", true},
		{"compute.instances.create", "compute.instances.delete", false},
		{"ec2:Describe*", "ec2:DescribeInstances", true},
		{"ec2:describe*", "EC2:DescribeInstances", true},
		{"ec2:Describe*", "ec2:RunInstances", false},
		{"Microsoft.Compute/*/read", "Microsoft.Compute/virtualMachines/read", true},
		{"Microsoft.Compute/*/read", "Microsoft.Compute/virtualMachines/write", false},
		{"*", "s3:PutObject", true},
		{"a*b*c", "abc", true},
		{"a*b*c", "ac", false},
		{"*x*x", "x", false},
		{"*x*x", "xx", true},
	}
	for _, c := range cases {
		if got := match(c.grant, c.need); got != c.want {
			t.Errorf("match(%q, %q) = %v, want %v", c.grant, c.need, got, c.want)
		}
	}
}

func TestPolicyRecordsUsesAndDenials(t *testing.T) {
	p := Allow("compute.instances.create", "compute.disks.create", "compute.routes.delete")

	if missing := p.Check(Call{Method: "compute.instances.insert", Resource: "i", Needs: []string{"compute.instances.create", "compute.disks.create"}}); missing != nil {
		t.Fatalf("allowed call reported missing %v", missing)
	}
	missing := p.Check(Call{Method: "compute.instances.insert", Resource: "j", Needs: []string{"compute.instances.create", "compute.instances.setMetadata"}})
	if len(missing) != 1 || missing[0] != "compute.instances.setMetadata" {
		t.Fatalf("missing = %v, want [compute.instances.setMetadata]", missing)
	}

	if got := p.Used(); len(got) != 2 || got[0] != "compute.disks.create" || got[1] != "compute.instances.create" {
		t.Errorf("Used() = %v", got)
	}
	if got := p.Unused(); len(got) != 1 || got[0] != "compute.routes.delete" {
		t.Errorf("Unused() = %v, want [compute.routes.delete]", got)
	}
	denials := p.Denials()
	if len(denials) != 1 || denials[0].Resource != "j" || denials[0].Missing[0] != "compute.instances.setMetadata" {
		t.Errorf("Denials() = %+v", denials)
	}
}

func TestDeniedCallRecordsNoUse(t *testing.T) {
	p := Allow("s3:PutObject")
	p.Check(Call{Method: "s3.PutObject", Needs: []string{"s3:PutObject", "s3:PutObjectTagging"}})
	if got := p.Used(); len(got) != 0 {
		t.Errorf("Used() = %v, want none: the call was refused", got)
	}
}

func TestWildcardGrantCountsAsUsed(t *testing.T) {
	p := Allow("ec2:Describe*", "ec2:RunInstances")
	p.Check(Call{Method: "ec2.DescribeInstances", Needs: []string{"ec2:DescribeInstances"}})
	if got := p.Unused(); len(got) != 1 || got[0] != "ec2:RunInstances" {
		t.Errorf("Unused() = %v, want [ec2:RunInstances]", got)
	}
}

func TestAlternativeNeedIsMetByAnyOne(t *testing.T) {
	p := Allow("compute.addresses.createInternal")
	if missing := p.Check(Call{Needs: []string{"compute.addresses.create|compute.addresses.createInternal"}}); missing != nil {
		t.Fatalf("missing = %v, want none", missing)
	}
	if got := p.Used(); len(got) != 1 || got[0] != "compute.addresses.createInternal" {
		t.Errorf("Used() = %v, want the alternative that was granted", got)
	}
	missing := Allow("x").Check(Call{Needs: []string{"a|b"}})
	if len(missing) != 1 || missing[0] != "a|b" {
		t.Errorf("missing = %v, want [a|b]", missing)
	}
}

func TestNilPolicyAllowsEverything(t *testing.T) {
	var p *Policy
	if missing := p.Check(Call{Needs: []string{"anything"}}); missing != nil {
		t.Errorf("nil policy refused a call: %v", missing)
	}
	if p.Used() != nil || p.Unused() != nil || p.Denials() != nil {
		t.Error("nil policy recorded something")
	}
}

func TestAllowAllRecordsEveryNeed(t *testing.T) {
	p := AllowAll()
	p.Check(Call{Needs: []string{"b", "a"}})
	if got := p.Used(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("Used() = %v", got)
	}
}

func TestPolicyIsSafeForConcurrentUse(t *testing.T) {
	p := Allow("x")
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			p.Check(Call{Needs: []string{"x"}})
			p.Check(Call{Needs: []string{"y"}})
		})
	}
	wg.Wait()
	if got := len(p.Denials()); got != 50 {
		t.Errorf("%d denials, want 50", got)
	}
}
