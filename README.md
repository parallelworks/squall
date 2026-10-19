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
hard to reach against a real cloud. A squall server answers the same API
calls the official client libraries make, keeps the resources in memory, and
fails exactly the calls you choose, or a seeded random share of them, so a
failure replays from its seed.

## Packages

| Package | What it does |
| --- | --- |
| [`fault`](https://pkg.go.dev/github.com/parallelworks/squall/fault) | Decides which calls fail and how: rejected, applied with the response lost, applied with an error, an operation that ends in an error, or slow. Scripted rules and a seeded random rate. |
| [`aws/ec2`](https://pkg.go.dev/github.com/parallelworks/squall/aws/ec2) | Amazon EC2: instances (run with client tokens, describe with filters and pages, start, stop, terminate, change type), images, instance types, EBS volumes (create, describe, attach, detach, delete), placement groups, tags, Elastic IP addresses (allocate, describe, release, associate with an interface or instance, disassociate) and network interfaces (create with client tokens, describe, delete). |
| [`gcp/compute`](https://pkg.go.dev/github.com/parallelworks/squall/gcp/compute) | Compute Engine: instances (bulkInsert, insert, get, list, aggregatedList, delete, stop, start, setLabels, setMachineType, addAccessConfig, attachDisk, detachDisk), disks (get, list, aggregatedList, insert, delete), zones, regional addresses (get, list, insert, delete, setLabels), resource policies, instance templates, routes, and zone, region and global operations (get, wait). |

Coverage grows with use: each stand-in implements the methods someone needed,
answers anything else with 404, and documents how faithful it is. Contributions
of more methods and services are welcome.

## Example

```go
faults := fault.New().Add(fault.Rule{
	Method: "compute.addresses.insert",
	Nth:    1,
	Fault:  fault.LoseResponse(), // takes effect, but the caller never hears back
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
		fault.Unavailable(), fault.LoseResponse(), fault.FailAfter(500))
	// ... run the code under test against compute.New(compute.WithFaults(faults))
	// and on failure report the seed and faults.Injected().
}
```

## Faithfulness

Each stand-in follows the real API's requests, response shapes and error codes for the
methods it covers, and is tested through the official Go client library. Where
it simplifies, the package documentation says so. For Compute Engine:

- Changes to instances and disks finish before the call that makes them
  returns, so the SDK's `Operation.Wait` returns at once. `WithSettleAfter(n)`
  keeps them in progress, with their operation `RUNNING`, for `n` more reads
  of the resource or its operation, or until the test calls `Settle`: a new
  instance is `PROVISIONING` and then `STAGING`, a stopping or deleted one
  `STOPPING`, a starting one `STAGING`. An `operations.wait` finishes the
  operation it waits for. A change to an instance that has another in
  progress finishes that one first. Every other change finishes at once.
- A call that repeats an earlier call's `requestId` in the same project, for
  the same method, gets that call's operation back without taking effect
  again, as the real API does for retries. The server keeps the last 10,000
  operations.
- Images and snapshots are registered by the test with `PutImage` and
  `PutSnapshot`, and an image family names the newest image in it. Machine and
  disk types are any well-formed name in the call's zone. Zones come from a
  fixed set per region, which `WithZones` changes.
- Networks, subnetworks, service accounts and accelerators are not checked.
  Fields the stand-in only records keep the JSON the caller sent, and fields it
  does not know are rejected with 400. Instance group managers are not
  supported: their aggregated list is always empty.
- A `bulkInsert` is all or nothing. If a name it asks for, or a disk it would
  create, already exists, its operation fails with `VM_MIN_COUNT_NOT_REACHED`
  and a `RESOURCE_ALREADY_EXISTS` error per conflict, and nothing is created.
  It takes no fixed IP addresses or disk names, and ignores `locationPolicy`.
- List filters support `field = value` and `field != value` comparisons joined
  by AND, OR and parentheses (OR binds tighter than AND), and the regular
  expressions `field eq re` and `field ne re`, which must match the whole
  field. Lists are ordered by name. Anything else, such as a wildcard or
  `orderBy=creationTimestamp desc`, is rejected with 400 rather than ignored.
  An `aggregatedList` lists only the scopes that have items.
- IP addresses come from 100.64.0.0/10 (external) and 10.0.0.0/8 (internal),
  never from routable ranges. An external address is unique across every
  project and region; an internal one is unique within its region, which stands
  in for its subnetwork. An instance's ephemeral external address is released
  when it stops; a reserved one it takes is `IN_USE` until it goes.
- A fault rule's `Resource` for an insert is the resource being created, so a
  rule can target the insert of one name; for a `bulkInsert`, the first
  instance it names.

For Amazon EC2:

- Signatures are not checked. The region comes from the SigV4 credential
  scope, and each region keeps its own resources.
- Subnet and security group IDs and key pair names are not validated, and
  there are no VPCs, default or otherwise: `RunInstances` needs a subnet or a
  network interface.
  Images are registered by the test with `PutImage`; instance types come from
  a small built-in set or `PutInstanceType`.
- `RunInstances` records metadata, private DNS name and CPU options, and the
  primary interface's type (`interface` or `efa`; with an existing interface,
  only that interface's type) and network card index (only 0, the same launch
  as leaving it out); `DescribeInstances` returns them, with metadata options
  pending until the instance first settles. EFA and AMD SEV-SNP are not checked
  against the instance type. CPU options take up to the type's default cores
  and threads per core, which `DescribeInstanceTypes` reports, and any core
  count from 1 up, where EC2 accepts only the counts it lists for each type.
- Instances and volumes move through EC2's states when describes see them. By
  default the first `DescribeInstances` or `DescribeVolumes` after a change
  shows the state it settles in, so the SDK's waiters return at once;
  `WithSettleAfter` holds them in transition for more describes, or until the
  test calls `Settle`. Describes are consistent at once, not eventually.
- Terminated instances stay visible, as in EC2, until the test calls
  `ForgetInstance`. Termination deletes the interface and volumes marked to
  delete on termination and releases the rest.
- A `CreateNetworkInterface`, `RunInstances` or `CreateVolume` that repeats a
  `ClientToken` with the same parameters gets what the first call created, as
  the real API does for retries; with other parameters it fails with
  `IdempotentParameterMismatch`.
- Filters take EC2's `*` and `?` wildcards. Filter names and parameters the
  stand-in does not support are rejected with 400 rather than ignored.
- Public IP addresses come from 100.64.0.0/10 and private ones from
  10.0.0.0/8. There is no address quota unless the test sets one.
- EC2 has no long-running operations, so a failed operation fails the call
  before it takes effect. A fault's reason is the EC2 error code it answers
  with, and the generic ones become `InternalError`, `Unavailable` and
  `RequestLimitExceeded`. A fault rule's `Resource` is the region and ID, such
  as `us-east-1/eipalloc-…`, and for a create, the resource being created (the
  first instance, for `RunInstances`). A rejected call has no effect, so a
  rejected describe does not move an instance along.

## Requirements

Go 1.27 or later. The module requires nothing beyond the standard library; the
tests that use the official client libraries live in the separate `sdktest`
module.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: [SECURITY.md](SECURITY.md).

## License

[Apache 2.0](LICENSE).
