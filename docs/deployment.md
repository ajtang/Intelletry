# Deploying to Azure

Infrastructure is defined in Bicep and workloads are deployed by Flux from this repository. After the first deployment, changing what runs on a cluster means changing Git.

> **Not yet deployed.** None of this has run against a real Azure subscription. The Kubernetes and Flux manifests are validated against their schemas, the workflows pass `actionlint`, and CI builds and lints the Bicep. The Docker images and the Bicep deployment themselves are untested. Expect fixes on the first real deployment.

## Layout

| Path | Contents |
|---|---|
| `services/*/Dockerfile` | One image per service, built from the repository root. Go services use distroless images. All run as non-root. |
| `apps/base` | Deployments, Services, Ingresses, HPAs, PodDisruptionBudgets, NetworkPolicies, the Key Vault secret mapping and the Prometheus scrape config. |
| `apps/components/simulator` | The vehicle simulator, included only in dev. |
| `apps/overlays/{dev,prod}` | Per-environment replica counts, settings and image tags. |
| `platform` | Cluster add-ons that AKS doesn't provide: the OpenTelemetry collector. |
| `clusters/{dev,prod}` | Flux entry points. They apply `platform`, then the matching overlay. |
| `infrastructure` | Bicep for the network, AKS, PostgreSQL, Redis, Key Vault, Container Registry, identities and Azure Monitor. |
| `.github/workflows` | CI, CodeQL, image release, production promotion and infrastructure deployment. |

## How the pieces connect

1. **Infrastructure:** `infrastructure.yml` deploys the Bicep for one environment. That deployment also installs Flux on AKS and points it at `clusters/<environment>`.
2. **Cluster settings:** Bicep passes values that are only known after deployment, such as the registry address and Key Vault name, to Flux as substitutions. Flux writes them into `clusters/<environment>/cluster-settings.yaml`, and the platform and apps read them from there.
3. **Releases:** on every push to `main` that changes service code, `release.yml` builds the images, pushes them to the registry, and commits the new tags to `apps/overlays/dev`. Flux deploys them to dev.
4. **Production:** `promote.yml` opens a pull request copying dev's image tags into `apps/overlays/prod`. Merging it is the production deployment.

## Security

- **No stored Azure credentials.** GitHub Actions signs in to Azure with OIDC federation. Pods read Key Vault through workload identity, and nodes pull images with their managed identity. The registry has no admin user.
- **Secrets stay in Key Vault.** The Secrets Store CSI driver mirrors them into one Kubernetes Secret. Bicep writes them from pipeline secrets, and the parameter files contain no secrets.
- **Private data services.** PostgreSQL and Redis have no public endpoints and are reachable only inside the virtual network.
- **Locked-down network.** Ingress traffic between pods is denied unless a NetworkPolicy allows it, and Cilium enforces the policies.
- **Restricted pods.** The namespace enforces the `restricted` Pod Security Standard. Containers have read-only root filesystems and drop all Linux capabilities.
- **No local cluster accounts.** Cluster access is through Entra ID and Azure RBAC only.

## Observability

| Signal | Where it goes |
|---|---|
| Metrics | Each service's `/metrics` endpoint, scraped by Azure Monitor managed Prometheus through the PodMonitor in `apps/base` |
| Alerts | Ten Prometheus alert rules in `infrastructure/modules/alerts.bicep`, sent to an email action group |
| Logs | JSON logs on stdout, collected by Container Insights into Log Analytics |
| Traces | OTLP from each service to the collector in `platform`, which exports them to Application Insights |
| Health | `/healthz` for liveness, which ignores dependencies so a Redis outage doesn't restart every pod. `/readyz` for readiness, which checks dependencies and fails while a pod drains. |

The alert rules cover: publish failures, ingest latency, a fall in incoming telemetry, analysis lag, stream backlog, shards with no owner, dead-lettered telemetry, alert outbox backlog, API errors and restarting pods.

## First deployment

1. **Create the Azure identity for GitHub.** Create a resource group per environment, and an Entra app registration with federated credentials for this repository's `dev` and `prod` GitHub environments. Give it Contributor and User Access Administrator on each resource group, because the Bicep creates role assignments.
2. **Configure the GitHub environments.** In each of `dev` and `prod`, set:
   - **Variables:** `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_SUBSCRIPTION_ID`, `RESOURCE_GROUP` and `ALERT_EMAIL`.
   - **Secrets:** `POSTGRES_ADMIN_PASSWORD`, `API_TOKEN` and `INGEST_API_KEYS`, plus `SIMULATOR_INGEST_KEY` in dev.
3. **Set the host names.** Replace the `example.com` hosts in `infrastructure/environments/*.bicepparam` with your own.
4. **Deploy the infrastructure.** Run the Infrastructure workflow for `dev`.
5. **Finish the setup that depends on deployment outputs:**
   - Import a TLS certificate for both hosts into the Key Vault, named as in `tlsCertificateName`.
   - Set `ACR_NAME` in the `dev` environment from the deployment output.
   - Point DNS for both hosts at the ingress controller's public IP.
6. **Release.** Run the Release workflow. Flux deploys within a few minutes.

## Tradeoffs and known gaps

- **Autoscaling uses CPU.** Stream backlog would be a better signal for the anomaly engine. KEDA can scale on Redis stream lag, but it was outside this project's chosen technology, so the HPAs use CPU and the `StreamBacklogGrowing` alert covers what CPU misses. The engine's maximum replicas equals the shard count, since extra pods would sit idle.
- **Database password authentication.** Entra authentication for PostgreSQL needs token refresh in the connection pool, which the services don't do yet.
- **Secret rotation needs a restart.** Rotated secrets reach the mirrored Kubernetes Secret within two minutes, but pods read them as environment variables, so they need a rollout to pick them up.
- **Key Vault is publicly reachable.** Access is limited by RBAC, but the vault still has a public endpoint. A private endpoint would remove it, but the deploying pipeline would then need network access to the vault.
- **Azure Cache for Redis.** The Bicep uses Azure Cache for Redis, which this project was specified to use. Microsoft has announced its retirement in favour of Azure Managed Redis, so check current guidance before a long-lived deployment.
- **No dashboard sign-in.** The dashboard still has no sign-in. Put Entra ID authentication in front of the ingress before exposing it.
- **Unconfirmed Kubernetes version.** `kubernetesVersion` in the parameter files is a guess at a supported version. Check with `az aks get-versions`.
- **Automated pull requests don't trigger CI.** GitHub doesn't run workflows on pull requests opened with the default token, so promotion PRs need CI started by hand or a GitHub App token.
