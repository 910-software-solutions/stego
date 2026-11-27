# Day-2 operations

This guide tells an operator how to rotate the secrets and the keys of a
running STEGO service, how to restore it from a backup, which older release
can run on a newer database, and how to read its logs. Each procedure gives its steps in order
and the reason for the order. To move a service to a newer release, see [the
upgrade guide](upgrade.md).

A process of the service reads each Secret once, at start. A new value of a
Secret reaches a process only when its pod restarts, also with the Component
`secret-files` (see [Secrets as files](delivery.md#secrets-as-files)).

## Read the logs

Each process of a service writes one JSON object on each line. The telemetry
runtime and the failure record of the process write on standard error. The
probe runtime and the console of the log provider write on standard output.
`kubectl logs` shows both.

From runtime v0.195.0 (rest-crud 3.261.0, rpc-service and browser-service
1.261.0), each line of the runtime has one form. A query that reads one
member reads it on each line.
During the rollout to this release, the pods of the old release still write
the old shapes: the fields of an access line at the top level, the fields of
a worker event under `fields` with no prefix, and a timestamp with the offset
of the local zone, such as `-05:00`. Until the last pod of the old release
stops, make each query read both shapes, such as `attributes."http.route"`
or `http.route`, and `attributes."stego.worker"` or `fields.worker`. Compare
the times as instants, not as text. After the old release stops, remove the
old shape from the query.

A line holds these members, in this order:

| Member | Value | On each line |
|---|---|---|
| `timestamp` | The time in UTC, in RFC 3339 with nanoseconds, such as `2026-11-22T07:29:43.822602285Z`. | Yes |
| `severity` | `DEBUG`, `INFO`, `WARN`, `ERROR` or `FATAL`. | Yes |
| `service.name` | The name of the service: the declared name. On the lines of the runtime and of the HTTP telemetry, `OTEL_SERVICE_NAME` takes its place when it is set. A line before the start knows the name gives `unknown_service`. | Yes |
| `service.instance.id` | The identity of the process, a random UUID v4. Each line of one process that has it shows the same identity: the runtime of the service takes the identity of the HTTP telemetry (runtime v0.196.0, rest-crud 3.262.0). The failure record has none. | No |
| `event.name` | The name of the event, such as `worker.run.failed`. | Yes |
| `message` | A fixed text. | Yes |
| `trace_id`, `span_id` | The trace and the span of the line, when it has them. | No |
| `attributes` | The fields of the event, when it has fields. | No |

Each key under `attributes` has the prefix `stego.`, such as
`stego.worker`, or it is a name of the OpenTelemetry semantic conventions for
HTTP and RPC: `http.request.method`, `http.route`,
`http.response.status_code`, `rpc.method`, `rpc.response.status_code` and
`error.type`. A value is a string, a number or a bool. A list, such as the
modes of a failure record, is one string with its names joined by commas.

A line of other code, such as of a fill, has the event name `stego.log`, so
that it cannot pass as an event of the runtime. Each line of the runtime has
an event name of its own.

A sample of each kind of line:

```json
{"timestamp":"2026-11-22T07:54:43.322614534Z","severity":"WARN","service.name":"companies","service.instance.id":"e3dd9d43-92f9-41df-b603-3e1f0f44fc6a","event.name":"http.server.request.completed","message":"HTTP request completed","trace_id":"7ce09775ddd4461b3c5bc81023e6b6ff","span_id":"4ea5b0b83ebc6dcc","attributes":{"http.request.method":"GET","http.route":"/companies/{id}","http.response.status_code":404,"stego.duration_seconds":0.000057969}}
{"timestamp":"2026-11-22T07:54:44.100000000Z","severity":"WARN","service.name":"companies","service.instance.id":"e3dd9d43-92f9-41df-b603-3e1f0f44fc6a","event.name":"rpc.server.call.completed","message":"RPC call completed","attributes":{"rpc.method":"/companies.v1.Companies/Get","rpc.response.status_code":"NOT_FOUND","stego.duration_seconds":0.0012}}
{"timestamp":"2026-11-22T07:55:43.000000000Z","severity":"WARN","service.name":"companies","service.instance.id":"e3dd9d43-92f9-41df-b603-3e1f0f44fc6a","event.name":"http.server.request.completed","message":"the local log dropped lines of this route or method and status in the last window","attributes":{"http.route":"/companies","http.response.status_code":401,"stego.dropped":140}}
{"timestamp":"2026-11-22T07:29:43.822602285Z","severity":"ERROR","service.name":"procurement","service.instance.id":"bf92e4a3-cf54-426b-85c5-f53949eea54f","event.name":"worker.run.failed","message":"A worker run did not commit","attributes":{"stego.worker":"record-jobs","stego.outcome":"rolled_back","stego.cause":"fill_error"}}
{"timestamp":"2026-11-22T07:29:43.823000000Z","severity":"WARN","service.name":"procurement","service.instance.id":"bf92e4a3-cf54-426b-85c5-f53949eea54f","event.name":"command.consumer.run_failed","message":"a consuming worker's run did not commit: record-jobs rolled_back fill_error","attributes":{"stego.worker":"record-jobs","stego.destination":"procurement","stego.event_id":"0190a1b2-0000-7000-8000-000000000001","stego.attempt":3,"stego.outcome":"rolled_back","stego.cause":"fill_error"}}
{"timestamp":"2026-11-22T07:27:14.240366648Z","severity":"INFO","service.name":"procurement","service.instance.id":"bf92e4a3-cf54-426b-85c5-f53949eea54f","event.name":"migrate.step.completed","message":"the step of the migration job completed","attributes":{"stego.step":"tables","stego.outcome":"ok","stego.duration_seconds":0.41}}
{"timestamp":"2026-11-22T07:27:13.205904421Z","severity":"INFO","service.name":"procurement","event.name":"service.listening","message":"the server listens","attributes":{"stego.server":"http","stego.address":"[::]:8080"}}
{"timestamp":"2026-11-22T07:20:01.000000000Z","severity":"ERROR","service.name":"procurement","event.name":"service.failed","message":"Service failed","attributes":{"stego.stage":"service.start.database","stego.modes":"api","stego.fault":"database.connection","stego.object":"database","stego.remedy":"...","stego.cause":"authentication_failed","stego.sqlstate":"28P01"}}
{"timestamp":"2026-11-22T07:29:50.000000000Z","severity":"INFO","service.name":"procurement","service.instance.id":"bf92e4a3-cf54-426b-85c5-f53949eea54f","event.name":"stego.log","message":"a line of a fill"}
```

The lines in order: an access line, the line of a gRPC call, the count line
of dropped access lines, a built-in worker event of the probe, a warning
record of the log provider, a line of the runtime through `log/slog`, the
line of a listener, the failure record of a process, and a line of a fill.

At the default level, `info`, a service writes these lines. The fields are
the keys under `attributes`:

| Event | When | Fields | Severity |
|---|---|---|---|
| `http.server.request.completed` | Each HTTP request, with or without a collector | `http.request.method`, `http.route` (the route template, such as `/orders/{id}`, or `/jobs/{job_id}/status:convert` for a declared command route), `http.response.status_code`, `stego.duration_seconds`, `error.type` for a status of 500 or more, and `stego.error_code`, the fixed code of the answer, for a status of 400 or more, such as `SVC-AUT-001` for a 401 of the authentication, `tenant.not_provisioned` for a 403 of the tenant check, `unavailable` or `internal` (from otel-tracing 1.26.0 and runtime v0.208.0; never a value of the request). The router sets `http.route`, so an answer that comes before the router has no `http.route`, also on a path that a route matches: a 401 of the authentication; a 403 `tenant.not_provisioned` and a 503 of the tenant check, such as when the database takes no connection; a 503 of a process that does not run the api mode; and a 400 or 413 of the request check of a collection body, such as a body that is not JSON, or a missing `Idempotency-Key` on a route that requires the key (walkthrough 27, R-ROUTE; walkthrough 28, R-ROUTE2). A path that no route matches has none too. To find why such a request was refused, read `stego.error_code`. To find such lines of one route, filter on the status and the method. | INFO for a normal status, WARN for 400 to 499, ERROR for 500 or more |
| `rpc.server.call.completed` | Each gRPC call | `rpc.method`, `rpc.response.status_code`, `stego.duration_seconds`, `error.type` for a server failure | INFO for OK, ERROR for a server failure, WARN for other codes |
| `command.refused` | A command that answers 400 to 499, such as a declared refusal | `stego.action`, `stego.resource_type`, `stego.status`, `stego.code` (such as `shipment.not_draft`, `denied` or `invalid`), `stego.field` when the refusal names one | INFO |
| `worker.run.completed` | A worker run that committed: the run of one tenant in a tick, or one consumed message | `stego.worker` | INFO |
| `worker.run.failed` | A worker run that did not commit | `stego.worker`, `stego.outcome`, `stego.cause` (a fixed class) | WARN for the cause `relations_pending`, ERROR for each other cause |
| `service.listening` | The HTTP or the gRPC server listens | `stego.server` (`http` or `grpc`), `stego.address` | INFO |
| `migrate.job.started` | The migration Job starts | none | INFO |
| `migrate.step.started` | A step of the migration Job starts | `stego.step` | INFO |
| `migrate.step.completed` | A step of the migration Job ends | `stego.step`, `stego.outcome` (`ok` or `failed`), `stego.duration_seconds`, `stego.cause` for a failed step that has a fixed class | INFO for `ok`, ERROR for `failed` |
| `migrate.job.completed` | The migration Job ends with no failure | `stego.outcome`, `stego.duration_seconds` | INFO |

A consuming run with the cause `relations_pending` waits for the relations
of a row that the relay did not write yet, such as the row that the message
before made. The outbox dispatches the message again, and a later run
commits, so the line is a WARN and not an alert. A run that waits past the
bound of the wait (10 attempts or 5 minutes) has another cause, such as
`fill_error`, and is an ERROR, as is each run whose message is parked.

A failed migration Job ends with the failure record of the process, which
names the step in `stego.stage`. A start line with no end line names the
step that waits, such as for a lock.

Each database call writes `db.client.operation.completed`. A call that does
not fail is DEBUG, so it shows only at the level `debug`. A call that fails is
ERROR. A request makes several database calls, so the access line of the
request is the INFO line.

### Alert on a full disk or no memory of the database

When the database cannot serve a command now, the command answers 503 with
`Retry-After`, and the runtime writes one WARN line
`command.database.unavailable` for each action and step in 10 seconds. The
line has `stego.action`, `stego.step` and `stego.sqlstate_class`: the class
of the SQLSTATE only, never the full code and never the message. From runtime
v0.202.0 (rest-crud 3.269.0, rpc-service and browser-service 1.269.0):

| `stego.sqlstate_class` | Cause | Action |
|---|---|---|
| `53` | The server has no resources: a full disk (53100), no memory (53200) or too many connections (53300). | Page the database operator. A retry does not help until the operator adds disk or memory, or lowers the load. |
| `57` | The server shuts down or restarts, such as for a failover (57P01, 57P03). | Wait for the failover. Page when the lines continue past it. |
| `08` | A connection was lost or refused by the server. | Look at the network and at the server. |
| `55` | The database takes no new connection, such as after `ALTER DATABASE ... ALLOW_CONNECTIONS false`. | See the runbook line on an interrupted tenant-database delete below. |
| `none` | No SQLSTATE, such as a refused TCP connection or a lost socket. | Look at the network and at the server. |

Make an alert on `event.name` `command.database.unavailable` with
`attributes."stego.sqlstate_class"` `53` a page of its own. A disk or memory
fault does not end when the clients retry, and the 503 lines alone look like
an outage. The volume of the database and the memory of its host also need
alerts of their own in the database platform, so that the operator acts
before the class 53 lines start.

### An interrupted tenant-database delete

The delete of a tenant database (`DeleteDatabase` of `postgres-client`) sets
`ALLOW_CONNECTIONS false` on the database before it drops it. When the delete
stops between the two steps, such as for a lost connection or a stopped
process, the database stays, and it takes no new connection. Each client of
that database then gets 503 with `Retry-After`, with
`stego.sqlstate_class` `55` on the WARN line, until an operator acts. A retry
of the client does not help.

To end it, run the delete again: it reads its ledger, finds the state
`deleting` and continues. Do not set `ALLOW_CONNECTIONS true` by hand on a
database whose ledger row is `deleting`.

No line holds a value of a request: no path value, query, header, body,
token, tenant, principal, resource ID or error text. The route is the
template, never the path. A line also holds no IP address and no user agent
of the client, because these are personal data. To find the source of a
request, such as of a flood of 401 answers, use the access log of the ingress
or the router in front of the service.

The access lines cannot push out the failure lines:

- The WARN access lines of one route and status, and in a budget of their
  own the ERROR access lines, write at most 10 lines in one minute. When the
  minute ends with lines dropped, one line of the same event with the route,
  the status and `stego.dropped`, the count of the dropped lines, follows at
  the end of the minute, also when no later request of that route and status
  comes (from runtime v0.195.0; before, the line came only with the next
  request of the same route and status). The same is true for the method and
  the status of a gRPC call, and for the WARN and ERROR lines of one event
  name on the console of the log provider and of the probe.
- The last 64 places of the local queue (of 256) take only the service events
  and the ERROR lines.
- `worker.run.completed` never takes the last 16 places of the queue of the
  built-in events (of 64), which stay for `worker.run.failed` and the
  service events.

A line that the queue drops is counted, and the line is not written.

### Lower or raise the volume

`STEGO_LOG_LEVEL` sets the lowest severity of the lines on standard output
and standard error: `debug`, `info`, `warn` or `error`. When it has no value,
the level is `info`. Give it in the ConfigMap `<service>-config`, as the other
optional settings. The service and the migration Job do not start with
another value, and the error does not show the value.

- `warn` keeps the access lines of the answers of 400 or more, the failed
  worker runs, the failed steps and each other warning and error. It removes
  the lines of the normal answers, of the refusals, of the worker runs that
  committed and of the steps that end ok.
- `error` keeps only the errors.
- `debug` adds the database calls that do not fail.

The service events, such as `service.ready` and `service.stopping`, are on
the console at each level. The lines of the security class are written at
each level too:

- `audit.sink.entry_conflict` (WARN, `auditsink: the default sink holds
  another entry under the ID of an entry`): a row of the default audit sink
  changed.
- `idempotency.answer.unopened` (WARN, `idempotency: a stored answer does not
  open`): a key of the key ring was removed too early, or a row changed.
- `command.authz.undecided` and `query.authz.undecided` (WARN, `command: the
  authorization provider could not decide` and `query: the authorization
  provider could not decide the read`).
- `command.seal.pass` and `command.reseal.pass` (INFO): a pass of the seal or
  the re-seal of a secret column.
 The export to an OpenTelemetry collector does not
change with the level: a collector filters the records by severity. Keep the
migration Job at `info`, so its log shows each step.

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
`github.com/910-software-solutions/stego/registry/runtime` of its `go.mod`.

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

## Accept an ID token as the bearer (oidc-auth)

From runtime v0.204.0 (rest-crud 3.273.0, rpc-service and browser-service
1.273.0, oidc-auth 0.2.0), a service that signs in with `oidc-auth` can accept
an OpenID Connect ID token as the bearer token. By default the verifier
refuses each ID token: a token that holds `nonce`, `at_hash` or `c_hash` gets
401 (decision ST-SUP-Q218 (b)). To turn it on, state it in `service.yaml`:

```yaml
  auth-provider: oidc-auth
  oidc-auth:
    issuer: https://homelab-idp.example.ts.net
    audience: erp-web
    accept_id_token: true
```

`accept_id_token` is `true` or `false`, and `false` when you do not state it.
No environment variable turns it on, so the declaration alone shows if the
service accepts ID tokens.

### When to turn it on

Turn it on only for a backend-for-frontend:

- The web client and the API share one client ID at the provider, and the
  `audience` of the service is that client ID.
- The provider gives no JWT access token. For example, tsidp (the OpenID
  Connect provider of Tailscale) gives an opaque access token, and its only
  JWT is the ID token.

When the provider gives JWT access tokens, send the access token and keep the
setting off.

### What the verifier accepts

With `accept_id_token: true`, the verifier checks a token as an ID token only
when the token is marked as an ID token: it holds `nonce`, `at_hash` or
`c_hash`, or the payload `typ` `ID` (a Keycloak ID token). The verifier
accepts a marked ID token only when all of these are true:

- `aud` is exactly the `audience`: a string that is equal to it, or an array
  of one member that is equal to it. An ID token of two or more audiences is
  refused.
- `azp`, when it is in the token, is equal to the `audience`.
- The payload `typ`, when it is in the token, is `Bearer` or `ID`.

A token with no marker is checked as an access token, with the opt-in or
without it: `aud` must hold the `audience`, as a string or as one member of an
array, and `azp` is not checked. Thus an ID token with no marker is accepted
when one of its audiences is the `audience`, also when the provider issued it
to another client as well. The backend-for-frontend MUST send a `nonce` in
every authorization request, so that each ID token holds it and gets the ID
token rules. tsidp, for example, puts `nonce` in the ID token only when the
client sends one, and `at_hash` is optional in the authorization code flow.

Each other check stays: the algorithms of the allow list and the algorithm of
the key, the header `typ` (`JWT`, `at+jwt` or `application/at+jwt`), the
refusal of `crit`, `jku`, `jwk` and `x5u`, the issuer, the expiry, the issue
time and the not-before time with the clock skew, the subject claim and the
tenant claim. A logout token or another security event token (a token that
holds `events`) is always refused. The setting does not change how the
verifier checks an access token.

### The risk

An ID token is for the client that asked for it, not for an API (OpenID
Connect Core 1.0, section 2). The verifier refuses a marked ID token of
another client: an ID token with a `nonce`, `at_hash`, `c_hash` or the payload
`typ` `ID` must name the `audience` alone, and its `azp`, when present, must
be the `audience`. An ID token with no marker gets only the access token
checks (see above). Also:

- Keep the ID token on the server side of the backend-for-frontend: do not
  give it to JavaScript in the browser, and do not send it to another service.
  A service that accepts ID tokens cannot tell an ID token that a user copied
  from the web client from a request of the web client, so treat each ID token
  as a bearer credential of the user for its lifetime.
- RP-initiated logout with `id_token_hint` in the query of a GET request puts
  the ID token in the access logs of the provider and of each proxy, and in
  the browser history. Each copy works as a bearer token for the API until the
  token expires. Use POST for the logout request, or leave out
  `id_token_hint`.
- An ID token cannot be revoked, and a logout does not end it. It stays valid
  for the API until its `exp`.
- The provider sets the lifetime of an ID token for the client, not for an
  API. Keep the lifetime of the ID token short.
- The browser-backend that STEGO generates sends the access token to the API,
  not the ID token. The opt-in is for a backend-for-frontend that is not
  generated by STEGO, such as the web server of erp-demo.

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

From runtime v0.137.0, the service seals each readable secret field (a field
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
derived from the first key, and no plain hash of the body. From runtime
v0.138.0 the key ring also seals the stored answers of the idempotency keys,
and a service with a `pii` field and no `secret` field needs a key ring too,
because an answer can hold the `pii` field. Keep an earlier key in the ring
at least for the idempotency retention after it stops being first: a replay
of an answer that it sealed needs it.

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
   command: re-seal of a secret column entity=Order column=api_token moved=1200 left=0 failed=0 first_key=2
   ```

   `left` is the number of values that are still on an earlier key at the
   end of the run, from a count that takes no lock. A row that a command held
   locked while the run passed it, and a row that a pod of the old ring wrote
   during the run, count in `left` until a later run moves it. `failed` is
   the number of those values that do not open, for example of a key that the
   ring no longer holds. The run logs the first three such rows with their
   column and row, and then one count for the column and the tenant.
3. Remove the old key only when both of these are true:
   - each column logged `left=0` in one complete run of runtime v0.207.0 or
     later that started after both rollouts of phase 2 completed. A run that
     started earlier can miss values that an old pod wrote. A run of a runtime
     before v0.207.0 passed over the active tenants alone, so its `left=0`
     does not count the values of a retired tenant, which stay on the old key
     (decision ST-SUP-Q219 (a)). After an upgrade from such a runtime, wait for
     one complete run of the new runtime before you remove a key;
   - the idempotency retention of the service (24 hours when the declaration
     gives none) has passed since phase 2. An idempotency key of that time
     holds a fingerprint of the old key, and a retry with that key needs the
     old key to match it.

   **Never remove a key while a run logs `left` above 0 for a column.** Each
   such value needs a key of the ring to open. Find why it stays: a value with
   `failed` above 0 needs its key back, or a write of a new value through the
   service.

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

1. Stop the service: each process of each release, also of the release of the
   expand step of a seal. Pause the automatic sync of its Argo CD Application, if
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
   migrates the database to the release. When the dump is from before the
   contract step of a seal, the Job refuses at that step until the seal job of
   the release of its expand step completes a pass: see [Restore a database
   with an open or finished seal](schema-changes.md#restore-a-database-with-an-open-or-finished-seal).
   A dump from before the seal holds the field in clear. So when the dump is
   from before the expand step or the contract step of a seal, first run the
   release of the expand step: its migration Job and its worker Deployment,
   which runs the controller, for one pass of the seal job. The controller
   runs the seal job when it gets the controller lease, at its start, and then
   each 10 minutes. A run starts a pass only when the checkpoint row of the
   seal is 3 minutes old or older, and no process of a release before the
   step reported itself in its last three report intervals. Else the run
   seals nothing, and the next run is 10 minutes later. The age of the row is
   the time since the migration Job first applied the expand step on the
   database, or since the first run of the seal job saw the row on this
   database instance, whichever is later. The database instance is the
   database, the start of its server, and the table of the seal checkpoints.
   A restore keeps the row of the dump with its time, but not the database
   instance where the job saw it (runtime v0.168.0 or later):
   - When the dump is from after the expand step, the first run after the
     restore records that it saw the row, and waits. The pass runs at the next
     run, 10 minutes later, when no process of the release before reported
     itself in its last three report intervals. Before you start the worker
     Deployment, stop each process of the release before and make sure that
     its Deployments have no pods. With a runtime before v0.168.0, the
     3-minute wait does not apply after a restore, and the stop of the release
     before is the only guard.
   - A pass that completed before the dump does not count after the restore
     (runtime v0.172.0 and postgres-adapter 6.21.0 or later). The first run
     after the restore ends that pass and waits, and the next run does a new
     pass. Until the new pass completes, the migration Job of the contract
     step refuses with `the last pass of the seal job did not run in this
     database instance`. Thus each restore costs one more pass of the seal
     job.
   - A restore in place, with `pg_restore --clean` and no `--create`, keeps
     the database and the server. It makes the table of the seal checkpoints
     again, so it is a new database instance too, and gets the same wait and
     the same new pass (postgres-adapter 6.21.0 or later).
   - A restore of the data alone, such as `pg_restore --data-only` into the
     tables that are there, keeps the table of the seal checkpoints and its
     file node. So the rows of the dump can look as seen in this database
     instance. Before such a restore, run `TRUNCATE stego_schema.seals`, or
     restart the database server after it. Either one makes a new database
     instance, and the seal job does a new pass.
   - A restart of the database server also makes a new database instance. The
     first run after the restart waits, and the next run does a new pass, also
     when a pass completed before the restart. A `VACUUM FULL`, `CLUSTER` or
     `TRUNCATE` of `stego_schema.seals` has the same effect. When the server
     restarts more often than each 10 minutes, the seal job never starts a
     pass, because each run sees a new database instance and waits. A pass
     completes only when the server stays up for two runs of the seal job, at
     most 20 minutes, and then for the time of the pass.
   - When the dump is from before the expand step, the migration Job of that
     release makes a new row. Wait 3 minutes after the Job before you start
     the worker Deployment, or the first pass runs only 10 minutes later.

   A pass reads 100 rows at a time, with a pause of 50 ms after each batch,
   so its time grows with the number of rows. When the job logs `seal of a
   secret column` with `completed=true`, stop that release and run the
   migration Job of the release that you start again. Only then does that Job
   pass.
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

### Drill a restore beside the live database

A drill proves that the backup and the backup of the key ring open together.
It restores the dump into a second database of the same server, and it does
not stop the service.

1. Make the drill database, owned by the owner role, and restore the dump
   into it with no `--create`:

   ```sh
   psql -v ON_ERROR_STOP=1 -c 'CREATE DATABASE <service>_drill OWNER <service>_owner' postgres
   pg_restore --no-owner --role=<service>_owner --dbname=<service>_drill <service>.dump
   ```

2. Copy the environment of the worker Deployment. Change only the database
   name in `DATABASE_URL` and the other database URLs to `<service>_drill`,
   and set `STEGO_SECRET_KEY_RING_FILE` to the backup of the key ring.
3. Start one process of the release with `--mode=controller` alone, on a
   host or pod that no Service and no client reaches. Do not start
   `--mode=worker`: its relay would send the outbox of the dump to SpiceDB
   and Kafka of the live service.
4. Wait for the line `command.reseal.pass` of each secret column. The
   controller runs the re-seal job at start. The job opens each value that
   is not on the first key of the ring, and seals it again:
   - `failed=0` and `moved` plus `left` equal to the values on earlier keys:
     the ring opens the dump. When each value of the dump is on the first
     key, the job moves nothing. Then put the key of the dump second in a
     copy of the ring with a new first key, and run the drill again.
   - `failed` above 0, with `command.reseal.skipped` for each row that does
     not open (or `command.reseal.unopened` with the count after the first
     three): the ring does not hold the key of those values. The backup of the
     ring is not usable with this dump.

   For a negative proof, run the drill again with a ring that does not hold
   the key of the dump. Each value then counts in `failed`.
5. Stop the process, and drop the drill database:

   ```sh
   psql -v ON_ERROR_STOP=1 -c 'DROP DATABASE <service>_drill' postgres
   ```

The drill process logs `service.schema_manifest.fault` with the classes
`owner` and `trigger`, such as `function stego_outbox.claim has another
owner`. These faults are expected in the drill. `pg_restore --no-owner`
gives each object to the role of `--role`, but the schema manifest wants
some functions owned by other roles of the service, such as the relay role.
The process starts, and the migration Job would refuse that database. The
faults do not change the result of the re-seal job. Do not run the
migration Job on the drill database, and do not use a drill database as the
database of the service: for a real restore, use `--create` as in
[Restore](#restore).

### The offsets of a consumer group

Each subscription has a consumer group of its own, so reset the group of each
subscription of the service. The consumer group of a subscription committed its offsets after `T0`, but the
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

## Provision or retire a tenant on more than one replica

Each API replica keeps the answer of the tenant list for each tenant for at
most 5 seconds (`tenant.MaxGateAge`). The replica that serves a Provision or
a Retire drops its own answer for that tenant after the commit, so it acts on
the change at once. Each other replica can keep its old answer for up to
5 seconds after the commit (erp-demo production run, 2026-11-27):

- After a Provision, another replica can refuse a request of the new tenant
  with 403 and the code `tenant.not_provisioned` for up to 5 seconds.
- After a Retire, another replica can admit a request of the retired tenant
  for up to 5 seconds. A request that it admitted runs to its end, with its
  writes: up to 10 seconds for an HTTP request or a unary gRPC call, and up
  to `STEGO_GRPC_STREAM_TIMEOUT` (5 minutes when it is not set) for a gRPC
  stream.

What to do:

- In a client or a setup script that sends its first requests just after a
  Provision, retry a 403 `tenant.not_provisioned` for up to 5 seconds, with a
  short pause, such as 500 ms, between the attempts. Do not retry it after
  that time: then the tenant is not provisioned.
- Before you tell a customer that a retire is done, wait 5 seconds plus the
  longest request time after the Retire answered: 15 seconds for a service
  with no gRPC stream, else 5 seconds plus `STEGO_GRPC_STREAM_TIMEOUT`
  (security review of PR #14, L4).

## Write a grant in SpiceDB yourself

Most grants come from the service: Provision, a command, or the owner of a new
row. Each of them goes through the outbox and the relay, and its write raises
the revocation floor, the token that each check of the service reads at least.
Thus a client that sends `Authz-Revocation-After` sees the grant at once.

An operator can also write a grant in SpiceDB with `zed relationship create`,
such as the relation `operator` on `outbox_destination:<base64url of the
tenant ID>/<base64url of the destination>`. Such a grant does not raise the
floor. A check reads at least the floor, and SpiceDB can answer from a
revision that is older than the grant. SpiceDB rounds the revision of a read
to its quantization window: the flag
`--datastore-revision-quantization-interval`, 5 seconds by default. Thus:

- The grant can take up to that interval to work. A request in that time gets
  403 or 404, as with no grant.
- A removal that you write in SpiceDB yourself can also take up to that
  interval to stop the access.

Wait for that interval after you write the grant, then try the request again.
To remove an access at once, use the command of the service that removes it:
its revocation raises the floor.

## Which older releases can run on a newer database

STEGO supports a rollback by one release: the pods of release N-1 on the
database that the migration Job of release N made. `stego compat` holds this
rule when you make release N: it refuses a change that cannot run beside its
base on a database with rows. Nothing checks a rollback of two or more
releases. Such a rollback can start, but a contract step of a later release
can remove what the older release uses.

`stego compat` exits 2 with a cause class, and never says `No fault`, when it cannot run the checks of `stego validate` for a cause other than the registry (see [stego compat](compat.md)).

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

A table or a column that the older release uses can be missing, for example
after a contract step of a later release dropped it. Then each statement on
that table fails. In this case the error line says `the service starts not
ready`, with the table or the column, and each mode of the pod answers
`/readyz/modes`, the readiness probe of each Deployment, with 503. The answer
names no table or column, because a caller of the network can read it: read
the log of the pod. The pod does not restart. A rollout of
the older release stops at its first pod, and the pods that serve stay. Roll
forward to the release that changed the database. There is one exception: the
release of the expand step of a seal goes on after the contract step drops
the clear column, so its pods are ready.

Do these steps for a rollback:

1. Deploy only the Deployments of the older release. Do not run its migration
   Job. Its check refuses the newer database, so you cannot deploy a schema
   change of the older release.
2. Roll forward again as soon as you can.
