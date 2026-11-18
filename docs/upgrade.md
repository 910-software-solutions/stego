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
`stego apply` used.

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
   without `--saved-plan`. See [Schema changes](schema-changes.md).
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
| the release whose base holds `deploy/base/worker-service-account.yaml` | the same | - | The worker Deployment runs as the ServiceAccount `<service>-worker`, not `<service>`. | [Give the worker ServiceAccount its grants](#give-the-worker-serviceaccount-its-grants) |
| 3.177.0 | 1.179.0 | 1.180.0 | `stego fill create` writes a test that does not name `ErrNotImplemented`. `stego test` names the cause when an old scaffold test still expects it. | [Write the tests of your fills](#write-the-tests-of-your-fills) |
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
   `<service>`. For each row of the first two queries, run `REVOKE <role> FROM
   <member>`. For each row of the third query, run `REVOKE ALL ON DATABASE
   <service> FROM <grantee>`. The second query is important after the move:
   a login that got the old LOGIN owner, such as a CI or DBA login, can still
   take the owner role and read or drop the session table:

   ```sql
   -- The roles that the owner role and the runtime login are members of:
   SELECT g.rolname AS role, m.rolname AS member FROM pg_catalog.pg_auth_members a JOIN pg_catalog.pg_roles g ON g.oid = a.roleid JOIN pg_catalog.pg_roles m ON m.oid = a.member WHERE m.rolname IN ('<service>_owner', '<service>_runtime');
   -- The members of the owner role and the runtime login, other than the migration login:
   SELECT g.rolname AS role, m.rolname AS member FROM pg_catalog.pg_auth_members a JOIN pg_catalog.pg_roles g ON g.oid = a.roleid JOIN pg_catalog.pg_roles m ON m.oid = a.member WHERE g.rolname IN ('<service>_owner', '<service>_runtime') AND m.rolname <> '<service>_migrator';
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
an owner role with a member other than the migration login, and a runtime
login with a member, with the fault `owner` or `login`.

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
`deploy/base/worker-service-account.yaml`, give `<service>-worker` each grant
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

## Add a subscription to a running service

A release that adds a subscription, a `kafka-consumer` with a consuming
worker, starts a new consumer group. The group reads the topic from its
first record that Kafka still holds. Thus the worker gets the backlog of the
topic at its first start.

Why the backlog can park: the worker acts with its own service account. Its
grant in each tenant is a tuple that the controller writes. The controller
of the new release writes the tuples only when it holds the controller lease
and its release is the release of the last migration Job. In a rolling update,
the pod of the release before holds the lease until it stops. Thus the new
worker can run before its tuple is in SpiceDB. Each such run is refused, and
after 5 refused runs in a row the worker parks the message. This takes a few
seconds. A parked message holds back each later event of the same source row.
No event is lost.

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
4. Wait until the worker applies a new event of the source, or 2 minutes after
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

A runtime change that removes steps 4 to 7 is planned: the migration Job
writes the worker tuples, and a refused run of a worker whose tuple is not
there yet is not counted for the poison bound.
