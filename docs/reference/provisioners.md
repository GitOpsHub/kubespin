# Provisioners: AWS vs. GCP vs. Azure

All three clouds implement the same `ClusterProvisioner` / `IdentityProvisioner`
/ `NetworkProvisioner` interfaces from
[`internal/provisioner`](provisioner-aws.md#shared-interfaces-provisionergo_1) —
this page compares how each cloud fills them in. For full method-level detail,
follow the "Full reference" link at the bottom of each tab.

## Cluster lifecycle

=== "AWS (EKS)"

    `internal/provisioner/aws` (`cluster.go`). `Create` ensures the EKS
    service role, creates the control plane, and defers node groups to
    `Reconcile` since they can't attach until the control plane is active.
    `Reconcile` compares access mode and node-pool sizing against the spec
    and never deletes a node group. `Delete` removes node groups first (EKS
    refuses to delete a cluster with any attached), then the cluster.

    [Full reference →](provisioner-aws.md#clusterprovisioner_1)

=== "GCP (GKE)"

    `internal/provisioner/gcp` (`cluster.go`). `Create`/`Reconcile`/`Delete`
    drive the GKE Cluster API directly. `EnablePrivateNodes` is **always**
    `true` regardless of access mode — only `EnablePrivateEndpoint` toggles
    with `AccessPrivate`. `Reconcile` handles access mode and node pool
    drift the same way AWS does.

    [Full reference →](provisioner-gcp.md#clusterprovisioner)

=== "Azure (AKS)"

    `internal/provisioner/azure` (`cluster.go`). `APIServerAccessProfile`
    (`EnablePrivateCluster`, `AuthorizedIPRanges`) is set from `spec.Access`
    at creation. `Reconcile` merges `reconcileAccess` (only touches
    `APIServerAccessProfile` if it drifted from the spec) with node pool
    reconciliation.

    [Full reference →](provisioner-azure.md#clusterprovisioner)

## Identity binding

=== "AWS (IRSA)"

    `ensureOIDCProvider` registers the cluster's OIDC issuer with IAM (or
    reuses an existing registration), then `ensureIRSARole` **unconditionally
    rewrites** the IAM role's trust policy on every call — trust policy drift
    is a privilege-escalation risk, not staleness, so it is never merely
    compared. The trust policy scopes the role to one namespace/service
    account pair via two `StringEquals` conditions (`sub` and `aud`).

    [Full reference →](provisioner-aws.md#identityprovisioner_1)

=== "GCP (Workload Identity)"

    Binds a Kubernetes service account to a GCP IAM service account via the
    `iam.gke.io/gcp-service-account` annotation, scoped to
    `<project>.svc.id.goog`. The identity exists to be *proven* to Google's
    STS, not to grant permissions directly — same pattern as AWS/Azure.

    [Full reference →](provisioner-gcp.md#identityprovisioner)

=== "Azure (federated credential)"

    Binds via a federated identity credential + managed identity rather than
    a long-lived secret — "prove identity, not grant access" is the same
    pattern IRSA and Workload Identity use. Granting the identity actual
    Azure permissions is a separate, deliberate step outside provisioning.

    [Full reference →](provisioner-azure.md#identityprovisioner)

## Network auto-creation and access mode

=== "AWS"

    | Access mode | `EndpointPrivateAccess` | `EndpointPublicAccess` | `PublicAccessCidrs` |
    |---|---|---|---|
    | `private` | `true` | `false` | not set |
    | `public` | `true` | `true` | `spec.AuthorizedCIDRs` if non-empty, else unrestricted |

    If `spec.Subnets` is empty, kubespin creates a VPC + 2 subnets across 2
    AZs (EKS requires ≥2) + an Internet Gateway + route table, deterministically
    named from the cluster ID so a resumed `apply` adopts rather than
    duplicates.

    [Full reference →](provisioner-aws.md#access-mode-summary-aws)

=== "GCP"

    `Access: public` alone is **not** enough to reach the endpoint — GKE
    enables master-authorized-networks with an empty allowlist by default,
    so `--authorized-cidrs` must include the caller's IP before anyone,
    including the operator, can reach it. `EnablePrivateNodes` is always on,
    so a kubespin-managed network also provisions a Cloud Router + Cloud NAT
    — without it nodes have no path to pull an addon's image from a public
    registry.

    [Full reference →](provisioner-gcp.md#networkprovisioner)

=== "Azure"

    `EnablePrivateCluster` and `AuthorizedIPRanges` are set directly from
    `spec.Access`/`spec.AuthorizedCIDRs` at creation. If `spec.Subnets` is
    empty, kubespin creates a resource group + VNet + subnet, deterministically
    named from the cluster ID. An operator-supplied `--subnets` value is
    passed through unchanged.

    [Full reference →](provisioner-azure.md#networkprovisioner)

## See also

- [Architecture — access modes](../architecture.md) for why private access
  requires the operator's machine to already reach the cluster's VPC/VNet.
- [Code organization](../code-organization.md) for where a fourth cloud
  provider would plug in.
