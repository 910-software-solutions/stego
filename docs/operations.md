# Day-2 operations

This guide tells an operator how to rotate the secrets and the keys of a
running STEGO service, how to restore it from a backup, and which older
release can run on a newer database. Each procedure gives its steps in order
and the reason for the order. To move a service to a newer release, see [the
upgrade guide](upgrade.md).

A process of the service reads each Secret once, at start. A new value of a
Secret reaches a process only when its pod restarts, also with the Component
`secret-files` (see [Secrets as files](delivery.md#secrets-as-files)).

## Where each secret is

Restart each workload that the table names for a key after you change the
key. `<service>` is the name of the Deployment of the api mode,
`<service>-worker` the Deployment of the worker and controller modes, and the
migration Job is `<service>-migrate-<release>`.

| Secret | Key | What it holds | Read by |
|---|---|---|---|
| `<service>-database` | `url` | the connection string of the runtime login | `<service>`, `<service>-worker` |
| `<service>-database` | `worker-url` | the connection string of the worker login | `<service>-worker` |
| `<service>-database-relay` | `url` | the connection string of the relay login | `<service>-worker` |
| `<service>-database-owner` | `url` | the connection string of the migration login | the migration Job |
| `<service>-kafka-client` | `password`, `ca-crt` | the SCRAM password of the Kafka user, and the CA of the brokers | `<service>-worker` |
| `<service>-query-cursor` | `key` | the key of the list cursors | `<service>`, `<service>-worker` |
| `<service>-spicedb` | `preshared-key` | the key of SpiceDB | `<service>`, `<service>-worker`, the migration Job |

A browser service has one Deployment, `<service>`, which reads the key `url`
of `<service>-database`. Its migration Job reads `<service>-database-owner`.

## Rotate a database password

PostgreSQL holds one password for each login. Thus, when you change the
password of a login, each new connection with the old password fails with
SQLSTATE 28P01, and each connection that is open stays open. The service
keeps a pool of connections, and it opens a new connection when it needs
more. STEGO does not let a second login act as the runtime role: the
migration Job refuses a member of a role of the service that it did not
grant. So use the method "change, then roll". The method has a short window:
a request that needs a new connection between step 2 and the end of step 3
fails. The question ST-SUP-Q180 asks for a method with no window.

Do these steps for a login, in this order, with no pause between them:

1. Make a new secret value. Write the new connection string into the key of
   the Secret that holds it (see the table above), with your secret operator.
   The running pods continue with the old value.
2. As a database administrator, set the new password of the login in `psql`.
   `\password` sends only the SCRAM verifier:

   ```psql
   \password <service>_runtime
   ```

3. Restart each workload that reads the key. For the runtime login, these are
   both Deployments:

   ```sh
   kubectl rollout restart -n <namespace> deployment/<service> deployment/<service>-worker
   kubectl rollout status -n <namespace> deployment/<service>
   kubectl rollout status -n <namespace> deployment/<service>-worker
   ```

4. Look at the logs of the new pods. A failure record with the stage
   `service.start.database` and the cause `authentication_failed` shows a pod
   that has the old value.

Why this order:

- Step 1 is first, so that each pod that starts from now on gets the new
  value. A pod that starts between step 1 and step 2 fails with
  `authentication_failed` and restarts, and it starts after step 2.
- Step 3 must restart the worker Deployment too. It also holds the key `url`
  of `<service>-database`. A worker pod that keeps its open connections works
  until it restarts for another reason. Then it does not start, and no event
  goes out until it starts.

For the other logins:

- The worker login (`worker-url`) and the relay login: restart
  `<service>-worker` only.
- The migration login: change it when no migration Job runs. No pod holds it.
  The next migration Job reads the new value.
- The login of a browser service: restart `<service>`.

## Rotate the Kafka SCRAM password

Kafka holds one SCRAM password for each user and mechanism, and a session
that is open stays open. Thus "change, then roll" works as for PostgreSQL.
With two Kafka users, the rotation has no window. The worker Deployment is the
only workload that holds the Kafka credential.

The method of two users, with no window:

1. Make a second user for the service, such as `orders-b` for `orders`, with
   a new password:

   ```sh
   kafka-configs.sh --bootstrap-server <broker> --command-config <admin properties> --alter \
     --add-config 'SCRAM-SHA-512=[password=<new password>]' --entity-type users --entity-name orders-b
   ```

2. Give the second user the same ACLs as the first. List them with
   `kafka-acls.sh --list --principal User:orders`, and add each one for
   `User:orders-b`: write on the topic of the service, and read on each source
   topic and on each consumer group of a subscription (see [Access to the
   topic](subscriptions.md#access-to-the-topic)).
3. Set `STEGO_KAFKA_USERNAME=orders-b` in the ConfigMap `<service>-config`, and
   write the new password into the key `password` of `<service>-kafka-client`.
4. Restart the worker Deployment, and wait until it is available:

   ```sh
   kubectl rollout restart -n <namespace> deployment/<service>-worker
   kubectl rollout status -n <namespace> deployment/<service>-worker
   ```

5. Remove each ACL of the first user with `kafka-acls.sh --remove` and the
   options that added it. Then remove its SCRAM credential:

   ```sh
   kafka-configs.sh --bootstrap-server <broker> --command-config <admin properties> --alter \
     --delete-config 'SCRAM-SHA-512' --entity-type users --entity-name orders
   ```

The next rotation moves back to the first user. The consumer group is a name
of the service, not of the user, so its offsets stay.

Why this order: in step 4, the old pods use the first user and the new pods use
the second user, and both users are valid. Only after the last old pod stopped
does step 5 remove the first user.

The method "change, then roll" with one user: set the new password with
`kafka-configs.sh --alter --add-config`, write it into the Secret, and restart
the worker Deployment at once. The running worker keeps its session. A worker
pod that starts with the old password cannot send or read events until it has
the new value.

## Rotate the OIDC signing key

The API of a rest-crud or an rpc-service service verifies a token with the key
set of the issuer, from `STEGO_AUTHN_KEYS_URL`. Each pod keeps a copy of the
key set, and it loads the key set again in these cases:

- A token names a key ID (`kid`) that the copy does not hold. The pod loads the
  key set before it answers. It loads at most once in 30 seconds (the
  cooldown). A token of an unknown key in the cooldown gets 401.
- The copy is older than 5 minutes. The next request still uses the copy, and it
  starts a load in the background.
- When no load succeeded in 15 minutes, the pod refuses each token.

A browser service loads the key set of the issuer again when a token names an
unknown key ID.

Do the steps in this order (OpenID Connect Core 1.0, section 10.1.1, and RFC
7517):

1. Publish the new key in the key set of the issuer, beside the old key. Do
   not sign with it yet.
2. Wait until each replica and each cache of the issuer serves the new key
   set. Then wait at least 30 seconds more. We recommend 6 minutes: then each
   pod that gets requests has a copy with the new key, and no request waits for
   a load.
3. Sign the new tokens with the new key.
4. Keep the old key in the key set until each token that it signed has
   expired: the longest lifetime of an access token of the issuer, plus 60
   seconds of clock skew.
5. Remove the old key from the key set.

Why this order:

- A token of the new key that comes before the key set holds the key gets 401.
  It also starts the cooldown, so the pod refuses the new key for up to 30
  seconds after you publish it.
- A token of the old key that comes after you remove the key is refused.
  Step 4 makes sure that no valid token is of the old key.

A pod accepts a token of a removed key until its copy is 5 minutes old and a
request loads the key set again. Thus a removal is not a revocation. When the
old key leaked, remove it from the key set, and then restart the Deployments,
so that each pod loads the key set at start:

```sh
kubectl rollout restart -n <namespace> deployment/<service>
```

When the service reads the key set from the file `STEGO_AUTHN_KEYS_FILE`, it
reads the file only at start. Restart the pods after step 1 and after step 5.

## Rotate the cursor key

A list gives a cursor for its next page. The cursor is sealed with the key of
`<service>-query-cursor`. A pod refuses a cursor that another key sealed, with
400 and the detail `the cursor is not valid`. Each pod has one key.

Do these steps:

1. Make a new key of at least 32 random bytes, and write it into the key `key`
   of `<service>-query-cursor`:

   ```sh
   head -c 32 /dev/urandom > cursor-key
   ```

2. Restart the two Deployments:

   ```sh
   kubectl rollout restart -n <namespace> deployment/<service> deployment/<service>-worker
   ```

During the restart, a cursor of an old pod can go to a new pod, and the reverse.
The pod then answers 400. The client must start the list again, with no
cursor. Write each client so that it starts a list again when a cursor gets
400. A list is not a snapshot, so a client must also accept rows that change
between two pages.

A runtime change that removes the 400 is planned: a list of keys, where the
first key seals each cursor and each key opens a cursor.

## Restore from a backup

### What the backup must hold

Back up two things. Keep them as secrets, because they hold password
verifiers and data:

- The roles of the server, once and after each change of a role:

  ```sh
  pg_dumpall --roles-only --file=roles.sql
  ```

- The database of the service. Write down the time at which the dump starts.
  The dump holds the data of that time, `T0`:

  ```sh
  pg_dump --format=custom --file=<service>.dump <service>
  ```

The custom archive holds the owner of each object, the grants, the owner of
the database and its grants. The roles are objects of the server, so the dump
of the database does not hold them.

### Restore

Do these steps in this order:

1. Stop the service. Pause the automatic sync of its Argo CD Application, if
   it has one, so that Argo CD does not start it again. Then:

   ```sh
   kubectl scale -n <namespace> deployment/<service> deployment/<service>-worker --replicas=0
   ```

2. On a new server, make the roles first:

   ```sh
   psql --file=roles.sql postgres
   ```

3. Move the damaged database aside, then restore the archive with `--create`:

   ```sh
   psql -c 'ALTER DATABASE <service> RENAME TO <service>_damaged' postgres
   pg_restore --create --exit-on-error --dbname=postgres <service>.dump
   ```

   `--create` makes the database with its owner, its grants and its settings
   from the archive. Without it, the database gets the owner that makes it,
   such as `postgres`, and no grant. The service still starts, because its start
   check does not read the owner of the database.
4. Run the migration Job of the release that you start. It checks the roles,
   the grants and the schema again, and it refuses a database that does not
   satisfy the schema manifest. When the dump is of an older release, it also
   migrates the database to the release.
5. For each subscription of the service, set the offsets of its consumer group
   to one minute before `T0` (see below).
6. Start the service: scale the two Deployments back, or start the sync of
   Argo CD again.
7. Do the checks below for the outbox, SpiceDB and the audit sink.

### The offsets of a consumer group

The consumer group of a subscription committed its offsets after `T0`, but the
database lost the messages that the inbox wrote after `T0`. Thus, without a
reset, the service never applies the events after `T0`. With the worker
Deployment stopped, set the offsets of the group to one minute before `T0`, in
UTC:

```sh
kafka-consumer-groups.sh --bootstrap-server <broker> --command-config <client properties> \
  --group <service>.<subscription> --topic <source topic> \
  --reset-offsets --to-datetime 2026-11-18T05:27:00.000 --execute
```

The events of the minute before `T0` come again. The inbox makes the ID of a
message from the subscription, the tenant and the event ID, so it writes
nothing new for an event that it holds. The outbox keeps a done message for 7
days, so an overlap of one minute is safe.

### The outbox

- A message that was pending at `T0` and that the relay sent after `T0` goes
  again. A consumer of the event writes nothing new, for the reason above. A
  write or a delete of a SpiceDB relationship gives the same result again.
- An event of a write after `T0` is in the topic, and the consumers applied
  it, but the source no longer holds the write. STEGO has no tool that finds
  these events. To find them, read the topic of the service from `T0`. The key
  of each record is `<tenant>\x1f<resource type>:<row ID>`. Compare each row ID
  with the database, and tell the owners of the consumers.

### SpiceDB

The tuples of SpiceDB do not go back to `T0`:

- The tuples of a row that the service made after `T0` stay in SpiceDB. The
  database does not hold the row, and the row ID is a new UUID, so the tuples
  give access to nothing. STEGO has no tool that removes them yet.
- A grant or a removal of a grant after `T0` stays in SpiceDB. The database
  does not hold a copy of the grants, so it does not disagree.
- The status of a revocation after `T0` is lost. Its tuple changes are in
  SpiceDB. A client that asks for the status of such a revocation gets 404.

To remove the tuples of a lost row, read them with `zed relationship read
<resource type>:<id>` and delete them with `zed relationship delete`. The
question ST-SUP-Q181 asks for a tool that finds the tuples and the events of
the lost rows.

### The audit sink

The audit rows after `T0` are lost in the database. The audit sink holds the
copies that it got. Keep them as the record of that time.

## Which older releases can run on a newer database

STEGO supports a rollback by one release: the pods of release N-1 on the
database that the migration Job of release N made. `stego compat` holds this
rule when you make release N: it refuses a change that cannot run beside its
base on a database with rows. Nothing checks a rollback of two or more
releases. Such a rollback can start, but a contract step of a later release
can remove what the older release uses.

At start, a pod reads the migration ledger. It accepts a ledger row of a
newer release only when the row is a schema step and the pod holds each
migration that the row needs:

- a step at expand needs no migration, so a pod of an older release accepts
  it;
- a step at contract needs the expand step of the same change, so a pod
  accepts it only when its release has that expand step.

Any other row of a newer release stops the pod at start.

When the newer release has a newer STEGO runtime, the runtime tables and
functions of the outbox, the audit and the authorization can differ. Then each
pod of the older release logs the error `the database does not satisfy the
schema manifest` once for each difference, with its class, such as
`class=owner`. The pod starts and serves. This is expected during a rollback.

Do these steps for a rollback:

1. Deploy only the Deployments of the older release. Do not run its migration
   Job. Its check refuses the newer database, so you cannot deploy a schema
   change of the older release.
2. Roll forward again as soon as you can.
