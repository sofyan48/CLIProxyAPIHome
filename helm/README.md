# CLIProxyAPI Helm deployment

This chart follows the topology in `install.sh`: optional internal PostgreSQL, one CLIProxyAPIHome instance, and a configurable CPA worker pool. Workers run as a StatefulSet with optional HPA and automatic JWT enrollment. There is **no Nginx or headless Service**. The application Services expose Home on `8327` and workers on `8317`; enabling internal PostgreSQL adds its database Service.

## Requirements

- Helm 3+, Kubernetes, and a default StorageClass or configured `global.storageClass` / component `persistence.storageClass`.
- For HPA: Metrics Server and Kubernetes 1.30+ for stable container resource metrics.
- Images accessible to the cluster. Configure `imagePullSecrets` for private registries.
- PostgreSQL credentials, a management key, and a gateway API key.

## Install

Create a private values file outside version control and restrict its permissions:

```yaml
secrets:
  managementKey: "REPLACE_WITH_RANDOM_MANAGEMENT_KEY"
  gatewayApiKey: "REPLACE_WITH_RANDOM_API_KEY"
postgresql:
  auth:
    password: "REPLACE_WITH_RANDOM_DATABASE_PASSWORD"
```

Generate each value independently, for example with `openssl rand -hex 32`.

From the repository root, verify the target cluster and install the source chart:

```bash
kubectl config current-context
chmod 600 /path/to/private-values.yaml
```

```bash
helm upgrade --install cliproxy ./helm/cliproxy \
  --namespace cliproxy --create-namespace \
  -f /path/to/private-values.yaml --wait --timeout 10m
```

For Maxcloud, run `kube_maxcloud` first in your interactive shell. From the `helm/` directory, package and install the chart into namespace `dev`:

```bash
kube_maxcloud
helm lint ./cliproxy --strict -f values.yml
helm package ./cliproxy --destination .
helm upgrade --install cliproxy cliproxy-0.1.0.tgz \
  -n dev --create-namespace -f values.yml --wait --timeout 10m
```

`values.yml` is deployment-specific and may contain credentials. Do not commit or share it; use your own private values file for other environments. Repackage the chart after template changes: Helm does not read source templates when installing a `.tgz` archive.

Pin `home.image.tag` to a tested version/digest-compatible tag for production; its default follows the unpinned Home image in `install.sh`.

## Optional PostgreSQL

`postgresql.enabled: true` creates an internal PostgreSQL StatefulSet, Service, and persistent volume. Set it to `false` to use an external database:

```yaml
postgresql:
  enabled: false
  auth:
    username: cliproxy
    database: cliproxy_home
    password: "YOUR_EXTERNAL_DATABASE_PASSWORD"
  external:
    host: postgres.example.com
    port: 5432
    sslmode: require
```

The external database must already exist, and the user must have permissions to create and migrate tables. Pods must be able to reach its host and port. Disabling PostgreSQL does **not** migrate internal data. Back up and migrate the database before switching.

The chart derives PostgreSQL environment variables and Home's `cluster.yaml` from `postgresql.auth` and `postgresql.external`. Do not set connection details only through `home.environment`: Home reads concrete database settings from `cluster.yaml`. The password is stored in a Secret, not the environment ConfigMap. `sslmode: require` encrypts the connection but does not verify server identity like `verify-full`.

## Services (no Nginx)

With the default release `cliproxy`:

| Service | Port | Purpose |
| --- | --- | --- |
| `cliproxy-cliproxy-home` | 8327 | Home Management HTTP and RESP/mTLS |
| `cliproxy-cliproxy-worker` | 8317 | Worker API traffic |

Set `fullnameOverride: cliproxy` for shorter `cliproxy-home` and `cliproxy-worker` names.

Workers use one regular Service; there is no headless Service or per-pod DNS discovery. StatefulSet names and per-worker PVC identities remain stable.

When upgrading from the chart with a headless Service, `serviceName` changes and Kubernetes requires recreating the StatefulSet. Preserve pods and PVCs using `kubectl -n dev delete statefulset cliproxy-workers --cascade=orphan`, then run Helm upgrade. Adjust the namespace and StatefulSet name for your release. Helm removes the obsolete headless Service.

```yaml
home:
  service:
    type: ClusterIP
workers:
  service:
    type: LoadBalancer
```

Both support `ClusterIP`, `NodePort`, or `LoadBalancer`. Keep Home private unless deliberately exposing it. Home JWTs advertise the in-cluster Home DNS name; workers outside the cluster are not supported by these defaults. Use a TCP-capable endpoint for RESP/mTLS, not an HTTP-only Ingress.

For local access:

```bash
kubectl -n cliproxy port-forward service/cliproxy-cliproxy-home 8327:8327
kubectl -n cliproxy port-forward service/cliproxy-cliproxy-worker 8317:8317
```

Kubernetes Service balancing is connection-based, not Nginx `least_conn`. EndpointSlices track Ready workers as HPA changes replicas. New connections can reach available workers; existing keep-alive/streaming connections stay with their selected worker and cannot migrate when that pod terminates.

Local forwarding is for debugging only. Even `kubectl port-forward service/...` selects one backing pod and may stop when that pod is replaced or removed. Opening a Service through a desktop tool may also use local forwarding; inspect the actual transport rather than assuming a localhost URL uses ClusterIP balancing. For production, access the Service from inside the cluster or through a configured Ingress/LoadBalancer. This chart does not create an Ingress. A `LoadBalancer` Service requires a load-balancer implementation in the cluster.

## Environment ConfigMaps and Secrets

The chart creates `<fullname>-home-env`, `<fullname>-worker-env`, and `<fullname>-postgresql-env`, consumed using `envFrom`. They hold timezone, database connection settings (excluding password), Home URL, worker startup delay, and extra non-sensitive environment values:

```yaml
timezone: Asia/Jakarta
home:
  environment:
    HOME_USER_EMAIL_SMTP_PASSWORD: ""
workers:
  environment:
    EXAMPLE_NON_SECRET_SETTING: "value"
postgresql:
  environment: {}
```

Do not put tokens, passwords, or API keys in `environment`. Required worker/database connection variables are chart-managed. Only use environment variables supported by the selected application image. Home does **not** expand environment variables inside `cluster.yaml`; the chart renders concrete connection settings into a Secret.

`<fullname>-config` stores the PostgreSQL password, management key, gateway API key, and secret-bearing `config.yaml` / `cluster.yaml`. Kubernetes Secrets are not automatically encrypted at rest; enable cluster encryption and restrict RBAC. Helm release metadata also contains supplied values: protect Helm storage access. Environment changes trigger pod rollouts via checksums.

## Bootstrap and persistence

- Home checks the database before startup and imports `config.yaml` only if `public.config` is absent or empty. Startup fails on database errors rather than blindly importing.
- The normal Home container mounts only `cluster.yaml`; runtime configuration remains DB-backed. After first import, changing the Helm management/API keys does not update existing DB config. Make changes through the Management API and keep chart values synchronized, particularly the management key used for worker enrollment.
- Each worker has its own PVC and creates a unique Home JWT through the Management API. The JWT and enrollment identity survive restarts. Startup is staggered by 25 seconds plus 2 seconds per ordinal to allow old membership to expire. Increase the delay if you increase heartbeat timeout.
- Home uses one replica with `Recreate`, avoiding overlapping instances with the same advertised identity. Do not point multiple chart releases at the same empty database during bootstrap.
- Internal PostgreSQL uses persistent storage. Changing `postgresql.auth.password` does not rotate an existing database password; rotate the database separately and synchronize values.
- Worker scaling is controlled by `workers.replicas` when autoscaling is disabled, or by HPA when enabled. Do not share worker PVCs or certificates. StatefulSet PVCs remain on scale-down/uninstall by default; back them up and delete explicitly only when intended. The standalone Home PVC is deleted by Helm uninstall, subject to the PV reclaim policy; back up first.

## StorageClass and Pending PVCs

Home and workers still need storage when `postgresql.enabled: false`. Maxcloud provides `nfs-client` without a default StorageClass, so configure:

```yaml
global:
  storageClass: nfs-client
```

A component's `persistence.storageClass` overrides this fallback. An explicit empty string opts out of dynamic provisioning and requires a manually provisioned PV; omitting both settings uses the cluster default.

```bash
kubectl get storageclass
kubectl -n dev get pvc
kubectl -n dev describe pvc identity-cliproxy-workers-0
```

Changing the StatefulSet template does not update existing PVCs. If a PVC is Pending and `storageClassName` is unset (not explicitly empty), Kubernetes allows assigning a class:

```bash
kubectl -n dev patch pvc identity-cliproxy-workers-0 \
  --type=merge -p '{"spec":{"storageClassName":"nfs-client"}}'
```

Inspect each PVC before patching; do not change or delete a Bound PVC to resolve scheduling. StatefulSet `volumeClaimTemplates` and `serviceName` are immutable. If an intentional upgrade changes these fields, recreate only the StatefulSet with `--cascade=orphan`, then upgrade Helm. This preserves pods and PVCs, but does not migrate or modify existing volumes.

## Worker autoscaling

Enable HPA for the worker StatefulSet (requires Metrics Server and Kubernetes 1.30+ for stable container resource metrics):

```yaml
workers:
  autoscaling:
    enabled: true
    minReplicas: 3
    maxReplicas: 10
    targetCPUUtilizationPercentage: 70
    scaleDownStabilizationSeconds: 300
  resources:
    requests:
      cpu: 250m
      memory: 256Mi
```

HPA targets the StatefulSet through its scale subresource; Deployment is not required. StatefulSet preserves per-ordinal PVC/JWT identities, HPA manages the replica count, and the worker Service manages routing to Ready endpoints. Manual `HOME_JWT` generation is not required.

HPA measures the `worker` container CPU against its request, ignoring bootstrap containers. With a 250m request, the 70% target is approximately 175m average CPU per worker. These resource requests are starting values, not measured capacity recommendations.

CPU-based scaling can underrepresent I/O-bound proxy traffic: low CPU does not mean there are no active requests or streams. Start with at least 2–3 minimum workers for redundancy and tune using realistic traffic. Active-request/concurrency custom metrics may be more suitable, but this chart does not currently implement them. HPA creates pods, not cluster nodes; scaling still requires node capacity and working storage provisioning.

When enabled, Helm omits StatefulSet `spec.replicas` so subsequent upgrades do not reset HPA scaling. Switching from Helm-managed replicas may temporarily reset replicas during the first upgrade before HPA reconciles. New ordinals automatically provision a PVC and enroll a unique JWT, without manual HOME_JWT generation. Retained PVCs reuse their identities on scale-up. Storage must support dynamic provisioning through a default or configured StorageClass.

Scale-up is limited to two pods per minute. Scale-down uses a five-minute stabilization window and removes at most one pod per minute. These policies do not guarantee completion of long-running streams: the current pod termination grace period is 30 seconds. HPA applies to the shared worker pool, not per-user isolation.

```bash
kubectl -n dev get hpa cliproxy-workers
kubectl -n dev describe hpa cliproxy-workers
kubectl -n dev top pod -l app.kubernetes.io/instance=cliproxy --containers
kubectl -n dev get endpointslice -l kubernetes.io/service-name=cliproxy-worker
```

During bootstrap or rolling updates, `FailedGetContainerResourceMetric` / `pods might be unready` can occur until workers are Ready and CPU samples are available. Check current HPA conditions rather than old events: `ScalingActive=True` with `ValidMetricFound` and a numeric CPU target means metrics are being read. `ScalingLimited=True` with `TooFewReplicas` means the minimum replica floor is preventing further scale-down, not a metrics failure.

For `socket hang up`, correlate the failing request with pod termination, rollout, and HPA events. Check the client-to-Service path, Ready endpoints, and worker logs. A successful HTTP 401 without a key proves HTTP connectivity only; it does not validate authenticated inference or streaming. Neither a healthy HPA nor a Ready TCP probe guarantees that an in-flight stream survives scale-down. CPU HPA is a capacity mechanism, not a zero-disconnect guarantee.

## Validation

```bash
helm lint ./helm/cliproxy --strict -f /path/to/private-values.yaml
helm template cliproxy ./helm/cliproxy \
  --namespace cliproxy -f /path/to/private-values.yaml
```

Rendered manifests contain secrets. Do not publish them or commit them to source control.
