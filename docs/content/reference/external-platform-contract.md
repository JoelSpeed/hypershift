# The External platform contract

`platform: External` lets a provider integrate with HyperShift without any provider-specific
code in HyperShift. The HostedCluster references integrator-owned objects, HyperShift
instantiates them and wires them into the Cluster API objects it already creates, and a
controller the integrator owns and releases does all the provider work.

This page is the contract between the two. It is the reference an integrator implements
against, and the reference a HyperShift reviewer checks a change against.

The feature is behind the `ExternalPlatform` feature gate and is TechPreview only.

## Vocabulary

| Term | Meaning |
|---|---|
| **Integrator** | Whoever owns the provider controller. Not necessarily Red Hat. |
| **Hosted cluster template** | An integrator-defined object in the HostedCluster's namespace, referenced by `spec.platform.external.hostedClusterTemplate`. |
| **Hosted cluster object** | The instance HyperShift creates from that template, in the control plane namespace. This is the handoff point. |
| **Provider controller** | The integrator's controller. One per management cluster, watching every External HostedCluster. |

The hosted cluster object is **not** a Cluster API `InfraCluster`, and Cluster API never
resolves it. It borrows the template-and-instance shape, nothing more. The integrator's own
Cluster API infrastructure object is a separate object that the integrator creates and then
names on the hosted cluster object's status.

## Topology

The provider controller is a **fleet singleton**: one Deployment per management cluster,
installed by the integrator alongside the HyperShift Operator, watching all External
HostedClusters. This is how every Cluster API provider in the ecosystem already works. The
integrator ships fixes on its own release cadence, the support blast radius is one clearly
non-HyperShift Deployment, and the cost is O(1) rather than O(clusters).

An integrator may additionally run per-HostedCluster workloads in each control plane
namespace, such as a cloud controller manager or a CSI driver. Those are covered by
[ControlPlaneComponent](#health-and-version-reporting) below.

## Registration

Installing an integrator's custom resource definitions is not by itself consent to let that
integrator into other tenants' control plane namespaces. A cluster administrator registers
each integrator explicitly at install time:

```shell
hypershift install \
  --tech-preview-no-upgrade \
  --external-platform-provider=example.io=example-system/example-provider
```

The value is `<apiGroup>=<namespace>/<serviceAccountName>`, may be repeated, and keys on the
**API group** rather than on the platform name, because the platform name is not known until
the integrator declares it — long after the access decision has to be made.

Registration does three things:

1. A HostedCluster whose `spec.platform.external.hostedClusterTemplate.apiGroup` is not
   registered is rejected, with a message naming the groups that are.
2. The named ServiceAccount is bound into the control plane namespace of every HostedCluster
   that named its API group, and no others. The binding is namespaced and HyperShift mints
   it, which is the whole isolation story: partner A cannot read partner B's control plane
   namespaces.
3. The HyperShift Operator's own ClusterRole gains the API group. Kubernetes escalation
   prevention means it cannot grant an API group it does not hold, so without this neither
   the per-namespace provider Role nor the control plane operator's Role could be created.

### What the provider ServiceAccount is granted

In each matching control plane namespace, and nowhere else:

| Resources | Verbs |
|---|---|
| The integrator's own API group | all |
| `cluster.x-k8s.io`, `infrastructure.cluster.x-k8s.io` | all |
| `controlplanecomponents`, `controlplanecomponents/status` | all |
| `hostedcontrolplanes` | `get`, `list`, `watch` |
| `secrets` named `service-network-admin-kubeconfig` and `pull-secret` | `get` |
| `deployments`, `statefulsets` | all |
| `services`, `serviceaccounts`, `configmaps` | all |
| `pods`, `pods/log`, `events` | `get`, `list`, `watch` |

Deliberately not granted: anything cluster-scoped, anything outside the control plane
namespace, write access to the HostedControlPlane, and any secret other than the two named
above. The etcd encryption key and the Kubernetes API server signing keys live in the same
namespace, so the secrets rule names its secrets. `resourceNames` cannot restrict `list` or
`watch`, which is why that rule is `get` only — a controller that wants to react to those
secrets builds a single-object informer or polls.

The kubeconfig granted is the **service network** one, not the admin one: the provider
controller runs on the management cluster and so reaches the guest Kubernetes API server the
same way the control plane's own components do, without depending on the external endpoint
being published.

## Naming

Integrator types are named `FooHostedCluster` / `FooHostedClusterTemplate`, following
HyperShift's own `HostedCluster` / `HostedControlPlane` style rather than Cluster API's bare
`FooCluster`. The `HostedCluster` suffix makes it obvious in a `kubectl get` that the object
belongs to a hosted control plane.

HyperShift resolves the object it creates by **stripping `templates` from the plural
resource**: `foohostedclustertemplates` yields `foohostedclusters`. A resource that does not
resolve this way is reported as invalid configuration on the HostedCluster.

## What HyperShift provides

### The hosted cluster object

Given

```yaml
apiVersion: hypershift.openshift.io/v1beta1
kind: HostedCluster
metadata:
  name: example
  namespace: clusters
spec:
  platform:
    type: External
    external:
      hostedClusterTemplate:
        apiGroup: example.io
        resource: foohostedclustertemplates
        name: my-infra
```

HyperShift creates, in the control plane namespace `clusters-example`:

- Kind `FooHostedCluster`, same API group and served version as the template.
- `metadata.name` equal to the HostedCluster's name.
- `spec` a deep copy of the template's `spec.template.spec`.
- `spec.controlPlaneEndpoint.host` and `.port` from the HostedControlPlane.
- Label `cluster.x-k8s.io/cluster-name` set to the HostedCluster's `spec.infraID`, which is
  the name of the Cluster API `Cluster` HyperShift will create, so the integrator can
  correlate its own Cluster API objects with this one.
- Label `hypershift.openshift.io/external-platform-group` set to the integrator's API group.

**The spec is written on create only.** Re-copying the template on every reconcile would
fight the integrator's own defaulting and would silently re-provision when someone edited the
template. `spec.controlPlaneEndpoint` is the exception: it is not knowable when the user
writes the template, it is HyperShift's to report, and it can legitimately change.
`hostedClusterTemplate` is immutable for the same reason — repointing it after creation would
silently do nothing.

The object is not created until the control plane endpoint is known, so an integrator always
sees a populated `spec.controlPlaneEndpoint`.

### Everything else

- The control plane namespace, and the RBAC above.
- The guest kubeconfig and the pull secret, in that namespace.
- The Cluster API `Cluster`, once the integrator has reported its infrastructure object.
- Per-NodePool machine templates, instantiated per configuration hash.
- The guest cluster's `Infrastructure`, rendered from the integrator's declaration.

## What the integrator must provide

### 1. The platform declaration, on `status.platform`

```yaml
status:
  platform:
    name: ExampleCloud
    cloudControllerManager:
      state: External   # or None
```

This is the integrator's one-time declaration of what this platform *is*. It is a status
field rather than a label on the CRD so that the control plane operator can read it from the
control plane namespace without cluster-scoped access.

`name` is propagated verbatim to the guest's
`Infrastructure.status.platformStatus.external.platformName`, which is where everything
running in the cluster looks to identify its platform.

`cloudControllerManager.state` decides the kubelet's cloud provider:

- **`External`** — the kubelet runs with `--cloud-provider=external` and every new node is
  tainted `node.cloudprovider.kubernetes.io/uninitialized`. **The integrator's cloud
  controller manager must remove that taint.** If it does not, the control plane looks
  healthy and the cluster has zero usable nodes.
- **`None`** — no taint and no cloud controller manager. The integrator is responsible for
  node addresses and `spec.providerID` through its Machine controller.

**Publish both values together, before or at the same time as reporting ready.** HyperShift
will not render the guest `Infrastructure` until the declaration is present, because guessing
would bake the wrong `--cloud-provider` into every node that boots and the NodePool
configuration hash would then change when the real value arrived, rolling the whole pool.

**The declaration is recorded once and never re-read.** HyperShift copies it to
`HostedCluster.status.platform.external` the first time it observes it. A later change is
reported as `ValidExternalPlatformDeclaration=False` with reason
`ExternalPlatformDeclarationChanged` and otherwise ignored, because acting on it would roll
every node in the cluster onto a configuration that disagrees with the one it was installed
with. In practice an integrator hardcodes both values per platform.

### 2. The Cluster API infrastructure object, on `status.infrastructure`

The integrator creates its own Cluster API infrastructure object in the control plane
namespace, and then names it:

```yaml
status:
  infrastructure:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: FooCluster
    name: example
```

Carrying no version, matching Cluster API's own `ContractVersionedObjectReference`.
HyperShift resolves the served version from the management cluster.

Only once all three values are present does HyperShift create the Cluster API `Cluster` with
`spec.infrastructureRef` pointing at that object. That field is immutable in Cluster API,
which is why HyperShift cannot create the `Cluster` up front and guess. HyperShift never
creates or mutates the infrastructure object itself — it reads a group, kind and name off it
and nothing else.

The object must satisfy the ordinary Cluster API infrastructure contract:
`status.initialization.provisioned`, optional `status.failureDomains`, and the Cluster API
contract labels on its CRD. Without those labels Cluster API cannot resolve a contract
version, and the failure is opaque.

### 3. A `Ready` condition, on `status.conditions`

```yaml
status:
  conditions:
    - type: Ready
      status: "False"
      reason: Provisioning
      message: Waiting for the load balancer to become active
```

**HyperShift mirrors the message verbatim** onto the HostedCluster's
`ExternalInfrastructureReady` condition. It is the integrator's user-facing error channel,
and the only provider-specific detail an operator will see, so write it for a human who
cannot see the provider's logs.

After 30 minutes without becoming ready, HyperShift appends the elapsed time to the message,
because an integrator controller that was never installed is otherwise indistinguishable from
a slow cloud.

### 4. Machines

Standard Cluster API: `status.ready`, `spec.providerID`, and `status.addresses`.

A NodePool references a Cluster API machine template through
`spec.platform.external.machineTemplate`, whose API group must be a `.cluster.x-k8s.io` group.
Unlike the HostedCluster's template this really is a Cluster API object, because the
`MachineDeployment` resolves it directly.

HyperShift instantiates a copy per configuration hash into the control plane namespace, named
from a hash of the resolved spec, so **editing the referenced template is the supported way to
change instance shape or image and it triggers a rolling upgrade** — exactly like editing an
in-tree platform's NodePool configuration.

The corollary is a requirement on the integrator: **the template spec must be stable.** An
integrator that writes resolved or observed values back into a template's spec causes a
continuous rolling upgrade across every NodePool using it.

### 5. Health and version reporting

Any workload the integrator runs in the control plane namespace must publish a
`ControlPlaneComponent` in that namespace.

```yaml
apiVersion: hypershift.openshift.io/v1beta1
kind: ControlPlaneComponent
metadata:
  name: example-cloud-controller-manager
  namespace: clusters-example
  ownerReferences:
    - apiVersion: hypershift.openshift.io/v1beta1
      kind: HostedControlPlane
      name: example
      uid: ...
status:
  version: 4.21.0          # required, see below
  conditions:
    - type: Available
      status: "True"
    - type: RolloutComplete
      status: "True"
```

Two things about this are traps, and both are load-bearing:

- **`status.version` is required and must be the HostedControlPlane's current release
  version.** The control plane operator requires every `ControlPlaneComponent` in the
  namespace to be at the target version before it will complete a version rollout. There is
  no allowlist and no registry filter. A blank or stale version permanently blocks version
  completion for the whole cluster.
- **Nothing garbage-collects a `ControlPlaneComponent` the integrator stops managing.**
  `Available=True` on every item in the namespace is what gates the HostedControlPlane's
  `Available`, so an abandoned object leaves the cluster `Available=False` forever. Set an
  owner reference to the HostedControlPlane so it is removed with the control plane.

## Deletion

Ordering matters, and HyperShift sequences it:

1. NodePools' MachineSets and MachineDeployments are deleted. Cluster API deletes the
   Machines; the integrator's Machine controller drops its finalizers.
2. HyperShift deletes the Cluster API `Cluster` and waits for it to go. Cluster API cascades
   to the integrator's infrastructure object.
3. HyperShift deletes the **hosted cluster object** and waits for it to go. This is the
   signal to tear down. The integrator holds a finalizer on it and removes the finalizer only
   once teardown is complete.
4. Only then are the HostedControlPlane and the control plane namespace deleted.

Step 3 is deliberately between 2 and 4. It is after the `Cluster` so that machines and the
Cluster API infrastructure object are gone before whatever they were built on, and before the
namespace because the guest kubeconfig and the integrator's RoleBinding both live there and
the provider needs them to finish.

If the integrator's CRD has been uninstalled, there is nothing left to wait for and deletion
proceeds — erroring would make the HostedCluster undeletable for a reason nobody can act on.

### The escape hatch

A provider controller that has been uninstalled or is permanently broken leaves a finalizer
that makes the HostedCluster undeletable. An administrator can override it:

```shell
kubectl annotate hostedcluster -n clusters example \
  hypershift.openshift.io/force-external-cleanup=true
```

HyperShift then strips the integrator's finalizers instead of waiting. **This very likely
leaks whatever the provider provisioned**, because nothing has told the provider to tear it
down, so it is an explicit administrator decision and not a timeout HyperShift takes on its
own. Every step is logged.

## Guest cluster defaults

HyperShift cannot know what an External platform supports, so the defaults are conservative
and set on create only, leaving an integrator free to configure something better:

- **Ingress.** `LoadBalancerService` publishing when the integrator declared
  `cloudControllerManager.state: External`, since a cloud controller manager implies load
  balancer support; `HostNetwork` otherwise. HyperShift skips the default IngressController
  entirely if one already exists, so creating it first is the supported override.
- **Image registry.** `emptyDir` storage, because HyperShift knows of no object storage and
  without a backend the registry sits Degraded. Configure real storage afterwards; it is
  kept.
- **Storage.** The cluster storage operator is deployed, but HyperShift waits for no CSI
  driver. Any driver is the integrator's to deploy and to report on through its own
  `ControlPlaneComponent`.

## Conditions to watch

| Condition | On | Meaning |
|---|---|---|
| `ExternalInfrastructureReady` | HostedCluster | Whether the integrator has finished provisioning. Its message is the integrator's, verbatim. |
| `ValidExternalPlatformDeclaration` | HostedCluster | Whether the declaration on `status.platform` is well formed and unchanged since it was recorded. |
| `ValidConfiguration` | HostedCluster | Includes rejection of an unregistered API group and of a template resource that does not resolve. |

## Smoke test

```shell
hypershift install --tech-preview-no-upgrade \
  --external-platform-provider=example.io=example-system/example-provider

kubectl apply -f <the integrator's CRDs and a FooHostedClusterTemplate>

hypershift create cluster external \
  --hosted-cluster-template-api-group example.io \
  --hosted-cluster-template-resource foohostedclustertemplates \
  --hosted-cluster-template-name my-infra \
  ...

kubectl get hc example -n clusters \
  -o jsonpath='{.status.conditions[?(@.type=="ExternalInfrastructureReady")]}'
kubectl -n clusters-example get foohostedcluster,cluster,controlplanecomponents
kubectl get infrastructure cluster --kubeconfig <guest kubeconfig> \
  -o jsonpath='{.status.platformStatus.external}'
```
