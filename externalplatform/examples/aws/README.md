# An External platform integration for AWS

A complete, working `platform: External` integration, small enough to read in one sitting.

HyperShift already supports AWS in-tree. This exists anyway, because AWS is the platform
every reader already understands, which makes it the honest way to show what the contract
costs: the same platform, implemented on the other side of it, is one file of provider logic
and a cloud controller manager.

## What is here

| | |
|---|---|
| [`manifests/01-crds.yaml`](manifests/01-crds.yaml) | `AWSHostedClusterTemplate` and `AWSHostedCluster`, the integration's own API |
| [`awsprovider/provisioner.go`](awsprovider/provisioner.go) | The integration: translate the spec into a Cluster API `AWSCluster`, report on it, tear it down |
| [`awsprovider/cloudcontrollermanager.go`](awsprovider/cloudcontrollermanager.go) | The AWS cloud controller manager, run per control plane namespace and reported to HyperShift |
| [`cmd/main.go`](cmd/main.go) | The binary: a controller-runtime manager and twenty lines of wiring |
| [`manifests/02-provider.yaml`](manifests/02-provider.yaml) | How the provider is deployed and what it is granted |
| [`manifests/03-example-cluster.yaml`](manifests/03-example-cluster.yaml) | The two objects a user writes |

## The shape of it

Three objects, and knowing which is which is most of understanding the contract.

**`AWSHostedClusterTemplate`** is written by the user, in the HostedCluster's namespace. It
is the integration's own type and not a Cluster API type, and it holds the inputs a user
supplies: region, VPC, subnets.

**`AWSHostedCluster`** is instantiated by HyperShift into the control plane namespace, with
the template's spec copied verbatim. It is where the two sides meet: HyperShift creates it
and watches its status, the provider reconciles it and writes that status.

**`AWSCluster`** is Cluster API Provider AWS's own type, created by the provider and named
back to HyperShift, which points the Cluster API `Cluster` at it. The contract asks for a
Cluster API infrastructure object and CAPA already publishes a good one; inventing a second
would mean reimplementing its controller too.

So the provisioner is a translation between the first and the third, plus reporting. That is
genuinely all of it:

```go
func (p *Provisioner) Provision(ctx context.Context, request *reconcile.Request) (reconcile.ProvisionResult, error)
func (p *Provisioner) Deprovision(ctx context.Context, request *reconcile.Request) (reconcile.DeprovisionResult, error)
func (p *Provisioner) Platform() (string, hyperv1.ExternalCloudControllerManagerState)
```

Everything the contract requires beyond the provider work itself — the finalizer, the
ordering of the platform declaration against readiness, the immutability of the
infrastructure reference, holding teardown open until the provider is done — is in
`externalplatform/reconcile`, not here.

## Two decisions worth copying

**The platform is called `ExampleAWS`, not `AWS`.** A guest cluster that claims to be the
in-tree AWS platform has operators in it looking for in-tree AWS behaviour that an external
integration does not provide. The External platform is a different platform that happens to
run on AWS.

**It declares an external cloud controller manager, and then runs one.** Declaring `External`
is what makes kubelets start with `--cloud-provider=external` and nodes join tainted
`node.cloudprovider.kubernetes.io/uninitialized`. If the cloud controller manager never runs,
the cluster has a perfectly healthy control plane and not one schedulable node. That is why
the Deployment is reported to HyperShift as a `ControlPlaneComponent`: having made every node
depend on it, a cluster that reported `Available=True` while it crash-looped would be telling
its owner something false.

## Two decisions worth not copying

**It reads `AWSCluster` as unstructured** rather than importing CAPA's types, so that this
example, and anyone who copies it, does not vendor CAPA's module to set six fields. An
integrator that already vendors CAPA should use the typed API and delete `mutateAWSCluster`'s
`SetNestedField` calls.

**It reuses CAPA's API group** for its infrastructure object, which means the provider needs
cluster-wide access to `awsclusters` and so can see other tenants'. An integrator that needs
its infrastructure objects isolated should define its own infrastructure type in its own API
group. Everything else the provider touches is granted per control plane namespace by
HyperShift, and is isolated.

## Running it

```bash
# 1. Install HyperShift with this integration registered. Installing a partner's custom
#    resource definitions is not by itself consent to let it into other tenants' control
#    plane namespaces, so a HostedCluster naming an unregistered API group is rejected.
hypershift install --tech-preview-no-upgrade \
  --external-platform-provider=aws.example.hypershift.openshift.io=example-aws-external-platform/example-aws-external-platform

# 2. Install the integration's API and its provider.
kubectl apply -f manifests/01-crds.yaml
kubectl apply -f manifests/02-provider.yaml

# 3. Create a cluster. Edit the AWS identifiers first.
kubectl apply -f manifests/03-example-cluster.yaml

# 4. Watch the handoff.
kubectl get hostedcluster -n clusters example \
  -o jsonpath='{.status.conditions[?(@.type=="ExternalInfrastructureReady")]}{"\n"}'
kubectl get awshostedcluster,awscluster,cluster,controlplanecomponents -n clusters-example

# 5. What the guest ends up believing about itself, which is the point of the declaration.
kubectl --kubeconfig guest.kubeconfig get infrastructure cluster \
  -o jsonpath='{.status.platformStatus.external}{"\n"}'
```

Cluster API Provider AWS must be installed on the management cluster, with credentials for
the account. This example brings its own network — it does not create VPCs, because an
example that did would need a real account to be worth reading — so the VPC and subnets in
`03-example-cluster.yaml` must exist.

## Testing an integration

Run the conformance suite in your own CI:

```go
func TestConformance(t *testing.T) {
	conformance.Run(t, context.Background(), conformance.Options{
		Client:           managementClient,
		APIGroup:         awsprovider.APIGroup,
		TemplateResource: awsprovider.TemplateResource,
		Spec: map[string]any{
			"region":    "eu-west-1",
			"vpcID":     "vpc-0123456789abcdef0",
			"subnetIDs": []any{"subnet-0123456789abcdef0"},
		},
	})
}
```

It plays HyperShift's half of the contract against your controller and asserts the things
that otherwise fail silently: that the platform declaration arrives before readiness and
never changes afterwards, that the infrastructure reference is named early and stays put,
that a finalizer is held, and that deletion completes. Every one of those failures looks
like a HyperShift bug from the outside, and none of them are caught by testing that your
provider provisions — in every case it does.
