// Package arm is a stand-in for Azure Resource Manager, for testing code that
// calls it through the official Azure SDK for Go.
//
// [Server] is an [http.Handler] that keeps resources in memory and answers the
// REST calls the SDK's arm* client packages make. Serve it with
// [net/http/httptest] and point a client at it by giving the client options a
// cloud configuration whose ResourceManager endpoint is the server's URL, with
// any token credential and InsecureAllowCredentialWithHTTP. A
// [fault.Injector] makes chosen calls fail, including after they took effect,
// so tests can check that code which retries converges.
//
// It covers:
//
//   - resource groups: get, check existence, create or update, list (with
//     tag filters) and delete, which deletes everything in the group that
//     nothing outside it uses;
//   - Microsoft.Network: virtual networks, NAT gateways, load balancers,
//     network security groups, network interfaces, public IP addresses and
//     private endpoints (create or update, update tags, get, delete and list
//     in a subscription; virtual networks, NAT gateways and load balancers
//     also list in a group), subnets (create or update, get, list
//     and delete), and the IP configurations and public IP addresses of scale
//     set VMs (get);
//   - Microsoft.Network private DNS: zones and virtual network links in the
//     global location (create or update, get; another location fails with
//     MissingRegistrationForLocation), and A record sets (create or update,
//     get, delete);
//   - Microsoft.Compute virtual machines: create or update, get, list in a
//     group, instance view, deallocate, start and delete, and their
//     extensions (create or update, get, list on a VM and delete);
//   - Microsoft.Compute scale sets in the Uniform orchestration mode: create
//     or update, update, get, list in a group, delete and delete instances,
//     and their VMs (list, get);
//   - Microsoft.Compute resource SKUs: list, which lists none;
//   - Microsoft.Compute managed disks and snapshots: create or update, get,
//     list in a group and delete;
//   - Microsoft.Compute galleries, their image definitions and image
//     versions: create or update, get, list in their group, gallery or image
//     definition, and delete;
//   - Microsoft.Storage: storage accounts (create, get, list in a group or
//     subscription, list keys and delete), name availability checks, the
//     blob and file services' properties (set and get), and blob containers
//     and file shares (create, get, list and delete);
//   - Microsoft.NetApp: accounts, capacity pools and volumes (create or
//     update, get, list in their group, account or pool, and delete);
//   - Microsoft.StorageCache: Azure Managed Lustre file systems (create or
//     update, get, list in a group and delete), and subnet checks;
//   - Microsoft.CognitiveServices: accounts (create or update, get, list keys
//     and delete), their deployments (create or update, get, list and
//     delete), deleted accounts (get and purge) and model capacities (list,
//     as [WithModelCapacities] sets them);
//   - Microsoft.Search: search services (create or update, get and delete)
//     and their admin keys (list);
//   - Microsoft.KeyVault: vaults (create or update, get and list in a group);
//   - Microsoft.Insights: Application Insights components (create or update,
//     get and list in a group);
//   - Microsoft.OperationalInsights: Log Analytics workspaces (create or
//     update, which recovers a deleted one, get, list in a group and delete);
//   - Microsoft.MachineLearningServices: workspaces (create or update, get and
//     delete);
//   - Microsoft.Authorization: role assignments at a resource group's scope
//     (create and delete);
//   - Microsoft.Resources resource providers: a subscription's registration
//     of a namespace (get and register);
//   - Microsoft.CostManagement: exports at a subscription's scope (create or
//     update, get, list and delete).
//
// Other calls answer 404. Every call needs an api-version, which may be any
// value, and a bearer token, which is not checked. Where it differs from
// Azure:
//
//   - Long-running operations finish before the call that starts them
//     returns: a create answers 201, an update 200, with provisioningState
//     Succeeded, and an action or delete answers 200 with no polling header,
//     so the SDK's pollers are done at once. [WithSettleAfter] holds changes in
//     progress instead: the call answers 200, 201 or 202 with an
//     Azure-AsyncOperation URL the server answers (and, for an action or
//     delete, a Location URL), and the resource shows Creating, Updating or
//     Deleting, for more reads of it or its operation, or until
//     [Server.Settle]. Polling URLs carry a retry-after-ms of 10, so the SDK
//     polls quickly. Deleting a resource group answers 202 with a Location
//     URL only, and a private DNS zone or link change in progress answers 202
//     with an empty object, as Azure does.
//   - A change to a resource that has another in progress is answered as its
//     provider answers it. Virtual networks, subnets, NAT gateways, load
//     balancers, network security groups, network interfaces and public IP
//     addresses fail it with 429 RetryableError
//     (RetryableErrorDueToAnotherOperation) and virtual network links with 409
//     Conflict, counting the call as a read of the change in progress. A VM
//     or scale set change preempts the one in progress, which takes effect
//     and ends Canceled with OperationPreempted, unless that one is a
//     deletion: then a delete answers with it and other changes fail with 409
//     OperationNotAllowed. Other changes in progress, such as a zone's, and
//     those to resources a call references, finish first. A change to a
//     resource group being deleted, or to anything in it, fails with
//     ResourceGroupBeingDeleted.
//   - Deleting a resource group deletes each resource in it once nothing uses
//     it. When something outside the group still uses a resource in it, such
//     as a VM in another group attached to one of its network interfaces, or
//     an interface elsewhere that references its network security group or
//     public IP address, the deletion deletes the rest and its operation fails
//     with 409 ResourceGroupDeletionBlocked, leaving the group Succeeded with
//     the resources in use and what they use. The error has a detail for each
//     resource left, whose target is its ID and whose message is the error
//     deleting it alone answers, such as NicInUse, as Azure reports it. The
//     status, and that everything else is deleted before the operation
//     fails, are inferred.
//   - Resource names, subscription IDs and locations are not validated, and
//     names match without regard to case. Tags are not validated. Fields the
//     server only records keep the JSON the caller sent; fields it does not
//     know are rejected with InvalidRequestContent, and values it does not
//     model, such as a Flexible scale set, with BadRequest.
//   - A virtual network PUT replaces its subnets: properties.subnets creates
//     or updates those it lists and deletes the rest, failing with
//     InUseSubnetCannotBeDeleted when one holds an address. A subnet takes
//     addressPrefix or addressPrefixes, IPv4 only, which must lie in the
//     virtual network's address space (400 NetcfgSubnetRangeOutsideVnet), not
//     overlap another subnet's (400 NetcfgSubnetRangesOverlap) and be no
//     smaller than a /29, the smallest Microsoft documents; a smaller one fails
//     with 400 NetcfgInvalidSubnet, the code Azure answers for an invalid
//     subnet, which for this case is inferred. A prefix that is not a network
//     address in CIDR notation fails with 400 InvalidCIDRNotation, the code
//     Microsoft documents; its message is inferred. Changing the prefixes of a
//     subnet that holds an address fails with 400 InUseSubnetCannotBeUpdated.
//     A subnet's NAT gateway and network security group must exist in its
//     virtual network's location, or the write fails with
//     InvalidResourceReference, and a PUT that leaves one out detaches it.
//     Delegations and service endpoints are recorded: their actions and
//     locations are as the caller sent them, where Azure fills them in. A
//     subnet does not list the IP configurations, private endpoints or other
//     resources in it. Deleting a subnet, or the virtual network holding it,
//     while it holds an address fails with 400 InUseSubnetCannotBeDeleted,
//     naming what holds it; deleting a virtual network deletes its subnets.
//     A missing subnet answers 404 ResourceNotFound, as other types do, which
//     is inferred.
//   - A NAT gateway takes the Standard SKU, which it shows with the Regional
//     tier, and is Standard when the body names none, which is inferred; its
//     idleTimeoutInMinutes is 4 unless set. Its public IP addresses must exist
//     in its location, and each shows the NAT gateway as its natGateway. One
//     already used by an interface, a load balancer or another NAT gateway
//     fails with 400 PublicIPAddressInUse, and a public IP address a NAT
//     gateway uses cannot be deleted (PublicIPAddressCannotBeDeleted); both
//     are inferred from how Azure answers an interface's public IP address.
//     Public IP prefixes are recorded, not checked. The NAT gateway lists the
//     subnets that use it, and deleting it while one does fails with 400
//     InUseNatGatewayCannotBeDeleted; the code and message are inferred.
//   - A load balancer is public and takes the Standard SKU, which it shows
//     with the Regional tier, and is Standard when the body names none, which
//     is inferred. Each frontend IP configuration takes a public IP address in
//     its location, which shows the frontend as its ipConfiguration. One
//     already used by an interface, a NAT gateway or another frontend fails
//     with 400 PublicIPAddressInUse, inferred as for a NAT gateway, and one a
//     frontend uses cannot be deleted. Frontends, backend address pools and
//     outbound rules show as children, with IDs under the load balancer, its
//     etag and provisioningState, and the references between them. An
//     outbound rule's backendAddressPool and frontendIPConfigurations must
//     name children in the same body, or the write fails with
//     InvalidResourceReference, the code Azure answers for a reference that
//     does not resolve, which for one inside a load balancer is inferred. An
//     outbound rule needs protocol, as the API requires; its
//     idleTimeoutInMinutes is 4 and enableTcpReset false unless set. A scale
//     set's or interface's IP configuration joins a pool through
//     loadBalancerBackendAddressPools, which must name a pool a load balancer
//     in its location holds, or the write fails with InvalidResourceReference,
//     and the pool lists it in backendIPConfigurations. A scale set VM's entry
//     there, its IP configuration under
//     virtualMachines/{instance}/networkInterfaces, is inferred.
//   - Outbound rule ports are checked as Microsoft documents outbound rules,
//     with the codes it lists for them: allocatedOutboundPorts that is not a
//     multiple of 8 fails with 400
//     SpecifiedAllocatedOutboundPortsForOutboundRuleIsNotAMultipleOfEight,
//     more than 64,000 with
//     SpecifiedAllocatedOutboundPortsForOutboundRuleExceedsTotalNumberOfAllowedPortsPerRule,
//     and more than the rule's frontends give each member of its pool, 64,000
//     per public IP address, with
//     SpecifiedAllocatedOutboundPortsForOutboundRuleExceedsTotalNumberOfAvailablePorts,
//     naming the ports each member could have, in multiples of 8. The members
//     are the primary IP configurations in the pool, one for each VM of a
//     scale set; secondary ones do not count, as outbound rules do not apply
//     to them. The check fails whichever write would break it: the load
//     balancer's, or the scale set or interface write that grows the pool,
//     such as a capacity raise, as Azure fails a scale set operation with it.
//     The first two messages and the status of all three are inferred. A rule
//     with no ports, or 0, takes the default allocation, is not checked and
//     shows the ports Microsoft's default allocation table gives the pool's
//     size, times its frontends, up to 1,024; that an empty pool's shows 1,024
//     is recorded, the rest inferred.
//   - Deleting a load balancer whose pool a scale set uses fails with 400
//     LoadBalancerInUseByVirtualMachineScaleSet, as Azure answers it. Deleting
//     one whose pool an interface uses, or a PUT that leaves out a pool in
//     use, fails with 400 InUseBackendAddressPoolCannotBeDeleted, whose code
//     and message are inferred. Load balancing rules, probes, inbound NAT
//     rules and pools, internal frontends, public IP prefixes and backend
//     addresses given by IP address are not modeled, and a body that gives
//     one fails with BadRequest; nor are an outbound rule's allocationPolicy,
//     which is not shown, or the routes of a load balancer's children, such
//     as backendAddressPools/{name}. The operation name a change in progress
//     gives in RetryableErrorDueToAnotherOperation, PutLoadBalancerOperation,
//     is inferred.
//   - A subnet the server does not hold, such as one a network interface,
//     private endpoint or scale set names in a virtual network the server
//     never saw, is recorded, not checked. Private IP addresses in a subnet
//     the server holds come from its first prefix; in any other they come
//     from 10.0.0.0/8 ([WithPrivateRange]), unique within the subnet ID. Either
//     way the range's first four and last addresses, which Azure reserves in
//     a subnet, are left out. Public addresses come from 100.64.0.0/10
//     ([WithPublicRange]), unique across the server, assigned when the address
//     is created whatever its allocation method. A change that needs more
//     addresses than the range has free fails with SubnetIsFull or
//     PublicIPCountLimitReached, the codes Azure answers for a full subnet and
//     the public IP address quota. A static private address need not lie in
//     its subnet's prefix. Network security groups list no default rules.
//   - A virtual machine takes existing network interfaces only, not
//     networkInterfaceConfigurations. Its OS disk gets a name and a managed
//     disk ID, with the group's name upper-cased as Azure writes it. Deleting
//     it deletes the interfaces whose deleteOption is Delete and detaches the
//     rest.
//   - A VM made from an image makes its OS disk a managed disk, Attached and
//     managed by the VM, and Reserved while the VM is deallocated, as Azure
//     documents those states. Its SKU is the OS disk's storageAccountType and
//     its size the OS disk's diskSizeGB, or Standard_LRS and 30 GB, which are
//     inferred. Deleting the VM deletes the disk when the OS disk's
//     deleteOption is Delete and detaches it otherwise. A VM that attaches an
//     OS disk the server holds (createOption Attach) attaches it, and fails
//     with 409 OperationNotAllowed when another VM has it, inferred from how
//     Azure refuses to delete an attached disk; one the server does not hold
//     is recorded, not checked. Deleting an attached disk fails with 409
//     OperationNotAllowed, naming the VM. Data disks, and the OS disks of
//     scale set VMs, are not modeled.
//   - A VM extension create answers 201, and an update 200, with an
//     Azure-AsyncOperation URL under
//     Microsoft.Compute/locations/{location}/operations; a delete in progress
//     answers 202 with a Location URL as well, the same path with
//     monitor=true, which answers 200 with no body once it is done, as Azure
//     answers. The extension shows Creating while it is created, then
//     Succeeded, as Azure shows it; that an update shows Updating is
//     inferred. Its protectedSettings are recorded, not returned, as Azure
//     leaves them out. Its handler does not run, so it never ends Failed, and
//     its instance view is not served. A write under a VM that does not exist
//     fails with ParentResourceNotFound and the message Microsoft documents
//     for it; the status, 404, is inferred. A get or list under a missing VM
//     fails with 404 ResourceNotFound naming the VM, a get of a missing
//     extension with 404 ResourceNotFound naming it, and a delete of either
//     answers 204; all three are inferred. Deleting the VM, or its group,
//     deletes its extensions, as Microsoft documents deleting a parent
//     deleting its children. Its location is not checked against the VM's.
//   - A disk or snapshot change in progress answers 202 with the resource,
//     which shows Updating even while it is created, and an
//     Azure-AsyncOperation URL under
//     Microsoft.Compute/locations/{location}/DiskOperations with a Location
//     URL, the same path with monitor=true; one that finishes at once answers
//     200. Once a change succeeds its status carries the resource as
//     properties.output and the Location URL answers 200 with it, as Azure
//     answers. That a delete is polled the same way is inferred. A disk takes
//     createOption Empty, which needs diskSizeGB, Copy from a disk or
//     snapshot, or FromImage, which is recorded, not checked; a snapshot takes
//     Copy from a disk or snapshot, or CopyStart. A copy takes its source's
//     diskSizeGB, as Azure shows, and osType, hyperVGeneration and
//     supportedCapabilities, which is inferred, unless it gives them. A source
//     that does not exist or is in another location fails with 404 NotFound,
//     as Azure answers a copy from another region. Disks take the SKUs the API
//     lists and snapshots Standard_LRS, Premium_LRS and Standard_ZRS; another
//     fails with InvalidParameter, and none gives Standard_LRS, both inferred.
//     Azure gives an incremental snapshot Standard_ZRS in a region that offers
//     it; that, and which regions offer ZRS, are not modeled. The SKU's tier
//     is its first part, such as Premium for Premium_LRS. An update that
//     changes creationData fails with PropertyChangeNotAllowed, which is
//     inferred. Incremental snapshots of one disk share an
//     incrementalSnapshotFamilyId, and a CopyStart copy has its own.
//   - A CopyStart snapshot copies an incremental snapshot, in its location or
//     another, as Microsoft documents. It is incremental: leaving incremental
//     out makes it so, as Microsoft's example of the call leaves it out, and
//     false, or a full snapshot as the source, fails with InvalidParameter,
//     which is inferred. Its operation succeeds while its background copy
//     goes on: completionPercent reads 0, then 50, then 100, and
//     snapshotAccessState Pending, then Available at 100. The copy takes a
//     step on each get of the snapshot once its operation is done, unless
//     [WithSettleAfter] is negative, and on each [Server.Settle] that finds it
//     copying, so it completes two Settle calls after its operation; the
//     steps are inferred. A disk or snapshot copied from it before then fails
//     with 409 Conflict, "Source incremental snapshot {name} copy is still in
//     progress", as Azure answers a disk; for a snapshot it is inferred. A
//     second CopyStart of a snapshot of the same disk while one copies fails
//     with 409 Conflict, as Microsoft documents copying one at a time; its
//     message is inferred. That copies must follow the order the snapshots
//     were made in is not modeled. Deleting the source while it copies
//     succeeds and ends the copy with copyCompletionError CopySourceNotFound,
//     the code the API defines for it; the error message is inferred.
//   - A gallery, image definition or image version create answers 201, and an
//     update 200, with an Azure-AsyncOperation URL under
//     Microsoft.Compute/locations/{location}/capsOperations; a delete in
//     progress answers 202 with a Location URL as well, the same path with
//     monitor=true, which answers 200 with no body once it is done. A
//     gallery's identifier.uniqueName is {subscription}-{NAME}. An image
//     definition needs osType, osState and identifier, and its architecture
//     is x64 unless set; changing its osType, osState, hyperVGeneration or
//     identifier fails with 409 PropertyChangeNotAllowed, naming
//     galleryImage.properties.{name} as Microsoft documents. Deleting a
//     gallery or image definition that holds others fails with 409
//     CannotDeleteResource, with the message Microsoft documents; the code is
//     inferred.
//   - An image version's name must be Major.Minor.Patch. It takes either
//     storageProfile.source.id, a VM or another version that must exist, or a
//     managed image, recorded, not checked, or disks and snapshots in its
//     location as the sources of osDiskImage and dataDiskImages; its
//     targetRegions must include its location. Breaking these fails with 400
//     InvalidParameter and the messages Microsoft documents; the code is
//     inferred. A version is Creating until its operation is done, then
//     Succeeded, its osDiskImage showing its source's size. One whose source
//     snapshot's copy has not completed is accepted and ends Failed, its
//     operation failing with the Conflict a disk would get; with
//     $expand=ReplicationStatus each target region shows Failed with "Replication
//     failed in this region due to '{message}'.", as Microsoft documents
//     replication failures. That it is accepted rather than refused is
//     inferred. Its publishingProfile shows replicaCount 1, storageAccountType
//     Standard_LRS and excludeFromLatest false unless set, and each target
//     region regionalReplicaCount and storageAccountType from it, as Azure
//     shows them; target regions keep the names the caller sent, where Azure
//     shows display names such as West US. Replication between regions,
//     sharing, soft deletion, checking a version's source against its image
//     definition, and VMs made from a version are not modeled.
//   - A scale set's VMs are numbered from 0 and never reuse a number. Raising
//     sku.capacity creates VMs, each with the network interfaces and public
//     IP addresses its profile describes; lowering it deletes the newest,
//     whatever the scaleInPolicy. Overprovisioning is not modeled. A new
//     scale set needs a sku with a name. One created without a capacity
//     starts with no VMs, as a Flexible one does in Azure; for Uniform, both
//     are inferred. A PUT to an existing one that leaves out sku keeps its
//     sku and capacity. One that gives a sku without a capacity fails, as
//     Azure answers one that changes the model, unless it repeats the create
//     of a scale set made without a capacity and nothing has changed its
//     model since: then it succeeds and changes nothing, so a retry after a
//     lost response converges. That exception is inferred from ARM PUTs being
//     idempotent. A PUT that changes the VM profile marks existing VMs
//     latestModelApplied false, and one that changes computerNamePrefix fails
//     with PropertyChangeNotAllowed. A PATCH takes sku and tags only.
//     Deleting instances that do not exist fails with InvalidParameter and
//     deletes nothing. Scale set VMs are always running.
//   - Resource SKUs list none: a caller that checks availability sees no
//     restrictions.
//   - A storage account create that finishes at once answers 200, as Azure
//     Storage does, and one in progress answers 202 with an empty body and
//     only a Location URL, under Microsoft.Storage/operations, which answers
//     200 with the account once it is done. Account names are unique across
//     the server, as they are across Azure: taking one another group holds
//     fails with 409 StorageAccountInAnotherResourceGroup in the same
//     subscription and StorageAccountAlreadyTaken in another, and a name that
//     is not 3 to 24 lower-case letters and digits fails with
//     AccountNameInvalid. Setting blob service properties keeps the ones the
//     call leaves out, inferred from the Blob service's own Set Blob Service
//     Properties; setting file service properties replaces them all, also
//     inferred. Account keys are derived from the account and never change.
//     Blob and file data are not served.
//   - A private endpoint takes one private link service connection, whose
//     target is recorded, not checked. A new one gets a network interface
//     with an address in its subnet, named customNetworkInterfaceName or
//     {endpoint}.nic.{guid}, without the endpoint's tags, as Azure makes it;
//     deleting the endpoint deletes it. The interface can be deleted on its
//     own, which Azure refuses. An automatic connection
//     (privateLinkServiceConnections) is Approved and a manual one Pending.
//   - A NetApp change in progress answers with an Azure-AsyncOperation URL
//     under Microsoft.NetApp/locations/{location}/operationResults, whose
//     status reads as the state the resource shows, such as Creating or
//     Deleting, until it succeeds; a delete also answers a Location URL, the
//     same path with operationResultResponseType=Location, which answers 200
//     with no body once it is done. NetApp names a pool account/pool and a
//     volume account/pool/volume. A new pool shows a poolId, and a new volume
//     a fileSystemId and a mount target with an address in its subnetId, once
//     created. A pool needs serviceLevel and size, and a volume
//     creationToken, subnetId and usageThreshold; their values are not
//     checked. Deleting an account or pool that holds pools or volumes fails
//     with 409 CannotDeleteResource, as Azure refuses it; the code is
//     inferred. An update's status, Updating, is inferred too.
//   - A Managed Lustre change in progress answers with an
//     Azure-AsyncOperation URL under
//     Microsoft.StorageCache/locations/{location}/ascOperations; a delete
//     also answers a Location URL, the same path with monitor=true, which
//     answers 200 with no body once it is done. A new file system takes an
//     address in its filesystemSubnet, which its clientInfo shows as
//     mgsAddress, with a mount command, once created; until then they are
//     empty and its health is Transitioning. Its throughput is its capacity
//     times the MB/s per TiB its SKU names, such as 250 for
//     AMLFS-Durable-Premium-250. An update answers 200 and shows Updating,
//     where Azure answers 201 and shows Creating. A subnet check answers 200
//     for any subnet.
//   - A Cognitive Services account change in progress answers with an
//     Azure-AsyncOperation URL under
//     Microsoft.CognitiveServices/locations/{location}/operationResults, whose
//     status reads as the state the account shows, such as Creating, until it
//     succeeds. An account with a customSubDomainName has the endpoint
//     https://{subdomain}.cognitiveservices.azure.com/ and one without it the
//     region's. Deleting an account answers 200 at once and keeps it, without
//     its deployments, as a deleted account, as does deleting its group. Its
//     subdomain stays taken until a purge: an account that asks for a
//     subdomain another account holds, deleted or not, fails with 409
//     CustomDomainInUse. Azure also refuses an account named as a deleted one
//     in the same group; the server does not. A purge in progress answers
//     202 with an Azure-AsyncOperation URL there and a Location URL, the same
//     path with operationResultResponseType=Location, which answers 200 with
//     no body once it is done; that its status reads Deleting while it runs
//     is inferred. Account keys are derived from the account and never
//     change. A deployment needs sku.name and properties.model's name and
//     format.
//   - Cognitive Services deployments and search services have no operation
//     to poll: a create or update answers 201 or 200 with no polling header,
//     and the SDK reads the resource until its provisioningState ends, so
//     [fault.ModeFailOperation] fails their writes as [fault.ModeReject]
//     does. A deployment being created shows Accepted. A search service shows
//     provisioningState and status provisioning, then succeeded and running,
//     in lower case as Search writes them, and the endpoint
//     https://{name}.search.windows.net. Deleting a deployment or a search
//     service answers 200 at once. Search service names are not unique across
//     the server, where Azure makes them unique across Azure. Admin keys are
//     derived from the service and never change.
//   - Key vaults are body-polled too: a create or update answers 200, as Key
//     Vault does, and the vault shows RegisteringDns until it is done, and
//     the URI https://{name}.vault.azure.net/. A vault needs tenantId and
//     sku.name. Vault names are not unique across the server, where Azure
//     makes them unique across Azure, and deleting a vault's group deletes
//     it outright, where Azure keeps a deleted vault until it is purged.
//   - An Application Insights component create or update finishes at once
//     and answers 200, whatever [WithSettleAfter] says, as Application
//     Insights answers. The component's type, and the namespace in its ID,
//     are in lower case, as Application Insights writes them. It needs kind
//     and Application_Type, and its instrumentation key and connection string
//     are derived from it and never change. Its WorkspaceResourceId is
//     echoed as sent, not checked, with IngestionMode LogAnalytics unless
//     set, as the API's example shows. A component created without one is
//     linked to a workspace made for it, as Microsoft documents: the
//     workspace DefaultWorkspace-{subscription}-{code} in the group
//     DefaultResourceGroup-{code}, in the component's location, where {code}
//     is the location's Log Analytics region code, the names the Azure CLI
//     uses; that Application Insights makes them, where, and the code for a
//     location without one, the location itself, are inferred. Later
//     components reuse them, an update that leaves out the link keeps it, and
//     they stay when the component's group is deleted.
//   - A Log Analytics workspace create answers 201 and an update 200 with an
//     Azure-AsyncOperation URL under
//     Microsoft.OperationalInsights/locations/{location}/operationstatuses,
//     and a delete 200, or 202 with a Location URL under the workspace's
//     operationresults, as Azure answers; a missing one answers 204. A
//     workspace shows a customerId, and 30 days of retention unless set, as
//     Microsoft documents, and the PerGB2018 SKU unless set, which is
//     inferred. Deleting it, or its group, keeps it as deleted for 14 days
//     unless force=true, as Microsoft documents; a forced delete of a deleted
//     one answers 204 and leaves it deleted, as Microsoft documents it must
//     be recovered first. A create with its group, name and location
//     recovers it, showing Restoring while in progress and keeping its
//     customerId and the properties the create leaves out, as Microsoft
//     documents. A create in another location while it is deleted fails with
//     409 Conflict, which is inferred.
//   - A Cognitive Services deployment records modelProviderData (industry,
//     organizationName and countryCode), which clients send for partner
//     models; its values are not checked, and a partner model without it is
//     not failed.
//   - A Machine Learning workspace change in progress answers 202 with no
//     body and an Azure-AsyncOperation URL under
//     Microsoft.MachineLearningServices/locations/{location}/workspaceOperationsStatus,
//     and a Location URL, the same path with type=location, which answers 200
//     once it is done; a create that finishes at once answers 200. The
//     workspace shows a workspaceId and, with a system-assigned identity, a
//     principalId. The storage account, key vault and Application Insights
//     component it names are recorded, not checked. Deleting a workspace
//     deletes it outright, with or without forceToPurge, where Azure keeps a
//     workspace deleted without it as soft-deleted.
//   - A role assignment's create answers 201, and one that puts it again with
//     the same principal and role answers 200, as the 2022-04-01 API
//     documents. Changing either fails with 400
//     RoleAssignmentUpdateNotPermitted, and assigning a role a principal holds
//     at the scope under another name fails with 409 RoleAssignmentExists.
//     A delete answers 200 with the assignment, or 204 when there is none.
//     Principals and role definitions are not checked, so a principal Azure
//     has not replicated yet never fails with PrincipalNotFound, and names
//     need not be GUIDs or unique across scopes. Deleting the group deletes
//     the assignments at its scope.
//   - Every namespace is Registered in every subscription unless
//     [WithUnregisteredProviders] names it; Azure registers only some by
//     default, such as Microsoft.Resources and Microsoft.CostManagement but
//     not Microsoft.CostManagementExports, as Microsoft documents. A call to a
//     namespace that is NotRegistered fails with 409
//     MissingSubscriptionRegistration, as Azure answers. Registering answers
//     200 with the provider Registering and no operation to poll, as Azure
//     answers; it is Registered after the reads [WithSettleAfter] sets, which
//     are gets of the provider and export writes it turns away, or on
//     [Server.Settle], and at once by default. A namespace being registered
//     takes calls, as Microsoft documents that callers need not wait. A
//     provider lists no resource types, any namespace is one, and
//     unregistering is not served. That registering a Registered namespace
//     leaves it Registered is inferred.
//   - An export create answers 201 and an update 200, each with a new eTag,
//     as Azure answers. An update needs the export's current eTag in the
//     body: without one it fails with 400 and the message Azure answers, and
//     with another, 412 with a message of its own, which is inferred. Cost
//     Management errors carry the HTTP status as their code, such as "404",
//     and a message ending in a request ID, as Azure answers them. A get of a
//     missing export answers 404 "No export found for {name}, Subscription:
//     {subscription}.", as Azure answers, and a delete of one the same, which
//     is inferred; the SDK takes only 200 from a delete. An export write while
//     Microsoft.CostManagementExports is Registering fails with 503, as
//     Microsoft documents; its message is inferred. One while it is
//     NotRegistered is not refused, as no source shows how Azure answers it.
//     The destination storage account and container are recorded, not
//     checked, and the schedule's dates are not checked. An export shows its
//     destination without its type and with a null rootFolderPath when it has
//     none, and its recurrence dates in RFC 3339 UTC, as Azure shows them. A
//     system-assigned identity gets a principalId; its tenantId is a zero
//     GUID. Exports do not run, so they have no run history or next run
//     time, and $expand and run are not served. Exports are not resources in
//     a group, so deleting a group leaves them. Scopes other than a
//     subscription are not served.
//
// Fault method names are the operation's Azure RBAC action, such as
// "Microsoft.Compute/virtualMachines/write",
// "Microsoft.Compute/virtualMachines/deallocate/action" or
// "Microsoft.Resources/subscriptions/resourceGroups/delete". A call's
// resource is the resource ID in its path, as the caller spelled it, or for a
// list, the collection. [fault.ModeFailOperation] answers a long-running call
// with an operation that ends in the fault's error without the call taking
// effect, whatever [WithSettleAfter] says; other calls fail as
// [fault.ModeReject] does. A fault's Reason is the error code, except that the
// fault package's generic reasons become the code ARM uses, such as
// ServiceUnavailable or SubscriptionRequestsThrottled. The SDK retries 408,
// 429 and 5xx answers, and keeps polling a resource group deletion whose
// Location answers one, so fail that operation with a status such as 409.
//
// [WithPolicy] grants the caller a set of RBAC actions and refuses a call
// that needs one it lacks with 403 AuthorizationFailed, before any fault. A
// write that references another resource also needs the action ARM checks on
// it, such as Microsoft.Network/virtualNetworks/subnets/join/action for a
// network interface's subnet, Microsoft.Network/natGateways/join/action and
// Microsoft.Network/networkSecurityGroups/join/action for a subnet's NAT
// gateway and security group, whether the subnet is written alone or in its
// virtual network, Microsoft.Network/publicIPAddresses/join/action for a NAT
// gateway's or load balancer frontend's public IP address,
// Microsoft.Network/loadBalancers/backendAddressPools/join/action for the
// pools an interface's or scale set's IP configurations join, or
// Microsoft.Network/networkInterfaces/join/action
// for a VM's interface, and fails without it with LinkedAuthorizationFailed,
// naming each action and resource it lacks. A private endpoint with an
// automatic connection needs the target's privateEndpointConnectionsApproval
// action, such as
// Microsoft.Storage/storageAccounts/privateEndpointConnectionsApproval/action,
// which Azure documents as needed for automatic approval; that it refuses
// the create without it, rather than leaving the connection pending, is
// inferred from the LinkedAuthorizationFailed it reports for other services. A
// snapshot whose source is a disk needs
// Microsoft.Compute/disks/beginGetAccess/action on the disk, as Azure reports
// it; the linked checks of a disk copied from a snapshot, a CopyStart and an
// image version's sources are not modeled. Polling a Microsoft.Compute
// operation result needs Microsoft.Compute/locations/operations/read, as
// Microsoft.Compute defines no operationResults action, a disk operation
// Microsoft.Compute/locations/diskOperations/read and a gallery operation
// Microsoft.Compute/locations/capsOperations/read, and polling a
// Microsoft.Storage operation needs Microsoft.Storage/operations/read, and
// polling a Microsoft.NetApp one needs
// Microsoft.NetApp/locations/operationresults/read, a Microsoft.StorageCache
// one Microsoft.StorageCache/locations/ascOperations/read and a
// Microsoft.CognitiveServices one
// Microsoft.CognitiveServices/locations/operationresults/read, and a
// Microsoft.MachineLearningServices one
// Microsoft.MachineLearningServices/locations/workspaceOperationsStatus/read,
// and a Microsoft.OperationalInsights one
// Microsoft.OperationalInsights/locations/operationstatuses/read, or
// Microsoft.OperationalInsights/workspaces/operationresults/read for a
// delete's Location URL, which is inferred. Getting a
// resource provider needs Microsoft.Resources/subscriptions/providers/read and
// registering one {namespace}/register/action, such as
// Microsoft.CostManagementExports/register/action. Actions are not
// checked against the scope they act on: a grant covers every subscription.
//
// A lost response aborts the handler with [http.ErrAbortHandler] after
// sending the response header, so the [http.Server] serving it closes the
// connection. Calling ServeHTTP directly propagates that panic. A HEAD's
// header is its whole answer, so a lost one instead ends the connection
// inside its status line.
package arm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/internal/stub"
)

const (
	maxBody       = 1 << 20
	maxOperations = 10_000
	retryAfterMS  = "10"

	stateSucceeded = "Succeeded"
	stateCreating  = "Creating"
	stateUpdating  = "Updating"
	stateDeleting  = "Deleting"
	stateFailed    = "Failed"
)

// Server is a stand-in for Azure Resource Manager. Create one with [New]. It
// is safe for concurrent use.
type Server struct {
	faults      *fault.Injector
	policy      *access.Policy
	now         func() time.Time
	settleAfter int
	copySteps   int
	routes      []route

	mu         sync.Mutex
	seq        uint64
	groups     map[string]*group
	resources  map[string]*resource
	ops        map[string]*operation
	opOrder    []string
	opNext     int
	pending    map[*operation]struct{}
	privateIPs map[string]map[netip.Addr]string
	publicIPs  map[netip.Addr]string
	private    *pool
	public     *pool
	deleted    map[string]*softDeleted
	capacities []ModelCapacity
	// unregistered are the namespaces that start NotRegistered, by their lower-case names.
	unregistered map[string]string
	providers    map[string]*provider
	exports      map[string]*export
	// logWorkspacesDeleted are the deleted Log Analytics workspaces, by their IDs' keys.
	logWorkspacesDeleted map[string]*softDeleted
}

// Option configures a [Server].
type Option func(*Server)

// WithFaults makes the server ask in which calls to fail.
func WithFaults(in *fault.Injector) Option {
	return func(s *Server) { s.faults = in }
}

// WithClock sets the clock for creation and operation times. The default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}

// WithSettleAfter sets how many reads see a change in progress before it
// finishes: gets and lists that return the resource, polls of its operation,
// and calls turned away because of it. The default, 0, finishes every change
// before the call that makes it returns, with no operation to poll. A
// negative n finishes changes only on [Server.Settle].
func WithSettleAfter(n int) Option {
	return func(s *Server) { s.settleAfter = n }
}

// WithCopySteps sets how many steps a CopyStart snapshot's background copy
// takes to reach a completionPercent of 100. It takes a step on each read of
// the snapshot once its operation is done, and on each [Server.Settle]. The
// default is 2.
func WithCopySteps(n int) Option {
	return func(s *Server) { s.copySteps = max(n, 1) }
}

// New returns an empty server.
func New(opts ...Option) *Server {
	s := &Server{
		now:          time.Now,
		copySteps:    2,
		groups:       map[string]*group{},
		resources:    map[string]*resource{},
		ops:          map[string]*operation{},
		pending:      map[*operation]struct{}{},
		privateIPs:   map[string]map[netip.Addr]string{},
		publicIPs:    map[netip.Addr]string{},
		deleted:      map[string]*softDeleted{},
		unregistered: map[string]string{},
		providers:    map[string]*provider{},
		exports:      map[string]*export{},

		logWorkspacesDeleted: map[string]*softDeleted{},
		// Neither range is routable on the internet, so a test cannot reach a real host by mistake.
		private: newPool(netip.MustParsePrefix("10.0.0.0/8"), 4, 1),
		public:  newPool(netip.MustParsePrefix("100.64.0.0/10"), 1, 0),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.registerRoutes()
	return s
}

// handler answers one call.
type handler func(c *call) (*reply, *apiError)

type route struct {
	method   string
	segments []string
	action   string
	lro      bool
	h        handler
}

// call is one request matched to a route.
type call struct {
	r    *http.Request
	vals map[string]string
	// opFault, when set, is the error the call's operation must end in, without the call taking effect.
	opFault *apiError
}

func (c *call) val(name string) string { return c.vals[name] }

// base is the scheme and host polling URLs point at.
func (c *call) base() string {
	scheme := "http"
	if c.r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + c.r.Host
}

func (c *call) query(name string) string { return c.r.URL.Query().Get(name) }

type reply struct {
	status int
	header http.Header
	body   json.RawMessage
}

func ok(body json.RawMessage) *reply { return &reply{status: http.StatusOK, body: body} }

const (
	sub     = "subscriptions/{sub}"
	rg      = sub + "/resourceGroups/{rg}"
	compute = rg + "/providers/Microsoft.Compute"
	network = rg + "/providers/Microsoft.Network"
)

func (s *Server) registerRoutes() {
	const groupAction = "Microsoft.Resources/subscriptions/resourceGroups/"
	s.add("GET", sub+"/resourcegroups", groupAction+"read", false, s.listGroups)
	s.add("GET", rg, groupAction+"read", false, s.getGroup)
	s.add("HEAD", rg, groupAction+"read", false, s.checkGroup)
	s.add("PUT", rg, groupAction+"write", false, s.putGroup)
	s.add("DELETE", rg, groupAction+"delete", true, s.deleteGroup)
	s.add("GET", sub+"/operationresults/{op}", "Microsoft.Resources/subscriptions/operationresults/read", false, s.operationResult)

	s.add("GET", sub+"/providers/Microsoft.Compute/locations/{loc}/operations/{op}", "Microsoft.Compute/locations/operations/read", false,
		s.onePathOperation(cacheMonitor, s.operationStatus))
	s.add("GET", sub+"/providers/Microsoft.Network/locations/{loc}/operations/{op}", "Microsoft.Network/locations/operations/read", false, s.operationStatus)
	for _, ns := range []string{"Microsoft.Compute", "Microsoft.Network"} {
		s.add("GET", sub+"/providers/"+ns+"/locations/{loc}/operationResults/{op}", ns+"/locations/operationResults/read", false, s.operationResult)
	}
	s.add("GET", network+"/privateDnsOperationStatuses/{op}", "Microsoft.Network/privateDnsOperationStatuses/read", false, s.operationStatus)
	s.add("GET", network+"/privateDnsOperationResults/{op}", "Microsoft.Network/privateDnsOperationResults/read", false, s.operationResult)

	for _, k := range []*kind{nsgKind, nicKind, pipKind, peKind, vnetKind, natKind, lbKind} {
		item := network + "/" + k.plural + "/{name}"
		s.add("PUT", item, k.action("write"), true, s.putNetwork(k))
		s.add("PATCH", item, k.action("write"), false, s.patchTags(k))
		s.add("GET", item, k.action("read"), false, s.getResource(k))
		s.add("DELETE", item, k.action("delete"), true, s.deleteNetwork(k))
		s.add("GET", sub+"/providers/Microsoft.Network/"+k.plural, k.action("read"), false, s.listNetworkInSubscription(k))
	}
	for _, k := range []*kind{vnetKind, natKind, lbKind} {
		s.add("GET", network+"/"+k.plural, k.action("read"), false, s.listNetwork(k))
	}
	subnets := network + "/virtualNetworks/{vnet}/subnets"
	s.add("GET", subnets, subnetKind.action("read"), false, s.listSubnets)
	s.add("PUT", subnets+"/{name}", subnetKind.action("write"), true, s.putNetwork(subnetKind))
	s.add("GET", subnets+"/{name}", subnetKind.action("read"), false, s.getResource(subnetKind))
	s.add("DELETE", subnets+"/{name}", subnetKind.action("delete"), true, s.deleteNetwork(subnetKind))

	zone := network + "/privateDnsZones/{zone}"
	s.add("PUT", zone, zoneKind.action("write"), true, s.putZone)
	s.add("GET", zone, zoneKind.action("read"), false, s.getZone)
	s.add("PUT", zone+"/virtualNetworkLinks/{name}", linkKind.action("write"), true, s.putLink)
	s.add("GET", zone+"/virtualNetworkLinks/{name}", linkKind.action("read"), false, s.getLink)
	s.add("PUT", zone+"/A/{name}", recordKind.action("write"), false, s.putRecord)
	s.add("GET", zone+"/A/{name}", recordKind.action("read"), false, s.getRecord)
	s.add("DELETE", zone+"/A/{name}", recordKind.action("delete"), false, s.deleteRecord)

	vm := compute + "/virtualMachines/{name}"
	s.add("GET", compute+"/virtualMachines", vmKind.action("read"), false, s.listVMs)
	s.add("PUT", vm, vmKind.action("write"), true, s.putVM)
	s.add("GET", vm, vmKind.action("read"), false, s.getVM)
	s.add("GET", vm+"/instanceView", vmKind.action("instanceView/read"), false, s.vmInstanceView)
	s.add("POST", vm+"/deallocate", vmKind.action("deallocate/action"), true, s.powerVM(powerDeallocated))
	s.add("POST", vm+"/start", vmKind.action("start/action"), true, s.powerVM(powerRunning))
	s.add("DELETE", vm, vmKind.action("delete"), true, s.deleteVM)
	s.registerExtensionRoutes()

	vmss := compute + "/virtualMachineScaleSets/{name}"
	s.add("GET", compute+"/virtualMachineScaleSets", vmssKind.action("read"), false, s.listScaleSets)
	s.add("PUT", vmss, vmssKind.action("write"), true, s.putScaleSet)
	s.add("PATCH", vmss, vmssKind.action("write"), true, s.patchScaleSet)
	s.add("GET", vmss, vmssKind.action("read"), false, s.getScaleSet)
	s.add("DELETE", vmss, vmssKind.action("delete"), true, s.deleteScaleSet)
	s.add("POST", vmss+"/delete", vmssKind.action("delete/action"), true, s.deleteInstances)
	s.add("GET", vmss+"/virtualMachines", vmssKind.action("virtualMachines/read"), false, s.listInstances)
	s.add("GET", vmss+"/virtualMachines/{index}", vmssKind.action("virtualMachines/read"), false, s.getInstance)
	ipConfig := vmss + "/virtualMachines/{index}/networkInterfaces/{nic}/ipConfigurations/{ip}"
	s.add("GET", ipConfig, vmssKind.action("virtualMachines/networkInterfaces/ipConfigurations/read"), false, s.getInstanceIPConfig)
	s.add("GET", ipConfig+"/publicIPAddresses/{pip}", vmssKind.action("virtualMachines/networkInterfaces/ipConfigurations/publicIPAddresses/read"), false, s.getInstancePublicIP)

	s.add("GET", sub+"/providers/Microsoft.Compute/skus", "Microsoft.Compute/skus/read", false, s.listSKUs)
	s.registerDiskRoutes()
	s.registerGalleryRoutes()
	s.registerStorageRoutes()
	s.registerNetAppRoutes()
	s.registerStorageCacheRoutes()
	s.registerCognitiveRoutes()
	s.registerSearchRoutes()
	s.registerKeyVaultRoutes()
	s.registerInsightsRoutes()
	s.registerMachineLearningRoutes()
	s.registerAuthorizationRoutes()
	s.registerProviderRoutes()
	s.registerExportRoutes()
	s.registerLogAnalyticsRoutes()
}

func (s *Server) add(method, pattern, action string, lro bool, h handler) {
	s.routes = append(s.routes, route{method: method, segments: strings.Split(pattern, "/"), action: action, lro: lro, h: h})
}

// match finds the route for r and the values of its wildcards.
func (s *Server) match(r *http.Request, segments []string) (*route, map[string]string) {
	for i := range s.routes {
		if s.routes[i].method != r.Method {
			continue
		}
		if v, ok := matchSegments(s.routes[i].segments, segments); ok {
			return &s.routes[i], v
		}
	}
	return nil, nil
}

func matchSegments(pattern, segments []string) (map[string]string, bool) {
	if len(pattern) != len(segments) {
		return nil, false
	}
	vals := map[string]string{}
	for i, p := range pattern {
		if name, ok := strings.CutPrefix(p, "{"); ok {
			if segments[i] == "" {
				return nil, false
			}
			vals[strings.TrimSuffix(name, "}")] = segments[i]
			continue
		}
		if !strings.EqualFold(p, segments[i]) {
			return nil, false
		}
	}
	return vals, true
}

// ServeHTTP answers an Azure Resource Manager REST call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		writeError(w, newError(http.StatusUnauthorized, "AuthenticationFailed", "Authentication failed. The 'Authorization' header is missing."))
		return
	}
	if r.URL.Query().Get("api-version") == "" {
		writeError(w, newError(http.StatusBadRequest, "MissingApiVersionParameter",
			"The api-version query parameter (?api-version=) is required for all requests."))
		return
	}
	path := strings.Trim(r.URL.Path, "/")
	rt, vals := s.match(r, strings.Split(path, "/"))
	if rt == nil {
		writeError(w, newError(http.StatusNotFound, "NotFound", "squall: %s /%s is not served", r.Method, path))
		return
	}
	resource := "/" + path
	if strings.Contains(rt.action, "{") {
		rt = &route{method: rt.method, segments: rt.segments, action: expand(rt.action, vals), lro: rt.lro, h: rt.h}
	}
	if err := s.authorize(rt, r, resource); err != nil {
		writeError(w, err)
		return
	}
	if err := s.checkRegistration(rt, vals); err != nil {
		writeError(w, err)
		return
	}
	f, _ := s.faults.Decide(fault.Call{Method: rt.action, Resource: resource})
	reject := f.Mode == fault.ModeReject || f.Mode == fault.ModeFailOperation && !rt.lro
	stub.Serve(w, r, f, reject, "application/json; charset=utf-8", func() { writeError(w, faultError(f)) }, func() (func(), bool) {
		c := &call{r: r, vals: vals}
		if f.Mode == fault.ModeFailOperation {
			c.opFault = faultError(f)
		}
		rep, err := rt.h(c)
		if err != nil {
			return func() { writeError(w, err) }, false
		}
		return func() { writeJSON(w, rep.status, rep.header, rep.body) }, true
	})
}

// expand fills the wildcards in an action, such as {ns}/register/action, from vals.
func expand(action string, vals map[string]string) string {
	for name, v := range vals {
		action = strings.ReplaceAll(action, "{"+name+"}", v)
	}
	return action
}

// newUUID returns a fresh UUID-shaped ID. The caller holds s.mu.
func (s *Server) newUUID() string {
	s.seq++
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", s.seq>>48, s.seq&(1<<48-1))
}

// newHex returns a fresh 32-digit hex ID, as Azure suffixes disk names. The caller holds s.mu.
func (s *Server) newHex() string {
	s.seq++
	return fmt.Sprintf("%032x", s.seq)
}

func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// readObject reads a JSON object from the request body, keeping numbers as
// the caller wrote them.
func readObject(r *http.Request) (map[string]any, *apiError) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return nil, badRequest("reading the request body: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, invalidContent("", err)
	}
	if m == nil {
		return nil, badRequest("The request body must be a JSON object.")
	}
	return m, nil
}

// checkMembers rejects a key of m that is not in known, as ARM rejects members
// it does not know. path names m in the request, for the message.
func checkMembers(m map[string]any, typeName, path string, known ...string) *apiError {
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !slices.Contains(known, k) {
			return unknownMember(k, typeName, strings.TrimPrefix(path+"."+k, "."))
		}
	}
	return nil
}

// object returns m[key] as an object, or nil when it is absent or null.
func object(m map[string]any, key string) (map[string]any, *apiError) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	o, ok := v.(map[string]any)
	if !ok {
		return nil, invalidContent(key, fmt.Errorf("expected an object, not %T", v))
	}
	return o, nil
}

// objects returns m[key] as an array of objects.
func objects(m map[string]any, key string) ([]map[string]any, *apiError) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, invalidContent(key, fmt.Errorf("expected an array, not %T", v))
	}
	out := make([]map[string]any, 0, len(arr))
	for i, e := range arr {
		o, ok := e.(map[string]any)
		if !ok {
			return nil, invalidContent(fmt.Sprintf("%s[%d]", key, i), fmt.Errorf("expected an object, not %T", e))
		}
		out = append(out, o)
	}
	return out, nil
}

// str returns m[key] as a string, or "" when it is absent or not a string.
func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func boolean(m map[string]any, key string) (value, set bool) {
	v, ok := m[key].(bool)
	return v, ok
}

// refID returns the id of the reference object m[key], such as {"id": "..."}.
func refID(m map[string]any, key string) string {
	o, _ := m[key].(map[string]any)
	return str(o, "id")
}

func ref(id string) map[string]any { return map[string]any{"id": id} }

// tagsOf reads a resource's tags.
func tagsOf(m map[string]any) (map[string]string, *apiError) {
	raw, err := object(m, "tags")
	if err != nil || raw == nil {
		return nil, err
	}
	tags := make(map[string]string, len(raw))
	for k, v := range raw {
		switch v := v.(type) {
		case string:
			tags[k] = v
		case nil:
			tags[k] = ""
		default:
			return nil, invalidContent("tags."+k, fmt.Errorf("expected a string, not %T", v))
		}
	}
	return tags, nil
}

// normalizeLocation spells a location as ARM returns it: "East US" is "eastus".
func normalizeLocation(l string) string {
	return strings.ToLower(strings.ReplaceAll(l, " ", ""))
}

func key(id string) string { return strings.ToLower(id) }
