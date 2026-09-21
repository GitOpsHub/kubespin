# internal/kubeconfig

`internal/kubeconfig` updates the operator's local kubeconfig file once a cluster has been successfully provisioned (phase `ready`), by shelling out to the cloud provider's official CLI (`aws eks update-kubeconfig`, `gcloud container clusters get-credentials`, or `az aks get-credentials`).

This approach deliberately shells out rather than synthesizing a static kubeconfig file in-process: in-process bearer tokens (such as AWS STS presigned tokens or GCP Application Default Credentials) expire within minutes, which is fine for the one-shot Helm install of Argo CD during provisioning, but useless when baked into a static local configuration file. Cloud CLIs configure exec-based credential plugins that re-authenticate indefinitely using the operator's existing cloud sessions.

## Quick reference

| Name | Kind | File | Summary |
|---|---|---|---|
| [Options](#options) | struct | kubeconfig.go | Parameters for kubeconfig update (path override, GCP project, Azure subscription) |
| [Update](#update) | func | kubeconfig.go | Updates or refreshes the local kubeconfig context for a cluster |

## kubeconfig.go

#### Options

<details>
<summary>Signature — `Options` struct</summary>

```go
type Options struct {
    Path              string
    GCPProject        string
    AzureSubscription string
}
```

- **Fields:**
    - `Path` — optional path to the kubeconfig file to update. When empty, each cloud CLI uses its own default (typically `~/.kube/config`, or the file specified by `$KUBECONFIG`).
    - `GCPProject` — Google Cloud project ID hosting the cluster. Required when `spec.Provider == core.ProviderGCP`.
    - `AzureSubscription` — Azure subscription ID hosting the cluster. Required when `spec.Provider == core.ProviderAzure`.
- **Behavior:** carries values needed by cloud CLI commands that are not part of `core.ClusterSpec` (which is provider-neutral).

</details>

#### Update

<details>
<summary>Signature — `Update` function</summary>

```go
func Update(ctx context.Context, spec core.ClusterSpec, opts Options) (string, error)
```

- **Params:**
    - `ctx` — execution context, passed through to CLI subcommands.
    - `spec` — the cluster specification (`core.ClusterSpec`) whose context should be configured.
    - `opts` — execution options (`Options`).
- **Returns:**
    - `string` — the exact name of the kubeconfig context written and activated, suitable for user-facing instructions (e.g. `kubectl config use-context <name>`).
    - `error` — non-nil if CLI detection or command execution fails.
- **Behavior:**
    1. Verifies that the required cloud CLI (`aws`, `gcloud`, or `az`) exists in the system `PATH` via `exec.LookPath`, returning an actionable error message with an installation URL if missing.
    2. Dispatches to the provider-specific command runner:
        - **AWS (`ProviderAWS`)**: runs `aws eks update-kubeconfig --name <id> --region <region> --alias <id>` (passing `--kubeconfig <path>` if configured). The `--alias` flag pins the context name to the cluster ID rather than the full EKS cluster ARN.
        - **GCP (`ProviderGCP`)**: validates `opts.GCPProject` is non-empty, then runs `gcloud container clusters get-credentials <id> --project <project>` with `--zone <zone>` (if zonal) or `--region <region>` (if regional). Sets `KUBECONFIG=<path>` in the subprocess environment if `opts.Path` is non-empty. The context name follows GKE convention: `gke_<project>_<location>_<id>`.
        - **Azure (`ProviderAzure`)**: validates `opts.AzureSubscription` is non-empty, then runs `az aks get-credentials --name <id> --resource-group kubespin-<id> --subscription <subId> --overwrite-existing` (passing `--file <path>` if configured). Returns `<id>` as the context name.
- **Invariants:**
    - The target cluster must already exist and be accessible.
    - Repeated calls against the same cluster are idempotent: `Update` refreshes the existing entry rather than creating duplicates.
    - Failure to update local kubeconfig in `apply` is treated as a non-fatal warning (logged via `logger.Warn`), since the cluster infrastructure and GitOps repository are already completely provisioned.

</details>
