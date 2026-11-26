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
`github.com/910-software-solutions/stego/registry/runtime` of its `go.mod` names.

## The procedure

Do these steps for each upgrade. Do them in this order.

1. Find the archetype version that your service runs now. Read each release
   note below whose version is after it and at or before the target version.
   Do the action of each note in the order of the table.
2. Take a backup of the database of the service. See [Restore from a
   backup](operations.md#restore-from-a-backup) for what the backup must hold.
3. Make the new release of the service. Use the new `stego` binary on the
   declaration of the release that runs now, and give `--base` the commit or
   tag of that release. Run the commands in this order:

   ```sh
   stego test
   stego fill sign <fill>
   stego validate
   stego compat --base <tag of the running release>
   stego plan --base <tag of the running release>
   stego apply --base <tag of the running release>
   stego deps
   ```

   From STEGO 3.269.0, `stego compat` exits 2 with a cause class when it cannot run the checks of `stego validate` for a cause other than the registry; run `stego validate` to see the cause.

   Before `stego apply`, `stego compat` can warn that `migrate.go`, or its
   digest in `.stego/state.yaml`, `is not what a new generation of the head
   gives`. In an upgrade, this warning is expected: the new `stego` generates
   other files than the release before, and `stego apply` writes them. compat
   checks the declaration and the committed files against the base all the
   same. Run `stego compat --base <tag of the running release>` again after
   `stego apply` and the commit, and the warning goes.

   The SIGNERS check of `stego validate` and `stego plan` reads the `SIGNERS`
   of `--base`. With the tag of the running release, each fill that a key
   signed which a reviewed change added to `SIGNERS` after that release gets a
   warning, and `--strict` refuses it. Thus in a release pipeline, run the
   SIGNERS check against the commit of the default branch that you release,
   whose `SIGNERS` each pull request checked and merged:
   `stego validate --strict --base origin/main`. Give `stego plan --base`
   and `stego apply --base` the tag of the running release, for the schema
   check, and no `--strict`: their SIGNERS warnings then name the keys that
   were added after that release, which the pull requests checked. See
   [Who signs the fills](../README.md#who-signs-the-fills).

   Run `stego test` first. A new STEGO release can change the contract of a
   fill, such as the command contract, also when you do not change the fill.
   Then the evidence of the fill is stale, and `stego validate`, `stego plan`
   and `stego apply` refuse it: `fill "<fill>" is stale ... the contract
   command.command changed after its evidence; run stego test, then stego
   fill sign <fill>`. `stego test` runs the tests of each fill on the new
   contracts and records new evidence. Review each fill, its diff and the
   change of its contract, then run `stego fill sign <fill>` for each fill
   whose evidence `stego test` recorded. When a test does not
   compile, such as with `too many arguments in call to`, do the action of
   its release note (step 1) and run `stego test` again. When `stego plan`
   says that the declaration needs add steps and saved no plan, run `stego
   apply --base` without `--saved-plan`. See [Schema changes](schema-changes.md). When a
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
   - The parked list of the destination `authz`, when the release adds a
     relation to `authz/schema.zed` (see [Resolve the parked messages of the
     destination authz](#resolve-the-parked-messages-of-the-destination-authz)).
7. If you must go back, go back by one release only. See [Which older releases
   can run on a newer database](operations.md#which-older-releases-can-run-on-a-newer-database).

Do not skip a release that holds a contract step of a schema change. `stego
compat` compares your change with the base that you give it. It cannot see a
release that you did not deploy.

## The clients of the release before

During the rollout, the release before and the new release serve the same
routes. After the rollout, the clients that you built for the release before
still call the service until you build them again. This section tells what
such a client gets from the new release. In walkthrough 18, the HTTP and the
gRPC clients of a release with runtime v0.138.0 worked against a release with
runtime v0.165.0, except for the cases below.

- **An idempotency key that the release before stored.** A request again
  with the same key, method, path and body gets the stored answer while the
  service keeps the key, 24 hours by default. A request with the same key and
  another path or body gets 422 with the problem type
  `idempotency-key-reused`. A request with the same key and the same body
  also gets 422 when the new release makes the fingerprint of the request in
  another way: when the release before had no key ring and the new release
  has one, such as from 3.189.0 for a service with a `pii` field or from
  3.211.0 for a service with a `debug_redact` member, or when the key ring no
  longer holds the key that made the fingerprint. The answer carries
  `Idempotency-Kept: 1` from 3.216.0. After a 422, do not send the key again.
  Read the state of the resource, and send a new write with a new key only
  when the first write was not done.
- **A field that a retype changes.** The new release reads and writes the
  field in its new JSON type, such as `"7"` in place of `7`. It refuses a
  write of the old type with 400 and the problem type `validation-error`,
  which names the field. A client must read both types while the two
  releases run, and write the new type only to the new release. A require
  also refuses a create that does not send the new field. See [Retype and
  require](schema-changes.md#retype-and-require).
- **A member that the entity does not take.** From 3.227.0, a create, an
  update and an upsert refuse such a member, such as a field that a drop
  removed, a typo or a name in another letter case, with 400 and the problem
  type `validation-error`. The problem names the member, and the write
  changes nothing. From 3.215.0, a patch refuses such a member in the same
  way. Before 3.227.0, a create and an update ignored the member and answered
  as for a body without it: a create gave 201, and the value was lost. See [A
  client that still sends the dropped field](schema-changes.md#a-client-that-still-sends-the-dropped-field).
  A command refuses such a member in its body too, with 400 and the problem
  type `bad-request`, and runs nothing. From 3.245.0, the member `field` of
  the problem names the member, such as `carrier` or `address.zip`, as for a
  collection write. Before 3.245.0, the problem of a command did not name the
  member: look for the member in the body that the client sent.
- **An idempotency key that is not a structured string.** A request whose
  `Idempotency-Key` header is there, but is not one structured string of 1 to
  255 characters, gets 400 with the problem type `idempotency-key-invalid`,
  and the write changes nothing. A gRPC command gives the message
  `idempotency-key-invalid`. The release that gives it depends on the
  request:
  - A command, and a collection write of a service with an `idempotency`
    setting: from 3.235.0. Before, such a key got `idempotency-key-missing`,
    which is now only for a request with no header.
  - A collection create or patch of a service with no `idempotency` setting:
    from 3.242.0, and a key used with another body gets 422 with
    `idempotency-key-reused`. Before 3.242.0, the problem type of both was
    `about:blank`.
  - A collection create or patch whose body is not valid too: from 3.243.0
    the key is checked first. Before, the body check answered first, with
    `validation-error`.

  For a collection write, the member `code` of the problem does not change:
  it stays `<SVC>-VAL-001` for the 400 and `<SVC>-INT-001` for the 422. The
  400 and the 422 of the key of a command have no member `code`, in the
  release before and in the new release: a client of a command reads `type`.
  Send the key in double quotes, such as `"k-1"`. A client of the release
  before that reads only `idempotency-key-missing` or `about:blank` must also
  read the new types; a client of a collection that reads only `code` sees no
  change.
- **A repeated apply of the CLI.** From 3.235.0, an apply of the same
  document, whose values the service already stores, reports `unchanged` and
  sends no patch. A CLI of the release before reports `configured` and sends
  the patch. A script that reads the status of an apply must accept both.
  A CLI before cli-application 1.25.0 (rest-crud 3.238.0) can also make a
  second resource of the same name: its lookup does not wait for the
  permission of the resource that the first apply made, so an apply of the
  same document soon after the first does not find the resource and creates
  it again. In walkthrough 20, a CLI of cli-application 1.24.0 applied one
  document two times and made two rows. Build the CLI again from the new
  release. Until then, run an apply of a new document only once, and use
  `get` to see the resource before you apply the document again. A service
  that holds the name unique refuses the second create with 409.
  A CLI before cli-application 1.30.0 (rest-crud 3.258.0) still fails a
  second apply of a document that holds a secret field, such as with
  `apply cannot change the field serial_ref`: it compares the secret field,
  whose stored value can differ from the document, and puts it in the
  write. The new release of the service does not change this, as the fault
  is in the CLI.
  In walkthrough 25, a CLI of the release before failed in this way against
  the new release. Build the CLI again from the new release: from 1.30.0,
  `apply` leaves each secret field of a resource that exists out of the
  patch, and `--set-secret FIELD` sends a new value.
- **An entity that a drop removes.** From the expand step of the drop, the
  new release serves no route of the entity and its collections, so a
  request to such a route gets 404. The release before still serves them
  until it stops. See [Remove an entity](schema-changes.md#remove-an-entity).
- **A generated client of the release before.** The CLI and the SDKs hold the
  OpenAPI document of the release that you built them from. Thus they do not
  know a later change of the document:
  - A CLI before cli-application 1.17.0 does not read `Idempotency-Kept`. It
    sends a write again after a 5xx answer by the flag of its own document.
  - A CLI that you built before a member had the mark `debug_redact` gives
    that member a flag, which puts the secret in the shell history and the
    process list. A CLI that you built before a command was Sensitive writes
    its answer to standard output.
  - A client that you built before a retype sends and reads the old JSON
    type of the field.
  - A TypeScript SDK before typescript-sdk 1.4.0 (rest-crud 3.245.0) never
    sets `SDKError.apiCode` for the refusal of a command, which answers
    `application/problem+json`, and refuses an error code with a dot in
    `error_codes`, such as `shipment.not_draft`. Such a client reads only
    `status` and `code` of the error. Build it again, and list the codes that
    the UI needs under `overrides.typescript-sdk.error_codes`.
  - A TypeScript SDK before typescript-sdk 1.5.0 (rest-crud 3.250.0) gives
    only `status`, `body` and `etag` in its result. It does not give
    `authzRevocationLocation`, so a page cannot send `authzRevocationAfter`
    on a write right after a create, and such a write can get 404 for about
    one second. Build it again (see [the README of the
    component](../registry/components/typescript-sdk/README.md)).
  - A TypeScript SDK that you built before the expand step of a seal fails
    each read and each list of the entity with `invalid_response`, because
    its schema requires the sealed field, and the new release does not give
    it. A TypeScript SDK that you built from the new release fails in the
    same way against the release before, which still gives the field. The Go
    SDK and the CLI keep reading in both cases. So build the TypeScript SDK
    and the browser service that holds it again from the release of the
    expand step, and deploy the browser service only after the rollout of the
    expand step, when the release before serves no more requests. Between the
    two, the browser pages that read the entity show an error: plan the two
    deploys close together, such as in one maintenance window.

  Build the CLI and the SDKs again from the OpenAPI document of each new
  release, and give the new clients to their users before you stop the
  release before. A CLI of the new release that calls the release before
  writes a warning to standard error when an answer has no
  `Idempotency-Kept` header. The warning changes no answer and no exit code.
- **A gRPC client.** A gRPC answer has no `Idempotency-Kept` metadata. After
  a failure, send the command again only with the same key: the service then
  gives the stored answer, or runs the command once.

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
| 3.211.0 | 1.211.0 | 1.211.0 | rest-api 4.22.0, cli-application 1.13.0, command 1.16.0 and runtime v0.158.0: a member of a command message that the proto file marks `[debug_redact = true]` is a secret. The OpenAPI document gives it `x-sensitive: true`. The generated CLI gives it no flag: give it in `--body FILE`. A command whose answer holds such a member writes its answer only to `--output-file FILE`. A service with such a member reads the key ring, which keys the fingerprints of its idempotency keys and seals their stored answers. `stego validate` gives the warning `unmarked-secret-member` for a member of a command request that has the name of a secret field of an entity that the command writes and has no mark. The warning does not stop validate, plan or apply. | Mark each secret member of a command request and response, such as a PIN, with `[debug_redact = true]` (see [A secret member](commands-queries-workers.md#a-secret-member)). A service with no `secret` and no `pii` field then needs the key ring: make it as for a `pii` field (see [Make the key ring of a service with a pii field](#make-the-key-ring-of-a-service-with-a-pii-field)). Build the CLI again, and give each secret member in a body file. |
| 3.214.0 | 1.214.0 | 1.214.0 | rest-api 4.23.0, command 1.17.0, cli-application 1.15.0 and runtime v0.160.0. The runtime refuses to serve a command with a secret member when the service has no key ring. Generation refuses a route whose path parameter is a secret member. A command whose answer holds a secret member at any depth is Sensitive. A Sensitive command refuses `--output-file -`. The CLI refuses a `--body FILE` that holds a secret member when the group or other users have access to the file. | Make the key ring before you deploy a release with a secret member. Give a secret member in the body, not in the path. Use `chmod 600` on each body file that holds a secret. |
| 3.215.0 | 1.215.0 | 1.215.0 | rest-api 4.24.0: a patch refuses each body member that is not a patchable field, `kind`, or a field that a collection write cannot change. The names must match in letter case: a member such as `Title` for the field `title` is refused. The answer is a 400 validation problem that names the member, and the patch changes nothing. Before, a patch ignored such a member and answered 200. The OpenAPI document gives each `<Collection>PatchRequest` with `additionalProperties: false`. cli-application 1.16.0: an apply of the same documents again leaves out each create-only field whose value is the stored one. | Send in a patch only the fields that `patchable` names, in the letter case of the OpenAPI document, and after a rename the API name of the field. Build the clients again from the new OpenAPI document (see the operations of a collection in [Quick start](../README.md#quick-start)). |
| 3.216.0 | 1.216.0 | 1.216.0 | idempotency 0.2.0, command 1.18.0, rest-api 4.25.0 and 4.26.0, cli-application 1.17.0 and 1.18.0, go-sdk 2.2.1, typescript-sdk 1.2.3, http-application 2.3.1 and runtime v0.161.0. Each answer to a create, a patch or a command whose request carries an `Idempotency-Key` that the service keeps carries the header `Idempotency-Kept: 1`, also a problem and a 5xx answer. The header is for HTTP only: a gRPC answer has no such metadata. The generated CLI sends a write again after a 5xx answer only when the answer carries the header. A 2xx answer with no header to a write whose key the CLI expects kept gives a warning on standard error, because the CLI and the service can be of different releases. The OpenAPI document marks the root of a command answer `x-stego-opaque` when the message holds a `google.protobuf.Any`, `Struct`, `Value` or `ListValue` member at any depth. The CLI makes such a command Sensitive: its answer goes only to `--output-file FILE`. A query answer gets no such mark. The CLI refuses a token file, a configuration file and a secret body file that another user owns. | Build the CLI and the SDKs again from the new OpenAPI document. Make sure that no proxy in front of the service adds `Idempotency-Kept`: the CLI trusts the header to send a write again. Give a command with an opaque answer `--output-file FILE`. Never put a secret in a query answer: a call that makes a secret, such as an API key, is a command with a `debug_redact` member. Make the CLI user the owner of its token, configuration and body files. |
| 3.220.0 | 1.220.0 | 1.220.0 | grpc-application 2.4.0, cli-application 1.20.0 and runtime v0.163.0. Only the api Deployment gets `STEGO_GRPC_TLS_CERT` and `STEGO_GRPC_TLS_KEY` and mounts the Secret `<service>-grpc-tls`, which holds the private key of the gRPC server. The worker Deployment and the migration Job no longer get them. The two variables are not required on their own in the platform requirements: they are required when the process serves gRPC, so `stego env check` does not name a missing path of them. `APPLY_CONFIG`, the configuration file of the generated CLI, is in no pod, and the platform requirements do not list it. `command.NewToken(size)` gives a fill 16 to 64 random bytes from `crypto/rand` for a secret that the command issues. The contract of each command fill and worker fill now imports the `command` package, so a fill can call it also in a service with no secret field. `crypto/rand` stays out of the imports of a fill. | Remove each overlay patch that gives the gRPC TLS Secret, or `STEGO_GRPC_TLS_*`, to the worker Deployment or the migration Job. Keep the Secret `<service>-grpc-tls`, and give the api pods the paths of its files, or list the Component `secret-files` in the overlay. Remove `APPLY_CONFIG` from the environment file and the overlays, and set it only in the shell of the CLI user. Make each secret that a fill issues with `command.NewToken`, never from an ID, the time or a hash of the input (see [The imports of a fill](fills.md#the-imports-of-a-fill)). When `stego validate` names a fill as stale, run `stego test` and `stego fill sign <fill>` (step 3 of [The procedure](#the-procedure)). |
| 3.226.0 | 1.226.0 | 1.226.0 | Runtime v0.165.0. When the database refuses a prepared statement with SQLSTATE 0A000 `cached plan must not change result type`, such as after a person drops a column and adds it again while the service runs, the runtime makes each connection of the pool prepare its statements again. It runs a command once more in a new transaction, with no wait. A query answers 500 one time, and the next call passes. Thus a fill can run two times in one request. | Make each fill do no effect outside its transaction, such as a call to another system, or make that effect idempotent: send each change to another system as an effect through the outbox (see [Effects](commands-queries-workers.md#effects)). |
| STEGO commit 40f7f991f | STEGO commit 40f7f991f | STEGO commit 40f7f991f | The archetype versions stay 3.226.0 and 1.226.0; grpc-processes 1.8.0. `stego compat` and `stego plan` refuse a drop of a field whose seal is open in the base, and a drop of an entity that has such a field. The fault names the seal and gives its contract step as the change to declare. A drop of a field that the base stores secret, whose `definition` gives the field in clear, gets a fault that tells you to add the item `secret` to the definition. Another field of the entity can drop while the seal stays open. `stego env check` prints a notice for a path variable of the api mode that names the file of a secret, such as `STEGO_GRPC_TLS_CERT`, when the environment file gives it no value. The notice is not a failure. | Contract the seal first, with the step `{change: <seal>, phase: contract}` in a release of its own, then declare the drop in a later release (see [The contract step of a seal](schema-changes.md#the-contract-step-of-a-seal)). In the definition of the drop of a sealed field, add the item `secret` before a pattern. For each notice of `stego env check`, give the path of the file in the environment file, or list the Component `secret-files` in the overlay. |
| 3.227.0 | 1.227.0 | 1.227.0 | rest-api 4.27.0: a create, an update and an upsert refuse each body member that is not a field of the entity by its API name, `kind`, `id` or `href`. The names must match in letter case: a member such as `Title` for the field `title`, a typo, the declared name of a renamed field or `tenant_id` is refused. The answer is a 400 validation problem that names the member, and the write changes nothing. Before, the write ignored such a member and answered 2xx, and the field was not set. A body may still give `id`, `href` and each field that the server sets, as a read gives them: the server ignores these values. A body that gives an implicit field of the collection another value than the value of the collection is refused too; before, the value of the collection replaced it. The OpenAPI document gives each `<Entity>CreateRequest` and `<Entity>UpdateRequest` with `additionalProperties: false`, marks each member that the server sets `x-output-only`, and each member that the body may only repeat `x-kept`. cli-application 1.21.0, go-sdk 2.2.2 and typescript-sdk 1.2.4 take the new document. | Send in a create, an update or an upsert only the fields of the entity, in the letter case of the OpenAPI document, and after a rename the API name of the field. Leave an implicit field out, or give the value of the collection. Build the CLI and the SDKs again from the new OpenAPI document (see the operations of a collection in [Quick start](../README.md#quick-start)). |
| 3.232.0 | 1.232.0 | 1.232.0 | Runtime v0.168.0 and postgres-adapter 6.20.0. The seal checkpoint keeps the database and the start of the database server where the seal job last saw it. After a restore, or a restart of the database server, the first run of the seal job records that it saw the row and waits; a pass that has not started runs 3 minutes later, at the next run. Before, a restore kept the time of the row from the dump, and the 3-minute wait did not apply. A runtime before v0.168.0 does not keep the first sight of the row, so its controller never starts a pass on such a row. | After a restore of a database with an open seal, expect the first pass at the second run of the seal job, 10 minutes after the start of the worker (see [Restore from a backup](operations.md#restore-from-a-backup)). To roll back to a release with a runtime before v0.168.0 while a seal is open, run the migration Job of that release again before you start its worker; else its controller never starts a pass. |
| 3.235.0 | 1.235.0 | 1.235.0 | Runtime v0.171.0, idempotency 0.3.0, command 1.23.0, rest-api 4.31.0 and cli-application 1.24.0. A request whose `Idempotency-Key` header is there but is not one structured string of 1 to 255 characters gets 400 with the new problem type `idempotency-key-invalid`; `idempotency-key-missing` is now only for a request with no header. A gRPC command gives the message `idempotency-key-invalid` for such a key. The create-request schema of the OpenAPI document does not require a field with a declared default, such as `stock` with `default: 0`: a create that leaves it out stores the default. The generated Go SDK thus gives such a member of a create request as a pointer, and the generated CLI no longer requires its flag. The CLI names the missing field when `apply` refuses a document, gives the problem type and the failed fields of a 400 problem, and reports `unchanged` and sends no patch when an apply of the same document finds the stored values. | A client that reads the problem type of a 400 also reads `idempotency-key-invalid`. Build the Go SDK, the TypeScript SDK and the CLI again from the new OpenAPI document; in Go, give a defaulted member of a create request as a pointer, or leave it out to store the default. A script that reads the status `configured` of a repeated apply also reads `unchanged`. |
| 3.236.0 | 1.236.0 | 1.236.0 | Runtime v0.172.0 and postgres-adapter 6.21.0. The database instance of a seal checkpoint is now the database, the start of its server, and the table `stego_schema.seals`. So a restore in place, with `pg_restore --clean` and no `--create`, is a new instance too. In a new instance, as after a restore or a restart of the database server, the first run of the seal job ends the pass of the row, also a completed pass, and waits. The next run does a new pass. The contract step of a seal refuses with `the last pass of the seal job did not run in this database instance` until that pass completes. Before, a completed pass in a dump counted after the restore. A database server that restarts more often than each 10 minutes never gets a pass. The functions of the seal checkpoints are no longer in the body of a migration: each migration Job runs them again with `CREATE OR REPLACE`, so a database where the expand step of a seal ran with postgres-adapter 6.19.0 or 6.20.0 passes the ledger check and gets the new functions. Its ledger rows keep their digests, which this release accepts as earlier digests of the same steps. The first run of the seal job after the upgrade sees each open seal in a new database instance, so each open seal does one more pass. | After the upgrade, after a restore of a database with an open seal, and after a restart of the database server, run the release of the expand step with its controller until the seal job logs `seal of a secret column` with `completed=true` again, and only then run the migration Job of the contract step (see [Restore from a backup](operations.md#restore-from-a-backup)). Keep the database server up for two runs of the seal job, at most 20 minutes, and the time of the pass. You need not contract an open seal before the upgrade. Do not put the contract step of a seal in the first release that you build with postgres-adapter 6.21.0 or later: first build the release of the expand step again with this STEGO, deploy it, and let its seal job complete a pass. Then deploy the release of the contract step. Until then the contract refuses, and its refusal names this remedy. Build that release with 3.242.0 or later: when postgres-adapter 6.14.0 to 6.20.0 wrote the expand step, `stego plan --base` of 3.236.0 to 3.241.0 refuses the build with ST-GEN-INV38 (see the row 3.242.0). On the services of walkthrough 20, `stego plan --base`, `stego apply --base`, `stego compat`, `stego deps` and the build of 3.243.0 accept the expand steps that runtime v0.171.0 (postgres-adapter 6.20.0) wrote, of an open seal and of a contracted seal. |
| 3.238.0 | 1.238.0 | 1.238.0 | cli-application 1.25.0. No kind loses `apply`. After a create that the service answers 409, because an entity holds `name` unique (`unique: true`, or a `unique_composite` of `name` and implicit fields), apply looks the resource up again and reports `unchanged`, or `failed` for other values or a resource that it cannot see. The CLI keeps the `Authz-Revocation-Location` of its last write in its configuration file and sends it in `Authz-Revocation-After` on each apply lookup, so an apply soon after a create finds the resource and makes no second one. It stops sending a kept value that is older than 10 minutes or whose lookup answer is no longer pending, and keeps nothing from a write after a login to another service. When the name is not unique, two clients that apply one new name at the same time can still make two resources; `apply --help` says so. When the patch takes `If-Match`, apply sends the generation that it read. A failed request gives the problem type and each entry in double quotes. Checking refuses a `default` on a field that a collection names in its `scope`. | To close the race of two clients on a new service, declare `unique: true` on the field `name` of the entity; on an entity of the base, declare a unique step of the field from 3.249.0 (see [Unique steps](schema-changes.md#unique-steps)). A script that reads the problem text of the CLI reads the quoted form. Remove the `default` of a scope field. |
| 3.240.0 | 1.240.0 | 1.240.0 | Runtime v0.175.0 and cli-application 1.26.0. `command.NewStruct(fields)` gives a command or worker fill the `google.protobuf.Struct` of a member of its message, with no import of a protobuf package, which the fill allow list refuses. It refuses objects and lists nested more than 64 levels. The generated CLI names the body member that it refuses before a request, such as `invalid request field "Title": the command takes no such member`; before, it said `invalid request field`. `stego fill sign <fill>` keeps a sign-off that already signs the current evidence with a key that `SIGNERS` lists, writes nothing and says so; `--force` signs such a fill again. | In a fill that must give a `Struct` member, call `command.NewStruct` (see [The imports of a fill](fills.md#the-imports-of-a-fill)). A script that reads the text of a refusal of the CLI reads the member name after `invalid request field`. A script that runs `stego fill sign` to sign again a fill whose sign-off is current, such as with another key, gives `--force`. |
| 3.241.0 | 1.241.0 | 1.241.0 | Runtime v0.176.0, command 1.24.0, worker 0.6.0 and rest-api 4.32.0: a worker can declare `reads`, a compare-only secret can have a limit of wrong attempts, and an entity can declare the fields of its change events. The change events of an entity with no `events` do not change. | None. A worker that only reads an entity can move it from `writes` to `reads`; its fill then uses `<Entity>Reader` (see [Who reads a secret](fills.md#who-reads-a-secret)). |
| 3.242.0 | 1.242.0 | 1.242.0 | rest-api 4.33.0 and postgres-adapter 6.22.0; no runtime change. A service whose history holds the expand step of a seal that postgres-adapter 6.14.0 to 6.20.0 wrote upgrades again: `stego plan --base`, `stego apply --base` and `stego compat` accept the earlier body of that step, with the functions of the seal checkpoints in it, because the release registers its digest as an earlier digest of the same migration. Before, they refused it with ST-GEN-INV38. Every other change of a released body is still refused. With no `idempotency` setting, a create or a patch with a malformed `Idempotency-Key` now gets 400 with the problem type `<problem base>idempotency-key-invalid`, and a key used with another body `idempotency-key-reused`, as a command does; before, the type was `about:blank` with the code `<SVC>-VAL-001`. A client that reads the type of these answers sees the new types. When the ledger of a database is not the history of the release, such as a shared preview database that ran the build of a branch, the migration Job logs a line that names the ledger row, the migration and the remedy. `stego test` now exits non-zero when it records no evidence for a fill, such as for a fill with no test file, a refused import, or a folder under `fills/` whose `fill.yaml` is missing or not valid, so a CI step of `stego test` fails then. A project with no fill folder still passes. | No change is needed: run step 3 with `--base <running tag>`, then the migration Job. A client that tells the faults of a key apart by the problem type reads the new types on a create and a patch. For a refused ledger, read the line of the Job and restore or make again the database that it names. When `stego test` fails with no evidence, correct each fill that its line names, or remove a folder under `fills/` that holds no fill, then run `stego test` again. |
| 3.243.0 | 1.243.0 | 1.243.0 | Runtime v0.177.0 and rest-api 4.34.0. A collection create or patch reads the `Idempotency-Key` header before the body, as a command does: a key that is not one structured string of 1 to 255 characters gets 400 with the problem type `idempotency-key-invalid`, also when the body is not valid, and an absent key that the service requires `idempotency-key-missing`. Before, the body check answered first. `worker.run.failed` and `command.consumer.run_failed` give the cause `fill_json`, `fill_parse` or `fill_canceled` for a fill error of `encoding/json`, of `strconv` or `time`, or of a canceled context, read from its type and never its text; `fill_error` stays for another error. The failure record of a migration Job step that SpiceDB failed gives the cause class of its status code, such as `unavailable`. | A client that sends a malformed key and a body that is not valid now reads `idempotency-key-invalid` first. A dashboard or an alert that counts `worker.run.failed` with the cause `fill_error` also counts `fill_json`, `fill_parse` and `fill_canceled`. A fill that must tell an operator more logs its own context with `log/slog`, names and counts only (see [A failed run of a consuming worker](commands-queries-workers.md#a-failed-run-of-a-consuming-worker)). |
| 3.244.0 | 1.244.0 | 1.244.0 | Runtime v0.178.0 and postgres-adapter 6.23.0. Each process opens a small pool of its own, of at most 4 connections, for the charges of checks of a compare-only secret with `attempts`; a fill that holds the last connection of the main pool no longer makes its check wait 5 s and answer 503. A rerun of a command in the same request, such as after a conflict, does not count again the checks that an earlier attempt counted. `sealed.Compared.Stored` gives the error `sealed.ErrCounted` for a value of a field with `attempts` that a read gave, and no hash. The migration Job adds the columns and the default of `stego_schema.seals` only when they are not there, so it no longer takes an `ACCESS EXCLUSIVE` lock on that table at each run, and the check of the contract step of a seal puts the seal functions of the release in place in its own transaction. | Give the database at least one more connection for each process of the service, and at most 4 more under load, in `max_connections` or the connection limit of the pooler. A fill that calls `Stored` on such a value, such as to compare the hash itself, calls `Matches` instead. |
| 3.245.0 | 1.245.0 | 1.245.0 | Runtime v0.179.0 and typescript-sdk 1.4.0. A command whose JSON body holds a member that its request message does not take gets 400 `bad-request` as before, and now the member `field` of the problem names the member, such as `carrier` or `address.zip`, at most 128 bytes and no value, as a collection write names it. A check of a counted secret whose charge cannot commit, which answers 503, now logs `command.attempts.failed` through the log provider of the runtime, in the same JSON format as each other line, with `stego.action`, `stego.entity` and the cause class `stego.cause` (`unavailable`, `deadline`, `canceled` or `internal`, and from 3.246.0 also `lock_timeout` for a charge that waited 1 s for a lock); any other cause, such as a grant of `attempt_counts` that the database revoked, is `internal`. Before, it was a plain-text line with no cause. Until 3.250.0, a generated service gave its commands and collections no log provider, so the line stayed plain text in a service; from 3.250.0 it is JSON. The TypeScript SDK reads the code of an error answer of the media type `application/problem+json` too, as a command refusal gives it, and `error_codes` takes a code with a dot, such as `shipment.not_draft`; before, `SDKError.apiCode` of a declared refusal was never set. `stego fill sign` with a key that `SIGNERS` does not list still signs, now prints a warning that names the fix, and writes the fingerprint of the key as the signer, not the path of the key file. | A client that reads the problem of a refused command body can read the member in `field`. A log alert on `command.attempts.failed` reads the JSON line and its attributes. To get `apiCode` of a refusal in the TypeScript SDK, list its code under `overrides.typescript-sdk.error_codes`, and build the SDK again. An author outside `SIGNERS` follows [When the author of a fill is not in SIGNERS](../README.md#when-the-author-of-a-fill-is-not-in-signers). |
| 3.246.0 | 1.246.0 | 1.246.0 | Runtime v0.180.0. The runtime writes the value of a field with `attempts` that a read gave only into that field of that row; a fill that gives it to a compare-only field with no `attempts`, or to another row, gets an internal error (500) and the command commits nothing. A rerun of a command does not count again only the checks of the same stored value, so a new value that a reset wrote between two attempts gets its own counts. A charge of a check waits at most 1 s for a lock, and then the command answers 503. A full pool of the charges writes the warn line `command.attempts.pool_saturated`, with no value. | A fill that copies the value of a field with `attempts` to another field or row gives a new value with `sealed.ComparedOf` instead. Alert on `command.attempts.pool_saturated` if you want to know when the pool of the charges is too small. |
| 3.247.0 | 1.247.0 | 1.247.0 | Runtime v0.181.0. The checks of a field with `attempts` of one tenant use at most half of the connections of the pool of the charges at one time, and the checks of all tenants all but one, so the checks of one or two tenants no longer make the checks of other tenants wait and answer 503. A pool of the charges of fewer than 3 connections or with no bound gets the warn line `command.attempts.pool_small` once. The error line `command.attempts.failed` comes at most once in 10 s for each action, entity and cause class, with the new attribute `stego.held_back`. `sealed.CountedBy` is deprecated and binds nothing. `stego fill sign` with a GPG key that `SIGNERS` does not list and that signs with a subkey names the primary key in its warning and as the signer, as `SIGNERS` names it; before, it named the subkey. `stego compat` writes a warning on standard error when the committed `out/.../migrate.go`, or its digest in `.stego/state.yaml`, is not what a new generation of the head gives; the warning changes no fault and no exit status. To generate the head, compat now resolves the registry, which can clone it, and runs `go list`, as `stego plan` does. | A log alert that counts `command.attempts.failed` lines adds `stego.held_back` to its count. Do not call `sealed.CountedBy`. In CI, use `stego plan --base <ref>` as the gate of ST-GEN-INV38, as it generates the head again (see [What compat trusts](compat.md#what-compat-trusts)). |
| 3.248.0 | 1.248.0 | 1.248.0 | Runtime v0.182.0 and kafka-consumer 0.5.0. A service has 1 to 8 subscriptions. The inbox of each subscription reads its topic in its own consumer group, `<service>.<subscription>` by default, with its own Kafka client, and takes its settings by the name of the subscription: `STEGO_KAFKA_CONSUMER_<S>_TOPIC`, `_GROUP` (each with a `_FILE` form) and `_ACCEPT_INFINITE_RETENTION`, where `<S>` is the subscription name in capital letters with `_` in place of `-`. The topic need of each subscription is `<service>-sub-<subscription>`. The worker mode does not start when the environment gives a value to `STEGO_KAFKA_CONSUMER_TOPIC`, `STEGO_KAFKA_CONSUMER_GROUP`, their `_FILE` forms or `STEGO_KAFKA_CONSUMER_ACCEPT_INFINITE_RETENTION`, or when two subscriptions have one group. A base check fault of a column out of place (ST-GEN-INV33) names the column, the column it comes after in the base, and the place to give in the definition of a drop. | For a service with a subscription, [Move the settings of a subscription to kafka-consumer 0.5.0](#move-the-settings-of-a-subscription-to-kafka-consumer-050) before you deploy. |
| 3.249.0 | 1.249.0 | 1.249.0 | Runtime v0.183.0, postgres-adapter 6.26.0 and cli-application 1.27.0. A field of an entity of the base can become unique: declare `unique: true` or a `unique_composite` on it, and a schema change of kind `unique` whose `field` names the field, with an expand step only. Its index migration builds the unique index `CONCURRENTLY`; when two rows of one tenant hold one value, the migration Job stops with a refusal that names the migration, the index and the table, and no value. The schema check of the runtime accepts that index on columns that the release before has, when the index is valid and ready and no column holds a secret, so its pods restart after the step; it now refuses a step unique index that a build left invalid or not ready; `stego compat` refuses a new unique step on a base whose runtime library is before v0.183.0. Checking now also refuses a field that a collection names in its `scope` when it is `computed` or `filled_by`, as it refuses a `default`. The refusals of a ledger that is not the history of the release quote each migration name. | Remove `computed` and `filled_by` from each scope field, as for a `default`. To make a field of a running service unique, such as `name` for `apply`, deploy this release first with no new schema step, then declare the unique step (see [Unique steps](schema-changes.md#unique-steps)); before its Job runs, find and correct the rows of one tenant that hold one value, and do not restart the pods of the release before while the index builds. Before you make a `pii` field unique, read what a 409 tells a writer in [Unique steps](schema-changes.md#unique-steps). A log search for the text of a ledger refusal reads the names in double quotes. |
| 3.250.0 | 1.250.0 | 1.250.0 | Runtime v0.184.0, postgres-adapter 6.27.0 and typescript-sdk 1.5.0. A generated service gives its commands and collections the log provider of the runtime, so `command.attempts.failed` and `command.attempts.pool_saturated` are JSON lines with `stego.*` attributes, as the 3.245.0 row says; before, a service wrote them as plain-text lines. Each other line that the runtime writes with `log/slog`, such as `command: seal of a secret column`, `command: re-seal of a secret column` and `migrate: the step is refused`, is a JSON line too, on standard error, with the event name `command.seal.pass`, `command.reseal.pass`, `migrate.step.refused` or `stego.log` and at most 12 attributes of at most 256 characters under `stego.`, such as `stego.step`; the line of a refused step gives the refusal in its message. Only the runtime names the event of a line: a line of a fill has the event name `stego.log`. The failure record of the migration Job names the fault class, the object, the remedy and the cause class `refused` for a ledger digest refusal (`migration_ledger.digest`) and for a contract step that waits (`schema_change.seal_pending`, `schema_change.backfill_pending`, `schema_change.release_reports` or `schema_change.release_before_running`). The failure record of a start whose database pool did not open (`service.start.database`) names the fault class `database.connection`, `database.settings` or `database.runtime_role`, the object `database`, a remedy and a cause class, such as `connection_closed` or `crash_shutdown` after a restart of the database server. The result of the TypeScript SDK gives `authzRevocation`, `authzRevocationLocation`, `preferenceApplied` and `idempotentReplayed`, and `SDKError.retryAfter` gives the seconds of `Retry-After`, at most 3600. `stego validate` and `stego plan` give a warning for each fill whose sign-off counts only because of a key that the change adds to `SIGNERS` or whose line it changes, against the merge base of the branch and the default branch, or the ref of `--base`; with `--strict` it is a fault. With no base, such as in a CI checkout of one commit, they write a `NOTE`, and `--strict` refuses. `stego fill sign` does not keep such a sign-off. | A log alert or a dashboard that parses the plain-text lines of these events reads the JSON lines and their attributes. An alert on a failed migration Job can read `fault` and `cause`. To write a child or run a command on a new row at once in a browser page, build the TypeScript SDK again and send `authzRevocationLocation` of the create as `authzRevocationAfter` (see [the README of the component](../registry/components/typescript-sdk/README.md)). Run `stego validate --strict --base <base commit of the pull request>` in CI (see [Who signs the fills](../README.md#who-signs-the-fills)), and follow the new `SIGNERS` steps of [The order of a merge](../README.md#the-order-of-a-merge). |
| 3.251.0 | 1.251.0 | 1.251.0 | Runtime v0.185.0. Each process counts the checks of a field with `attempts` whose charge cannot commit, so the call answers 503, in the counter `command.attempts.failed` of its metrics, by `action`, `entity` and `cause`, with no tenant and no value. The counter counts each failed charge, also when the rate limit holds back its log line. The start of the worker mode names the setting of a subscription that it cannot read, such as `STEGO_KAFKA_CONSUMER_<S>_TOPIC_FILE`; before, it named the group setting `STEGO_KAFKA_CONSUMER_<S>_GROUP` for each such setting. | An alert on failed charges can read the counter `command.attempts.failed` in place of a count of log lines. |
| 3.252.0 | 1.252.0 | 1.252.0 | Runtime v0.186.0 and cli-application 1.28.0. A consuming worker whose run fails after SpiceDB denied it a row whose relations the relay has not written yet, such as the row that the run of the create event made a moment before, now gets the outcome `undecided` and the cause `relations_pending` in `worker.run.failed` and `command.consumer.run_failed`, in place of `fill_error` or another class, when the failure comes from that deny. The message goes back to pending with the failure code `relations-pending.<n>.<time>`, the poison count does not count it, and a declared refusal of such a run does not park it, for at most 10 attempts and 5 minutes; then the attempt is classed as before and logs the warn line `command.consumer.relations_pending_past_bound`. `command.NewStruct` also refuses a value of more than 100,000 values in all (`command.MaxStructNodes`), where a map or a list that a value holds at two places counts at each place. A sign-off by a GPG key of `SIGNERS` that is revoked or expired, as its armored block says, no longer qualifies its fill, also when the key signed before it expired. The generated CLI `apply` takes a secret member of a create or a patch, such as a PIN, in the `spec` of a document from a file that only you can read, or from a pipe on standard input, and refuses a file that the group or other users can read, also on standard input, before any request. | An alert or a dashboard on `worker.run.failed` with the cause `fill_error` also reads `relations_pending`: it is not an error of the fill. Alert on a long streak of `relations_pending` for one worker, on the warn line `command.consumer.relations_pending_past_bound`, and on `outbox.oldest.age` of each consumed destination: each tells that the relay does not write the relations of rows. Before the upgrade, run `stego validate`: a fill whose sign-off key is revoked or expired is now unsigned, so sign it again with a valid key of `SIGNERS`. A fill that gives `NewStruct` more than 100,000 values gives a smaller value. Put a secret field of an apply document in a file with mode 600. |
| 3.253.0 | 1.253.0 | 1.253.0 | Runtime v0.187.0 and typescript-sdk 1.6.0. A command or a read whose `Authz-Revocation-After` names the revocation of a write of another user of the same tenant waits for it, as for a write of the caller (recommendation (a) of ST-SUP-Q206, open for the Product owner); a read then answers `Authz-Revocation: unknown`, and the status route still answers only the maker. The warn line `command.attempts.pool_saturated` comes when a count of wrong attempts waits for a slot of the pool of the counts, with `stego.pool.in_use`, `stego.pool.max_open` and the new `stego.pool.waiting`; `stego.pool.wait_count` is gone. Before, the line could not come. The failure record of the migration Job gives the cause of a SpiceDB refusal with the class of its reason, such as `invalid_argument.relationships_exist`. In the TypeScript SDK, `idempotentReplayed` is `true` or `false`, not a text or `null`. `stego plan` runs the SIGNERS check also when the declaration needs add steps, so `plan --strict` refuses there too. The fault of a fill whose key `SIGNERS` does not list says first to sign it with a listed key. | To let a second user act at once on a new row, give it the `Authz-Revocation-Location` of the create (see [Act at once on a row that another user created](commands-queries-workers.md#act-at-once-on-a-row-that-another-user-created)). An alert on `command.attempts.pool_saturated` reads `stego.pool.waiting` in place of `stego.pool.wait_count`. An alert on the failure record that matches the cause `invalid_argument` exactly also matches `invalid_argument.*`. Build the TypeScript SDK again, and compare `idempotentReplayed` with `true`, not with `"true"`. |
| 3.254.0 | 1.254.0 | 1.254.0 | Runtime v0.188.0, command 1.25.0 and rest-api 4.35.0. A field with `attempts` can give `unlock_after`, from `1m` to `24h`: a row at its limit of wrong checks then opens again `unlock_after` after its last counted check, on the clock of the database (decision ST-SUP-Q205 (c)). A field with no `unlock_after` keeps the lock until a new value, as before (decision ST-SUP-Q207 (a)). A second count of the checks since the last match or new value never decays: at 10 times the limit, at most 100, the row stays locked until a new value or a reset. A command can declare `resets_attempts: [<Entity>.<field>]` and reset the counts of a row with a permission of its own, which `stego validate` compares with the schema, and which the runtime refuses to a caller who also holds the permission of another command that writes the entity on that row (404 `denied`); each reset writes an audit row (see [Unlock a row at once](fills.md#unlock-a-row-at-once)). `stego validate` warns of `unlock_after` shorter than `24h` on a secret of `min_length` 6 or less. The migration Job adds the columns `charged_at` and `failures` to `attempt_counts`; the next check of a count that a release before wrote gives it a time, so a locked row stays locked for one window. The wait of a second user for the revocation of a create tells the time when the create completed, in the same tenant only. | No declaration has to change. To let a locked row open again with time, give `unlock_after: 24h`, and a reset command whose permission only a support role has. The checks that pods of the release before charge during the rollout count toward the cap too. |
| 3.255.0 | 1.255.0 | 1.255.0 | Runtime v0.189.0, rest-api 4.36.0, postgres-adapter 6.29.0 and spicedb-authz 1.6.0. In a rolling release that adds a relation or a resource type to `authz/schema.zed`, the relay of the release before leaves each tuple that only the new release can read in pending (failure code `authz-relation-unknown`, `authz-payload-newer` or `authz-type-unknown`) for the relay of the new release, and parks it only after one hour, with the cause `not_understood` (recommendation (a) of ST-SUP-Q208, open for the Product owner); before, it parked such a tuple at once, and the row had no relation. The line and the metric `outbox.parked` name the cause, and the line names the failure code. The failure record of the migration Job names a fault, an object and a remedy for `migrate.open` (`database.connection`), the SpiceDB steps (`spicedb.connection`, `spicedb.schema_refused`, `spicedb.schema_not_served`) and a refused unique or foreign key step (`schema_change.unique_duplicates`, `schema_change.foreign_key_broken`); a refused key source of a start names `authn.keys.load`, `authn.keys.file`, `authn.keys.pem`, `authn.keys.document` or `authn.discovery`. A 409 of a unique index that a unique step of a newer release built names the field on a pod of the release before too. A write that waited with `Prefer: wait=N` answers `Preference-Applied`. `stego plan --strict` also refuses when `stego apply` would create, update or delete a generated file or change the record, or when the declaration needs add steps. | This upgrade is from a release before runtime v0.189.0, whose relay still parks such tuples: after the rollout of a release that adds a relation, resolve the parked messages of the destination `authz` (see [Resolve the parked messages of the destination authz](#resolve-the-parked-messages-of-the-destination-authz)). An alert on `outbox.parked` can read `cause`; add the alert on `outbox.not_understood` of that section; an alert on a failed Job or start can read `fault`. Add `stego drift` to the checks of a pull request, and run `stego apply` and commit before `stego plan --strict` (see [The checks of a pull request](../README.md#the-checks-of-a-pull-request)). |
| 3.256.0 | 1.256.0 | 1.256.0 | Runtime v0.190.0 and command 1.26.0. A field with `attempts` that a command resets with `resets_attempts` is checked only on a row on which the command decided its own permission: the row of its `resource_member`, a row that it read with `GetForUpdate` or `GetForDelete`, or a row that it wrote. A `Matches` on another row, such as a row that a command of another resource type or a command on another row read with `Get`, gives `false`, counts nothing and fails the call with 500 (decision ST-SUP-Q209 (d)). Before, a caller who held the reset and the permission of such a command could check the row with no bound, a reset after each limit. The reset is also refused to a caller who holds, on the row, the permission of a command of another resource type that writes the entity, and `stego validate` refuses such a command whose permission is the reset permission, is not defined on the resource type of the entity, or shares a relation or an arrow with the reset permission there. A field with no reset command is checked as before. The runtime holds a reset field also in code that a generator before command 1.26.0 made. | In a fill that checks a field with a reset on a row other than the resource of its command, read the row with `GetForUpdate` before `Matches`. Run `stego validate`, and define the permission of each command of another resource type that writes the entity on the resource type of the entity, apart from the reset permission. |
| 3.257.0 | 1.257.0 | 1.257.0 | Runtime v0.191.0, otel-tracing 1.20.0, domain-probe 1.1.0 and postgres-adapter 6.30.0. A service writes one access line for each HTTP request (`http.server.request.completed`: the method, the route template, the status and the duration) and each gRPC call (`rpc.server.call.completed`), with or without a collector; the line `command.refused` with the code of each command that answers 400 to 499; `worker.run.completed` for each worker run that committed; and the migration Job writes a line when it starts, when each step starts and ends, and when it ends. A database call that does not fail is now DEBUG, so `db.client.operation.completed` lines of a normal call go off the console. The new setting `STEGO_LOG_LEVEL` (`debug`, `info`, `warn` or `error`; `info` when it has no value) sets the lowest severity of the lines on standard output and standard error; another value stops the start. The WARN and the ERROR access lines of one route and status write at most 10 lines in a minute and then one line with the count of the dropped lines, and the lines of the security class are written at each level. | Nothing to do. To lower the volume of the logs, set `STEGO_LOG_LEVEL` to `warn` in `<service>-config`. A log query that reads `db.client.operation.completed` lines of normal calls needs `STEGO_LOG_LEVEL=debug`. See [Read the logs](operations.md#read-the-logs). |
| 3.258.0 | 1.258.0 | 1.258.0 | Runtime v0.192.0, command 1.28.0, postgres-adapter 6.31.0, rest-api 4.38.0 and cli-application 1.30.0. A command with `resets_attempts` gets the helper `<Command>Reset<Entity><Field>AttemptsByID`, which takes the ID of the row: the runtime decides only the reset permission and the reset guards on the row, and gives the fill no value, so a caller with only the reset permission can reset (decision ST-SUP-Q211 (a)). The schema change kind `relate` gives each row the tuple of a relation that a release adds: the relate job of the controller stages the tuple of each row through the authz outbox after the last pod of the release before stops, and first deletes the tuples of a field or a reference relation on the row, so a former subject loses its access (decision ST-SUP-Q212 (a)). A start that cannot reach SpiceDB fails at the stage `service.start.spicedb` with the fault `spicedb.connection` or `spicedb.settings`, and a start whose ledger lacks a migration of the release names the fault `migration.pending` or `migration.changed`; before, the first gave the stage `component.stego-runtime.constructor[0]` alone. A 503 while the database cannot serve says that the database cannot serve, not the authorization provider. `apply` of the CLI leaves each secret field of a resource that exists out of the write and names it as not compared, as the stored value can differ, so a document with a required secret applies again; `--set-secret FIELD` sends a new value from a private file, only of a field that the document holds. A command of the CLI names the required field that it lacks. | In a release that adds a relation that the runtime writes for each row, declare a relate step, and for a field or a reference check the stored values first (see [Give the rows that are there a new relation](#give-the-rows-that-are-there-a-new-relation)); after an earlier release that added one, declare it in your next release. Use the helper `...ByID` in a reset fill whose caller has no read permission of the row. A script that relied on `apply` to send a secret again adds `--set-secret <field>`. An alert on a failed start can read the new faults. |
| 3.259.0 | 1.259.0 | 1.259.0 | Runtime v0.193.0 and browser-telemetry 1.4.0. A Provision or a Retire of a tenant whose change of the tenant list meets a database that cannot serve now, such as a server in recovery (SQLSTATE 57P03) or a lost connection before the COMMIT, answers 503 with the code `unavailable` and `Retry-After`. Before, it answered 500 `internal`. A COMMIT whose outcome is not known still answers 500. The browser telemetry runtime sends nothing on a page that has no `stego-runtime-config` meta element, also when the caller gives `sampleRatio`. Before, that option enabled traces, logs and metrics on such a page. | A client that provisions tenants tries again a 503 after `Retry-After` with the same `Idempotency-Key`. A page that uses `createBrowserTelemetry` outside a generated backend puts one `stego-runtime-config` meta element in its head to enable export. |
| 3.260.0 | 1.260.0 | 1.260.0 | Runtime v0.194.0, otel-tracing 1.21.0 and browser-telemetry 1.5.0. The browser telemetry relay refuses traces when the deployment sets the trace ratio to 0, as it refuses logs and metrics that are off, and drops each browser span whose trace ID the ratio sampler of the browser runtime does not sample at the deployment ratio. Before, it forwarded such spans to the collector. `stego compat` and `stego plan` refuse a release that adds a relation that the runtime writes for each row of an entity of the base, such as `relation tenant: tenant`, with no relate step, and suggest the relate step; they read the schema of the base at its commit. The runtime changes only the names of two tests. | A release that adds such a relation declares a relate step (see [Give the rows that are there a new relation](#give-the-rows-that-are-there-a-new-relation)). A page that calls `beginTrace` gives it a random trace ID only. |
| 3.261.0 | 1.261.0 | 1.261.0 | Runtime v0.195.0, otel-tracing 1.22.0, rest-api 4.39.0, browser-backend 4.15.0, controller 1.28.0 and grpc-application 2.5.0. The runtime and the generated tracing move to OpenTelemetry Go v1.47.0, which fixes GO-2026-6974. Each JSON line on standard output and standard error has one form: the timestamp in UTC with nanoseconds, `severity`, `service.name`, `event.name`, `message` and the fields under `attributes`, each with the prefix `stego.` or a name of the semantic conventions for HTTP and RPC (`http.route`, `http.response.status_code`, `rpc.method`, `error.type`). Before, the access lines had their fields at the top level and a timestamp of the local zone, the worker events had theirs under `fields` with no prefix, the failure record had its fields at the top level, and several lines had the event name `stego.log`. Each line of the runtime now has an event name of its own, and the line of the HTTP or gRPC listener is `service.listening`. The count line of dropped access lines comes at the end of the minute also when no later request of its route and status comes. A consumer run that waits for the relations of a row (cause `relations_pending`) is a WARN `worker.run.failed`; before, an ERROR. A 503 of the CRUD routes has the problem type `service-unavailable`; before, `internal-error` (the code does not change). The failure record of a browser service names a fault, an object, a remedy and a cause. | Change each log query, alert and dashboard that reads a field of a line: read it under `attributes` with its new key, such as `attributes."stego.worker"`, `attributes."http.route"`, `attributes."stego.stage"` or `attributes."stego.dropped"`, and the time in UTC. An alert on ERROR `worker.run.failed` no longer fires for a run that waits for relations; alert on `relations_pending` separately if you need it. A client that reads the problem type of a 503 accepts `service-unavailable`. During the rollout, the pods of the old release still write the old shapes: the fields of an access line at the top level, such as `http.route`, the fields of a worker event under `fields` with no prefix, such as `fields.worker`, and a timestamp with the offset of the local zone. Thus a query that reads only `attributes` does not find the lines of the old pods. Until the last pod of the old release stops, make each query read both shapes, such as `attributes."http.route"` or `http.route`, and compare the times as instants, not as text. After the old release stops, remove the old shape from the query. See [Read the logs](operations.md#read-the-logs). |
| 3.262.0 | 1.262.0 | 1.262.0 | Runtime v0.196.0 and otel-tracing 1.23.0. Each line of one process names one `service.instance.id`: the runtime of the service takes the identity of the HTTP telemetry. Before, the access lines and the lines of the runtime of one process named two identities. At the trace ratio 1 the browser telemetry relay keeps each span, as the browser runtime samples each trace ID there; before, it dropped a span whose trace ID words XOR to `0xffffffff`. `stego compat --base` and `stego plan --base` also refuse a new relation from the caller of the create on an entity of the base, a new relation of a field or a reference on a `shared` or `system` entity of the base, and a new `authorization` schema with no relate step for each relation that it gives the rows of the base. | A log query that joins the lines of one process on `service.instance.id` now finds each line of the process. When `stego compat` refuses a new relation that no relate step can give, give the permission another path. See [The check against the base](schema-changes.md#the-check-against-the-base). |
| - | - | STEGO commit 955d242c7 of pull request #506 (the archetype versions stay 3.262.0, 1.262.0 and 1.262.0) | kubernetes-service 1.31.0 and kubernetes-workload 1.1.0, which only browser-service holds, or a declaration that selects them. Each label and annotation key that STEGO writes starts with `stego.jsell-rh.github.io/` and not with `stego.dev/`: `config-sha256`, `allocator`, `allocation-profile`, `allocation-network-endpoints`, `network-spec-sha256` and `service-account-<alias>`, and the capability paths of the namespace allocator. The admission policies and the allocator select only the new keys. A namespace that an allocator of an earlier release made keeps the old labels, and loses the protection of the policies and the ownership of the allocator. The Pods restart once, because the key of the configuration digest annotation changes. | [Upgrade over the old label keys](#upgrade-over-the-old-label-keys) |
| 3.264.0 | 1.264.0 | 1.264.0 | Runtime v0.198.0, otel-tracing 1.25.0, browser-backend 4.17.0, rest-api 4.41.0 and command 1.30.0. A database that takes no new connection (SQLSTATE 55000 at connect, such as after `ALLOW_CONNECTIONS false`), a session that the server ended as idle (57P05) and a server with no memory or disk now (53000, 53100, 53200) give 503 with `Retry-After` and the code `unavailable`; before, 500 `internal`. Each 503 has the problem type `service-unavailable`, also from the command runtime, the query runtime and the tenant check; before, those had `unavailable` or `undecided` (the codes do not change). The access line of a declared command route, such as `/jobs/{job_id}/status:convert`, carries `http.route`. A browser service takes 1 to 15 entries in `apis`; before, 1 to 8. A retype from `int32` to `int64` with the same bounds on both sides, also with no `max`, passes the checks, also for a field that `patchable` names: the release of the expand step holds the field to the `int32` range until the contract step (see [Widen an int32 to an int64](schema-changes.md#widen-an-int32-to-an-int64)). `stego compat` runs the declaration checks of `stego validate` too, and exits 1 for each of their errors (rule `ST-DECL-UC01`). | A client or an alert that reads the problem type of a 503 accepts `service-unavailable` in place of `unavailable` and `undecided`, and reads the `code` for the cause. A log query by `http.route` now also finds the lines of command routes. A CI job that runs `stego compat` gets the faults of validate too. |
| 3.265.0 | 1.265.0 | 1.265.0 | Runtime v0.199.0 and postgres-adapter 6.32.0. The counts of wrong attempts of a compare-only field with `attempts` go with their row (decision ST-SUP-Q216): a delete of a row deletes the counts of its fields in the transaction of the delete; the retire of a tenant deletes each count of that tenant, and still no row of an entity; the contract step of a drop deletes the counts of the dropped field or entity in each tenant of the tenant list. Before, each count stayed in `attempt_counts` after its row was gone. The ledger accepts the digest of the contract step of a drop that an earlier adapter applied. | None. A count that an earlier release left for a row that it deleted stays until the retire of its tenant. See [Limit the wrong attempts](fills.md#limit-the-wrong-attempts). |
| 3.268.0 | 1.268.0 | 1.268.0 | Runtime v0.199.0 and rest-api 4.42.0. A create, its replay, a read, a list, an update and a patch of a collection in envelope mode answer an `int64` field exactly, also above 2^53 (9007199254740992). Before, the value went through a float64: 9007199254740993 was answered as 9007199254740992, and 9223372036854775807 as 9223372036854776000 (walkthrough 27, W-PREC). A number inside a `jsonb` field is now answered exactly too, as it is stored. The stored value was always exact. | None for a Go client, the Go SDK or the CLI, which read the exact number. The generated TypeScript client refuses a number above 2^53 with `invalid_response`, before and after: keep such values in a field of type `string`, or read the body with a parser that keeps big integers. |
| 3.269.0 | 1.269.0 | 1.269.0 | Runtime v0.202.0. The WARN line `command.database.unavailable` names `stego.sqlstate_class`, the class of the SQLSTATE only, such as `53` for a full disk or no memory of the database (see [Alert on a full disk or no memory of the database](operations.md#alert-on-a-full-disk-or-no-memory-of-the-database)). `stego compat` exits 2 with the cause class `fill` or `declaration` when the checks of `stego validate` cannot run for a cause other than the registry, such as a `fill.yaml` that does not parse; before, it said `No fault`. A fill name of a slot binding outside the fill-name form is not read, and the fill manifest of a slot binding is read inside the project folder only. | An alert on a full disk or no memory of the database can read `stego.sqlstate_class`. A CI job that runs `stego compat` fails on a fill manifest that does not parse. A fill folder of a slot binding that is a link out of the project folder stops the checks: make it a folder of the project. |
| 3.270.0 | 1.270.0 | 1.270.0 | Runtime v0.202.0, rest-api 4.43.0 and browser-backend 4.18.0. An update, an upsert of a stored row and a patch compare a `jsonb` field with `writable: create` or `never` with each number as its exact value. Before, the compare went through a float64, so a value that differed from the stored one only above 2^53, such as 9007199254740992 for 9007199254740993, was taken as the stored value: the write answered 200 and stored the stored value. Now it answers 400 with a validation entry that names the field. The same value in another form, such as `1.0` for `1` or `25e-1` for `2.5`, is still the stored value. A browser backend keeps 8 permits for `/auth/*` and for the assets and documents that it serves itself. Before, slow APIs could hold each of its 16 permits, and a login, a sign-out and the assets got 503 until an API answered. | None. A client that sends back the value it read gives the stored value. |
| 3.271.0 | 1.271.0 | 1.271.0 | Runtime v0.203.0 and spicedb-authz 1.7.0. The runtime holds each change of v0.202.0, and the setting `STEGO_SPICEDB_CA_FILE`: it names a PEM file of certificate authorities that the connection to SpiceDB adds to the system roots, for a SpiceDB whose certificate a private authority signs, such as a CA issuer of cert-manager (decision ST-SUP-Q217 (a), ST-SUP-RP14-LC14). The api, worker and controller modes and the migration Job read it, and the generated Deployments and the migration Job take it from the ConfigMap. A file that holds no PEM certificate, a file that other users can read or that its group can write (such as the Kubernetes default mode 0644), or a value together with `STEGO_SPICEDB_INSECURE_LOOPBACK=1`, stops the start with the stage `service.start.setting.spicedb-authz.STEGO_SPICEDB_CA_FILE`. With no value, the connection verifies with the system roots, as before. The process reads the file once, at the start. | None for a SpiceDB that a public authority signs. For a private authority, set `STEGO_SPICEDB_CA_FILE` in the ConfigMap, and mount the file with the mode 0440 or 0400 on each Deployment and on the migration Job. When the authority changes, restart the pods. See [Trust a SpiceDB that a private CA signs](delivery.md#trust-a-spicedb-that-a-private-ca-signs). |
| 3.272.0 | 1.272.0 | 1.272.0 | The Go module path of STEGO and of its runtime library changes from `github.com/jsell-rh/stego` to `github.com/910-software-solutions/stego`, because the work moved to the repository github.com/910-software-solutions/stego. The archetypes pin `github.com/910-software-solutions/stego/registry/runtime` v0.203.0 at h1:xRcbNj2gfrnY2gKw0mM02tbltv7nrBr5MWbqvRd2EAI=. The runtime code is the code of 3.271.0. The generated code imports the new path. The trusted workflows of the Assuring attestations are the workflows of the new repository. Their predicate types do not change: they are names, and they stay `https://github.com/jsell-rh/stego/assure/proof-run/v1` and `https://github.com/jsell-rh/stego/assure/release-decision/v1`. The label and annotation prefix `stego.jsell-rh.github.io/` does not change. | Change each import and the `require` line of `go.mod` of the service from `github.com/jsell-rh/stego/registry/runtime` to `github.com/910-software-solutions/stego/registry/runtime`, also in your fills. Then run `stego deps` and `stego apply`, and remove the old lines from `go.sum`. Do not mix the two paths in one service: Go takes them as two modules. The old tags, up to `registry/runtime/v0.202.0`, stay on `github.com/jsell-rh/stego`, so a service that stays on an earlier release still builds. A verifier of the Assuring attestations trusts the workflows of the new repository. |
| 3.273.0 | 1.273.0 | 1.273.0 | Runtime v0.204.0 and oidc-auth 0.2.0. oidc-auth takes the setting `accept_id_token` (`true` or `false`, `false` by default): with `true`, the verifier accepts an OpenID Connect ID token as the bearer, for a web client and an API that share one client ID at a provider that gives no JWT access token, such as tsidp (decision ST-SUP-Q218 (b)). It accepts an ID token only when its `aud` is exactly the `audience` (a string, or an array of that one member) and its `azp`, when present, is the `audience`; a payload `typ` may then be `Bearer` or `ID`. A logout token (`events`) is always refused, every other check stays, and access tokens do not change. With no setting, each ID token is refused, as before. No environment variable turns it on. jwt-auth and rh-sso-auth do not change. | None. To accept ID tokens, set `accept_id_token: true` under `oidc-auth` in `service.yaml` only when the `audience` is the client ID of the web client, and keep the ID token on the server side. See [Accept an ID token as the bearer](operations.md#accept-an-id-token-as-the-bearer-oidc-auth). |
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
   `sealed.Secret` (package `github.com/910-software-solutions/stego/registry/runtime/sealed`):
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

## Move the settings of a subscription to kafka-consumer 0.5.0

Do this once, for a service with a subscription, before you deploy its first
release of archetypes 3.248.0 or 1.248.0 (`kafka-consumer` 0.5.0).

Why: `kafka-consumer` 0.3.0 read one subscription with fixed names. From 0.5.0,
a service has up to 8 subscriptions, and each takes its settings by its name.
The worker mode of 0.5.0 does not start while the environment gives a value to
a fixed name, so no value is ignored in silence. The default consumer group is
`<service>.<subscription>`, as before, so the group keeps its offsets and the
inbox does not read the topic again.

1. Find `<S>`: the name of the subscription in capital letters with `_` in
   place of `-`, such as `ORDER_CHANGES` for `order-changes`.
2. In each environment file and overlay, rename each variable:

   | 0.3.0 | 0.5.0 |
   |---|---|
   | `STEGO_KAFKA_CONSUMER_TOPIC` | `STEGO_KAFKA_CONSUMER_<S>_TOPIC` |
   | `STEGO_KAFKA_CONSUMER_TOPIC_FILE` | `STEGO_KAFKA_CONSUMER_<S>_TOPIC_FILE` |
   | `STEGO_KAFKA_CONSUMER_GROUP` | `STEGO_KAFKA_CONSUMER_<S>_GROUP` |
   | `STEGO_KAFKA_CONSUMER_GROUP_FILE` | `STEGO_KAFKA_CONSUMER_<S>_GROUP_FILE` |
   | `STEGO_KAFKA_CONSUMER_ACCEPT_INFINITE_RETENTION` | `STEGO_KAFKA_CONSUMER_<S>_ACCEPT_INFINITE_RETENTION` |

3. The topic need `<service>-subscription` of the platform requirements is now
   `<service>-sub-<subscription>`. Give it the same topic and the same ACLs.
4. Run `stego env check` on each environment file. It names the topic of each
   subscription that has no value. A fixed name of 0.3.0 is in the list of the
   variables that the service does not read.

If you deploy with a fixed name, the worker Deployment does not start. Its
failure record names the variable in the stage, such as
`service.start.setting.kafka-consumer.STEGO_KAFKA_CONSUMER_TOPIC`, and no
value. The api Deployment continues to serve.

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
   [Access to the topic](subscriptions.md#access-to-the-topic)), and set the
   topic of the new subscription, `STEGO_KAFKA_CONSUMER_<S>_TOPIC` (see
   [Subscriptions](subscriptions.md)). The other subscriptions keep their
   settings and their groups.

   In `authz/schema.zed`, give the type `tenant` the relation of the new
   consuming worker, with its name in snake case, and point the permission
   that its command checks at it, such as:

   ```text
   definition tenant {
   	relation worker_record_payment: service_account
   }

   definition payment {
   	relation tenant: tenant
   	permission record = tenant->worker_record_payment
   }
   ```

   Else `stego validate` refuses the declaration, and names the missing
   relation `worker_<name>` of the type `tenant` (ST-SUP-RC35-OC13). See
   [Worker grants in each tenant](workers.md).
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

## Resolve the parked messages of the destination authz

The messages of the destination `authz` are the relationship writes of the
rows: the relay writes them to SpiceDB. In a rolling update, the worker of the
release before and the worker of the new release run at the same time, and
both take the messages of the outbox.

A release can add a relation to `authz/schema.zed`, such as `relation tenant:
tenant` on `payout` with `permission edit = owner + tenant->approver`. The
migration Job writes the new schema to SpiceDB before the rollout. Then the
pods of the new release write the tuples of the new relation for each row
that they make. The rows that are there, and the rows that the pods of the
release before make, get the tuple only from a relate step (see [Give the rows
that are there a new relation](#give-the-rows-that-are-there-a-new-relation)).

- From runtime v0.189.0, the relay of the release before does not write a
  tuple of a relation that its release does not know. It leaves the message in
  pending, with the failure code `authz-relation-unknown` and no attempt
  counted, and the relay of the new release writes it. A message that no relay
  writes within one hour is parked. The line `outbox.parked` then has the cause
  `not_understood`. The relay of the release before also leaves a payload of a
  newer runtime (`authz-payload-newer`) and the delete of a retired tenant of a
  new resource type (`authz-type-unknown`). The first hold of such a message
  writes the WARN line `outbox.not_understood`, with the destination and the
  failure code, at most one line a minute for each destination. The bound of
  the destination `authz` is one hour, and no setting changes it: a held
  tuple is a write or a revocation that SpiceDB does not have yet.

  Alert on the metric `outbox.not_understood` of the destination `authz`
  when it grows for more than 10 minutes, such as with the PromQL rule
  `increase(outbox_not_understood_total{destination="authz"}[10m]) > 0` for
  10 minutes. A rollout takes minutes, so a longer growth tells that no
  worker of the new release runs, such as in a paused rollout. Finish the
  rollout, or roll forward, before the hour of the bound ends.
- Before runtime v0.189.0, the relay of the release before parks such a
  message at once, with the failure code `delivery-refused-permanent`. A row
  that the new release made during the rollout then has no tuple of the new
  relation, and a check that needs it gives 404. Thus, when you upgrade from a
  release before runtime v0.189.0 with such a release, do the steps below after
  the rollout. To prevent the parks, scale the Deployment `<service>-worker`
  of the release before to 0 before the rollout, and accept that the messages
  wait until the new worker starts.

Do these steps in each tenant, after the last pod of the release before
stopped:

1. Give the operator the permission `resolve` on the destination `authz` in
   the tenant: the relation `operator` on `outbox_destination`, with the
   tenant-qualified ID of `authz`. For the tenant `acme` and the user `ops` of
   the issuer `https://idp.example.test`:

   ```bash
   b64() { printf %s "$1" | base64 | tr '+/' '-_' | tr -d '=\n'; }
   zed relationship create "outbox_destination:$(b64 acme)/$(b64 authz)" operator "user:$(b64 https://idp.example.test)|$(b64 ops)"
   ```

   SpiceDB can take up to its quantization interval to use the grant (see
   [Operations](operations.md)). The schema must declare `outbox_destination`
   with the relation `operator` (see [See and resolve the parked
   messages](commands-queries-workers.md#see-and-resolve-the-parked-messages)).
2. Write the header file of the token of the operator, as in step 5 of [Add a
   subscription to a running service](#add-a-subscription-to-a-running-service),
   and list the parked messages of `authz`:

   ```sh
   curl -sS -H @auth-header "https://<host><base path>/stego/outbox/authz/parked"
   ```

   Each message has the failure code. A message of a new relation has
   `delivery-refused-permanent` before runtime v0.189.0, and
   `authz-relation-unknown` from runtime v0.189.0.
3. Send each parked message again with `not_acted`, then remove the header
   file. Do not use `discarded`: a discarded message of `authz` is a
   relationship change that SpiceDB never gets. The destination `authz` refuses
   `acted`.

   ```sh
   curl -sS -H @auth-header "https://<host><base path>/stego/outbox/authz/parked" |
     jq -r '.items[].id' | while read -r id; do
       curl -sS -X POST -H @auth-header -H 'Content-Type: application/json' \
         -d '{"resolution": "not_acted"}' "https://<host><base path>/stego/outbox/authz/parked/$id:resolve"
     done
   shred -u auth-header
   ```

   Do this again while the list has a `next` cursor.
4. List the parked messages again. The relay of the new release writes each
   message that you sent again. A message that parks again has another cause,
   such as a relation that `authz/schema.zed` of the release does not hold.
   Correct the cause, then send it again.
5. Remove the grant of step 1 with `zed relationship delete` and the same
   three arguments, unless the operator keeps it.

## Give the rows that are there a new relation

A release that adds a relation that the runtime writes for each row, such as
`relation tenant: tenant` on `payout`, writes its tuple only for the rows
that its pods make. The rows that were there before the release, and the rows
that the pods of the release before make during the rollout, have no tuple
of the new relation. A check through it, such as `tenant->approver`, gives 404
on those rows, also after the release before stopped (walkthrough 24, P2-R2).
The steps of the section above do not change this: they send again only the
messages that a relay parked.

From runtime v0.192.0 (rest-crud 3.258.0), declare a relate step in the
release that adds the relation (see [Relate steps](schema-changes.md#relate-steps)):

```yaml
schema_changes:
  - {id: payout-tenant-relate, kind: relate, entity: Payout, relation: tenant}
schema_history:
  - {change: payout-tenant-relate, phase: expand}
```

From rest-crud 3.260.0, `stego compat` and `stego plan` refuse a release
that adds such a relation to an entity of the base with no relate step. The
fault names the entity and the relation, and suggests the relate step at
expand. The check reads the schema of the base at its commit: the schema file
that the base declaration names must be in the service folder of the base.

A relate of a field or a reference, such as the relation `approver` from the
field `approver`, grants access from the values that the rows hold now, which
the release before wrote under its own rules. Before the release, check those
values and correct each one that must not grant the relation; `stego
validate` warns of each such relate. A relationship whose subject is a field
needs a create that decides a permission: generation refuses it on an entity
with a public create, also under a parent. See [Relate a
field](schema-changes.md#relate-a-field) for the forms that you can use.

Then do the procedure as for each release:

1. Run `stego apply` and commit. The release gets the migration
   `step_<position>_payout-tenant-relate_expand`, which changes no table.
2. Roll out the release. The migration Job makes the checkpoint and the lease
   of the relate job. The controller of the new release waits while a pod of
   the release before runs, and logs `command.relate.pass` with `waits`
   `true`.
3. After the last pod of the release before stops, the next run of the
   relate job, at most 10 minutes later, stages the tuple of each row in each
   tenant through the authz outbox, and the relay writes it. The line
   `command.relate.pass` then has `completed` `true`, `checked` the rows read
   and `related` the tuples staged.
4. Check a row that the release before made: the check through the new
   relation now passes.

A release before runtime v0.192.0 that added a relation left its rows with no
tuple. To give them the tuple, declare a relate step of that relation in your
next release: the relate job reads each row that is there, and a tuple that
is there does not change.

## Upgrade over the old label keys

Do this before you apply kubernetes-service 1.31.0 or kubernetes-workload 1.1.0 to a cluster.

1. List the namespaces that an allocator of an earlier release made:

   ```sh
   kubectl --context <context> get ns -l stego.dev/allocator
   kubectl --context <context> get ns -l stego.dev/allocation-profile
   ```

2. Each command must give no namespace. The new release reads only the new keys, so it does not own or protect
   a namespace with the old labels. If a namespace is in the list, release it with the earlier release before
   the upgrade, and allocate it again after the upgrade.
3. Apply the release. Each Pod of the service restarts one time, because the annotation of the configuration
   digest has a new key.

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
