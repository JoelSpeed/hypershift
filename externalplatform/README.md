# `github.com/openshift/hypershift/externalplatform`

The Go library for building a HyperShift `platform: External` integration.

```
go get github.com/openshift/hypershift/externalplatform
```

The contract this implements is documented at
[docs/content/reference/external-platform-contract.md](../docs/content/reference/external-platform-contract.md).
That page is the prose; this module is the same thing expressed as code, and HyperShift
itself imports it, so the two sides cannot drift into disagreeing about what the contract
says.

## Why a separate module

An integrator is not necessarily Red Hat, and should not have to vendor HyperShift's
Cluster API, CVO, MCO and OLM dependency graph to write a controller that sets three status
fields. This module depends only on `hypershift/api`, `k8s.io/apimachinery` and, in the
packages that talk to a cluster, `k8s.io/client-go` and `sigs.k8s.io/controller-runtime`.
`hypershift/api` is itself an in-repo module for the same reason.

## Packages

| Package | What it is for |
|---|---|
| `contract` | The contract version, the label keys, the resource-name stripping, and typed accessors for everything HyperShift writes onto and reads back off the hosted cluster object. |

More packages land as the implementation does; see the staging section of the enhancement.

## Versioning

`v0.x` while the `ExternalPlatform` feature gate is TechPreview, `v1.0.0` at GA, semver
after that. `contract.Version` is separate from the module version: it identifies the
*contract*, HyperShift supports the current one and the one before it, and bumping it
requires a deprecation window of two OpenShift minor versions and a published compatibility
matrix, because integrators are released independently of HyperShift.
