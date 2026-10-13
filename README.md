# squall

[![CI](https://github.com/parallelworks/squall/actions/workflows/ci.yml/badge.svg)](https://github.com/parallelworks/squall/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/parallelworks/squall.svg)](https://pkg.go.dev/github.com/parallelworks/squall)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/parallelworks/squall/badge)](https://scorecard.dev/viewer/?uri=github.com/parallelworks/squall)

In-memory stand-ins for cloud APIs that fail on cue, for testing code that must
survive partial failure.

Code that creates cloud resources has to cope with calls that fail after they
took effect: an insert that succeeded but whose response was lost, an operation
that ended in an error, a retry that finds the resource already there. Those
paths are where leaked resources and stuck records come from, and they are
hard to reach against a real cloud. A squall server answers the same REST
calls the official client libraries make, keeps the resources in memory, and
fails exactly the calls you choose, or a seeded random share of them, so a
failure replays from its seed.

## Packages

| Package | What it does |
| --- | --- |
| [`fault`](https://pkg.go.dev/github.com/parallelworks/squall/fault) | Decides which calls fail and how: rejected, applied with the response lost, applied with an error, an operation that ends in an error, or slow. Scripted rules and a seeded random rate. |
| [`gcp/compute`](https://pkg.go.dev/github.com/parallelworks/squall/gcp/compute) | Compute Engine: regional addresses (get, list with filters, insert, delete, setLabels) and region operations. |

Coverage grows with use: each stand-in implements the methods someone needed,
answers anything else with 404, and documents how faithful it is. Contributions
of more methods and services are welcome.

## Example

```go
faults := fault.New().Add(fault.Rule{
	Method: "compute.addresses.insert",
	Nth:    1,
	Fault:  fault.LostResponse(), // takes effect, but the caller never hears back
})
s := compute.New(compute.WithFaults(faults))
srv := httptest.NewServer(s)
defer srv.Close()

client, _ := gcompute.NewAddressesRESTClient(ctx,
	option.WithEndpoint(srv.URL), option.WithoutAuthentication())

// Run the code under test with client, then check what it left behind:
for _, a := range s.Addresses() {
	fmt.Println(a.Name, a.Address, a.Labels)
}
```

For a property test, inject random faults and check that every seed converges:

```go
for seed := range uint64(100) {
	faults := fault.New(fault.Seed(seed)).Random(0.3,
		fault.Unavailable(), fault.LostResponse(), fault.ErrorAfterApply(500))
	// ... run the code under test against compute.New(compute.WithFaults(faults))
	// and on failure report the seed and faults.Injected().
}
```

## Faithfulness

Each stand-in follows the real API's paths, JSON shapes and error codes for the
methods it covers, and is tested through the official Go client library. Where
it simplifies, the package documentation says so. For Compute Engine:

- Long-running operations finish before the call that started them returns.
- List filters support `field = value` and `field != value` comparisons on
  name, description, status, address, addressType and labels, joined by AND or
  in parentheses. Anything else is rejected with 400 rather than ignored.
- IP addresses come from 100.64.0.0/10 (external) and 10.0.0.0/8 (internal),
  never from routable ranges.

## Requirements

Go 1.27 or later.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: [SECURITY.md](SECURITY.md).

## License

[Apache 2.0](LICENSE).
