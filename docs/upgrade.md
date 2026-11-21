# Upgrade a STEGO service

This guide tells an operator how to move a running service to a newer STEGO
release. It gives the general procedure, the release notes of each change
that needs an action from you, and the steps of each action. For the
operations after the upgrade, such as a rotation of a secret or a restore,
see [Day-2 operations](operations.md).

A release of STEGO is a commit of this repository. The archetypes
`rest-crud`, `rpc-service` and `browser-service` have their own versions. The
release notes below name the first archetype versions that hold each change.
The version of an archetype is in `registry/archetypes/<archetype>/archetype.yaml`,
and `.stego/state.yaml` of your project records the versions that your last
`stego apply` used. Some notes name a version of the STEGO runtime library
instead. Your service uses the version that the line
`github.com/jsell-rh/stego/registry/runtime` of its `go.mod` names.

## The procedure

Do these steps for each upgrade. Do them in this order.

1. Find the archetype version that your service runs now. Read each release
   note below whose version is after it and at or before the target version.
   Do the action of each note in the order of the table.
2. Take a backup of the database of the service. See [Restore from a
   backup](operations.md#restore-from-a-backup) for what the backup must hold.
3. Make the new release of the service. Use the new `stego` binary on the
   declaration of the release that runs now, and give `--base` the commit or
   tag of that release:

   ```sh
   stego validate
   stego compat --base <tag of the running release>
   stego test
   stego plan --base <tag of the running release>
   stego apply --base <tag of the running release>
   stego deps
   ```

   `stego test` runs the tests of each fill. Sign each fill whose evidence is
   new with `stego fill sign <fill>`. When `stego plan` says that the
   declaration needs add steps and saved no plan, run `stego apply --base`
   without `--saved-plan`. See [Schema changes](schema-changes.md). When a
   Go build fails with `could not import log/slog`, see [A Go build fails
   with "could not import log/slog"](#a-go-build-fails-with-could-not-import-logslog).
4. Make each new Secret, grant and role that the release notes name, before
   you deploy. A pod that needs a Secret key that is not there does not start.
   `stego env check` finds a missing Secret of the environment file:

   ```sh
   stego env check --requirements deploy/platform-requirements.yaml env/<environment>.yaml
   ```

5. Deploy the release. The migration Job runs first, then the Deployments.
   See [The migration Job](delivery.md#the-migration-job).
6. Look at the result:
   - `kubectl rollout status` of `<service>` and of `<service>-worker`.
   - The log of each pod: a line `the database does not satisfy the schema
     manifest` is an alert. A pod of the new release must not write it.
   - The parked list of each destination of a consuming worker (see [Add a
     subscription to a running service](#add-a-subscription-to-a-running-service)).
7. If you must go back, go back by one release only. See [Which older releases
   can run on a newer database](operations.md#which-older-releases-can-run-on-a-newer-database).

Do not skip a release that holds a contract step of a schema change. `stego
compat` compares your change with the base that you give it. It cannot see a
release that you did not deploy.

## Release notes

Each row is a change that needs an action from you. The versions are the
first versions of the archetypes that hold the change. A dash means that the
archetype does not have the change.

| rest-crud | rpc-service | browser-service | Change | Action |
|---|---|---|---|---|
| 3.147.0 | 1.147.0 | 1.149.0 | browser-backend 4.6.0. The binary of a browser service has the mode `migrate`. The migration Job makes the session table and checks the database roles. The check needs a NOLOGIN owner role and a migration login in it. | [Move a browser-service database to the owner and migrator logins](#move-a-browser-service-database-to-the-owner-and-migrator-logins) |
| 3.160.0 | 1.161.0 | 1.163.0 | `stego init` writes `go.mod`, and each later command reads the module path from it. `STEGO_MODULE` has no effect. | [Remove STEGO_MODULE](#remove-stego_module) |
| 3.169.0 | 1.169.0 | - | The base has two Deployments: `<service>` runs the api mode, and `<service>-worker` runs the worker and controller modes. Both hold `DATABASE_URL`. | Remove an overlay patch of `STEGO_MODE` (see [Probes and modes](delivery.md#probes-and-modes)). Roll both Deployments after each change of a database password ([Rotate a database password](operations.md#rotate-a-database-password)). |
| 3.173.0 | 1.175.0 | - | kafka-producer 3.3.0 and kafka-consumer 0.3.0 read the Kafka password and the CA file from the Secret `<service>-kafka-client`. | [Make the Secret of the Kafka client](#make-the-secret-of-the-kafka-client) |
| 3.176.0 | 1.178.0 | - | The worker Deployment runs as the ServiceAccount `<service>-worker`, not `<service>`. | [Give the worker ServiceAccount its grants](#give-the-worker-serviceaccount-its-grants) |
| 3.179.0 | 1.181.0 | 1.182.0 | `stego fill create` writes a test that does not name `ErrNotImplemented`. `stego test` names the cause when an old scaffold test still expects it. | [Write the tests of your fills](#write-the-tests-of-your-fills) |
| runtime v0.134.0 | runtime v0.134.0 | - | A new consumer waits for its worker tuple, so a new subscription no longer parks its backlog. The refresh age of the OIDC key set is the `max-age` of the issuer. A second cursor key can open cursors. A route that does not exist answers 404. A retire removes the tuples of the rows of the tenant. A message that an older runtime counted toward poison keeps its count. | [Add a subscription to a running service](#add-a-subscription-to-a-running-service), [Rotate the OIDC signing key](operations.md#rotate-the-oidc-signing-key), [Rotate the cursor key](operations.md#rotate-the-cursor-key). Before the upgrade, resolve each parked message of a consuming worker (see [See and resolve the parked messages](commands-queries-workers.md#see-and-resolve-the-parked-messages)): a message that the older runtime almost parked can park soon after the upgrade. |
| 3.182.0 | 1.184.0 | - | Checking refuses a `route` on a query whose `method` is a gRPC server stream: no handler served it. The query contract names the repository of an entity with the plural of its table, such as `GetEntryEntries`; the old name, such as `GetEntryEntrys`, stays one release as a deprecated function. Runtime v0.136.0: a write waits for the revocation that `Authz-Revocation-After` names, and a unique violation names the fields of its key. | [Remove the route of a stream query](#remove-the-route-of-a-stream-query), [Use the new names of the query repositories](#use-the-new-names-of-the-query-repositories). A client that creates a child at once after its parent can send `Authz-Revocation-After` (see [Write a child at once after its parent](commands-queries-workers.md#write-a-child-at-once-after-its-parent)). |
| runtime v0.137.0 | runtime v0.137.0 | - | A field with `sensitivity: secret` is sealed at rest: the column of a readable secret holds sealed bytes (`bytea`), and a `compare_only` field holds an Argon2id hash. The api, worker and controller modes do not start without the key ring of the Secret `<service>-secret-key-ring`. In a fill, the type of a secret field is `sealed.Secret` (read its text with `Reveal()`) or `sealed.Compared` (check a value with `Matches`). A field that a release before v0.135.0 declared secret was stored in clear: `stego compat` and `stego plan` refuse the release until you declare a seal step for it. | [Seal the secret fields of a release before runtime v0.135.0](#seal-the-secret-fields-of-a-release-before-runtime-v01350) |
| 3.189.0 | 1.190.0 | 1.191.0 | Runtime v0.138.0 and postgres-adapter 6.14.0: the seal step is built. A service with a `pii` field and no `secret` field also needs the key ring, which seals the stored answers of its idempotency keys. A retry of a key that the release before stored gets `idempotency-key-reused`. | [Make the key ring of a service with a pii field](#make-the-key-ring-of-a-service-with-a-pii-field) |
| 3.199.0 | 1.199.0 | 1.199.0 | Runtime v0.150.0, postgres-adapter 6.17.0, command 1.13.0 and rest-api 4.19.0. A process of the release of the expand step of a seal goes on after the contract step drops the clear column. `stego compat` and `stego plan` refuse the contract step of a seal over a base with a runtime before v0.150.0. The update request of an entity with a secret field requires no secret: an update that leaves it out keeps it. Checking refuses a field named `kind` or `href` of an entity that a collection serves. | Before the contract step of an open seal, deploy a release with runtime v0.150.0 or later and the seal still at expand (see [The contract step of a seal](schema-changes.md#the-contract-step-of-a-seal)). Give a field named `kind` or `href` another name: when the base stores it, add a field of another name and drop the old one, because a rename keeps the API name. |
| 3.206.0 | 1.206.0 | 1.206.0 | command 1.14.0: the repository of an entity with its own resource type is the type `<Entity>Repo`. Its `GetForUpdate`, `GetForDelete`, `Update` and `Delete` take no parent, and an update that changes a reference is `Move`. A fill that gives a parent to one of them does not compile: `too many arguments in call to`. Runtime v0.154.0: a process whose database lacks a table or a column of its manifest, such as after a roll back past a contract step, starts not ready; the api Deployment of a service with modes has the readiness `/readyz/modes`. After an offset reset, a record that comes again takes its place before the later messages of its row, and a message parked as poison goes back to pending when an earlier message of its row is done. | [Give no parent where the repository takes none](#give-no-parent-where-the-repository-takes-none). Remove an overlay patch of the readiness probe of `<service>`. See [Which older releases can run on a newer database](operations.md#which-older-releases-can-run-on-a-newer-database) and [Repair a wrong order with an offset reset](subscriptions.md#repair-a-wrong-order-with-an-offset-reset). |
| 3.209.0 | 1.209.0 | 1.209.0 | Runtime v0.157.0, postgres-adapter 6.19.0, command 1.15.0 and rest-api 4.21.0. A field written `sensitivity: public` is a field with no sensitivity: a list searches and orders it, and its table has its search index. A seal `from: public` keeps the search index of the field, as a seal with no `from`. `stego compat` and `stego plan` refuse the contract step of one seal of an entity while the base holds another seal of the same entity open. A process of the release of the expand step writes the clear column again when a restore brings it back, and the seal job completes no pass with no checkpoint while the clear column is there. | Declare the contract steps of the open seals of one entity in the same release (see [The contract step of a seal](schema-changes.md#the-contract-step-of-a-seal)). |
| 3.215.0 | 1.215.0 | 1.215.0 | rest-api 4.24.0: a patch refuses each body member that is not a patchable field, `kind`, or a field that a collection write cannot change. The names must match in letter case: a member such as `Title` for the field `title` is refused. The answer is a 400 validation problem that names the member, and the patch changes nothing. Before, a patch ignored such a member and answered 200. The OpenAPI document gives each `<Collection>PatchRequest` with `additionalProperties: false`. cli-application 1.16.0: an apply of the same documents again leaves out each create-only field whose value is the stored one. | Send in a patch only the fields that `patchable` names, in the letter case of the OpenAPI document, and after a rename the API name of the field. Build the clients again from the new OpenAPI document (see the operations of a collection in [Quick start](../README.md#quick-start)). |
| 3.227.0 | 1.227.0 | 1.227.0 | rest-api 4.27.0: a create, an update and an upsert refuse each body member that is not a field of the entity by its API name, `kind`, `id` or `href`. The names must match in letter case: a member such as `Title` for the field `title`, a typo, the declared name of a renamed field or `tenant_id` is refused. The answer is a 400 validation problem that names the member, and the write changes nothing. Before, the write ignored such a member and answered 2xx, and the field was not set. A body may still give `id`, `href` and each field that the server sets, as a read gives them: the server ignores these values. A body that gives an implicit field of the collection another value than the value of the collection is refused too; before, the value of the collection replaced it. The OpenAPI document gives each `<Entity>CreateRequest` and `<Entity>UpdateRequest` with `additionalProperties: false`, marks each member that the server sets `x-output-only`, and each member that the body may only repeat `x-kept`. cli-application 1.21.0, go-sdk 2.2.2 and typescript-sdk 1.2.4 take the new document. | Send in a create, an update or an upsert only the fields of the entity, in the letter case of the OpenAPI document, and after a rename the API name of the field. Leave an implicit field out, or give the value of the collection. Build the CLI and the SDKs again from the new OpenAPI document (see the operations of a collection in [Quick start](../README.md#quick-start)). |
| each release | each release | - | A release that adds a subscription. | [Add a subscription to a running service](#add-a-subscription-to-a-running-service) |

## Move a browser-service database to the owner and migrator logins

Do this once, before the first migration Job of a browser service at
browser-service 1.149.0 or later.

Why: a release before browser-backend 4.6.0 used a LOGIN owner. That login
owned the database and the session table, and your controller or your
database administrator made the table with it. The migration Job of
browser-backend 4.6.0 and later checks a different layout:

- The owner role `<service>_owner` owns the database and the session table.
  It cannot log in, it is NOINHERIT, it has no other attribute, and it is a
  member of no role.
- The migration login `<service>_migrator` is a member of the owner role. The
  Job connects as it and takes the owner role.
- The runtime login `<service>_runtime` is NOINHERIT and a member of no role.
  It has CONNECT on the database, USAGE on the schema `public`, and SELECT,
  INSERT, UPDATE and DELETE on the session table.
- No other role, and not PUBLIC, has a grant on the database or on the schema
  `public`.

On the old layout, the Job stops with this failure record:

```json
{"severity":"ERROR","event.name":"service.failed","stage":"database.schema","fault":"owner","object":"public.stego_browser_sessions","remedy":"the database roles are not the layout of browser-backend 4.6.0 and later: ..."}
```

The fault `login` or `grant` with the same remedy also tells you to do these
steps. The move keeps the session table and its rows. The pods of the running
release connect as the runtime login, so they continue to serve during the
move.

Replace `<service>` with the service name, with each `-` as `_`. Do the steps
in this order:

1. As a database administrator, connected to any database of the server, run:

   ```sql
   -- As a database administrator, connected to any database of the server:
   ALTER ROLE <service>_owner NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD NULL;
   ALTER ROLE <service>_runtime LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
   CREATE ROLE <service>_migrator LOGIN IN ROLE <service>_owner;
   ALTER DATABASE <service> OWNER TO <service>_owner;
   REVOKE ALL ON DATABASE <service> FROM PUBLIC;
   GRANT CONNECT ON DATABASE <service> TO <service>_runtime;
   ```

2. Give the migration login a new secret in `psql`. `SHOW
   password_encryption` must give `scram-sha-256`. The `\password` command
   sends only the SCRAM verifier of the secret:

   ```psql
   SHOW password_encryption;
   \password <service>_migrator
   ```

3. As a database administrator, connected to the database `<service>`, run:

   ```sql
   -- As a database administrator, connected to the database <service>:
   ALTER TABLE public.stego_browser_sessions OWNER TO <service>_owner;
   ALTER SCHEMA public OWNER TO pg_database_owner;
   REVOKE ALL ON SCHEMA public FROM PUBLIC;
   GRANT USAGE ON SCHEMA public TO <service>_runtime;
   ```

4. Make sure that the layout has no other membership and no other grant. Each
   of these queries must give no row. Run them connected to the database
   `<service>`. For each row of the first three queries, run `REVOKE <role>
   FROM <member>`. For each row of the last query, run `REVOKE ALL ON DATABASE
   <service> FROM <grantee>`. The second and the third query are important
   after the move: a login that got the old LOGIN owner, such as a CI or DBA
   login, or a member of the migration login, can still take the owner role and
   read or drop the session table.

   The queries list only the memberships that give the privileges of the role
   (INHERIT or SET). On PostgreSQL 16 and later, an administrator that is not a
   superuser, such as the master user of a managed server (Amazon RDS, Cloud
   SQL, Azure), gets a membership WITH ADMIN OPTION only in each role that it
   makes. That membership cannot use the role, and the check accepts it. Do not
   revoke it: the administrator needs it to change the password of a login.

   ```sql
   -- The roles that the owner role and the runtime login are members of:
   SELECT g.rolname AS role, m.rolname AS member FROM pg_catalog.pg_auth_members a JOIN pg_catalog.pg_roles g ON g.oid = a.roleid JOIN pg_catalog.pg_roles m ON m.oid = a.member WHERE m.rolname IN ('<service>_owner', '<service>_runtime') AND (a.inherit_option OR a.set_option);
   -- The members of the owner role and the runtime login, other than the migration login:
   SELECT g.rolname AS role, m.rolname AS member FROM pg_catalog.pg_auth_members a JOIN pg_catalog.pg_roles g ON g.oid = a.roleid JOIN pg_catalog.pg_roles m ON m.oid = a.member WHERE g.rolname IN ('<service>_owner', '<service>_runtime') AND m.rolname <> '<service>_migrator' AND (a.inherit_option OR a.set_option);
   -- The members of the migration login, which can take the owner role through it:
   SELECT g.rolname AS role, m.rolname AS member FROM pg_catalog.pg_auth_members a JOIN pg_catalog.pg_roles g ON g.oid = a.roleid JOIN pg_catalog.pg_roles m ON m.oid = a.member WHERE g.rolname = '<service>_migrator' AND (a.inherit_option OR a.set_option);
   -- The grants on the database to a role that is not the owner role or the runtime login:
   SELECT a.grantee, a.privilege_type FROM pg_catalog.pg_database d CROSS JOIN pg_catalog.aclexplode(d.datacl) a WHERE d.datname = '<service>' AND a.grantee NOT IN (SELECT oid FROM pg_catalog.pg_roles WHERE rolname IN ('<service>_owner', '<service>_runtime'));
   ```

5. Change the key `url` of the Secret `<service>-database-owner` to the
   connection string of `<service>_migrator`, for example
   `postgres://<service>_migrator:<secret>@<host>:5432/<service>?sslmode=verify-full`.
   If your own controller made the session table with the old owner login,
   change it to use the migration login too.
6. Run the migration Job of the new release. It exits 0 when the layout is
   correct. The Job runs at each release, and a second run changes nothing.
7. Deploy the new release.

`PASSWORD NULL` removes the old password of the owner role, so a later
`ALTER ROLE ... LOGIN` cannot bring it back. The migration Job also refuses
an owner role with a member other than the migration login, a member of the
migration login, and a runtime login with a member, with the fault `owner` or
`login`. It counts only the memberships with INHERIT or SET, so an ADMIN-only
membership of the administrator is accepted.

When the Job still stops, its failure record names the fault class. See the
list of what the check needs in the README of the component
[browser-backend](../registry/components/browser-backend/README.md).

## Remove STEGO_MODULE

Why: `stego init` writes the module path into `go.mod`, and each later command
reads it from there. The variable `STEGO_MODULE` has no effect.

1. Make sure that `go.mod` of the project names the module path that your
   imports use.
2. Remove `STEGO_MODULE` from your shell profile, your CI file and your build
   scripts, so that nobody thinks that it sets the module.

## Make the Secret of the Kafka client

Why: kafka-producer 3.3.0 and kafka-consumer 0.3.0 give the SCRAM password
file and the CA file of the brokers a secret source. The worker Deployment
mounts the Secret `<service>-kafka-client` with the Component `secret-files`.
Without the Secret, the worker pod does not start.

Do this before you deploy the release, for each service with
`kafka-producer` or `kafka-consumer`:

1. Make the Secret `<service>-kafka-client` with two keys:
   - `password`: the SCRAM-SHA-512 password of the Kafka user of the service.
   - `ca-crt`: the PEM certificate authorities of the brokers.

   For example, with kubectl:

   ```sh
   kubectl create secret generic -n <namespace> <service>-kafka-client \
     --from-file=password=<file of the password> --from-file=ca-crt=<file of the CA>
   ```

   Use your secret operator in production, as for the other Secrets.
2. Set `STEGO_KAFKA_SASL_MECHANISM=SCRAM-SHA-512` and `STEGO_KAFKA_USERNAME` in
   the ConfigMap `<service>-config`, if they are not there.
3. Run `stego env check`. It refuses an environment file that does not give
   this Secret.

A cluster that uses mutual TLS or OAUTHBEARER in place of SCRAM does not set a
password. See [Secrets as files](delivery.md#secrets-as-files) for the patch
of the overlay.

## Give the worker ServiceAccount its grants

Why: the worker Deployment runs as the ServiceAccount `<service>-worker`. A
grant that you made outside the base for the ServiceAccount `<service>` does
not apply to it.

Before you apply the first release whose base holds
`deploy/base/worker-service-account.yaml` (rest-crud 3.176.0, rpc-service
1.178.0), give `<service>-worker` each grant
that the worker needs:

- a workload identity annotation, such as `eks.amazonaws.com/role-arn` or
  `iam.gke.io/gcp-service-account`, and the trust policy of that cloud role,
  which names the ServiceAccount;
- the `imagePullSecrets` of the ServiceAccount, when your overlay puts them
  there;
- the Vault Kubernetes auth role, whose `bound_service_account_names` must
  name `<service>-worker`;
- on OpenShift, an SCC that you gave to `<service>` with `oc adm policy
  add-scc-to-user`. Give the worker only the SCCs that it needs. Do not copy a
  broad SCC that only the API needs;
- each RoleBinding or ClusterRoleBinding that your platform made for
  `<service>`.

If you do not, the worker pods start without the grant: they cannot pull the
image, or they cannot get their cloud or Vault credentials. See [Probes and
modes](delivery.md#probes-and-modes).

## Write the tests of your fills

Why: an older `stego fill create` wrote a test that expects the unfinished
fill to return `ErrNotImplemented`. When you write the fill and remove
`ErrNotImplemented`, the test does not compile. An older `stego test` did not
run these tests, so you did not see this. A newer `stego test` runs them, and
it records no evidence for a fill whose tests fail. Then `stego plan`,
`stego apply` and `stego build` refuse the fill.

`stego test` names this cause for each such fill:

```text
fill register-technician: no evidence recorded: its tests failed: ...; fill_test.go still expects ErrNotImplemented, ...
```

For each fill that it names:

1. Open `fills/<fill>/fill_test.go`. Remove the test that expects
   `ErrNotImplemented`.
2. Write the tests of what the fill does. Give the fill a handle of a test,
   or call the functions that the fill uses with the values of the test.
3. Run `stego test`. Then sign the fill again with `stego fill sign <fill>`.

A new `stego fill create` writes a test that passes while the fill is not
written. When you write the fill, the test fails with a message that tells you
to replace it. It does not stop the compile.

## Seal the secret fields of a release before runtime v0.135.0

Do this when your running release is before runtime v0.135.0
(postgres-adapter 6.13.0) and its declaration has a field with `sensitivity:
secret`. Do it in the first release with runtime v0.137.0 or later.

Why: a release before runtime v0.135.0 stored a secret field in clear, in a
column of its type, such as `character varying(128)`. A newer release stores
it sealed, in a `bytea` column. The database holds the rows in clear, so the
newer release must move them with a seal step (decision ST-DECL-Q44). Without
it, `stego compat --base` and `stego plan --base` refuse the release with the
fault `the head stores <Entity>.<field> sealed, which the base stores in
clear`, and its suggested change is a seal `with from: secret`. The check reads
the version of postgres-adapter in `.stego/state.yaml` of the base.

1. If the running release is before runtime v0.120.0 (postgres-adapter
   6.4.0), first deploy a release with runtime v0.120.0 or later and no new
   schema step. Its processes report themselves, which the seal needs: `stego
   compat` refuses the seal over an older release.
2. Make the Secret `<service>-secret-key-ring` before you deploy. See [The key
   ring](operations.md#the-key-ring). `stego env check` names it when it is
   not there.
3. Change each fill that reads or writes the secret field. Its Go type is
   `sealed.Secret` (package `github.com/jsell-rh/stego/registry/runtime/sealed`):
   read its text with `v.ApiKey.Reveal()`, and make a value with
   `sealed.SecretOf(text)`. A `Secret` prints and logs as `[secret]` and
   encodes as JSON `null`, so a log line does not hold the value.
4. Declare a seal step with `from: secret` for each secret field, and keep the
   field as it is:

   ```yaml
   schema_changes:
     - {id: purse-api-key-seal, kind: seal, entity: Purse, field: api_key, from: secret}
   schema_history:
     - {change: purse-api-key-seal, phase: expand}
   ```

   The first migration `001_initial` stays the one of the running release.
   Run `stego compat --base <tag>` and `stego plan --base <tag>`: they accept
   the release.
5. Deploy the release. The migration Job adds the column `api_key_sealed`, and
   the release before goes on with the clear column while the Deployments
   roll. The new release writes both columns. The controller seals each row
   when no pod of the release before runs. See [Seal steps](schema-changes.md#seal-steps).
   You can go back to the release before in this phase: it reads the clear
   column, and the seal job checks each row again later. While a seal is open,
   do not go back past a release that reports itself (runtime v0.120.0 or
   later): an older release writes the clear column unseen.
6. In the next release, declare the contract step of each seal. Until then the
   clear column holds the secret, also in each backup that you take. Its migration
   refuses until the seal job completed a pass after the last pod of the
   release before stopped. Then it drops the clear column. After it, you cannot
   go back to a release before the expand step.

## Make the key ring of a service with a pii field

Do this before you deploy a release with runtime v0.138.0 or later, for a
service with a `pii` field and no `secret` field. A service with a `secret`
field has its key ring already.

Why: an answer of a command can hold a `pii` field, and the idempotency store
keeps each answer for the retention of its key, 24 hours by default. From
runtime v0.138.0 the store seals each answer with the key ring of the service
(ST-SUP-Q182 (b)). The api, worker and controller modes do not start without
it.

1. Make the Secret `<service>-secret-key-ring` with the key `key-ring`, as
   [The key ring](operations.md#the-key-ring) tells. Keep a copy of it in your
   secret store.
2. Run `stego env check`. It refuses an environment file that does not give
   this Secret.

A retry of an idempotency key that the release before stored, or that a
newer release stored when you go back, gets `idempotency-key-reused`; the
client sends it again with a new key.

## Remove the route of a stream query

Do this when `stego validate` gives the fault `query-stream-route`.

Why: a query whose `method` is a gRPC server stream is served over gRPC only
(ST-SUP-RC33-OC08). Its `route` was served by no handler: a request to it got
404 or 405. Checking now refuses the route, so the declaration says what the
service serves.

1. Remove the `route` of the stream query from `service.yaml`.
2. When an HTTP client must list the same rows, declare a second list query
   with a `route` and a unary `method`, or with no `method`, and its own fill.
   See [Declare a query](commands-queries-workers.md#a-get-query-and-a-list-query).
3. Run `stego validate` and `stego apply`.

## Use the new names of the query repositories

Do this in the first release at rest-crud 3.182.0 or rpc-service 1.184.0, or
in the release after it.

Why: the query contract named the repository of an entity with the plural
`<Entity>s`, such as `GetEntryEntrys` for the table `entries`. It now uses the
plural of the table names: `GetEntryEntries`, `ListBoxesBoxes`,
`ListCategoriesCategories`. The old name stays for one release as a function
marked `Deprecated` that calls the new one, so a fill still builds. Only an
entity whose name ends in `s`, `x`, `z`, `sh`, `ch`, or a consonant and `y`
has a new name.

1. In each query fill, replace each call of the old name with the new name.
   `staticcheck` (SA1019) and `gopls` name each call of a deprecated function.
2. Run `stego test` and `stego fill sign <fill>` for each fill that you
   changed.

## Give no parent where the repository takes none

Do this in the first release at rest-crud 3.206.0, rpc-service 1.206.0 or
browser-service 1.206.0.

Why: the runtime refused at run time each parent of a locked read, an update
that keeps its references and a delete of an entity with its own resource
type, and the command answered 500 (walkthrough 15). The generated repository
of such an entity now takes no parent for these calls, so the mistake fails
in `go build` and `stego test`. See [The repository of an
entity](fills.md#the-repository-of-an-entity).

1. Run `stego test`. Go names each call to change, such as `too many
   arguments in call to parcels.Update`.
2. Change each call that it names:
   - Remove the parent, or `nil`, from `GetForUpdate`, `GetForDelete`,
     `Delete` and an `Update` that keeps the references of the row.
   - Replace `Update` with `Move` for an update that changes a reference,
     and keep its parents: `parcels.Move(ctx, from, value, newShipment)`.
3. A fill that keeps the repository in a variable or a parameter of the type
   `*command.Repository[<Entity>]` uses the type `*commands.<Entity>Repo` or
   `*workers.<Entity>Repo` in its place.
4. Run `stego test` and `stego fill sign <fill>` for each fill that you
   changed.

A child entity, which has no resource type of its own, does not change: its
calls still take the loaded parent row.

## Add a subscription to a running service

A release that adds a subscription, a `kafka-consumer` with a consuming
worker, starts a new consumer group. The group reads the topic from its
first record that Kafka still holds. Thus the worker gets the backlog of the
topic at its first start.

The worker acts with its own service account. Its grant in each tenant is a
tuple that the controller writes. The controller of the new release writes
the tuples only when it holds the controller lease and its release is the
release of the last migration Job. In a rolling update, the pod of the release
before holds the lease until it stops. Thus the new worker can run before its
tuple is in SpiceDB.

- From runtime v0.134.0: the worker checks its tuple before each run. While
  the tuple is not there, the message stays pending with the failure code
  `worker-tuple-missing`, and the runtime does not count it toward poison.
  The backlog is applied when the controller of the new release has written
  the tuples. Do steps 1 to 3 below. Steps 4 to 7 are not necessary.
- Before runtime v0.134.0: each such run is refused, and after 5 refused runs
  in a row the worker parks the message. This takes a few seconds. A parked
  message holds back each later event of the same source row. No event is
  lost. Do all the steps below.

Do these steps:

1. Give the Kafka user of the service the ACLs of the subscription (see
   [Access to the topic](subscriptions.md#access-to-the-topic)).
2. Choose where the group starts. To apply the backlog, do nothing. To skip
   the backlog, set the offsets of the group to the end of the topic before the
   first start:

   ```sh
   kafka-consumer-groups.sh --bootstrap-server <broker> --command-config <client properties> \
     --group <service>.<subscription> --topic <source topic> --reset-offsets --to-latest --execute
   ```

3. Deploy the release.
4. Before runtime v0.134.0 only: wait until the worker applies a new event of the source, or 2 minutes after
   the last pod of the release before stopped. Then the controller of the new
   release holds the lease, and it wrote the tuples.
5. In each tenant, list the parked messages of the destination of the
   subscription. The destination has the name of the subscription. Do not put
   the token on the command line, where `ps` and the shell history show it.
   Write the header into a file that only you can read (curl 7.55 or later):

   ```sh
   (umask 077; printf 'Authorization: Bearer %s\n' "$(cat <file of the token>)" > auth-header)
   curl -sS -H @auth-header "https://<host><base path>/stego/outbox/<subscription>/parked"
   ```

6. Send each parked message again, then remove the header file:

   ```sh
   curl -sS -H @auth-header "https://<host><base path>/stego/outbox/<subscription>/parked" |
     jq -r '.items[].id' | while read -r id; do
       curl -sS -X POST -H @auth-header -H 'Content-Type: application/json' \
         -d '{"resolution": "not_acted"}' "https://<host><base path>/stego/outbox/<subscription>/parked/$id:resolve"
     done
   shred -u auth-header
   ```

   Do this again while the list has a `next` cursor. The caller needs the
   permission `resolve` on `outbox_destination:<subscription>` in the tenant
   (see [See and resolve the parked
   messages](commands-queries-workers.md#see-and-resolve-the-parked-messages)).
7. List the parked messages again. A message that parks again has another
   cause, such as a row that the fill needs and that does not exist. Correct
   the cause, then send it again.

From runtime v0.134.0, look at the parked list once after the first start
all the same: a message that parks has another cause, such as a row that the
fill needs and that does not exist.

## A Go build fails with "could not import log/slog"

The Go command keeps an index of each module version of the module cache in
its build cache (`go env GOCACHE`). It makes the index one time and uses it
again, because it takes the content of a module version as fixed. A released
version of the STEGO runtime library never changes its content. A runtime
version that you fetched before its release, such as from a branch of a pull
request, can change its content and its sum before the release. Take such a
new sum only from the go.sum line of the project that gives the version, and
never turn off the check of sums (do not set GONOSUMDB, GONOSUMCHECK,
GOSUMDB=off or GOFLAGS=-insecure). When the
module cache then holds the new content and the build cache holds the index
of the old content, a Go build can fail with an error such as:

```text
could not import log/slog (open : no such file or directory)
```

The same error can come from `go build`, `go test` or `go vet`, and from
`stego test` or `stego deps`, which run Go commands. The error names a
package of the standard library, but the cause is the old index. To correct
it, clear the build cache, then build again:

```sh
go clean -cache
```

To build one time without the index, and keep the build cache, set
`GODEBUG=goindex=0` for that command only, such as
`GODEBUG=goindex=0 go build ./...`. When the error comes back, clear the build
cache. A build cache that more users or builds share has the same old index
for each of them: clear it when no build runs.
