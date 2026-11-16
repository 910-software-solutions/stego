# Deliver a service with Argo CD

`stego apply` writes the files that deliver a service. It writes no value that differs per environment: the
platform supplies those values.

## What STEGO writes

- `deploy/base/`: a plain kustomize base. Its image is `stego.invalid/<service>:set-by-overlay`, which no registry
  serves. The container has one `env` entry for each variable that the service reads. See "Secrets and
  configuration".
- `deploy/components/secret-files/`, when a variable of the service comes from a secret: an opt-in kustomize
  Component that gives each secret as a file. See "Secrets as files".
- `deploy/argocd/applicationset.yaml`, when the declaration has `delivery.argocd`: an Argo CD ApplicationSet. Its git
  files generator reads the platform's environment files. Each Application has one source: the overlay at the
  `destination.overlay_path` of its environment file, in the repository of the environment files.

## What the platform must do

1. Write one overlay for each environment. The overlay must pull the base as a remote git resource, pinned to the
   full 40-character commit of the release. `stego apply` prints the exact line in the plan and writes it in the
   header of the ApplicationSet, for example:

   ```yaml
   resources:
     - https://git.example/orders.git//deploy/base?ref=0123456789abcdef0123456789abcdef01234567
   ```

2. Set the image by digest in each overlay. A pod with the placeholder image does not start.
3. Give each service a locked AppProject: restrict `sourceRepos` and `destinations`, and refuse the namespaces
   `kube-system` and `argocd`. STEGO refuses the project `default`.
4. Choose the revision of the environment files. A full commit pins them. A protected branch follows them.

## Secrets and configuration

The base names each Secret, each ConfigMap and each key. It never holds a value. The platform makes the Secrets
and the ConfigMap with its own tool, for example the External Secrets Operator, Sealed Secrets, SOPS or a kustomize
generator in the overlay. `deploy/platform-requirements.yaml` lists the same names, and `stego env check` checks an
environment file against them.

- A secret variable takes its value from a key of a secret need, for example:

  ```yaml
  - name: DATABASE_URL
    valueFrom:
      secretKeyRef:
        name: orders-database
        key: url
  ```

  The Secret is `<service>-<need>`, and the key is a key of that need. The entry has no `optional` flag, because
  the secret need requires each of its keys.
- Each other variable takes its value from the ConfigMap `<service>-config`, with the variable name as the key. A
  required variable has no `optional` flag. An optional variable has `optional: true`: when the key is not there,
  the service uses the default in its code. The base writes no default.
- A variable with a `*_FILE` form, such as `DATABASE_URL` and `DATABASE_URL_FILE`, gets the environment form only.
  The service refuses the two together.
- A secret variable that no secret need gives, such as `PGPASSWORD`, gets no entry.
- A variable that only the migration job reads (`read_by_mode: migrate` in the platform requirements), such as
  `STEGO_MIGRATION_DATABASE_URL`, gets no entry. The Deployment serves, and it never holds the credential of the
  owner role. The secret need still has the key, for the migration job: run the job with its own manifest
  (`<binary> migrate`), and give it the key there.
- `STEGO_PROBE_CONSOLE` gets no entry, so the domain probes stay off the console. An overlay that wants them on
  the console sets the variable itself.
- A variable that the service refuses at start gets no entry: the `PG` names that the pgx driver reads, the
  `GRPC_` names except the three log settings, `SSL_CERT_FILE` and `SSL_CERT_DIR`. The platform requirements mark
  each of them `refused_at_start: true`, and `stego env check` fails when the environment file gives one a value.
  `GODEBUG` keeps its entry, but the service does not start when it holds a `tls`, `x509` or `http2debug` setting.

When a component names a secret source that the service does not state, such as a key that its secret need does
not have, `stego validate`, `stego plan` and `stego apply` write a `WARNING` with the variable, the need and the
key, and the base gets no entry for that variable.

When a required key is not there, the pod does not start. The kubelet does not start the container, and the pod
stays in `CreateContainerConfigError`. The pod event names the Secret or the ConfigMap and the key, for example
`couldn't find key url in Secret orders-test/orders-database`, or `secret "orders-database" not found`. The kubelet
tries again, and the pod starts when the key is there. The process never runs without a required value. Argo CD
shows the Application as Progressing or Degraded.

A new value of a Secret or of the ConfigMap reaches the process only after a restart of the pod, also with the
Component. Use a tool such as Reloader, or a rollout from your secret operator, to restart the pods.

When the platform uses other names, the overlay must patch them. A `namePrefix` or a `nameSuffix` of the overlay
does not rename a Secret that the overlay does not make, such as one that an operator makes on the cluster.

## Secrets as files

The Component `deploy/components/secret-files/` gives each secret to the service as a file, in place of an
environment variable. A file does not go to child processes, logs or process dumps. Kubernetes writes a new value
of the Secret to the file in place, but the service reads each file once at start, so a new value reaches the
process only after a restart of the pod, as with an environment variable. List the Component in the overlay, next to the base, at the same commit:

```yaml
resources:
  - https://git.example/orders.git//deploy/base?ref=0123456789abcdef0123456789abcdef01234567
components:
  - https://git.example/orders.git//deploy/components/secret-files?ref=0123456789abcdef0123456789abcdef01234567
```

The Component has two patches of the Deployment. The forms are exclusive, so the first patch removes the
environment entry of each secret variable, and of each path variable that names the file of a secret. The second
patch then:

- mounts each Secret read-only at `/var/run/secrets/stego/<secret>`, with no `subPath`, and with the mode 0400,
  with only the keys that the switched variables read;
- sets the `*_FILE` variable, or the path variable, to the path of the file of its key, for example
  `DATABASE_URL_FILE=/var/run/secrets/stego/orders-database/url`.

The files have the mode 0400 and the owner root, so that no other user can read them. The process runs as a user
that is not root, so give the pod an `fsGroup`: Kubernetes then gives the files to that group with read access.
The restricted SCC of OpenShift sets an `fsGroup` for each namespace, and it refuses a pod that sets another one,
so the base sets none. On another cluster, set `fsGroup` in the overlay, for example to the user of the image.
Do not widen the mode of the files, and do not run the process as root.

The Component switches only the variables that state a secret source. With the shipped registry, these are:

| Variable that the Component sets | Environment entry that it removes | Secret and key |
|---|---|---|
| `DATABASE_URL_FILE` | `DATABASE_URL` | `<service>-database`, `url` |
| `STEGO_WORKER_DATABASE_URL_FILE` | `STEGO_WORKER_DATABASE_URL` | `<service>-database`, `worker-url` |
| `STEGO_RELAY_DATABASE_URL_FILE` | `STEGO_RELAY_DATABASE_URL` | `<service>-database-relay`, `url` |
| `STEGO_SPICEDB_KEY_FILE` | `STEGO_SPICEDB_KEY_FILE` | `<service>-spicedb`, `preshared-key` |
| `STEGO_QUERY_CURSOR_KEY_FILE` | `STEGO_QUERY_CURSOR_KEY_FILE` | `<service>-query-cursor`, `key` |
| `STEGO_HTTP_TLS_CERT`, `STEGO_HTTP_TLS_KEY` | the same | `<service>-tls`, `tls-crt` and `tls-key` (browser-backend) |
| `STEGO_BROWSER_CLIENT_SECRET_FILE`, `STEGO_BROWSER_SESSION_KEY_FILE` | the same | `<service>-browser`, `client-secret` and `session-keys` |
| `STEGO_GRPC_TLS_CERT`, `STEGO_GRPC_TLS_KEY` | the same | `<service>-grpc-tls`, `tls-crt` and `tls-key` |

The Component writes a row only when the service has that variable and states that secret need. It does not
switch a credential file that has no secret source, such as `STEGO_KAFKA_PASSWORD_FILE`,
`STEGO_AUDIT_SINK_TOKEN_FILE` or `STEGO_OTEL_TOKEN_FILE`: the base wires its path from the ConfigMap, and the
overlay mounts the file.

When a Secret is not there, the pod stays in `ContainerCreating`, and the pod event `FailedMount` names the Secret.
When a key is not there, the service does not start and names the variable of the file.

## The CI file

When the declaration has `delivery.ci`, `stego apply` writes the CI file from the pins of the registry's
`ci-pipeline` component. The shipped component states no provider yet, so `stego apply` writes no CI file and says
so in the plan.

- `github-actions` (`.github/workflows/stego.yml`): the `check` job runs on a pull request, with read permission to
  the contents only, and never pushes or signs. The `release` job runs only on a push to the repository's default
  branch or on a tag, and it pushes and signs the image.
- `konflux` (`.tekton/<service>-push.yaml`): one PipelineRun on a push to the branch `main`. The branch is fixed
  for now. A repository whose default branch is not `main` gets no Konflux run on push.
