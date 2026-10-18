package ec2

import "cmp"

const (
	enabled  = "enabled"
	disabled = "disabled"

	hostnameIPName       = "ip-name"
	hostnameResourceName = "resource-name"

	interfaceTypeDefault = "interface"

	maxHopLimit = 64
)

var interfaceTypes = []string{interfaceTypeDefault, "efa"}

// MetadataOptions are an instance's instance metadata service settings.
type MetadataOptions struct {
	// HTTPTokens is "optional" or "required".
	HTTPTokens              string
	HTTPPutResponseHopLimit int
	// HTTPEndpoint, HTTPProtocolIPv6 and InstanceMetadataTags are "enabled"
	// or "disabled".
	HTTPEndpoint         string
	HTTPProtocolIPv6     string
	InstanceMetadataTags string
}

// PrivateDNSNameOptions are how an instance's private DNS name is formed.
type PrivateDNSNameOptions struct {
	// HostnameType is "ip-name" or "resource-name".
	HostnameType                    string
	EnableResourceNameDNSARecord    bool
	EnableResourceNameDNSAAAARecord bool
}

// CPUOptions are an instance's CPU settings.
type CPUOptions struct {
	CoreCount      int
	ThreadsPerCore int
	// AmdSevSnp is "enabled", "disabled", or empty when the launch left it out.
	AmdSevSnp string
}

type metadataOptionsXML struct {
	State                   string `xml:"state"`
	HTTPTokens              string `xml:"httpTokens"`
	HTTPPutResponseHopLimit int    `xml:"httpPutResponseHopLimit"`
	HTTPEndpoint            string `xml:"httpEndpoint"`
	HTTPProtocolIPv6        string `xml:"httpProtocolIpv6"`
	InstanceMetadataTags    string `xml:"instanceMetadataTags"`
}

type privateDNSNameOptionsXML struct {
	HostnameType                    string `xml:"hostnameType"`
	EnableResourceNameDNSARecord    bool   `xml:"enableResourceNameDnsARecord"`
	EnableResourceNameDNSAAAARecord bool   `xml:"enableResourceNameDnsAAAARecord"`
}

type cpuOptionsXML struct {
	CoreCount      int    `xml:"coreCount"`
	ThreadsPerCore int    `xml:"threadsPerCore"`
	AmdSevSnp      string `xml:"amdSevSnp,omitempty"`
}

func readMetadataOptions(p *params) (MetadataOptions, *apiError) {
	const k = "MetadataOptions."
	var m MetadataOptions
	var err *apiError
	if m.HTTPTokens, err = p.enum(k+"HttpTokens", "optional", "optional", "required"); err != nil {
		return m, err
	}
	if m.HTTPEndpoint, err = p.enum(k+"HttpEndpoint", enabled, enabled, disabled); err != nil {
		return m, err
	}
	if m.HTTPProtocolIPv6, err = p.enum(k+"HttpProtocolIpv6", disabled, enabled, disabled); err != nil {
		return m, err
	}
	if m.InstanceMetadataTags, err = p.enum(k+"InstanceMetadataTags", disabled, enabled, disabled); err != nil {
		return m, err
	}
	key := k + "HttpPutResponseHopLimit"
	if p.values.Has(key) {
		if m.HTTPPutResponseHopLimit, err = p.int(key); err != nil {
			return m, err
		}
		if m.HTTPPutResponseHopLimit < 1 || m.HTTPPutResponseHopLimit > maxHopLimit {
			return m, invalidValue("Value (%d) for parameter %s is invalid. Expected a value between 1 and %d.", m.HTTPPutResponseHopLimit, key, maxHopLimit)
		}
	} else {
		m.HTTPPutResponseHopLimit = 1
	}
	return m, nil
}

func readPrivateDNSNameOptions(p *params) (PrivateDNSNameOptions, *apiError) {
	const k = "PrivateDnsNameOptions."
	var o PrivateDNSNameOptions
	var err *apiError
	if o.HostnameType, err = p.enum(k+"HostnameType", hostnameIPName, hostnameIPName, hostnameResourceName); err != nil {
		return o, err
	}
	if o.EnableResourceNameDNSARecord, err = p.bool(k+"EnableResourceNameDnsARecord", false); err != nil {
		return o, err
	}
	if o.EnableResourceNameDNSAAAARecord, err = p.bool(k+"EnableResourceNameDnsAAAARecord", false); err != nil {
		return o, err
	}
	return o, nil
}

func readCPUOptions(p *params) (CPUOptions, *apiError) {
	const k = "CpuOptions."
	var c CPUOptions
	var err *apiError
	if p.values.Has(k+"CoreCount") != p.values.Has(k+"ThreadsPerCore") {
		return c, clientError("MissingParameter", "When specifying CpuOptions you must specify both CoreCount and ThreadsPerCore.")
	}
	for _, f := range []struct {
		key string
		v   *int
	}{{"CoreCount", &c.CoreCount}, {"ThreadsPerCore", &c.ThreadsPerCore}} {
		if *f.v, err = p.int(k + f.key); err != nil {
			return c, err
		}
		if p.values.Has(k+f.key) && *f.v < 1 {
			return c, invalidValue("Value (%d) for parameter %s is invalid. Expected a positive integer.", *f.v, k+f.key)
		}
	}
	if c.AmdSevSnp, err = p.enum(k+"AmdSevSnp", "", enabled, disabled); err != nil {
		return c, err
	}
	return c, nil
}

// defaultCPUOptions are the CPU options of t when a launch leaves them out.
func (t InstanceType) defaultCPUOptions() CPUOptions {
	threads := max(t.ThreadsPerCore, 1)
	return CPUOptions{CoreCount: max(t.VCPUs/threads, 1), ThreadsPerCore: threads}
}

func oneTo(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i + 1
	}
	return out
}

// cpuOptionsFor fills in c with t's defaults and checks that t can run it.
func cpuOptionsFor(c CPUOptions, t InstanceType) (CPUOptions, *apiError) {
	d := t.defaultCPUOptions()
	out := CPUOptions{CoreCount: cmp.Or(c.CoreCount, d.CoreCount), ThreadsPerCore: cmp.Or(c.ThreadsPerCore, d.ThreadsPerCore), AmdSevSnp: c.AmdSevSnp}
	if out.ThreadsPerCore > d.ThreadsPerCore {
		return out, invalidValue("The specified ThreadsPerCore (%d) is not valid for instance type %s. Valid values are 1 to %d.",
			out.ThreadsPerCore, t.InstanceType, d.ThreadsPerCore)
	}
	if out.CoreCount > d.CoreCount {
		return out, invalidValue("The specified CoreCount (%d) is not valid for instance type %s. Valid values are 1 to %d.",
			out.CoreCount, t.InstanceType, d.CoreCount)
	}
	return out, nil
}

func (m MetadataOptions) xml(applied bool) metadataOptionsXML {
	s := "pending"
	if applied {
		s = "applied"
	}
	return metadataOptionsXML{s, m.HTTPTokens, m.HTTPPutResponseHopLimit, m.HTTPEndpoint, m.HTTPProtocolIPv6, m.InstanceMetadataTags}
}
