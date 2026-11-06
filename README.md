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
| [`aws/cur`](https://pkg.go.dev/github.com/parallelworks/squall/aws/cur) | AWS Cost and Usage Reports: report definitions (put with the API's field rules, a per-account limit and a check that the S3 bucket's policy allows report delivery, describe with pages, delete). |
| [`aws/ec2`](https://pkg.go.dev/github.com/parallelworks/squall/aws/ec2) | Amazon EC2: instances (run with client tokens, describe with filters and pages, start, stop, terminate, change type or which attached volumes terminating deletes), images (describe, register from snapshots, deregister), instance types, EBS volumes (create, describe, attach, detach, delete), EBS snapshots (create, copy across regions, describe with filters and pages, delete), placement groups, tags, Elastic IP addresses (allocate, describe, release, associate with an interface or instance, disassociate), network interfaces (create with client tokens, describe, delete), and VPC networking: VPCs and their DNS attributes, subnets, availability zones, internet gateways (create, attach, detach, delete), NAT gateways on Elastic IP addresses, route tables with their routes and subnet associations, DHCP options, security groups (create, describe groups and rules with filters and pages, authorize and revoke ingress and egress rules, delete) with each VPC's default group, and VPC attachments to transit gateways the test sets up, refusing deletes with dependencies as EC2 does. |
| [`aws/route53`](https://pkg.go.dev/github.com/parallelworks/squall/aws/route53) | Amazon Route 53: private hosted zones (create with caller references, get, list by name with pages, delete), their VPC associations, tags, and record sets (change in atomic batches with CREATE, UPSERT and DELETE, list with pages), and changes (get). |
| [`aws/s3`](https://pkg.go.dev/github.com/parallelworks/squall/aws/s3) | Amazon S3: general purpose buckets (create, head, list with pages and filters, delete), their tags, versioning, policy, CORS rules and request payment, objects (put, delete one or many, list with pages, prefixes and delimiters, list versions and delete markers), multipart uploads (create, upload parts, complete with S3's part checks, abort, list uploads and parts with pages), and Object Lock: bucket configuration with default retention, per-version GOVERNANCE and COMPLIANCE retention and legal holds that refuse deletes of a locked version, with governance bypass, on the server's clock. |
| [`azure/arm`](https://pkg.go.dev/github.com/parallelworks/squall/azure/arm) | Azure Resource Manager: resource groups (get, create or update, list with tag filters, list the resources in one, delete with everything in them that nothing outside uses), virtual machines (create or update, get, list, instance view, deallocate, start, delete) and their extensions (create or update, get, list, delete), Uniform scale sets (create or update, update, get, list, delete, delete instances) and their VMs, managed disks (VM OS disks included) and snapshots, with cross-region incremental snapshot copies whose completionPercent climbs to 100, Azure Compute Gallery galleries, image definitions and image versions with their replication status, virtual networks and their subnets, NAT gateways, public load balancers with backend pools and outbound rules, network security groups, network interfaces and public IP addresses, the IP configurations of scale set VMs, private DNS zones, virtual network links and A record sets, resource provider registration, storage accounts with their blob containers (soft-deleted ones listed, legal holds and immutability policies that block deletes), file shares with usage stats, snapshots and soft delete, queues and tables, Azure NetApp Files accounts, capacity pools, volumes, volume snapshots, caches, backup vaults with their backups, backup policies, snapshot policies and volume groups, the Blob service's data plane (containers, and block blobs put, listed with versions, snapshots, soft-deleted and uncommitted blobs, and deleted) with RBAC data actions and account network rules, Cost Management exports at a subscription's scope, Log Analytics workspaces (create or update, recover, get, list, delete with soft deletion) and the default workspace Application Insights makes for a component that names none, Cognitive Services deployments with partner model attestations, and long-running operations to poll. |
| [`azure/graph`](https://pkg.go.dev/github.com/parallelworks/squall/azure/graph) | Microsoft Graph: the organization (list), a directory's domains (list) and users (create with Microsoft Entra's name and password rules, get by ID or user principal name, delete, permanently delete once deleted), with application permissions checked as Graph documents them. |
| [`gcp/bigquery`](https://pkg.go.dev/github.com/parallelworks/squall/gcp/bigquery) | BigQuery: datasets (insert with the API's ID and label rules, get, list with pages, patch friendly name, description and labels with etag checks, delete refusing a dataset that holds tables unless `deleteContents`), optionally refused in a project where a `gcp/serviceusage` server has not enabled the API. |
| [`gcp/compute`](https://pkg.go.dev/github.com/parallelworks/squall/gcp/compute) | Compute Engine: instances (bulkInsert, insert, get, list, aggregatedList, delete, stop, start, setLabels, setMachineType, addAccessConfig, attachDisk, detachDisk), disks (get, list, aggregatedList, insert, delete), snapshots (insert, get, list, delete), zones, regional addresses (get, list, insert, delete, setLabels), resource policies, instance templates, routes, custom mode VPC networks with their default route, subnetworks with secondary ranges, routers with Cloud NAT (insert, get, patch, delete, list, aggregatedList), firewalls, global addresses including ranges for private services access, and zone, region and global operations (get, wait). |
| [`gcp/lustre`](https://pkg.go.dev/github.com/parallelworks/squall/gcp/lustre) | Managed Lustre: instances (create with the API's capacity, tier, ID, filesystem, root squash and maintenance rules, get, list with filters on name, state, filesystem, network, description and labels, patch labels, description, gkeSupportEnabled and capacity, delete) and their long-running operations (get, list, delete, cancel). |
| [`gcp/resourcemanager`](https://pkg.go.dev/github.com/parallelworks/squall/gcp/resourcemanager) | Cloud Resource Manager: projects (get by ID or number, getIamPolicy, setIamPolicy with etag checks, testIamPermissions answered from the server's access policy), seeded by the test. |
| [`gcp/servicenetworking`](https://pkg.go.dev/github.com/parallelworks/squall/gcp/servicenetworking) | Service Networking: private service connections (create, list by network, patch with force, deleteConnection), checked against a `gcp/compute` server's networks and allocated ranges and peering the network there, and their long-running operations (get). |
| [`gcp/serviceusage`](https://pkg.go.dev/github.com/parallelworks/squall/gcp/serviceusage) | Service Usage: services (enable, disable, get, list by state) in projects seeded by the test and named by number or ID, and their long-running operations (get). |
| [`gcp/storage`](https://pkg.go.dev/github.com/parallelworks/squall/gcp/storage) | Cloud Storage: buckets (insert, get, list, patch labels, versioning, CORS, retention policies, soft delete policies, Requester Pays and uniform bucket-level access, lock a retention policy, delete with soft delete, list and restore soft-deleted ones), managed folders (insert, get, list with prefixes and pages, delete, empty unless allowNonEmpty) and objects (multipart and media uploads, get, patch and update metadata, temporary, event-based and default holds, object retention, list with versions or soft-deleted generations, delete by generation, restore). |

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
- Networks are custom mode only, and each gets the default route to
  `0.0.0.0/0` that goes with it. Subnetworks, routers, firewalls and
  `VPC_PEERING` global addresses must name a network that exists (404
  otherwise), and subnetwork ranges must not overlap within the network or
  with its allocated ranges. Deleting a network or subnetwork that something
  still uses, a peering included, fails with 400
  `resourceInUseByAnotherResource`. A router patch replaces its `nats`, as a
  JSON merge patch does. A `VPC_PEERING` range with no address gets the first
  free block of its size in 10.0.0.0/8.
- Instances, routes and regional addresses may name networks and subnetworks
  the stand-in does not hold, and service accounts and accelerators are not
  checked. Fields the stand-in only records keep the JSON the caller sent, and
  fields it does not know are rejected with 400. Instance group managers are
  not supported: their aggregated list is always empty.
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

For Cloud Resource Manager:

- Projects are seeded with `WithProject` or `PutProject` and are always
  `ACTIVE`. A project the server does not hold answers 403
  `PERMISSION_DENIED`, as the API does for one that does not exist.
- A `setIamPolicy` with a stale etag answers 409 `ABORTED`. Conditional
  bindings need policy version 3. Audit configs are rejected with 400.
- `testIamPermissions` answers from the server's `access.Policy`, not the
  project's own policy, so a test can check code that tests its own
  permissions before acting.

For BigQuery:

- Point the client at `srv.URL + "/bigquery/v2/"`.
- Datasets hold a friendly name, a description, labels and a location, which
  defaults to `US`. Other dataset fields are rejected with 400, and projects
  are not checked.
- Tables are not served; `AddTable` puts one in a dataset, so a delete
  without `deleteContents` is refused with 400 `resourceInUse`.
- `WithServiceUsage` refuses calls in a project where a `gcp/serviceusage`
  server has not enabled `bigquery.googleapis.com`, with 403
  `accessNotConfigured`.

For Service Usage:

- Projects are seeded with `WithProject` and start with no service enabled.
  A project the server does not hold, or a service outside its catalog
  (extended with `WithService`), answers 403 `PERMISSION_DENIED`, as the API
  does for one that does not exist.
- Changes finish before the call returns. Enabling an enabled service
  answers with the done operation `operations/noop.DONE_OPERATION`;
  disabling a disabled one is refused with `FAILED_PRECONDITION`.
  Dependencies between services are not modeled.

For Service Networking:

- `WithCompute` checks a connection's network and reserved ranges against a
  `gcp/compute` server and adds the `servicenetworking-googleapis-com` peering
  to the network, so the network cannot be deleted until `deleteConnection`.
  `WithProjectNumber` lets a network be named by project number or ID;
  connections name it by number, as the API does.
- Changes finish before the call returns. A second create on a network, or
  one naming a range that is not a `VPC_PEERING` address on it, ends its
  operation in `FAILED_PRECONDITION`, the first with the API's "Please use
  UpdateConnection." message. A patch that drops a range needs `force=true`.
  Producer services are not modeled, so ranges are never in use by them.

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
- Key pair names are not validated, and there is no default VPC:
  `RunInstances` needs a subnet or a network interface. A subnet ID the
  stand-in does not hold is accepted there and by `CreateNetworkInterface`,
  with any security group IDs, as before VPCs existed; in a subnet created
  with `CreateSubnet`, interfaces and instances take the subnet's VPC, zone
  and an address from its CIDR block, and security groups of that VPC: the
  ones named, which must exist there, or the VPC's default group.
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
  `Encrypted=false`, and a `KmsKeyId` without `Encrypted=true`, are accepted,
  though the API reference allows neither.
- `ModifyInstanceAttribute` changes the instance type of a stopped instance,
  or, with `BlockDeviceMapping`, the `DeleteOnTermination` of volumes
  attached to the instance in any state, `true` when a mapping leaves it
  out; `DescribeVolumes`, `DescribeInstances` and termination follow the new
  value (sourced: the API reference for `ModifyInstanceAttribute` and
  `BlockDeviceMapping.N`). A device with no volume of the instance attached,
  or a `VolumeId` other than the one there, fails with
  `InvalidBlockDeviceMapping` and changes nothing, and both attributes in one
  call fail with `InvalidParameterCombination` (inferred: the reference says
  only one attribute can change at a time and names no codes).
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
  10.0.0.0/8, or from the subnet's CIDR block for subnets the stand-in holds.
  There is no address quota unless the test sets one.
- VPCs and subnets take IPv4 CIDR blocks from /16 to /28; IPv6, secondary
  CIDR blocks and IPAM pools are not supported. Each VPC gets a main route
  table with the local route, a `default` security group, and the region's
  default DHCP options set. A region has the zones `WithAvailabilityZones`
  sets, or the region with `a`, `b` and `c` appended, with zone IDs such as
  `use1-az1`.
- Security groups belong to a VPC. The `default` group allows all traffic
  from its members and all outbound traffic, is deleted with its VPC and
  cannot be deleted on its own (`CannotDelete`); a new group starts with the
  allow-all outbound rule. Rules take IPv4 and IPv6 CIDR blocks and groups in
  the same VPC as peers, one rule with an `sgr-` ID per peer. Duplicates fail
  with `InvalidPermission.Duplicate` and revoking a missing rule with
  `InvalidPermission.NotFound`, as in a nondefault VPC. Prefix lists, the
  flat `CidrIp` parameters and rule tags are not supported. Group names stand
  in for IDs only in `DescribeSecurityGroups`, where they match groups of that
  name in any VPC.
- Deletes fail with `DependencyViolation` while something depends on the
  resource, as in EC2: a VPC with subnets, an attached internet gateway,
  extra route tables or security groups other than its default one; a
  security group an interface or instance uses or another group's rule refers
  to; a subnet with network interfaces (a NAT gateway's
  included until it is deleted) or a transit gateway attachment; an attached
  internet gateway; a main or associated route table; DHCP options in use.
  Detaching an internet gateway while the VPC has public addresses mapped
  fails the same way.
- NAT gateways and transit gateway attachments move from `pending` to
  `available` and from `deleting` to `deleted` as describes see them, like
  instances, and stay visible once deleted. A NAT gateway on an Elastic IP
  address that is already associated, or in a VPC with no internet gateway,
  ends `failed` with EC2's failure code. Routes whose target is gone read
  `blackhole`. Transit gateways themselves are set up by the test with
  `PutTransitGateway`; the stand-in has no API to create them.
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
  without regions. `ListObjectsV2` lists each key whose newest version is
  not a delete marker, in UTF-8 binary order, and its continuation tokens are
  positions. `ListObjectVersions` orders by key and then newest first, and its markers
  are positions, so the next page still follows one whose versions were just
  deleted. `DeleteBucket` fails with `BucketNotEmpty` while any version or
  delete marker remains.
- Objects keep their size and ETag, not their data. Checksums are not
  verified, and copies, object tags and lifecycle are not supported. Policies
  and CORS rules are recorded, not enforced, and so is request payment: with
  one account, every caller owns the bucket and pays nothing extra.
- Multipart uploads check their parts on completion as S3 does
  (`InvalidPartOrder`, `InvalidPart`, `EntityTooSmall` for a part under 5 MiB
  before the last), and completing one ends it, so a retry after a lost
  response fails with `NoSuchUpload`. `MultipartUploads` reports the parts an
  upload holds, which S3 bills as storage, and `CreateMultipartUpload` starts
  one directly. S3 documents in-progress uploads as blocking `DeleteBucket`
  only for directory buckets, so deleting a general purpose bucket discards
  them.
- Object Lock refuses to delete a version under an unexpired retention or a
  legal hold with 403 `AccessDenied`, by the server's clock. `COMPLIANCE`
  never yields; `GOVERNANCE` yields to `x-amz-bypass-governance-retention`
  from a caller granted `s3:BypassGovernanceRetention`. A delete without a
  version ID adds a delete marker, and `DeleteBucket` fails while any version
  remains. Changing a `GOVERNANCE` retention to `COMPLIANCE` also needs the
  bypass, which is inferred: S3 documents it only for shortening and removal,
  but lets no one alter governance lock settings without special permissions.
  Object Lock writes need Content-MD5 or a checksum, multipart parts
  included. `SetRetention` and `SetLegalHold` lock a version directly. Event
  holds are not supported.
- A fault rule's `Resource` is the bucket name, or `bucket/key` for an object
  call. A lost `HeadBucket` breaks off inside its status line, since a sent
  header would read as an answer.

For Amazon Route 53:

- Only private hosted zones are supported: `CreateHostedZone` needs a VPC.
  VPCs are any ID and region unless `WithVPCLookup` says which exist, so a
  test can back it with another stand-in. Signatures are not checked.
- A reused `CallerReference` fails with `HostedZoneAlreadyExists`, even after
  its zone is deleted. A zone name already associated with the same VPC fails
  with `ConflictingDomainExists`, on create and on `AssociateVPCWithHostedZone`;
  removing a zone's last VPC fails with `LastVPCAssociation`.
- Each zone starts with its SOA and NS record sets, which cannot be deleted,
  and `DeleteHostedZone` fails with `HostedZoneNotEmpty` while any other
  remains. Changes are `INSYNC` at once.
- A change batch applies in order and atomically. A failed one answers
  `InvalidChangeBatch` with a message per failed change: creating a set that
  exists, deleting one that is missing or whose TTL and values differ, a name
  outside the zone, a bad A or AAAA value, or a CNAME that shares its name.
  Record sets are simple or alias ones: set identifiers, routing policies and
  health checks are rejected with `InvalidInput`.
- Names are lowercased with a trailing dot, and other characters are `\ooo`
  escapes, so `*` lists as `\052`. Lists order by name with its labels
  reversed, then by zone ID or record type.
- A fault rule's `Resource` is the bare hosted zone ID, or the zone's name for
  `CreateHostedZone`.

For Cloud Storage:

- Point the client at `srv.URL + "/storage/v1/"`, or set
  `STORAGE_EMULATOR_HOST`. There is one caller, which reaches every project;
  `PutForeignBucket` holds a name it cannot reach, so inserting it fails with
  409 `conflict` and calls on it with 403 `forbidden`. A repeated insert of
  the caller's own bucket fails with 409, as the client's retry of a create
  that took effect does.
- Bucket names and labels follow the API's rules; locations are upper-cased
  but not checked. Fields the server does not model, such as lifecycle rules,
  are rejected with 400. A patch with a field set to null clears it, and a
  label set to null is deleted.
- Retention policies, holds and object retention refuse deletes and
  overwrites with Cloud Storage's 403s; a locked policy can only grow. Soft
  delete is on by default for 7 days: deleted and replaced generations are
  listed with `softDeleted=true` and restored until their `hardDeleteTime` on
  the server's clock (`WithClock`), and deleted buckets are kept the same
  way. Requester Pays is enforced only under `WithPolicy`. `SetObjectHolds`,
  `SetObjectRetention`, `SetRetentionPolicy` and `PutSoftDeletedObject` set
  up protected or soft-deleted state directly.
- Objects keep their size, MD5 and CRC32C, which an upload's checksums must
  match. With versioning on, overwrites and deletes keep the old generation as
  noncurrent, and a bucket with any generation left cannot be deleted. Page
  tokens are positions, so a listing survives deletes between pages.
  Resumable uploads, media reads, IAM policies and HMAC keys are not
  supported.
- Managed folders need uniform bucket-level access on their bucket, and their
  names end with `/`. Deleting one that holds objects or other managed
  folders fails unless `allowNonEmpty=true`, which needs
  `storage.managedFolders.setIamPolicy` under `WithPolicy` and leaves what it
  held in place. A bucket with a managed folder left cannot be deleted, nor
  can its uniform bucket-level access be turned off. `PutManagedFolder` makes
  one directly. Managed folder IAM policies are not supported.
- A fault rule's `Resource` is the path below `/storage/v1/`, such as
  `b/my-bucket/o/my-object` or `b/my-bucket/managedFolders/logs/`, and for an
  insert the bucket, object or managed folder it makes.

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
- Listing a group's resources lists the types ARM tracks, those with a
  location, nested ones such as VM extensions and virtual network links
  included, ordered by ID; subnets, record sets, containers and other proxy
  types are left out. Each entry shows the fields a GET of the resource shows
  among `id`, `type`, `location`, `tags`, `extendedLocation`, `kind`, `plan`,
  `sku`, `identity` and `managedBy`, and with `$expand` its
  `provisioningState`. It takes a tag `$filter`, and with a `tagValue`
  clause leaves out each resource's tags, as Azure does, and an `$expand` of
  `createdTime` and `provisioningState`. Other filters (on name, location or
  `resourceType`, or with `substringof` or `startswith`) and `changedTime`
  fail with 400.
- Listing resource groups or a group's resources with `$top` returns at most
  that many, with no `nextLink`; Azure may page them instead.
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
- A Log Analytics workspace create answers 201 and an update 200, polled
  through an `Azure-AsyncOperation` URL under
  `Microsoft.OperationalInsights/locations/{location}/operationstatuses`, and
  a delete 200, or 202 with a `Location` URL under
  `workspaces/{name}/operationresults` as well, as Azure CLI recordings
  (`test_monitor_log_analytics_workspace_default`) and the REST API's
  `WorkspacesDelete` example show; deleting a missing workspace answers 204,
  as Microsoft documents. A workspace shows a `customerId`, and the PerGB2018
  SKU, 30 days of retention, public network access and the `features` and
  `workspaceCapping` a recording shows unless set: 30 days is the default
  Microsoft documents and PerGB2018 its pay-as-you-go tier, which as the
  default when a create names no SKU is inferred. Deleting a workspace, or
  its group, keeps it as deleted for 14 days, as Microsoft documents, unless
  the delete passes `force=true`; `DeletedLogAnalyticsWorkspaces` lists them.
  A forced delete of a workspace that is already deleted answers 204 and
  leaves it deleted, as Microsoft documents that it must be recovered first.
  A create with the deleted workspace's group, name and location recovers
  it, keeping its `customerId`, showing `Restoring` while in progress (201,
  as a recording of `az monitor log-analytics workspace recover` shows), and
  keeping the properties the create leaves out, as Microsoft documents that
  recovery brings back its configuration. A create in another location while the name is held fails with
  409 `Conflict`; the code and message are inferred. Polling a delete's
  `operationresults` URL needs
  `Microsoft.OperationalInsights/workspaces/operations/read`, which is
  inferred, as Azure defines no `operationresults` action for workspaces. Tables, data sources, deleted workspace listing and
  shared keys are not served.
- An Application Insights component records and echoes
  `properties.WorkspaceResourceId` as sent, and shows `IngestionMode`
  `LogAnalytics` when linked unless set, as the REST API's
  `ComponentsCreate` example shows. The workspace it names is not checked. A
  component created without one gets a workspace made for it, as Microsoft
  Learn documents ("a new Log Analytics resource is created automatically");
  it is `DefaultWorkspace-{subscription}-{code}` in the group
  `DefaultResourceGroup-{code}`, both in the component's location, where
  `{code}` is the location's `laRegionCode` in the Application Insights
  portal's region mapping, the names the Azure CLI uses for the same default
  (`appservice/_create_util.py`). Making them in Application Insights rather
  than the client, their location, and using the location's name as the code
  for one the mapping gives none are inferred. Later components reuse them,
  an update that leaves out the link keeps it (inferred), and they outlive
  the component's group, as `Resources` and `ResourceGroups` show. A deleted
  default workspace is recovered by the next component that needs it, as a
  create recovers one (inferred). No RBAC action is checked on them.
- A Cognitive Services deployment records and echoes
  `properties.modelProviderData` (`industry`, `organizationName`,
  `countryCode`), which clients send for partner models such as Anthropic's
  on newer api-versions than the SDK's, as the Azure-Samples Claude starter
  kit and an azure-rest-api-specs issue (#43610) show; the published specs
  do not describe it. Other members fail with `InvalidRequestContent`.
  Values are not checked, and a partner model deployed without it is not
  failed, though those sources report Azure failing it with
  `AnthropicOrganizationCreationFailed`. `Deployments` shows it.
- The Blob service is `srv.Blob()`, an `http.Handler` of its own: serve it
  with `httptest` and make the azblob client with `{its URL}/{account}`, as
  for the storage emulator, any token credential and
  `InsecureAllowCredentialWithHTTP`; a host name such as
  `{account}.blob.core.windows.net` (or `{account}.privatelink.blob...`)
  names the account too. Container names are lower case, 3 to 63 letters,
  digits and hyphens, through ARM as through the Blob service, or `$root`
  and `$web` (not `$logs`); a blob in the root container is addressed with
  `$root` in the path (with no `/` in its blob names), and implicit root
  addressing answers 400. It shares the ARM
  server's accounts, containers, blob service properties, faults and policy,
  so deleting a container or account through ARM deletes its blobs. A missing
  account answers 404 `ResourceNotFound`, where Azure's host name would not
  resolve. Blobs keep their size, MD5 and properties, not data (Get Blob is
  not served). Versioning, blob and container soft delete, snapshots and
  uncommitted blocks follow the blob service properties (versions written
  while versioning was on are kept after it is turned off, and a snapshot of
  a versioned blob makes a version); permanent delete,
  undelete, leases, tags, tiers and page or append blobs are not supported.
  Calls need the RBAC data actions Microsoft lists (403
  `AuthorizationPermissionMismatch`), which a grant of `*` covers here though
  a role's `*` does not in Azure; `networkAcls` that deny by default, or
  disabled public network access, answer 403 `AuthorizationFailure` unless an
  IP rule allows the caller.
- Version-level immutability policies are not modeled: a non-empty default
  policy on an account's version-level settings, or an immutability policy
  on a version-level immutable container, answers 400, as do changing a
  locked container policy, legal holds on a version-level immutable
  container and `allowProtectedAppendWritesAll`; deleting or extending a
  policy is not served (404). A container's policy can be read back (404
  when it has none, inferred, as Azure's spec lists only 200; prefer the
  container's `hasImmutabilityPolicy`). Locking takes the policy's ETag,
  not `*`, and locking a locked policy changes nothing. A container takes 1 to 10 legal hold tags, which
  are lower-cased so case variants count once.
- Container legal holds block overwrites, snapshots and deletes of their
  blobs, and deleting the container itself, even empty, until the hold is
  cleared (inferred: the container-level WORM table says container deletion
  fails while a legal hold is in effect, its Deletion section that the
  container must be empty). An immutability policy blocks deletes for its
  days, and overwrites and snapshots for as long as it exists; a container
  under one can be deleted through ARM once empty. Deleting an account with
  a container under a locked policy or a legal hold fails with 409
  `AccountProtectedFromDeletion`, as Azure answers. A legal hold protects
  the account even on an empty container, as Microsoft's data protection
  overview says, though the container-level WORM table adds "at least one
  blob" and its note says unlocked policies "and/or legal hold" don't
  provide delete protection (failing closed is inferred). Uncommitted
  blocks alone are not a blob, here or in `Blobs()`. A blocked group delete
  leaves the account whole, shares, queues and tables included. Soft-deleted
  containers do not protect their account (inferred from the soft delete
  page: "Container soft delete doesn't protect against the deletion of a
  storage account"), while soft-deleted blobs count as data for holds,
  policies and version-level immutability (inferred).
- A container with version-level immutability can be deleted only through
  ARM once empty (`ContainerImmutableStorageWithVersioningEnabled`
  otherwise, with the message Azure shows on Microsoft Q&A 5656033 and
  1300488; using it for the Blob service is inferred). An account with it
  enabled cannot be deleted while any container exists, and one without it
  cannot while it has such a container; an account with it and no
  container can be deleted (inferred, as the version-level WORM page says
  both "it can't be deleted until it's empty" and that deletion fails "if
  it's enabled for the account", and the setting cannot be turned off).
  Account version-level immutability is set only at creation, kept across
  updates and implies versioning; a container takes it only with
  versioning on, and versioning cannot then be turned off ("Conflicting
  feature 'VersionLevelWorm' is enabled. Please disable it and retry.", as
  on Q&A 5656033; that a soft-deleted container's counts is inferred).
  Blob-level immutability policies and legal holds are not supported
  (400), and version-level immutability with a hierarchical namespace, on
  the account or a container, answers 400. Retention days run against the
  server's clock, so a test of expiry should make the server with
  `arm.WithClock` and move the clock rather than wait.
- File share snapshots and soft delete follow Azure's ARM API: a create
  with `$expand=snapshots` snapshots a share, a GET with `x-ms-snapshot`
  reads one, and lists take `$expand=deleted,snapshots` and a name-prefix
  `$filter`. A DELETE takes its snapshots too unless `$include=none` (409
  `ShareHasSnapshots` then), and keeps the share as deleted while the file
  service's `shareDeleteRetentionPolicy` is on, which it is for 7 days on a
  new account, as in Azure: disable it to delete shares for good. Setting
  file service properties keeps the ones the call leaves out, as Microsoft
  documents for the data plane, and merges within objects, so setting only
  SMB versions keeps multichannel (inferred from the
  `Update-AzStorageFileServiceProperty` examples); arrays and values
  replace, so `corsRules: []` clears the rules. Restore
  brings a deleted share back. Snapshot times have 100ns resolution, as
  Azure's do. Leases are not modeled. The create's 201 with `snapshotTime`,
  a missing share's 404 there, a snapshot's usage, the error codes (the
  Files service's), unique snapshot times and not listing a deleted share's
  snapshots are inferred. `SnapshotShare` and
  `SoftDeleteShare` set either up directly. Deleting the account deletes
  both; snapshots do not protect it, as Microsoft documents, nor do
  soft-deleted shares (inferred).
- Azure NetApp Files accounts, capacity pools, volumes and volume snapshots
  report `systemData`, whose `createdAt` equals `lastModifiedAt` on create
  and whose `lastModifiedAt` moves only with a write that takes effect. A
  snapshot create is long-running like other NetApp writes; a snapshot shows
  `created` and a `snapshotId`. The API gives snapshots no tags, so a create
  that sends any, even `"tags": {}`, is rejected with 400, as squall rejects
  input it does not model. A volume holds up to 255 snapshots, as Microsoft
  documents; one more fails with 409 `SnapshotLimitReached` (status, code
  and message inferred). Deleting a volume deletes its snapshots, with or
  without `forceDelete`, as Microsoft documents, and a snapshot create in
  progress when the delete begins finishes first (inferred). Creating an
  existing snapshot answers 201 at once and changes nothing (inferred).
  Snapshots are listed among their group's resources (inferred).
  `SnapshotVolume` adds a snapshot directly, at a time the test chooses.
- An Azure NetApp Files account's backup vaults, backup policies, snapshot
  policies and volume groups, and a capacity pool's caches, are created
  (long-running, except a snapshot policy's, as the API has it), read, listed
  and deleted, with `systemData` and, where the API has them, tags. A get or
  list of any NetApp resource under a parent that does not exist fails with
  404 `ResourceNotFound` naming the topmost missing one, as one under a
  missing virtual network does (inferred). Deleting an account that holds any
  of them or a pool that holds a cache (inferred), a backup vault that holds
  backups (Microsoft documents it must be emptied first), a snapshot policy or
  vault a volume's `dataProtection` names or a volume group that holds volumes
  (documented), or a backup policy a volume names (inferred) fails with 409
  `CannotDeleteResource` (status and code inferred). Volume groups that create
  volumes are rejected with 400 as not modeled; `AddVolumeGroup` puts existing
  volumes in a group instead. A cache takes an address in its cache subnet;
  its peering, `cacheState` and write-back are not modeled. Backups are not
  created through the API: `AddBackup` adds one to a vault, and it outlives
  its volume, as Microsoft documents. A backups list's `$filter`, a volume's
  resource ID (case-insensitive), lists that volume's backups, as the API's
  description of `$filter` says (that Azure filters on it is inferred); the
  `volumeResourceId` parameter, which Microsoft's .NET client sends because
  its code notes the service expects it, does the same. Anything but a NetApp
  volume's ID, either one given twice, or the two naming different volumes is
  rejected with 400. `AddBackupVault`, `AddBackupPolicy`, `AddSnapshotPolicy`
  and `AddCache` add the rest directly.
- Fault method names are the Azure RBAC action, such as
  `Microsoft.Compute/virtualMachines/write`, and a rule's `Resource` is the
  resource ID in the call's path. Blob service faults name the operation,
  such as `PutBlob` or `ListBlobs`, and `{account}/{container}/{blob}`;
  calls the Blob service refuses as not served never match a fault.

For Microsoft Graph:

- Permanently deleting a deleted user (`DELETE /directory/deletedItems/{id}`)
  answers 204 and moves it from `DeletedUsers` to `PurgedUsers`, as
  Microsoft documents. It needs `User.DeleteRestore.All`, the only
  application permission Graph lists for a user; `Directory.ReadWrite.All` is
  documented as not allowing user deletion, and without it the call fails
  with the stand-in's usual 403 `Authorization_RequestDenied`. An ID that is
  not a deleted user's fails with 404 `Request_ResourceNotFound`, the status
  and code a public report records for the call; the message is inferred.
- `GET /organization` lists one organization, as Graph documents, in the
  shape of its example: `WithOrganization` sets its ID (the tenant ID),
  display name, `countryLetterCode` and `defaultUsageLocation`, which default
  to `00000000-0000-0000-0000-000000000001`, Squall, US and US, and
  `verifiedDomains` are the verified domains `WithDomains` sets. It needs
  `Organization.Read.All`, `Directory.Read.All`,
  `Organization.ReadWrite.All` or `Directory.ReadWrite.All`, as Graph
  documents. Each verified domain's capabilities and type are inferred.
- The initial domain, which domains and the organization's verified domains
  mark with `isInitial`, is the directory's `onmicrosoft.com` domain,
  whichever domain is the default, as Microsoft Entra documents; that it is
  the first such domain `WithDomains` sets is inferred.

## Requirements

Go 1.27 or later. The module requires nothing beyond the standard library; the
tests that use the official client libraries live in the separate `sdktest`
module.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: [SECURITY.md](SECURITY.md).

## License

[Apache 2.0](LICENSE).
