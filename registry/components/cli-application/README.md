The CLI component generates a separate command entry point, a command runtime,
and the common HTTPS client. Run or build `<out>/<namespace>/cmd`. The application
factory returns `command.Application` with command names, paths, methods,
request fields, and expected status codes. It supplies no networking code.

Declare status codes with no response body in `Command.EmptyResponses`. Each code
must also occur in `Success`. Duplicate or unaccepted codes are rejected before
network access. For an asynchronous delete, use `Success: []int{202}` and
`EmptyResponses: []int{202}`. A nonempty response then fails the contract. Other
accepted codes require valid JSON. HTTP 204 and 205 always require an empty body.

To get the CLI of the service's own API, bind the mixin `cli-application` in
the service declaration (`mixins: [cli-application]`); the rest-crud archetype
lists it, and an rpc-service can bind it too. With no `factory_package`, STEGO
makes the definitions from the OpenAPI document that the rest-api component
writes for the service, or, in a service with no rest-api component such as an
rpc-service, from the document of its command and query routes that the command
component writes (`<out>/commands/openapi.json`), in the package
`<out>/cli/app`:

- one command for each operation, named by the words of its operationId,
  such as `create-clinics`, `read-clinics` and `discharge-patient`;
- a required flag for each path parameter, such as `--clinic-id`, and the
  argument `ID` for `{id}`;
- a flag for each query parameter and body member of type string, integer,
  boolean or list of strings; give the other members in `--body FILE`;
- no flag for a secret member, which the document marks `writeOnly` or
  `x-sensitive`, such as a PIN: a flag puts its value in the shell history and
  the process list. Give it in `--body FILE`, a file that only you can read:
  the CLI refuses a body file that holds a secret member when the group or
  other users have access to the file.
  A secret field of an entity is `writeOnly` in the document of its
  collection. A member of a command message is a secret only when its proto
  file marks it `[debug_redact = true]`: mark each secret member of a command
  request and response, such as `string pin = 2 [debug_redact = true];`
  (see [A secret member](../../../docs/commands-queries-workers.md#a-secret-member)).
  `stego validate` warns of a request member with the name of a secret field
  of an entity that the command writes and no mark
  (`unmarked-secret-member`). A secret member cannot be a path parameter:
  generation refuses the route, because the URL goes in access logs;
- `Sensitive` for a command whose answer holds a member marked `x-sensitive`,
  or whose answer the document marks `x-stego-opaque` because it holds a
  `google.protobuf.Any`, `Struct`, `Value` or `ListValue` member, which can
  hold a secret that no mark reaches: it writes the answer only to
  `--output-file FILE`, and never to stdout;
- `--yes` for each delete;
- one `apply` resource for each collection with a create, a list and a patch
  that take a string `name`, at a path with no parameter. The kind is the
  name of the entity, and the API version is `<service>/v1`, such as
  `sellers/v1` for the service `sellers`. A scoped collection, whose path
  names its parent, gets no apply resource: use its create and patch
  commands. When the entity holds its `name` unique (the field `name` is
  `unique: true`, or a `unique_composite` holds `name` and, apart from it,
  only `implicit` fields of the collection), the service answers 409 to a
  second create of a name (ST-SUP-RC04-OC36). Warning: when the name is not
  unique, two clients that apply one new name at the same time can make two
  resources; `unique: true` on the field `name` closes this. On an entity
  of the base, declare it with a unique step of the field `name` (see
  [Unique steps](../../../docs/schema-changes.md#unique-steps)).
  A secret member of the create or the patch, such as a PIN, is a field of
  the resource with no flag: give it in the `spec` of a document in a file
  that only you can read, or from a pipe on standard input. Apply refuses a
  file that holds a secret field when the group or other users have access
  to it, also a file on standard input, before any request. The service never answers a secret field, so apply
  cannot compare it. For a resource that exists, apply does not send a
  secret field of the document, also a required one or one that only the
  create takes, and names it, never its value, as not compared in the
  result: `secrets_not_compared` with `-o json`, else
  `(secret fields not compared: pin)`. Thus you can apply one document
  again, and the result is `unchanged` when each other field has the
  stored value. To give a resource that exists a new value of a secret,
  add `--set-secret <field>`, such as `--set-secret pin`, once for each
  field; the field must be one that the patch takes, and the document must
  come from a private file or a pipe. Apply refuses a `--set-secret` of
  another field before any request (cli-application 1.30.0; before, a
  second apply of a document with a secret that only the create takes
  failed, and a secret that the patch takes was sent again at each apply).

The fills of the service follow the STEGO gates: `stego validate` and `stego plan`
also warn when the sign-off of a fill counts only because of a key that the
change adds to `SIGNERS`; with `--strict` this is a fault. `stego plan --strict`
also refuses when `stego apply` would change a generated file.

Run `<out>/cli/cmd --help` for the list. Each write whose operation carries the
`Idempotency-Key` header parameter states `IdempotentWrites`, as
`IdempotentWritesFrom` sets it: each command, and the create and the patch of a
service with the idempotency component. For other names or other commands,
write your own factory package and set `factory_package`.

The target must be Go 1.25.0 or later. Validation, plan, and apply reject an older
target before generation. This requirement covers file operations and the OIDC dependency.

Set `factory_package` to a module-relative Go package outside generated output.
The package must export `Commands() command.Application`. The compiler validates
that package path. The generated runtime checks the returned definitions before
it reads configuration or sends requests. Definitions with duplicate names,
ambiguous prefixes, invalid routes, or invalid fields are rejected. Command
names have one through four words. The limit is 128 commands and 64 fields per
command. Up to eight named path parameters can bind required flags to whole
route segments. They stay outside request bodies and query strings. Hypershell names and rules do not occur in this component.

Common commands include `login --url URL --token-file FILE [--ca-file FILE]`
and `logout`. Token-file login stores absolute file references in a private
JSON configuration;
it does not copy the token or claim to have verified it with the API. A command
reads the current token file before its request. Logout removes configuration
and retains the externally owned token file. The factory selects the environment
variable and application directory used to locate configuration.

New directories use mode 0700. Configuration and token files must be owned by
the effective user, as ssh checks for its keys, and must exclude group and other
access. A body file that holds a secret member has the same rules. Thus a CLI
that runs as root accepts only such files that root owns. The configuration directory must exclude writes by other
users. Configuration reads reject leaf symlinks. Writes use an open directory
handle, a random exclusive file, file sync, atomic rename, and directory sync.
Failed writes do not replace configuration with partial JSON. Input files must
be regular and bounded; a FIFO cannot block credential or body reads.

HTTPS and certificate verification are required. A supplied CA file selects an
explicit trust pool; omission selects system roots. The shared HTTP client does
not follow redirects or use environment proxy settings. CLI requests have a
15-second deadline.

Each POST and PATCH request has a new `Idempotency-Key` header: a structured
string that holds a random UUID. A service that does not use the key ignores
it. The client refuses the header on a GET, PUT, DELETE, or HEAD request.

Set `IdempotentWrites` on a `command.Command` or a `command.ApplyResource`
only when the service keeps the key of that write (the idempotency
component). For an apply resource, the flag covers its create and its patch.
`command.IdempotentWritesFrom(app, document)` sets the flag of each command
and apply resource from the OpenAPI document of the service: true when the
operation carries the `Idempotency-Key` header parameter, and false otherwise.
The rest-api component states that parameter on each create and patch, and
each command has it. A parameter that the document
gives by `$ref` does not count. A command that is not a POST or a PATCH must
not set the flag.

With the flag, the CLI sends a write again one time, with the same key and
body, after a transport failure, a request that expired, or a 5xx answer that
carries the header `Idempotency-Kept: 1`. It waits one second first, or follows a `Retry-After` of one to five seconds.
When the second request gets no success, the outcome is unknown: the first
request can still be in its transaction, as a 409 answer shows. The service
gives the stored answer of a write that it did, so the write does not occur
two times. The CLI does not send a GET, PUT, or DELETE again. It does not send
a write again after any other answer, after a redirect, after an answer body
over the client limit, after a certificate of the service that the TLS
handshake refuses or a handshake alert from the service on a new connection
(`ErrUntrusted`), or after a request that the client refused before it sent
it. Without the flag, the CLI does not send the write again. After an
uncertain failure, callers must check resource state before they retry a
write.

The flag comes from the OpenAPI document that the CLI holds, which is the
document of the service when the CLI was built. When a later release of the
service stops to keep the key of a write, a CLI of an earlier release still
sends that write again, and the write can then occur two times. Thus, build
the CLI from the same release as the service that it calls, and build it again
with each release of the service.

The service tells on each answer to a write whose key it keeps that it keeps
the key: the answer carries `Idempotency-Kept: 1`, also a problem and a 5xx
answer. A 5xx answer with no such header, such as a 502 or a 504 of a proxy,
or an answer of a service of another release, is not sent again: the command
ends with an error that says that the write was not sent again and can have
been done, so read the state of the resource before a new write. A transport
failure or a request that expired gives no answer, so the flag decides there.
When the flag says that the service keeps the key and a 2xx answer has no
`Idempotency-Kept` header, the CLI writes a warning to standard error that the
CLI and the service can be of different releases. The warning changes no
answer and no exit code. A proxy in front of the service must not add
`Idempotency-Kept`: the CLI trusts the header to send a write again.

Request fields support string, string-list, integer, and boolean values. A
string-list flag takes a JSON array. `--body FILE` accepts a JSON object and
cannot be combined with field flags. Unknown fields, duplicate keys, invalid
Unicode, invalid types, and unknown or repeated flags are rejected. Input
limits are 256 arguments, 4,096 bytes per argument or string field, 65,536 total
argument bytes, and a 65,536-byte body file. Responses have the shared 4 MiB
limit. JSON validation also limits depth and node count. Output retains exact
JSON numbers. Error responses expose the HTTP status and omit response bodies.

Commands can write to a new file with `--output-file FILE`. The CLI reserves the
file with exclusive creation and mode 0600 before it sends the request. The
parent directory must exist and must not permit writes by other users. Existing
files, symlinks, and devices are rejected. The CLI syncs successful file output
and its directory. An empty reservation is removed after a request or response
validation error. A write or sync error keeps the file for inspection; it can
contain the only copy of a credential. No file failure falls back to stdout.
A successful response with no body creates an empty file when requested.

Commands marked `Sensitive` require `--output-file FILE` and refuse
`--output-file -`: their answer holds a secret, and stdout can go to a
terminal or a log. Other commands use stdout by default, and `--output-file -`
selects stdout explicitly. This output
choice does not change uncertain mutation results: check resource state before
a retry. A process crash can leave an empty or partial private file. The CLI
cannot recover a secret that the server returned only once.

OIDC login uses `login --url URL --issuer-url URL [--client-id ID]`.
The factory can set `OIDCClientID` as the default public client ID. Add
`--issuer-ca-file FILE` for an explicit provider CA. The API and provider have
separate CA settings. Browser login uses an external browser, S256 PKCE, a
random state and nonce, and an HTTP callback on `127.0.0.1` at a random port.
Register the `/callback` path with the provider and permit random loopback
ports. The provider must advertise S256 support. The CLI prints the URL and
tries `xdg-open`; the user can open the URL if that program is absent.

Use `--no-browser` for device login. The provider must support device
authorization. The CLI also sends PKCE proof for device grants when discovery
advertises S256. The CLI prints a verification URL and user code. It follows the
provider polling interval and adds five seconds after each `slow_down` result.
Login has a five-minute limit. Individual remote calls have a 15-second limit.
Transport errors stop login. The CLI requests only the `openid` scope. Password grants and client secrets
are not used.

Discovery and credential endpoints require HTTPS. Discovery can name separate
HTTPS origins, with a limit of eight origins. The supplied issuer is the trust
source for these endpoints. OIDC documents and token responses have a 64 KiB
limit. The generated runtime uses `coreos/go-oidc/v3` for signature verification.
It requires the exact issuer, one matching audience, a valid expiry and issue
time, and RS256, ES256, or PS256 signatures. It also checks the authorized party,
browser nonce, and access-token hash when present. This is a restricted OIDC
profile; providers that use other signing algorithms or multiple audiences
need a separate contract change and tests.

OIDC configuration stores access and refresh tokens in the private file.
Commands refresh near expiry. A persistent private lock file prevents concurrent
CLI processes from using the same refresh token. Lock acquisition has a
15-second limit. Before the token request, the CLI saves a pending state. It
saves the new session before an API request can use the token. A lost response or a failure before replacement leaves a pending state and
requires a new login. If the final directory sync fails, a later read can find
the complete replacement. After a crash, it can instead find the durable
pending state. Neither state permits a retry with the old refresh token. Refresh cannot change the subject, nonce, or
original authentication time. Logout attempts provider token revocation and
then removes local configuration, even if revocation fails. A revocation error
is reported. Provider browser cookies and other applications are outside this
logout operation. The lock file remains in place.

## The environment of the CLI

The CLI reads one environment variable of its own:

| Variable | Required | Description |
|---|---|---|
| `APPLY_CONFIG` | no | The file of the configuration of the CLI: the URL, the CA file, the token file or the OIDC session. When it has no value, the CLI uses `<service>/config.json` in the configuration folder of the user (`os.UserConfigDir`, such as `~/.config/applicants/config.json`). |

Set it on the computer of the user that runs the CLI, never on a pod of the
service. The component states it with `read_by_client: true`, so the platform
requirements, the environment variable reference, `stego env check` and the
Kubernetes base of the service do not list it (walkthrough 17, W17-D2).

## The apply command

`apply` creates or changes resources from a set of documents, as
`kubectl apply` does. It is in each CLI that lists resources in
`command.Application.Resources`; with no resources, it fails with
`apply is not configured`.

```
apply -f FILE|DIRECTORY|- [--dry-run] [-o json] [--output-file FILE]
```

- `-f` (or `--filename`) names one file, a folder or `-` for stdin. In a
  folder, apply reads each `.yaml`, `.yml` and `.json` file, at most 16
  folders deep and 1,024 entries. Each file has at most 64 KiB, and all of
  them have at most 1 MiB and 128 documents. A link is refused. `-k` is
  refused: give rendered files.
- `--dry-run` checks every document and contacts no server. Each document
  gets the status `validated`.
- `-o json` writes the results as a JSON list. Else each line is
  `<kind>/"<name>" <status>`.
- `--output-file FILE` writes the results to a new private file.

A document holds the JSON subset of YAML: no aliases, merge keys or custom
tags. Its keys are `apiVersion`, `kind`, `metadata` and `spec`, and no other:

```yaml
apiVersion: clinics/v1
kind: Clinic
metadata:
  name: north
spec:
  code: NO
```

With no `factory_package`, `apiVersion` is `<service>/v1`, with the name of
the service, such as `clinics/v1` for the service `clinics`. Apply refuses
another `apiVersion` with `unsupported apply API version`, and accepts a
document with no `apiVersion`.

Give the application each kind as a `command.ApplyResource`: the `Kind`
and the `APIVersion` that a document states, the `Path` of the collection,
and the fields of a create (`CreateFields`) and of a patch (`PatchFields`).
`metadata` holds `name`, `id` and `description`. A field of `metadata`, other
than `id`, goes into the body, so it must not also be in `spec`.

For each document, apply first finds the resource. With `metadata.id`, it
reads `<Path>/<id>`. Else it lists `<Path>` with `search=name = '<name>'`,
two values a page, and follows the `next` cursor. More than one match is
refused: give `metadata.id`. A resource with `ImmutableIdentity` is found by
those fields, and is never changed after its create.

Apply checks every document and finds every resource before the first
write. Then it sends a POST for a resource that it did not find, and a
PATCH for one that it found, in the order of the documents. The status of
each document is `created`, `configured`, `unchanged`, `failed`, `unknown`
or `not_attempted`. Apply stops at the first write that fails. The writes
before it stay. A status of `unknown` means that the write can have
occurred: read the resource before you run apply again.

A field that the create takes and the patch does not, such as a field with
`writable: create`, is create-only. When apply patches a resource that it
found, it leaves out each create-only field whose value in the document is
the value that the service returned, so you can apply the same documents
again. When the value is another one, apply refuses the documents before
the first write, with `apply cannot change the field <field> of <kind>
"<name>"`. A create-only field that the answer of the service does not hold
is refused in the same way, because apply cannot compare it. To
change a create-only field, use a command of the service that changes it, or
delete the resource and apply again when the collection has a delete.

A create that the service answers 409, because a resource of that name
exists, is looked up again, and apply sends no other write for it. The status
is `unchanged` when the lookup finds the resource with the values of the
document. Otherwise the status is `failed`, and the error says that a
resource with a unique value of the document exists, and that the resource
of this name has other values (apply again to patch it), or that the lookup
does not find it by its name, such as a resource that you may not read or a
resource with another unique value of the document (ST-SUP-RC04-OC36).

The list of a service can show a new resource only after its relationships
are written. So the CLI keeps the `Authz-Revocation-Location` of its last
successful write in its configuration file, and each apply lookup sends it in
`Authz-Revocation-After` with `Prefer: wait=10`. The list then waits for that
write before it reads, so an apply soon after a create finds the resource
(ST-SUP-RC04-OC37). Each configuration file keeps its own value; a new login
starts with none. The CLI keeps the value only in the configuration that sent
the write: after a login to another service, a write of the earlier one keeps
nothing. A lookup no longer sends a value older than 10 minutes, or one whose
`Authz-Revocation` answer is `complete`, `failed`, `parked`, `expired` or
`unknown`; the CLI then removes it from the configuration.

Apply decides each patch, and the status `unchanged`, from the resource as
its lookup read it. When another writer changes the resource after that read,
apply can report `unchanged` and send no patch, or send a patch that replaces
the change of the other writer, as `kubectl apply` can. When the patch of the
collection takes `If-Match`, the definitions state `ConditionalPatch`, and
apply sends `If-Match` with the generation that it read. Then a change after
the read gives the status `failed` with `the resource changed after apply
read it; apply again` (ST-SUP-RC04-OC38).

Apply compares a create-only field only when its type is a string, an
integer, a boolean or a list of strings. It compares a string as text: a
timestamp, such as `2026-01-01T00:00:00Z`, is the same value only when the
document gives it in the form that the service returns. A field of type
number or object has no apply field, so a document that gives it is refused
with `invalid apply field`. A secret field is not in the answer, so apply
cannot compare it. Leave such create-only fields out of a document that you
apply again (security review of PR #461, I1).

Commands marked for confirmation require `--yes`. Interactive prompts,
stdin bodies, legacy configuration migration, and automatic SDK generation
are not supplied by this version. Current file operations target Linux; other
platforms need build and file-semantics checks before support can be claimed.

Generated tests use a Record application. They check request types, validation,
private files, token rotation, redirects, malformed responses, no response-status
retry, exact number output, and definition limits. The existing HTTP tests cover
transport failure, request completion, cancellation, and bounded timeouts.

Generated OIDC tests use a TLS provider with application-neutral endpoints.
They check signed claims, PKCE, wrong and repeated callbacks, device polling,
concurrent refresh across processes, changed identity, failed session saves,
and revocation. The protocol contracts use [OIDC Core](https://openid.net/specs/openid-connect-core-1_0.html),
[native application OAuth](https://www.rfc-editor.org/rfc/rfc8252), and
[device authorization](https://www.rfc-editor.org/rfc/rfc8628).

The device PKCE extension is tested with [Keycloak](https://github.com/keycloak/keycloak/issues/9710).
The standard device flow is also tested without a PKCE advertisement.

Generated HTTPS clients use the telemetry runtime in their call context.
The HTTP client contract (a retired record, in git history) defines
completion, propagation, privacy, and resource bounds. The generated CLI now owns its runtime; see the
CLI contract (a retired record, in git history).
