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
| [`access`](https://pkg.go.dev/github.com/parallelworks/squall/access) | Grants a set of permissions (Google Cloud IAM permissions, AWS IAM actions or Azure RBAC actions) and refuses calls that need one it lacks, with the error the cloud answers, including those a request's fields add and Azure's linked checks. Reports which permissions were needed but missing and which were granted but never used. |
| [`aws/ec2`](https://pkg.go.dev/github.com/parallelworks/squall/aws/ec2) | Amazon EC2: instances (run with client tokens, describe with filters and pages, start, stop, terminate, change type), images (describe, register from snapshots, deregister), instance types, EBS volumes (create, describe, attach, detach, delete), EBS snapshots (create, copy across regions, describe with filters and pages, delete), placement groups, tags, Elastic IP addresses (allocate, describe, release, associate with an interface or instance, disassociate) and network interfaces (create with client tokens, describe, delete). |
| [`aws/s3`](https://pkg.go.dev/github.com/parallelworks/squall/aws/s3) | Amazon S3: general purpose buckets (create, head, list with pages and filters, delete), their tags, versioning, policy and CORS rules, and objects (put, delete one or many, list versions and delete markers). |
| [`azure/arm`](https://pkg.go.dev/github.com/parallelworks/squall/azure/arm) | Azure Resource Manager: resource groups (get, create or update, list with tag filters, delete with everything in them that nothing outside uses), virtual machines (create or update, get, list, instance view, deallocate, start, delete) and their extensions (create or update, get, list, delete), Uniform scale sets (create or update, update, get, list, delete, delete instances) and their VMs, managed disks (VM OS disks included) and snapshots, with cross-region incremental snapshot copies whose completionPercent climbs to 100, Azure Compute Gallery galleries, image definitions and image versions with their replication status, virtual networks and their subnets, NAT gateways, public load balancers with backend pools and outbound rules, network security groups, network interfaces and public IP addresses, the IP configurations of scale set VMs, private DNS zones, virtual network links and A record sets, resource provider registration, Cost Management exports at a subscription's scope, and long-running operations to poll. |
| [`azure/graph`](https://pkg.go.dev/github.com/parallelworks/squall/azure/graph) | Microsoft Graph: a directory's domains (list) and users (create with Microsoft Entra's name and password rules, get by ID or user principal name, delete), with application permissions checked as Graph documents them. |
| [`gcp/compute`](https://pkg.go.dev/github.com/parallelworks/squall/gcp/compute) | Compute Engine: instances (bulkInsert, insert, get, list, aggregatedList, delete, stop, start, setLabels, setMachineType, addAccessConfig, attachDisk, detachDisk), disks (get, list, aggregatedList, insert, delete), snapshots (insert, get, list, delete), zones, regional addresses (get, list, insert, delete, setLabels), resource policies, instance templates, routes, and zone, region and global operations (get, wait). |
| [`gcp/lustre`](https://pkg.go.dev/github.com/parallelworks/squall/gcp/lustre) | Managed Lustre: instances (create with the API's capacity, tier, ID, filesystem, root squash and maintenance rules, get, list with filters on name, state, filesystem, network, description and labels, patch labels, description, gkeSupportEnabled and capacity, delete) and their long-running operations (get, list, delete, cancel). |
| [`gcp/storage`](https://pkg.go.dev/github.com/parallelworks/squall/gcp/storage) | Cloud Storage: buckets (insert, get, list, patch labels, versioning and CORS, delete) and objects (multipart and media uploads, list with versions, delete by generation). |

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

To check that a list of required permissions is exact, grant only it and see
what the code under test needed:

```go
policy := access.Allow(requiredPermissions...)
s := compute.New(compute.WithPolicy(policy))
// ... run the code under test against s
for _, d := range policy.Denials() {
	t.Errorf("%s on %s needs %v", d.Method, d.Resource, d.Missing)
}
if unused := policy.Unused(); len(unused) > 0 {
	t.Logf("granted but never used: %v", unused)
}
```

## Faithfulness

Each stand-in follows the real API's requests, response shapes and error codes for the
methods it covers, and is tested through the official Go client library. Where
it simplifies, the package documentation says so. For Compute Engine:

- Changes to instances, disks and snapshots finish before the call that makes
  them returns, so the SDK's `Operation.Wait` returns at once.
  `WithSettleAfter(n)` keeps them in progress, with their operation `RUNNING`,
  for `n` more reads of the resource or its operation, or until the test calls
  `Settle`: a new instance is `PROVISIONING` and then `STAGING`, a stopping or
  deleted one `STOPPING`, a starting one `STAGING`, a new snapshot `CREATING`
  and a deleted one `DELETING`. An `operations.wait` finishes the
  operation it waits for. A change to an instance that has another in
  progress finishes that one first. Every other change finishes at once.
- A call that repeats an earlier call's `requestId` in the same project, for
  the same method, gets that call's operation back without taking effect
  again, as the real API does for retries. The server keeps the last 10,000
  operations.
- Images are registered by the test with `PutImage`, and an image family
  names the newest image in it. Snapshots are taken of a `READY` disk with
  `snapshots.insert`, or registered with `PutSnapshot`; they record the disk's
  size, not its data, and a disk from one that is not `READY` fails with
  `resourceNotReady`. Machine and
  disk types are any well-formed name in the call's zone. Zones come from a
  fixed set per region, which `WithZones` changes.
- Networks, subnetworks, service accounts and accelerators are not checked.
  Fields the stand-in only records keep the JSON the caller sent, and fields it
  does not know are rejected with 400. Instance group managers are not
  supported: their aggregated list is always empty.
- A `bulkInsert` skips each instance whose name, or a disk it would create,
  already exists, and creates the rest when they number at least `minCount`;
  its operation succeeds, with `createdVmCount` and `failedToCreateVmCount`
  counting the two, and lists no warning for what it skipped. With fewer than
  `minCount`, its operation fails with `VM_MIN_COUNT_NOT_REACHED` and a
  `RESOURCE_ALREADY_EXISTS` error per conflict, and nothing is created.
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
  when it stops; a reserved one it takes is `IN_USE` until it goes, and stays
  with it until then if the address is deleted.
- A fault rule's `Resource` for an insert is the resource being created, so a
  rule can target the insert of one name; for a `bulkInsert`, the first
  instance it names.

For Managed Lustre:

- Creates, patches and deletes finish before the call that makes them returns,
  so the SDK's `Wait` returns at once. `WithSettleAfter(n)` keeps the instance
  `CREATING` or `DELETING` (a patched one keeps its state, since the API
  deprecates `UPDATING`), with its operation not done, for `n`
  more reads of it or its operation, or until the test calls `Settle`. The v1
  API has no `FAILED` state: a failed create ends its operation in an error and
  leaves no instance.
- Instance IDs, filesystem names, networks, labels and root squash options are
  checked as the API documents them, and `capacityGib` against its throughput
  tier's minimum, maximum and step, the step growing above the tier's
  threshold. Locations are not checked, and networks, KMS keys and quotas are
  not looked up. A mount point's address comes from 10.0.0.0/8.
- List filters follow AIP-160 for `name`, `state`, `filesystem`, `network`,
  `description` and labels (`labels.KEY = value`, `labels.KEY:*`,
  `labels:KEY`), with AND, OR, NOT and parentheses; anything else is rejected
  with 400. A patch changes labels, description, gkeSupportEnabled or
  capacityGib, named by `updateMask`, by `*`, or without a mask by the fields
  the body holds; output-only fields and unchanged ones are ignored, as
  AIP-203 directs. The API also changes throughput, access rules, maintenance
  windows, placement policy and target version, which the stand-in rejects
  with 400. Names match exactly, in lowerCamelCase or snake_case, and options
  read back the same whether their enums were sent by name or number. Error
  messages are the stand-in's own.
- Fault method names are the API's method IDs, such as
  `lustre.projects.locations.instances.create`, and a rule's `Resource` is the
  path below `/v1/`, for a create the instance it makes.

For Amazon EC2:

- Signatures are not checked. The region comes from the SigV4 credential
  scope, and each region keeps its own resources.
- Subnet and security group IDs and key pair names are not validated, and
  there are no VPCs, default or otherwise: `RunInstances` needs a subnet or a
  network interface.
  Images are registered by the test with `PutImage`, or with `RegisterImage`
  from snapshots the stand-in holds; instance types come from a small built-in
  set or `PutInstanceType`.
- Snapshots record the volume's size and encryption, not its data, and every
  one is the caller's own: `DescribeSnapshots` with an owner other than `self`
  lists none. `DeleteSnapshot` fails with `InvalidSnapshot.InUse` while a
  registered image maps the snapshot.
- `CopySnapshot`, called in the destination region, copies a `completed`
  snapshot from `SourceRegion` into a new snapshot there that starts `pending`
  and completes like a new snapshot. A source that region does not hold fails
  with `InvalidSnapshot.NotFound`; a `pending` source fails with
  `IncorrectState` (inferred: AWS documents copying only completed
  snapshots). The copy takes the request's description and tags, not the
  source's tags, reports the arbitrary volume ID `vol-ffffffff`, and is
  independent of its source: deleting either leaves the other (inferred from
  copies being full copies with their own ID). It is encrypted if its source
  is or `Encrypted` is set; KMS keys are accepted and not modeled, and Outpost,
  Local Zone and time-based copies are rejected as unknown parameters.
- `RunInstances` records metadata, private DNS name and CPU options, and the
  primary interface's type (`interface` or `efa`; with an existing interface,
  only that interface's type) and network card index (only 0, the same launch
  as leaving it out); `DescribeInstances` returns them, with metadata options
  pending until the instance first settles. EFA and AMD SEV-SNP are not checked
  against the instance type. CPU options take up to the type's default cores
  and threads per core, which `DescribeInstanceTypes` reports, and any core
  count from 1 up, where EC2 accepts only the counts it lists for each type.
- Instances, volumes and snapshots move through EC2's states when describes
  see them. By default the first `DescribeInstances`, `DescribeVolumes` or
  `DescribeSnapshots` after a change shows the state it settles in, so the
  SDK's waiters return at once; `WithSettleAfter` holds them in transition for
  more describes, or until the test calls `Settle`. Describes are consistent at once, not eventually.
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

For Amazon S3:

- Requests are path-style, which the SDK uses for an IP endpoint such as
  httptest's. Signatures are not checked; the region comes from the SigV4
  credential scope. A call on a bucket in another region fails with 301
  `PermanentRedirect` and `x-amz-bucket-region`, as S3 answers it.
- There is one account. `PutForeignBucket` holds a name for another one, so
  creating it fails with `BucketAlreadyExists` and calls on it with
  `AccessDenied` (403 for `HeadBucket`). Creating an owned bucket again
  succeeds in us-east-1 and fails with `BucketAlreadyOwnedByYou` elsewhere.
- `ListBuckets` with any parameter pages (10,000 buckets by default) and
  reports each bucket's region, as S3 does; with none it lists every bucket
  without regions. `ListObjectVersions` orders by key and then newest first, and its markers
  are positions, so the next page still follows one whose versions were just
  deleted. `DeleteBucket` fails with `BucketNotEmpty` while any version or
  delete marker remains.
- Objects keep their size and ETag, not their data. Checksums are not
  verified, and multipart uploads, copies, object tags and lifecycle are not
  supported. Policies and CORS rules are recorded, not enforced. S3 documents
  in-progress multipart uploads as blocking `DeleteBucket` only for directory
  buckets.
- A fault rule's `Resource` is the bucket name, or `bucket/key` for an object
  call. A lost `HeadBucket` breaks off inside its status line, since a sent
  header would read as an answer.

For Cloud Storage:

- Point the client at `srv.URL + "/storage/v1/"`, or set
  `STORAGE_EMULATOR_HOST`. There is one caller, which reaches every project;
  `PutForeignBucket` holds a name it cannot reach, so inserting it fails with
  409 `conflict` and calls on it with 403 `forbidden`. A repeated insert of
  the caller's own bucket fails with 409, as the client's retry of a create
  that took effect does.
- Bucket names and labels follow the API's rules; locations are upper-cased
  but not checked. Fields the server does not model, such as lifecycle rules
  or retention, are rejected with 400. A patch with a field set to null
  clears it, and a label set to null is deleted.
- Objects keep their size, MD5 and CRC32C, which an upload's checksums must
  match. With versioning on, overwrites and deletes keep the old generation as
  noncurrent, and a bucket with any generation left cannot be deleted. Page
  tokens are positions, so a listing survives deletes between pages.
  Resumable uploads, reads, IAM policies and HMAC keys are not supported.
- A fault rule's `Resource` is the path below `/storage/v1/`, such as
  `b/my-bucket/o/my-object`, and for an insert the bucket or object it makes.

For Azure Resource Manager:

- Long-running operations finish within the call that starts them, so the
  SDK's pollers are done at once. `WithSettleAfter(n)` answers with an
  `Azure-AsyncOperation` URL instead (and a `Location` URL for actions and
  deletes, the only one for a resource group deletion), keeping the resource
  `Creating`, `Updating` or `Deleting` for `n` more reads of it or its
  operation, or until the test calls `Settle`. Polls carry a `retry-after-ms`
  of 10. A fault in `FailOperation` mode ends the operation in its error
  without the call taking effect.
- PUT and DELETE are idempotent, as in Azure: a retried create answers 200 with
  the resource, and deleting what is not there answers 204. A change to a
  resource with another in progress is answered as its provider answers it:
  network resources fail it with 429 `RetryableError` and virtual network
  links with 409 `Conflict`, counting the call as a read of that change, and
  a VM or scale set change preempts the other, which ends `Canceled` with
  `OperationPreempted`, or fails with 409 `OperationNotAllowed` if that one is
  a deletion. A change in a resource group being deleted fails with
  `ResourceGroupBeingDeleted`.
- Deleting a resource group deletes what nothing outside it uses. When
  something outside still uses a resource in it, such as a VM in another group
  attached to its network interface, the rest is deleted and the operation
  fails with 409 `ResourceGroupDeletionBlocked`, leaving the group with the
  resources in use. Each resource left is a detail whose message is the error
  deleting it alone answers, such as `NicInUse`, as Azure reports it; the
  status, and that the rest is deleted first, are inferred.
- Resource names, subscription IDs, locations and tags are not validated, and
  names match without regard to case. Fields it does not know are rejected
  with `InvalidRequestContent`; values it does not model, such as a Flexible
  scale set or a VM with `networkInterfaceConfigurations`, with `BadRequest`.
- A subnet's address prefixes must lie in its virtual network's address space
  (`NetcfgSubnetRangeOutsideVnet`), not overlap another subnet's
  (`NetcfgSubnetRangesOverlap`) and be no smaller than a /29; a virtual
  network PUT replaces its subnets, and a subnet, virtual network or NAT
  gateway in use cannot be deleted. Interfaces in a subnet the server holds
  take addresses from its prefix. A subnet the server does not hold is
  recorded, not checked: its private addresses come from 10.0.0.0/8, unique
  within the subnet ID, and public ones from 100.64.0.0/10, assigned when
  created; `WithPrivateRange` and `WithPublicRange` change the ranges. Once a
  range is used up, a change that needs an address fails with `SubnetIsFull` or
  `PublicIPCountLimitReached`.
- A public load balancer's outbound rule gives SNAT ports in multiples of 8
  (`SpecifiedAllocatedOutboundPortsForOutboundRuleIsNotAMultipleOfEight`), and
  the write that would ask more of its frontends than 64,000 ports per public
  IP address, shared among the primary IP configurations in its backend pool
  (each scale set VM counts), fails with
  `SpecifiedAllocatedOutboundPortsForOutboundRuleExceedsTotalNumberOfAvailablePorts`:
  the load balancer's, or the scale set or interface write that grows the
  pool. A load balancer whose pool a scale set uses cannot be deleted
  (`LoadBalancerInUseByVirtualMachineScaleSet`). Load balancing rules, probes
  and inbound NAT rules are not modeled.
- A disk or snapshot change in progress answers 202 with the resource, showing
  `Updating` even while it is created, and polling URLs under
  `locations/{location}/DiskOperations`, the Location URL adding
  `monitor=true`; once done, the status carries the resource as
  `properties.output`. That a delete is polled the same way is inferred.
- A VM made from an image makes its OS disk a served disk, `Attached` and
  managed by the VM, `Reserved` while the VM is deallocated. Deleting the VM
  deletes the disk when its `deleteOption` is `Delete` and detaches it
  otherwise. Its default SKU and size, `Standard_LRS` and 30 GB, are inferred.
  An attached disk cannot be deleted (409 `OperationNotAllowed`). Data disks
  and scale set VM disks are not modeled.
- A VM extension create answers 201 with only an `Azure-AsyncOperation` URL
  under `Microsoft.Compute/locations/{location}/operations`, showing
  `Creating`, then `Succeeded`, and a delete 202 with a `Location` URL as well,
  the same path adding `monitor=true`, as Azure answers; that an update shows
  `Updating` is inferred. `protectedSettings` are recorded, as
  `VMExtensions` shows, but not returned, as Azure leaves them out. The
  handler does not run, so an extension never ends `Failed`, and its instance
  view is not served. A write under a missing VM fails with
  `ParentResourceNotFound` and the message Microsoft documents; its 404 status
  is inferred, as are 404 `ResourceNotFound` for a get or list under a missing
  VM or a get of a missing extension, and 204 for deleting either. Deleting
  the VM or its group deletes its extensions, as Microsoft documents deleting
  a parent deleting its children.
- A copy from a disk or snapshot that is missing or in another location fails
  with 404 `NotFound`. An in-region `Copy` snapshot shows no
  `completionPercent`. An incremental `CopyStart` copy, in any location,
  succeeds while its background copy goes on: `completionPercent` climbs from
  0 to 100 in `WithCopySteps` steps (2 by default: 50, then 100), one per get
  of the snapshot (unless `WithSettleAfter` is negative) or per `Settle`, a
  pace the stand-in sets. Using it as a source
  before then fails with 409 `Conflict` "Source incremental snapshot {name}
  copy is still in progress", as Azure answers a disk; for a snapshot or image
  version it is inferred. One copy of a disk's snapshots runs at a time (409
  `Conflict`, message inferred). Deleting the source mid-copy succeeds and the
  copy shows `copyCompletionError` `CopySourceNotFound`; its message is
  inferred. Copy order and the ZRS default for incremental snapshots are not
  modeled.
- Galleries, image definitions and image versions are polled under
  `locations/{location}/capsOperations`. A version is `Creating`, then
  `Succeeded`; `$expand=ReplicationStatus` shows each target region's state.
  A version from a copy still in progress is accepted and ends `Failed`, its
  operation and replication status giving the copy's `Conflict`, which is
  inferred. A missing source fails with 400 `InvalidParameter`, the code
  inferred. Deleting a gallery or image definition that holds others fails
  with 409 `CannotDeleteResource`, the code inferred. Replication, sharing and
  soft deletion are not modeled.
- A snapshot of a disk needs `Microsoft.Compute/disks/beginGetAccess/action`
  on the disk, or fails with `LinkedAuthorizationFailed`; linked checks on
  other disk, snapshot and gallery sources are not modeled.
- Scale set VMs are numbered from 0 without reuse; raising `sku.capacity`
  creates them with their interfaces and addresses, and lowering it deletes the
  newest. A PUT to an existing scale set that leaves out `sku` keeps the
  capacity. One that gives a `sku` without a capacity fails, as Azure answers
  one that changes the model, unless it repeats the create of a scale set made
  without a capacity with nothing changed since: then it succeeds and changes
  nothing, so a retry after a lost response converges, which is inferred from
  ARM PUTs being idempotent. A new scale set needs a `sku` with a name. One
  created without a capacity starts with no VMs, as a Flexible one does in
  Azure; for Uniform, both are inferred.
- Resource SKUs list none, so availability checks see no restrictions.
- Every resource provider namespace is `Registered` unless
  `WithUnregisteredProviders` names it; Azure registers only some by default,
  such as `Microsoft.Resources` and `Microsoft.CostManagement` but not
  `Microsoft.CostManagementExports`, as Microsoft documents. A call to a
  `NotRegistered` namespace fails with 409 `MissingSubscriptionRegistration`,
  naming it as the detail's target, as an Azure CLI recording shows.
  Registering answers 200 with the provider `Registering` and no operation to
  poll, as Azure answers; it becomes `Registered` after the reads
  `WithSettleAfter` sets (gets of the provider, and export writes it turns
  away) or on `Settle`, and at once by default. A namespace being registered
  takes calls, as Microsoft documents that callers need not wait for it.
  `ProviderState` and `ProviderRegistrations` show the states and how many
  register calls each got. Registering a `Registered` namespace leaves it
  `Registered`, which is inferred. A provider lists no resource types and
  unregistering is not served.
- A Cost Management export create answers 201 and an update 200, each with a
  new eTag, as Azure CLI recordings show. An update needs the current eTag in
  the body: without one it fails with 400 "Etag value required for update
  operation of report: {name}.Please get the latest to avoid overwrite.", as
  Azure answers; with a stale one, 412, which is inferred, as is its message.
  Cost Management errors carry the HTTP status as their code, such as `404`,
  and end in a request ID, as Azure answers. A missing export answers 404 "No
  export found for {name}, Subscription: {subscription}.", as Azure answers a
  get; that a delete of one answers the same is inferred (the SDK accepts only
  200 from a delete). An export write while `Microsoft.CostManagementExports`
  is `Registering` fails with 503, as Microsoft documents; the message is
  inferred. One while it is `NotRegistered` is not refused, since no source
  shows how Azure answers it. The destination storage account and container
  are recorded, not checked. The answer leaves out the destination's `type`,
  shows a null `rootFolderPath` when none was given, and writes recurrence
  dates in RFC 3339 UTC, as Azure does. Exports live at a subscription's scope,
  so deleting a resource group, even the destination's, leaves them. Runs,
  run history, `$expand` and other scopes are not served. `Exports` shows
  each export's definition, destination, schedule and eTag.
- Fault method names are the Azure RBAC action, such as
  `Microsoft.Compute/virtualMachines/write`, and a rule's `Resource` is the
  resource ID in the call's path.

## Requirements

Go 1.27 or later. The module requires nothing beyond the standard library; the
tests that use the official client libraries live in the separate `sdktest`
module.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: [SECURITY.md](SECURITY.md).

## License

[Apache 2.0](LICENSE).
