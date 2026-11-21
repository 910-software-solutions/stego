# Deliver a service with Argo CD

`stego apply` writes the files that deliver a service. It writes no value that differs per environment: the
platform supplies those values.

## What STEGO writes

- `deploy/base/`: a plain kustomize base. Its image is `stego.invalid/<service>:set-by-overlay`, which no registry
  serves. The container has one `env` entry for each variable that the service reads. See "Secrets and
  configuration".
- `deploy/base/migration-job.yaml`, when the service has a migration job: a Job that runs the schema migration of
  the release. See "The migration Job".
- `deploy/components/secret-files/`, when a variable of the service comes from a secret: an opt-in kustomize
  Component that gives each secret as a file. See "Secrets as files".
- `deploy/components/fs-group/`, next to `secret-files`: a second opt-in kustomize Component that sets the pod
  `fsGroup` 65532 on the Deployment and on the migration Job, so that the process can read the files. Use it only on
  a cluster other than OpenShift. See "Secrets as files".
- `Containerfile`: the offline Containerfile of the image that the base runs. It builds with the Go SDK, the
  settings and the arguments of `stego build`, with the module proxy off, from the Go modules at
  `/cachi2/output/deps/gomod/pkg/mod`, where the Go module prefetch of Konflux puts them. The build holds the modules
  to `go.sum` (`-mod=readonly`), and `go mod verify` checks the module cache. The file does not turn off the network
  itself: build it in the hermetic mode of Konflux, or locally with `--network=none`. The final image is `scratch`
  with the executable `/service` and the CA certificates. It runs as the user `65532:65532`, so it starts with the
  read-only root file system and the `/tmp` emptyDir of the base. For a local build, mount a writable copy of the
  module cache folder of `stego build download` at that path, for example
  `buildah build --network=none -v "$PWD/modules-copy:/cachi2/output/deps/gomod/pkg/mod" -f Containerfile .`.
  The build stage copies `go.mod`, `go.sum`, the output folder, the `fills` folder when it holds a fill, and each
  folder of the project that a `replace` directive of `go.mod` names with no version (a local module). A local
  module out of the project folder, in a folder whose name is not plain (a space, or a first `_` or `.`), or in a
  folder that holds the output folder is not copied, and the plan says so: move it to a plain folder of the
  project. A module in another local module folder is copied with that folder. After you add a `replace`,
  `stego deps` runs with no new apply, and the next `stego apply` writes the Containerfile again.
  `stego apply` checks the text that it writes against the offline shape before it writes the file, and the record
  keeps the digest of the file: `stego apply` and `stego drift` refuse a hand edit, so do not edit it. Check out the
  file with LF line ends: a CRLF checkout changes the digest.
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

## Probes and modes

The binary of a service on the runtime data path runs the modes api, worker and controller. When the declared
modes hold the api mode and the worker or controller mode (all three is the default), the base writes two
Deployments:

| Deployment | File | Modes (`args`) | Readiness | Service |
|---|---|---|---|---|
| `<service>` | `deploy/base/deployment.yaml` | `--mode=api` | `/readyz/modes` | The Service selects it. |
| `<service>-worker` | `deploy/base/worker-deployment.yaml` | `--mode=worker`, `--mode=controller` | `/readyz/modes` | No Service selects it, and it has no container port. |

- Each Deployment runs as a ServiceAccount of its own name: `<service>` in `deploy/base/service-account.yaml`, and
  `<service>-worker` in `deploy/base/worker-service-account.yaml`. Thus a token or an audit line of a worker pod
  does not name the identity of the API, and a platform can grant each one apart. A cluster permission states no
  mode, so the RoleBinding of the base binds both ServiceAccounts.
- Upgrade note: before this change, the worker pods ran as the ServiceAccount `<service>`. A grant that you made
  outside the base for `<service>` does not apply to `<service>-worker`. Before you apply a release that has
  `worker-service-account.yaml`, give the same grant to `<service>-worker`, where the worker needs it:
  - a workload identity annotation, such as `eks.amazonaws.com/role-arn` (IRSA) or
    `iam.gke.io/gcp-service-account`, and the trust policy of that role, which names the ServiceAccount;
  - `imagePullSecrets` on the ServiceAccount, when your overlay adds them there;
  - a Vault Kubernetes auth role, whose `bound_service_account_names` must name `<service>-worker`;
  - on OpenShift, an SCC that you granted to the ServiceAccount (`oc adm policy add-scc-to-user`);
  - each RoleBinding or ClusterRoleBinding that your platform made for `<service>`.

  If you do not, the worker pods start without that grant: they cannot pull the image, or they cannot get their
  cloud or Vault credentials.
- The liveness probe of each one is `/livez`. It answers 200 while the process runs, and it reads no database
  and no broker, so an outage of a dependency does not restart a pod.
- `/readyz/modes` of the api pod tells whether the pod can take requests of the API: it reads the api mode only,
  which the pod runs, and not the worker and controller modes. Thus an outage of Kafka or of the audit sink does
  not remove the API from the endpoints of its Service. It answers 503 while the HTTP server does not accept, at
  shutdown, and from the start when the database does not hold a table or a column that the code of the release
  needs, such as after a roll back past a contract step (see [Which older releases can run on a newer
  database](operations.md#which-older-releases-can-run-on-a-newer-database)). Then a rollout stops at its first
  pod, and the pods that serve stay.
- `/readyz/modes` answers 503 and names the cause when a worker stopped, such as
  `worker: not ready: the component inbox-order-changes stopped`. A service with several subscriptions has one
  inbox for each, `inbox-<subscription>`, and the text names each inbox that stopped or that fails, joined by
  `; `. No traffic goes to the worker Deployment, so
  its readiness removes nothing from a Service. It shows a stopped worker in `kubectl get pods` and in the
  `Available` condition, and a new release whose worker cannot start does not complete its rollout.
- `/readyz/modes` also answers 503 while a destination of the outbox fails each delivery in a row. The Kafka
  destination fails after 5 attempts in a row that fail at the broker, such as in a broker outage:
  `worker: not ready: the destination kafka fails: broker-unavailable`. Each such attempt also writes the log
  line `kafka.delivery_failed` with the failure class. The first delivery that the broker acknowledges makes the
  worker ready again.
- Each Deployment has the environment entries of the variables that its modes read. A variable that only the
  worker mode reads (`read_by_mode: worker` in the platform requirements), such as `STEGO_WORKER_DATABASE_URL`,
  `STEGO_RELAY_DATABASE_URL`, the audit sink settings and the Kafka settings, is only in the worker Deployment.
  Thus the API pod holds no credential of the worker role, the relay role or Kafka.
  A variable that only the api mode reads (`read_by_mode: api`), such as the CORS settings `CORS_ALLOWED_*` and
  the gRPC settings `STEGO_GRPC_ADDR`, `STEGO_GRPC_STREAM_TIMEOUT` and `STEGO_GRPC_STREAM_IO_TIMEOUT`, is only in
  the api Deployment. The worker Deployment and the migration Job do not get it. From grpc-application 2.4.0, the
  gRPC TLS files `STEGO_GRPC_TLS_CERT` and `STEGO_GRPC_TLS_KEY` are read by the api mode too, so only the api
  Deployment mounts the Secret `<service>-grpc-tls`, which holds the private key of the gRPC server. The worker
  modes serve no gRPC call. The two variables are required when the process runs the api mode.
- A variable of a client program of the service, such as `APPLY_CONFIG` of the generated CLI, is in no pod:
  from cli-application 1.20.0 the platform requirements do not list it.
- The `args` select the modes, so neither Deployment has an entry of `STEGO_MODE`.
- The pods of the worker Deployment have the label `app.kubernetes.io/name: <service>-worker`, so that the
  selector of `<service>`, which does not change, takes none of them.
- The Components `secret-files` and `fs-group` patch both Deployments.
- Give each Deployment its own replicas in the overlay, such as `replicas: [{name: orders, count: 3}, {name:
  orders-worker, count: 2}]`.

A service whose declared modes hold only the api mode, or only the worker and controller modes, gets one
Deployment, with the readiness `/readyz/modes`. A browser service has no modes: it gets one Deployment with the
readiness `/readyz` of health-check.

Upgrade: before this release, the one Deployment `<service>` ran every mode, as `STEGO_MODE` selected. After the
upgrade it runs the api mode only, whatever `STEGO_MODE` holds, and `<service>-worker` runs the worker and
controller modes. Remove an overlay patch that sets `STEGO_MODE` or the readiness probe of `<service>`.

### The api Deployment needs the worker Deployment

The relay of the authorization tuples runs in the worker mode only. After a write commits, the relay writes its
tuples to SpiceDB: the grants of the first administrators that Provision gives, the owner of a new row, and each
grant and removal of a command. Until the relay writes them, SpiceDB does not know them. Thus, when the api
Deployment runs and no pod of `<service>-worker` runs:

- The api pod accepts each write, but its tuples stay in the outbox.
- A first administrator of a new tenant gets 404 on each command that is decided on the service resource of the
  tenant, such as a create with no parent.
- The creator of a new row gets 404 when it reads the row.
- A write answers `Authz-Revocation: pending`, and its status route stays `pending`. A read with
  `Authz-Revocation-After` waits until the wait cap, then answers `Authz-Revocation: pending` with the 404.
- `/readyz/modes` of the api pod stays 200. This is correct: the readiness of the API does not read the worker modes, so
  an outage of the worker does not remove the API from its Service. Thus readiness does not show this case.

When the worker pod starts, the relay delivers the tuples in order, and the grants and rows become visible. No
write is lost.

Do these steps:

1. Deploy the two Deployments together. The base lists both in `kustomization.yaml`, so Argo CD and
   `kubectl apply -k` apply both.
2. Before you provision a tenant, make sure that the worker Deployment is available:
   `kubectl rollout status deployment/<service>-worker`.
3. Give the worker Deployment at least one replica in each overlay. More replicas are safe: each relay claims a
   message with a lease, so only one relay delivers it.
4. Monitor the `Available` condition of `<service>-worker`, and alert when it is false. A write whose status stays
   `pending` past the wait cap is also a sign that no relay runs.

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
  The service refuses the two together. When the service needs one of the two, the pair is a group of one of
  (`one_of`), and `stego env check` fails when the environment file gives none or both. The key of the Secret is
  the one value of the pair in both forms: the base gives it as the environment form, and the Component
  secret-files gives it as the `*_FILE` file. Give the Secret in the environment file, with or without the
  Component, and never the `*_FILE` variable: the base never wires it from the environment file, and the check
  prints a notice for it.
- A secret variable that no secret need gives, such as `PGPASSWORD`, gets no entry.
- A variable that only the migration job reads (`read_by_mode: migrate` in the platform requirements), such as
  `STEGO_MIGRATION_DATABASE_URL`, gets no entry in the Deployment. Only the migration Job takes it, from its own
  Secret `<service>-database-owner`. The Deployment serves, and it never references that Secret. See "The
  migration Job".
- `STEGO_PROBE_CONSOLE` gets no entry, so the domain probes stay off the console. An overlay that wants them on
  the console sets the variable itself.
- A variable that the service refuses at start gets no entry: the `PG` names that the pgx driver reads, the
  `GRPC_` names except the three log settings, `SSL_CERT_FILE` and `SSL_CERT_DIR`. The platform requirements mark
  each of them `refused_at_start: true`, and `stego env check` fails when the environment file gives one a value.
  `GODEBUG` keeps its entry, but the service does not start when it holds a `tls`, `x509` or `http2debug` setting.
- The database connection does not read the files in the home folder: no `~/.pgpass`, no
  `~/.pg_service.conf`, and no `root.crt`, `postgresql.crt` or `postgresql.key` in `~/.postgresql`. Without
  `sslrootcert`, the service verifies the database server against the system CA pool. When the server has a
  certificate from a private CA, put `sslrootcert=<path>` in `DATABASE_URL`, with a path that the overlay mounts
  (`sslmode=verify-full` is also necessary). A deployment that used `~/.postgresql/root.crt` must change to this.

When a component names a secret source that the service does not state, such as a key that its secret need does
not have, `stego validate`, `stego plan` and `stego apply` write a `WARNING` with the variable, the need and the
key, and the base gets no entry for that variable. With `--strict`, each of the three commands refuses such a source,
names each variable, need and key, and writes nothing. Use `--strict` in CI, so that a source that the base does not
wire stops the change. `stego plan --strict` also refuses a branch whose generated files are not those that its
declaration gives, and `stego drift` refuses a generated file that a person changed by hand: run both in CI, as
[the checks of a pull request](../README.md#the-checks-of-a-pull-request) say.

A need name is a Kubernetes name: lower-case letters, digits and `-`, which starts and ends with a letter or a digit,
of at most 63 characters, also after `{service}` and the settings fill it. `stego apply` refuses a need name with a
capital letter or `_`, and names the component and the need. A component can put a setting into a need name with
`{settings.<key>}` only when the setting is text, is not secret, and has an `enum` whose values are each such a
label. The registry refuses a component that puts free text, a number, a secret, a URL or another value that
differs per environment into a need name, and names the placeholder and the setting.

When a required key is not there, the pod does not start. The kubelet does not start the container, and the pod
stays in `CreateContainerConfigError`. The pod event names the Secret or the ConfigMap and the key, for example
`couldn't find key url in Secret orders-test/orders-database`, or `secret "orders-database" not found`. The kubelet
tries again, and the pod starts when the key is there. The process never runs without a required value. Argo CD
shows the Application as Progressing or Degraded.

A new value of a Secret or of the ConfigMap reaches the process only after a restart of the pod, also with the
Component. Use a tool such as Reloader, or a rollout from your secret operator, to restart the pods.

When the platform uses other names, the overlay must patch them. A `namePrefix` or a `nameSuffix` of the overlay
does not rename a Secret that the overlay does not make, such as one that an operator makes on the cluster.

## The migration Job

The schema migration of a release runs in a Kubernetes Job of its own, `deploy/base/migration-job.yaml`. The Job
runs the service binary with the one argument `migrate`. The migration job makes and changes the tables, writes
the authorization schema to SpiceDB, runs the backfills, and then checks the database against the schema manifest.

After the job writes the authorization schema, it waits until SpiceDB serves each definition of the schema to a
check at an older revision. SpiceDB can choose a revision from before the write for a short time after it serves
the new schema: the quantization interval (`--datastore-revision-quantization-interval`, 5 s by default) times the
max staleness percent (`--datastore-revision-quantization-max-staleness-percent`, 0.1 by default), so 0.5 s by
default. A check of a new resource type in that time fails. So the job waits until SpiceDB serves the schema on
probe rounds in a row that span 1 s. If your SpiceDB uses a product of these two flags of more than 1 s, a check of
a new resource type can fail for a short time after the release starts.

### Two database roles

The service uses two kinds of database role:

- The owner role owns the tables and runs the DDL and the backfills. Only the migration Job connects as it, with
  `STEGO_MIGRATION_DATABASE_URL` from the Secret `<service>-database-owner`, key `url`.
- The runtime role, the worker role and the relay role run the DML of the serving process. The Deployment
  connects as them, with `DATABASE_URL`, `STEGO_WORKER_DATABASE_URL` and `STEGO_RELAY_DATABASE_URL` from
  `<service>-database` and `<service>-database-relay`. The runtime role is not the owner role and is no member of
  it: the service does not start when it is.

So the platform makes two Secrets for the database: `<service>-database` with the keys `url` and `worker-url`,
and `<service>-database-owner` with the key `url`. The Deployment, the Component secret-files and their
projections never reference `<service>-database-owner`. `stego apply` refuses a base where they do. The Job
references no Secret that the Deployment takes a variable from.

### Upgrade from a release before the owner Secret

A release before this one read the owner credential from the key `migration-url` of `<service>-database`. To move:

1. Make the Secret `<service>-database-owner` with the key `url`, which holds the value of `migration-url`, for
   example with your secret operator.
2. Deploy the release, and wait until its migration Job completes.
3. Remove the key `migration-url` from `<service>-database`, so that no Secret that the serving pod references
   holds the owner credential:

   ```sh
   kubectl patch secret -n <namespace> <service>-database --type=json -p='[{"op":"remove","path":"/data/migration-url"}]'
   ```

   When an operator makes the Secret, such as the External Secrets Operator or Sealed Secrets, remove the key from
   its source instead, or the operator writes it again.

### What the Job holds

- The name `<service>-migrate-<release>`, where `<release>` is the first ten hex digits of the digest of the schema
  of the release: its migrations, its schema manifest and its runtime version. A new apply cannot change the pod
  template of a Job, so each release that changes the schema gets a Job of a new name. The name is at most 63
  characters, so the service name is at most 44 characters. A release that does not change the schema keeps the
  Job name; see "Plain kubectl".
- The annotations `argocd.argoproj.io/hook: PreSync`,
  `argocd.argoproj.io/hook-delete-policy: BeforeHookCreation,HookSucceeded` and `argocd.argoproj.io/sync-wave: "0"`.
  Plain Kubernetes ignores them.
- `backoffLimit: 2`, `activeDeadlineSeconds: 7200` (two hours: the schema steps have 10 minutes and the backfills
  one hour) and `ttlSecondsAfterFinished: 600`. Kubernetes deletes a finished Job after ten minutes. Read the logs of
  an older Job in the logging of the cluster.
- `restartPolicy: Never`, no service account token, and the default service account of the namespace. Argo CD runs
  the PreSync hook before it syncs the service account of the base, so the Job cannot use that account. When the
  image needs a pull secret, give it to the default service account, or patch the Job in the overlay.
- The pod security of the Deployment: non-root, the seccomp profile `RuntimeDefault`, no privilege escalation, every
  capability dropped, a read-only root file system and the emptyDir `/tmp`. It sets no `fsGroup`, for the same
  reason as the Deployment. The Component fs-group sets one on a cluster other than OpenShift (see "Secrets as
  files").
- On the Job, the labels `app.kubernetes.io/name: <service>`, `app.kubernetes.io/part-of: <service>` and
  `app.kubernetes.io/component: migration`. They do not change between releases, so one selector finds the Job of
  any release. On its pods, only `app.kubernetes.io/part-of` and `app.kubernetes.io/component`, with no
  `app.kubernetes.io/name`, so that the selectors of the Deployment and the Service never take a pod of the Job.
- An `env` entry, names only, for each variable that is not secret, from the ConfigMap `<service>-config`, as in
  the Deployment, and `STEGO_MIGRATION_DATABASE_URL` from `<service>-database-owner`. It has no credential of a
  serving role, and no path of a secret file except `STEGO_SPICEDB_KEY_FILE`, the key that the Job uses to write
  the authorization schema.

The image of the Job is the image of the base, so the `images` entry of the overlay sets its digest too.

### When the Job fails

A Job that fails writes one failure record, a JSON line with the event `service.failed`, to its error output. The
record never holds the error text. It gives:

- `stage`: the step that failed, such as `migrate.ledger_check` or `migrate.authorization_schema`.
- `cause`: a fixed class of the cause, when the Job knows one. For the database, the class of its SQLSTATE, such as
  `lock_timeout`, `authentication_failed`, `database_starting`, or `crash_shutdown` after a restart of the
  database server, with the SQLSTATE in `sqlstate`. For the network or a file, a class such as
  `connection_refused`, `connection_closed` (the connection ended in the middle of a message), `name_not_resolved`,
  `timeout` or `file_not_found`. For a call to SpiceDB, the class of its gRPC status code: `unavailable` (SpiceDB is
  not there or refused the connection), `timeout`, `canceled`, `authentication_failed` (the key is wrong),
  `permission_denied`, `insufficient_resources`, `precondition_failed`, `invalid_argument`, `not_found`,
  `unimplemented`, or `provider_error` for another code. A refusal of SpiceDB (`invalid_argument` or
  `precondition_failed`) adds the fixed class of its reason after a period (runtime v0.187.0):
  `invalid_argument.relationships_exist` for a schema that removes a definition or a relation that still has
  relationships, or the reason of the SpiceDB API, such as `invalid_argument.schema_type_error`. Remove the
  relationships first, or keep the definition for one more release. For a check that refused the step, `refused`.
- `fault`, `object` and `remedy`: for a refused check, its fault class, the object and what to do, such as
  `migration_ledger.digest` with `stego_schema.migrations` at the stage `migrate.ledger_check`, or
  `schema_change.seal_pending` with `stego_schema.seals` at the stage `migrate.tables`. The line
  `migrate: the step is refused` before the record, a JSON line with the event name `migrate.step.refused`, names
  the migration or the change in its message, after `migrate: the step is refused: `.

A start of the service whose database pool did not open gives the stage `service.start.database`, the fault
`database.connection`, `database.settings` or `database.runtime_role`, the object `database`, a remedy and a cause,
such as `connection_closed` while the database server restarts. A pod that stops so starts again; when it does not
pass, do what the remedy says.

For example, a Job whose SpiceDB refused the connection writes
`{"severity":"ERROR","event.name":"service.failed","stage":"migrate.authorization_schema","cause":"unavailable"}`
(with its time and message).

### With Argo CD

Argo CD runs the Job as a PreSync hook: before it syncs the Deployment, it deletes the Job of the sync before
(`BeforeHookCreation`), runs the Job, and syncs the rest only when the Job passed. A Job that passed is deleted
(`HookSucceeded`). A Job that failed stops the sync, and stays until the next sync or its time to live, so that you
can read its logs.

The Job runs in the PreSync phase, in the sync wave `0`, before Argo CD syncs the other resources of the
Application. So the Secret `<service>-database-owner`, the Secret `<service>-spicedb` and the ConfigMap
`<service>-config` (or the resource that makes each of them, such as an ExternalSecret) must be on the cluster
before the Job, outside the Sync phase. Do one of these:

- Make them in a separate Application that syncs before the service, for example an Application of the
  platform's secrets.
- Make them in the overlay as PreSync resources of an earlier wave, with the annotations
  `argocd.argoproj.io/hook: PreSync` and `argocd.argoproj.io/sync-wave: "-1"`, and no hook delete policy, so that
  Argo CD keeps them.

If the overlay makes them as normal resources, the first sync waits on the Job until its time limit, because the
pod of the Job cannot start without them (`CreateContainerConfigError`, or `FailedMount` with the Component).

### Plain kubectl

`kubectl apply -k <overlay>` makes the Job and the Deployment at the same time, and nothing runs the Job first.
Deploy each release with these two commands, in this order:

```sh
kubectl delete job -n <namespace> -l app.kubernetes.io/component=migration,app.kubernetes.io/name=<service> --ignore-not-found
kubectl apply -k <overlay>
```

The first command deletes the migration Job of the release before, if Kubernetes did not delete it yet. Without
it, a release that does not change the schema keeps the name of the Job but gets a new image, and the apply fails
with an error such as:

```text
The Job "orders-migrate-0123456789" is invalid: spec.template: Invalid value: ...: field is immutable
```

Kubernetes does not change the pod template of a Job that exists. The fix is the first command and then the apply
again: the Job of the release before has finished its work, because the pods of that release started only after
it did (see the start check below). A Job finishes within two hours, so do not delete a Job while it runs; `kubectl get job -n <namespace> -l
app.kubernetes.io/component=migration,app.kubernetes.io/name=<service>` shows its state.

The serving process checks the order at start: it reads the migration ledger `stego_schema.migrations`, and when
the ledger does not hold the last migration of its release, the process stops with the stage
`service.start.migration` and an error line that names the ledger and the migration, and says to run the migration
Job of the release and to wait until it completes. The pod restarts, with the back-off of Kubernetes, until the Job
is done, and then starts. The process also stops at the stage `service.start.migration` when the ledger holds another
digest of the body of a migration of its release: the error line names the migration and the two digests. The body of
an applied migration never changes, and nothing repairs a digest. The migration Job refuses the same database at its
ledger check, with the same fault. `stego plan`, `stego apply` and `stego compat` refuse such a release before it is
made (ST-GEN-INV38): keep the body, and bring the change in a new schema step. One other body passes the Job, the start
check and these commands: the body of the expand step of a seal, or of a step after it, that postgres-adapter 6.14.0 to
6.20.0 wrote with the functions of the seal checkpoints in it. The release registers the digest of that body as an earlier
digest of the same migration, so a database of such a release upgrades with no change. When the ledger is not the history
of the release, such as a shared preview database that ran the build of a branch whose schema steps took other numbers
before the merge, the Job stops at the stage `migrate.ledger_check` and logs a line that names the ledger row, the
migration and the remedy: restore a backup made before the other build, or make the database again. To wait in a script, after the two commands:

```sh
kubectl wait -n <namespace> --for=condition=complete job -l app.kubernetes.io/component=migration,app.kubernetes.io/name=<service> --timeout=2h
kubectl rollout status -n <namespace> deployment/<service>
```

A database whose ledger holds the migration of the release, but that does not hold the schema manifest, still
starts, with an alert for each fault. The migration Job refuses such a database.

### In local tests

The local tests run the migrate mode directly against the local PostgreSQL stand-in, with no pod, and prove the
manifest of the Job with the kustomize library. To run the migration of a service by hand, run the binary with
`migrate` and the variables of the Job, for example
`STEGO_MIGRATION_DATABASE_URL=... STEGO_SPICEDB_ENDPOINT=... STEGO_SPICEDB_KEY_FILE=... ./service migrate`.

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

The Component has two patches of the Deployment and, when the base has a migration Job, two patches of the Job.
The forms are exclusive, so the first patch removes the
environment entry of each secret variable, and of each path variable that names the file of a secret. The second
patch then:

- mounts each Secret read-only at `/var/run/secrets/stego/<secret>`, with no `subPath`, and with the mode 0400,
  with only the keys that the switched variables read;
- sets the `*_FILE` variable, or the path variable, to the path of the file of its key, for example
  `DATABASE_URL_FILE=/var/run/secrets/stego/orders-database/url`.

The files have the mode 0400 and the owner root, so that no other user can read them. The process runs as a user
that is not root, so the pod must have an `fsGroup`: Kubernetes then gives the files to that group with read
access. The restricted-v2 SCC of OpenShift gives each namespace its own `fsGroup` range, sets an `fsGroup` from it
on each pod, and refuses a pod that sets one outside the range. So the base sets none:

- On OpenShift, list only `secret-files`. Never list `fs-group`: the SCC refuses its pods.
- On a cluster other than OpenShift, list `fs-group` with `secret-files`. It sets the pod `fsGroup` 65532, the user
  and the group of the image, on the Deployment and on the migration Job, and it changes nothing else:

  ```yaml
  components:
    - https://git.example/orders.git//deploy/components/secret-files?ref=0123456789abcdef0123456789abcdef01234567
    - https://git.example/orders.git//deploy/components/fs-group?ref=0123456789abcdef0123456789abcdef01234567
  ```

  Without it, or another `fsGroup` from the overlay, the process cannot read the files and does not start.

Do not widen the mode of the files, and do not run the process as root.

The Component switches only the variables that state a secret source. In the Job, it switches only the owner
credential and the key of SpiceDB. With the shipped registry, these are:

| Variable that the Component sets | Environment entry that it removes | Secret and key |
|---|---|---|
| `DATABASE_URL_FILE` | `DATABASE_URL` | `<service>-database`, `url` |
| `STEGO_MIGRATION_DATABASE_URL_FILE` (the Job only) | `STEGO_MIGRATION_DATABASE_URL` | `<service>-database-owner`, `url` |
| `STEGO_WORKER_DATABASE_URL_FILE` | `STEGO_WORKER_DATABASE_URL` | `<service>-database`, `worker-url` |
| `STEGO_RELAY_DATABASE_URL_FILE` | `STEGO_RELAY_DATABASE_URL` | `<service>-database-relay`, `url` |
| `STEGO_SPICEDB_KEY_FILE` (the Deployment and the Job) | `STEGO_SPICEDB_KEY_FILE` | `<service>-spicedb`, `preshared-key` |
| `STEGO_QUERY_CURSOR_KEY_FILE` | `STEGO_QUERY_CURSOR_KEY_FILE` | `<service>-query-cursor`, `key` |
| `STEGO_SECRET_KEY_RING_FILE` (the Deployments only, for a service with a `secret` field, readable or compare-only, or a `pii` field) | `STEGO_SECRET_KEY_RING_FILE` | `<service>-secret-key-ring`, `key-ring` |
| `STEGO_HTTP_TLS_CERT`, `STEGO_HTTP_TLS_KEY` | the same | `<service>-tls`, `tls-crt` and `tls-key` (browser-backend) |
| `STEGO_BROWSER_CLIENT_SECRET_FILE`, `STEGO_BROWSER_SESSION_KEY_FILE` | the same | `<service>-browser`, `client-secret` and `session-keys` |
| `STEGO_GRPC_TLS_CERT`, `STEGO_GRPC_TLS_KEY` (the api Deployment only, from grpc-application 2.4.0) | the same | `<service>-grpc-tls`, `tls-crt` and `tls-key` |
| `STEGO_KAFKA_PASSWORD_FILE`, `STEGO_KAFKA_CA_FILE` (the worker Deployment only) | the same | `<service>-kafka-client`, `password` and `ca-crt` (kafka-producer, kafka-consumer) |

The Component writes a row only when the service has that variable and states that secret need. It does not
switch a credential file that has no secret source, such as `STEGO_KAFKA_TOKEN_FILE`,
`STEGO_KAFKA_CLIENT_CERT_FILE`, `STEGO_AUDIT_SINK_TOKEN_FILE` or `STEGO_OTEL_TOKEN_FILE`: the base wires its path
from the ConfigMap, and the overlay mounts the file.

A service with `kafka-producer` or `kafka-consumer` states the secret need `<service>-kafka-client` with the keys
`password`, the SCRAM-SHA-512 password of the service's Kafka user, and `ca-crt`, the PEM certificate authorities of
the brokers. Only the worker mode reads them, so only the worker Deployment references the Secret, and the API pod
holds no Kafka credential. `stego env check` refuses an environment file that does not supply this Secret. Set
`STEGO_KAFKA_SASL_MECHANISM=SCRAM-SHA-512` and `STEGO_KAFKA_USERNAME` in the ConfigMap. A cluster that uses mutual
TLS or OAUTHBEARER in place of SCRAM must not set a password file, because the worker does not start with a password
file and no SCRAM mechanism. In the overlay, remove the entry `STEGO_KAFKA_PASSWORD_FILE` and the item `password`
from the worker patch of the Component, and mount the client certificate or the token file. Do not put a
placeholder password in the Secret to satisfy the check: a stored value that no process should read is a credential
that can leak, and the worker refuses it at start. `stego env check` still names the key `password` of the secret
need; accept that one finding for such a cluster.

Use the Component `secret-files` in each production overlay. Without it, each secret is an environment variable of
the pod, which `kubectl describe`, a crash dump and each child process can read; with it, each secret is a file of
the mode 0400 that only the process reads.

With the Component, a missing Secret or a missing key stops the pod before the container starts. The kubelet cannot
mount the volume, so the pod stays in `ContainerCreating` (not `CreateContainerConfigError`), and the kubelet writes
a `FailedMount` event that names the volume, the Secret and the key, for example:

```text
Warning  FailedMount  kubelet  MountVolume.SetUp failed for volume "orders-database" : secret "orders-database" not found
Warning  FailedMount  kubelet  MountVolume.SetUp failed for volume "orders-database" : references non-existent secret key: url
```

The process never runs, so it writes no log. To find the cause, read the events of the pod:

```sh
kubectl describe pod -n <namespace> <pod>
kubectl get events -n <namespace> --field-selector reason=FailedMount,involvedObject.name=<pod>
```

The kubelet tries again, and the pod starts when the Secret holds the key.

## The CI file

When the declaration has `delivery.ci`, `stego apply` writes the CI file from the pins of the registry's
`ci-pipeline` component. The shipped component states no provider yet, so `stego apply` writes no CI file and says
so in the plan.

- `github-actions` (`.github/workflows/stego.yml`): the `check` job runs on a pull request, with read permission to
  the contents only, and never pushes or signs. The `release` job runs only on a push to the repository's default
  branch or on a tag, and it pushes and signs the image.
- `konflux` (`.tekton/<service>-push.yaml`): one PipelineRun on a push to the branch `main`. The branch is fixed
  for now. A repository whose default branch is not `main` gets no Konflux run on push. The PipelineRun gives the
  build of the standard pipeline the offline `Containerfile` (`dockerfile`), the hermetic mode (`hermetic: "true"`)
  and the Go module prefetch of the project folder (`prefetch-input`), so the build reads only the prefetched
  modules with the network off.
