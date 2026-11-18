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
| `<service>-query-cursor` | `key` | the key of the list cursors | `<service>` (the api mode only) |
| `<service>-spicedb` | `preshared-key` | the key of SpiceDB | `<service>`, `<service>-worker`, the migration Job |
| `<service>-secret-key-ring` | `key-ring` | the key ring of the secret fields, only for a service with a secret field | `<service>`, `<service>-worker` |

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
   a new password. Do not put the password on the command line, where `ps` and
   the shell history show it. Write the setting into a file that only you can
   read, give the file to `kafka-configs.sh`, and then remove the file:

   ```sh
   (umask 077; printf 'SCRAM-SHA-512=[password=%s]\n' "$(cat <file of the new password>)" > scram.properties)
   kafka-configs.sh --bootstrap-server <broker> --command-config <admin properties> --alter \
     --add-config-file scram.properties --entity-type users --entity-name orders-b
   shred -u scram.properties
   ```

   With Strimzi, change the `KafkaUser` resource in place of this command.

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
`kafka-configs.sh --alter --add-config-file`, as in step 1, write it into the Secret, and restart
the worker Deployment at once. The running worker keeps its session. A worker
pod that starts with the old password cannot send or read events until it has
the new value.

## When a credential leaked

A rotation does not close the sessions that are open. PostgreSQL keeps each
connection that authenticated with the old password, and Kafka keeps each
session of the old password or the removed user. A person who has the leaked
credential and an open connection keeps it after the rotation, with no end
time. Thus, when a credential leaked, do these steps after the rotation:

1. When the rollout is complete, close each connection of the login that
   started before the rollout. As a database administrator:

   ```sql
   SELECT pg_catalog.pg_terminate_backend(pid) FROM pg_catalog.pg_stat_activity WHERE usename = '<login>' AND backend_start < '<time of the end of the rollout>';
   ```

   The service opens new connections with the new password. To stop each new
   connection of the login at once, run `ALTER ROLE <login> NOLOGIN` first, and
   `ALTER ROLE <login> LOGIN` after the rotation. `NOLOGIN` does not close a
   connection that is open; only the `pg_terminate_backend` above does.
2. For Kafka, make sure that each broker sets `connections.max.reauth.ms`, such
   as 3600000. Then a session must authenticate again within that time, and a
   session of a removed credential ends. If the brokers do not set it, do a
   rolling restart of the brokers after the rotation.

## Rotate the OIDC signing key

The API of a rest-crud or an rpc-service service verifies a token with the key
set of the issuer, from `STEGO_AUTHN_KEYS_URL`. Each pod keeps a copy of the
key set, and it loads the key set again in these cases:

- A token names a key ID (`kid`) that the copy does not hold. The pod loads the
  key set before it answers. It loads at most once in 30 seconds (the
  cooldown). A token of an unknown key in the cooldown gets 401.
- From runtime v0.134.0: the refresh age of the copy is the `max-age` of the
  `Cache-Control` header of the key set, bounded to 1 to 5 minutes, and 5
  minutes when the issuer sends none. From half the refresh age, a request
  starts a load in the background. At the refresh age, a token of a known key
  waits for the load. Thus a removed key stops within the refresh age, plus
  the load timeout of 5 seconds.
- Before runtime v0.134.0: when the copy is older than 5 minutes, the next
  request still uses the copy, and it starts a load in the background. When
  no load succeeded in 15 minutes, the pod refuses each token.

The runtime version of your service is in the line
`github.com/jsell-rh/stego/registry/runtime` of its `go.mod`.

A browser service loads the key set of the issuer again when a token names an
unknown key ID.

Do the steps in this order (OpenID Connect Core 1.0, section 10.1.1, and RFC
7517):

1. Publish the new key in the key set of the issuer, beside the old key. Do
   not sign with it yet.
2. Wait until each replica and each cache of the issuer serves the new key
   set. Then wait the refresh age: from runtime v0.134.0, the `max-age` of
   the key set (5 minutes when the issuer sends none); before it, 6 minutes.
   Then each pod that gets requests has a copy with the new key, and no
   request waits for a load. A token of an unknown key, also one that a
   stranger sends, starts the cooldown, so do not wait less.
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

A pod accepts a token of a removed key until its copy reaches the refresh age:
from runtime v0.134.0, at most the `max-age` of the key set plus 5 seconds;
before it, until the copy is 5 minutes old and a request loads the key set
again, and up to 15 minutes when the loads fail. Thus a removal is not a
revocation. When a key leaked:

1. When the leaked key is the key that signs now, publish a new key and sign
   with it at once (steps 1 to 3 with no wait), so that the removal does not
   stop the new tokens.
2. Remove the leaked key from the key set.
3. Restart each Deployment that verifies tokens, so that each pod loads the key
   set at start. The worker pods serve the same routes as the API pods, so
   restart them too:

   ```sh
   kubectl rollout restart -n <namespace> deployment/<service> deployment/<service>-worker
   ```

   A browser service loads the key set again only on an unknown key ID, so
   restart its Deployment `<service>` too.

When the service reads the key set from the file `STEGO_AUTHN_KEYS_FILE`, it
reads the file only at start. Restart the pods after step 1 and after step 5.

## Rotate the cursor key

A list gives a cursor for its next page. The cursor is sealed with the key of
`<service>-query-cursor`. A pod refuses a cursor that no key of the pod
opens, with 400 and the detail `the cursor is not valid`.

### From runtime v0.134.0: three rollouts, with no 400

The api mode also reads the optional variable
`STEGO_QUERY_CURSOR_EXTRA_KEY_FILE`: a file of a second key, which opens
cursors and seals none. It is a plain path variable with no secret source,
so mount the file yourself, for example from a second key of the same
Secret, and set the variable in the ConfigMap. Make each key as in step 1 of
the next section. Then do three rollouts of the api Deployment, and wait for
each one to complete:

1. Give the new key as the extra key. Each pod then opens the cursors of both
   keys and seals with the old key.
2. Give the new key as the key, and the old key as the extra key. Each pod
   seals with the new key and still opens the cursors of the old key.
3. When no client holds a cursor of the old key, for example one hour later,
   remove the extra key and its variable.

Why this order: in each rollout, the old pods and the new pods open the
cursors that the other pods seal, so no client gets 400.

### Before runtime v0.134.0: change, then roll

Each pod has one key. Do these steps:

1. Make a new key of at least 32 random bytes in a file that only you can
   read, write it into the key `key` of `<service>-query-cursor`, and remove
   the file:

   ```sh
   (umask 077; head -c 32 /dev/urandom > cursor-key)
   kubectl create secret generic -n <namespace> <service>-query-cursor --from-file=key=cursor-key \
     --dry-run=client -o yaml | kubectl apply --server-side --force-conflicts -f -
   shred -u cursor-key
   ```

   When a secret operator makes the Secret, write the key into its source
   instead.
2. Restart the api Deployment, which reads the key:

   ```sh
   kubectl rollout restart -n <namespace> deployment/<service>
   ```

During the restart, a cursor of an old pod can go to a new pod, and the reverse.
The pod then answers 400. The client must start the list again, with no
cursor. Write each client so that it starts a list again when a cursor gets
400. A list is not a snapshot, so a client must also accept rows that change
between two pages.

From runtime v0.134.0, use the three rollouts above instead.

## Rotate and back up the key ring of the secret fields

From runtime v0.135.0, the service seals each readable secret field (a field
with `sensitivity: secret`) with AES-256-GCM before it writes the row. The
column holds the sealed bytes, never the value. A compare-only secret field
(`compare_only: true`) holds an Argon2id hash, which no key opens.

**If you lose every key of the key ring, you lose each readable secret
field. Nothing can bring the values back.** A backup of the database without
the key ring holds no secret that you can read. Keep the key ring in a
backup of its own, apart from the database backup.

### The key ring

The Secret `<service>-secret-key-ring` holds the key `key-ring`: a text file
with one key on each line, in the form `<key ID> <key>`. The key ID is a
number from 1 to 4294967295. The key is the base64 of exactly 32 random
bytes. Lines that start with `#` and empty lines are comments. The first key
seals each new value. Each key of the file opens a value that it sealed.

The api, worker and controller modes read the file at start, from
`STEGO_SECRET_KEY_RING_FILE`. They do not start without it. The migration Job
does not read it. Mount the file with the Component `secret-files`, which
gives it the mode 0400 (see [Secrets as files](delivery.md#secrets-as-files)).
A process refuses a file that others can read or that a group or others can
write. A file that the group can read, such as 0440 or 0640 for an `fsGroup`,
is accepted.

The key ring also keys the fingerprints of the idempotency keys of the
service, because a request body can hold a secret. The table
`idempotency_keys` holds an HMAC of each fingerprint with a key that is
derived from the first key, and no plain hash of the body.

Make the first key ring in a file that only you can read, write it into the
Secret, and remove the file:

```sh
(umask 077; printf '1 %s\n' "$(head -c 32 /dev/urandom | base64 -w0)" > key-ring)
kubectl create secret generic -n <namespace> <service>-secret-key-ring --from-file=key-ring=key-ring \
  --dry-run=client -o yaml | kubectl apply --server-side --force-conflicts -f -
```

Then put a copy of `key-ring` in your secret store or your encrypted backup
before you remove it with `shred -u key-ring`.

### Back up the key ring

- Back up the key ring each time you change it, before the first rollout of
  the change. Keep each old copy until no backup of the database that needs
  it is left.
- Keep the key ring apart from the database backup: in a secret store, or in
  an encrypted backup with other access. If the same backup holds both, a
  person who gets the backup gets the secrets.
- At a restore of the database, give the service a key ring that holds each
  key of the rows of the restored dump. A ring of today holds them when no
  rotation removed a key after the time of the dump. Otherwise, add the old
  key from its backup copy, after the first key.

### Rotate the key ring

Rotate the key at least once a year, at once after a leak of the key ring,
and before one key seals about 4 billion values. Do the rotation in three
phases, in this order. In each phase, write the file into the Secret, back
it up, and restart both Deployments, which read the key ring:

```sh
kubectl rollout restart -n <namespace> deployment/<service> deployment/<service>-worker
```

Wait until both rollouts complete before you start the next phase.

1. Add the new key, with a new key ID, as the second key. Keep the old key
   first:

   ```text
   1 <old key>
   2 <new key>
   ```

   Each pod still seals with the old key, and each pod can now open the
   values of both keys.
2. Put the new key first:

   ```text
   2 <new key>
   1 <old key>
   ```

   Each new pod seals with the new key. An old pod that still runs during
   this rollout opens these values too, because it got the new key in phase
   1. The controller runs the re-seal job at start and then each 10 minutes.
   It seals each value of an earlier key again with the first key, in pages
   of 100 rows for each tenant. It does not change the version of a row, and
   it writes no audit row and no event. Each run logs one line for each
   secret column, with no value:

   ```text
   command: re-seal of a secret column entity=Order column=api_token moved=1200 left=0 first_key=2
   ```

   A value that does not open, for example of a key that the ring no longer
   holds, is logged with its column and row and left as it is; it counts in
   `left`.
3. Remove the old key when both of these are true:
   - each column logged `left=0` in a run after phase 2;
   - the idempotency retention of the service (24 hours when the declaration
     gives none) has passed since phase 2. An idempotency key of that time
     holds a fingerprint of the old key, and a retry with that key needs the
     old key to match it.

Why this order: in each phase, each pod that runs opens each value that any
other pod seals, so no read fails during a rollout. The job moves each value
before you remove its key. If you remove a key too early, a read of a value of
that key fails with 500, and the job logs the column and the row that do not
open. Put the old key back to repair it.

A secret field cannot be renamed or retyped, because each sealed value is
bound to its column. Checking refuses such a schema change (ST-DECL-INV101).

### When the key ring leaked

Rotate the key ring as above at once. Then think about which values a person
with the old key could read: each readable secret field that the old key
sealed, in each backup of the database since that key was first in the ring.
Change those secrets where they are used, for example the tokens of a
partner. A compare-only secret is a hash, and the key ring does not open it.

## Restore from a backup

### What the backup must hold

Back up two things. Keep them encrypted, as secrets, because they hold the
roles and the data of the service:

- The roles of the server, once and after each change of a role, with no
  password. A password in the file would bring back an old password at a
  restore, also one that you changed after a leak:

  ```sh
  (umask 077; pg_dumpall --roles-only --no-role-passwords --file=roles.sql)
  ```

  Encrypt the file at rest, for example with your backup tool or with `age` or
  `gpg`, and remove the clear file.

- The database of the service. Write down the time at which the dump starts.
  The dump holds the data of that time, `T0`:

  ```sh
  (umask 077; pg_dump --format=custom --file=<service>.dump <service>)
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

2. Only when you restore on a new, empty server: make the roles first, and
   stop at the first error. Then give each login its password from its current
   Secret with `\password <login>` in `psql`, or do [Rotate a database
   password](#rotate-a-database-password) for each login:

   ```sh
   (umask 077; grep -v '^CREATE ROLE postgres;$' roles.sql > roles-new-server.sql)
   psql -v ON_ERROR_STOP=1 --file=roles-new-server.sql postgres
   ```

   `pg_dumpall` writes `CREATE ROLE` also for the role that it connects as,
   normally `postgres`, which the new server already has. The `grep` removes
   that line, so that the first statement does not stop the run. If you dump
   as another role, remove its `CREATE ROLE` line in the same way.

   Do not run `roles.sql` on a server that still runs: the roles are there, and
   the file would change their memberships back to the time of the backup.
3. Restore the archive with `--create`. On the same server, first move the
   damaged database aside. On a new server, the database is not there:

   ```sh
   psql -v ON_ERROR_STOP=1 -c 'ALTER DATABASE <service> RENAME TO <service>_damaged' postgres
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
6. List the messages of the destination `authz` that are not done, and review
   each one before the service starts (see [The outbox](#the-outbox)).
7. For a browser service, delete the sessions, so that each user logs in
   again. The restored table holds the sessions of `T0`, also the sessions that
   a user logged out of, or that you ended, after `T0`:

   ```sql
   -- Connected to the database <service>, as the owner role:
   DELETE FROM public.stego_browser_sessions;
   ```

8. Deny each request to the worker pods, then start the worker Deployment
   only. Its relay sends the messages of step 6. When none of them is pending,
   remove each relationship that you wrote down in step 6 (see [The
   outbox](#the-outbox)), and then remove the policy. The worker pods serve the
   same routes as the API pods, and no Service selects them, but a client on
   the pod network can reach them. The kubelet probes are not blocked by this
   policy on most network plugins; check yours:

   ```yaml
   apiVersion: networking.k8s.io/v1
   kind: NetworkPolicy
   metadata:
     name: <service>-worker-restore
     namespace: <namespace>
   spec:
     podSelector:
       matchLabels:
         app.kubernetes.io/name: <service>-worker
     policyTypes: [Ingress]
   ```

9. Start the api Deployment: scale it back, or start the sync of Argo CD again.
10. Do the checks below for the outbox, SpiceDB and the audit sink.

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

- An event that was pending at `T0` and that the relay sent after `T0` goes
  again. A consumer of the event writes nothing new, for the reason above.
- A message of the destination `authz` that was pending at `T0` goes to
  SpiceDB again. Its write has no precondition. Thus, when a grant was pending
  at `T0`, the relay wrote it after `T0`, and a person then removed it, the
  message writes the grant again: the removal is lost, and an access that you
  removed comes back. Before the service starts (step 6), list these messages:

  ```sql
  -- Connected to the restored database <service>, as a database administrator:
  SELECT id, tenant_id, created_at, convert_from(payload, 'UTF8') AS change FROM stego_outbox.messages WHERE destination = 'authz' AND state <> 'done' ORDER BY sequence;
  ```

  Each `change` names the operation and the relationship. For each one,
  compare it with SpiceDB (`zed relationship read`) and with the audit sink,
  which holds the commands after `T0`. Write down each write (`touch`) of a
  grant that a command removed after `T0`. In step 8, after the relay sent the
  messages and before the API starts, remove each such relationship again with
  `zed relationship delete`, and check the result with `zed permission check`.
  The API is still stopped, and the policy of step 8 denies each request to
  the worker pods, so no request uses the grant in that time. STEGO
  has no tool that discards a pending message while the service is stopped;
  the question ST-SUP-Q181 asks for one.
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
