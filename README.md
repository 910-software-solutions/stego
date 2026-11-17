# STEGO

Service Templates, Extensible Generation & Operations.

STEGO generates common service and client code from YAML declarations and
reusable components. The output is plain Go, with no framework dependency.
Application fills supply domain rules. The requirements are the
Lathe model under `data/`; `lathe check` reports what is designed, built and
proved. Production use requires validation for the selected application and
deployment.
The Kubernetes service component (a retired record, in git history) supplies common
container build files, restricted Pod settings, network policy, and HTTPS probes.
Its workload policies do not cover application-allocated namespaces. The
namespace allocator (a retired record, in git history) has an optional deny-all
policy with admission protection. Allowed traffic and live network enforcement
for the Hypershell Gateway profile remain open; the application has not enabled
that policy.

## Why

LLMs are increasingly writing code. But code involves two kinds of
complexity: essential (your business logic) and accidental (framework
wiring, boilerplate, conventions). LLMs make different accidental choices
every time. Across 100 services, that means 100 subtly different
implementations of the same patterns. Unreviewable. Ungreppable. Unknown
risk surface.

STEGO removes accidental complexity mechanically. Trusted components make
every accidental decision once. The only thing left in the YAML and in
the fills is essential complexity.

## How it works

1. Pick an **archetype** (e.g. `rest-crud`), a curated set of components
   that determines your architecture, conventions, and defaults.
2. Write a **service declaration** (`service.yaml`) with entities, operations,
   component settings, and slot bindings.
3. Write application code for domain rules. **Fills** implement typed slot
   contracts. The CLI component still takes a factory for its command
   definitions. Test each fill with `stego test`, and sign off its evidence
   with `stego fill sign`.
4. Run `stego apply` for deterministic Go and browser code generation.
   Validate, plan and apply stop on a bound fill that is not qualified.
   The output has no runtime dependency on STEGO.

Every service with data access runs on the runtime data path, the only data
path: its commands, its queries and its collection operations run on the
runtime library github.com/jsell-rh/stego/registry/runtime. Its archetype must
hold the command and query components; a service that declares entities,
commands or queries without them is refused. Its main reads the deployment settings from the environment:
`DATABASE_URL` or `DATABASE_URL_FILE` for the runtime role, `STEGO_SPICEDB_ENDPOINT`
and `STEGO_SPICEDB_KEY_FILE` for SpiceDB, `STEGO_QUERY_CURSOR_KEY_FILE` for the key
of the list cursors, and `STEGO_SERVICE_VERSION` for the release that each audit
row names. It takes the issuer and the audience that the declaration states for
jwt-auth; when the declaration states none, it reads `STEGO_AUTHN_ISSUER` and
`STEGO_AUTHN_AUDIENCE`. When the declaration states no public key file, it reads
the key document of the issuer from `STEGO_AUTHN_KEYS_URL` or `STEGO_AUTHN_KEYS_FILE`.
With rh-sso-auth it finds the key document through the discovery document of the
declared issuer, a Red Hat SSO realm, takes the tenant from the claim `org_id`, and
reads no key document setting. `STEGO_AUTHN_CA_FILE` adds certificate authorities
for the key document or the discovery document.
The binary runs the modes that the `--mode` option or `STEGO_MODE` selects: api,
worker and controller, all by default. The worker mode also reads
`STEGO_WORKER_DATABASE_URL` or `STEGO_WORKER_DATABASE_URL_FILE` for the worker role,
which runs the outbox worker and the after slots, and `STEGO_RELAY_DATABASE_URL` or
`STEGO_RELAY_DATABASE_URL_FILE` for the relay role, which writes the relationships to
SpiceDB. The worker mode sends each audit entry and anchor to the audit sink: with
`STEGO_AUDIT_SINK_URL` to an HTTPS endpoint as OTLP/HTTP log records, with the token of
`STEGO_AUDIT_SINK_TOKEN_FILE` (read again on each send; `STEGO_AUDIT_SINK_AUTH_HEADER` names
another header for it), the certificate authorities of `STEGO_AUDIT_SINK_CA_FILE` and the client
certificate of `STEGO_AUDIT_SINK_CLIENT_CERT_FILE` and `STEGO_AUDIT_SINK_CLIENT_KEY_FILE`; without
it, to the table `stego_audit_sink.entries` in the service database. A separate sink is the
stronger choice for production, because an administrator of the service database can change
both. With the event-publisher mixin, each write of a collection puts its change event in the
outbox in the transaction of its command, and the worker mode sends it to Kafka: it reads
`STEGO_KAFKA_BROKERS` (or `STEGO_KAFKA_BROKERS_FILE`), `STEGO_KAFKA_TOPIC` for one declared
topic (else each event goes to the topic of its entity, such as `user-management.user`, so the
actions of one row keep their order; the header `stego-event-action` holds the action),
`STEGO_KAFKA_CA_FILE`, `STEGO_KAFKA_CLIENT_CERT_FILE` and `STEGO_KAFKA_CLIENT_KEY_FILE` for
verified TLS, and `STEGO_KAFKA_SASL_MECHANISM` (SCRAM-SHA-512 with `STEGO_KAFKA_USERNAME` and
`STEGO_KAFKA_PASSWORD_FILE`, or OAUTHBEARER with `STEGO_KAFKA_TOKEN_FILE`). The worker mode does
not start without them. It refuses a password, token or client key file that its group may write
or that others may use: in Kubernetes, mount these secrets with `defaultMode: 0400` or `0440`. It
refuses a CA file or a `_FILE` setting that its group or others may write. An event holds the entity,
the action and the ID of the row, and no field value. The controller mode runs the audit jobs under one lease. `/readyz/modes`
answers the readiness of the worker and controller modes.
Its binary with the one argument `migrate` is the migration job: it reads
`STEGO_MIGRATION_DATABASE_URL` or `STEGO_MIGRATION_DATABASE_URL_FILE`, runs as the
owner role over verified TLS, refuses a database with tables that it did not make,
and writes the authorization schema, the tables and the tables of the runtime
library before the service starts. It runs again on every release.

The [Go SDK component](registry/components/go-sdk/README.md) generates typed
HTTPS clients from captured OpenAPI contracts. Its transport and optional
telemetry come from STEGO.

The [TypeScript SDK](registry/components/typescript-sdk/README.md) generates
same-origin browser clients, typed models, CSRF transport, and runtime contract
checks. Application code supplies its domain UI.

The [browser backend](registry/components/browser-backend/README.md) generates a
separate Go service for login, encrypted sessions, token renewal, sign-out,
static assets, and the API proxy. It uses the shared health and telemetry code.
Applications supply their pages and domain API. The real Hypershell protocol
check covers Gateway access and session recovery; a complete UI and production
browser deployment remain open.

The [CLI component](registry/components/cli-application/README.md) generates a
separate executable, command parsing, private configuration, and an HTTPS
client. Application code declares command names, paths, and fields. Common
code validates those declarations, binds path parameters, and reserves private
output files before credential requests. It also supplies OIDC browser and
device login, signed ID-token checks, and stored-session refresh. The component
tests use a Record
application. Hypershell tests exercise actual Gateway and service-account
workflows through the generated runtime.

## Prerequisites

- Go 1.26.11+ ([install](https://go.dev/dl/))

## Build from source

```bash
git clone https://github.com/jsell-rh/stego.git
cd stego
go build -o stego ./cmd/stego/
go test ./...
```
This produces a `stego` binary in the repo root.

`stego version --json` reports the commit only for a build from a clone. In a git worktree, `.git` is a file and
not a folder, and the Go toolchain (1.26) then records no version control data, so `vcs` and `revision` are
`unknown`. The CI and the release builds run in a clone with `-buildvcs=true`, and they record the commit.

## Quick start

Set up environment and initialize a new project:

```bash
export STEGO_REGISTRY=/path/to/stego/registry
export PATH="/path/to/stego:$PATH"
mkdir my-service && cd my-service
git init
stego init --archetype rest-crud
```

The scaffold of each archetype passes `stego validate` and builds as
written. An archetype that cannot be empty writes its example content from the
registry: `rpc-service` writes a proto file with one method, one query on it
with its fill, and the settings that the query needs; `browser-service` writes
an API prefix, one route and an index page. The service takes the name of the
project folder, and the fill imports the module path of `STEGO_MODULE` (or
`github.com/example/service`): give the same value to each later command. Use
`stego init --archetype <name> --empty` for a scaffold with no example
content. It then names each setting that you must give before `stego validate`
accepts the declaration.

Make the project in its own Git repository. `stego init` writes
`.stego/.gitignore`, so the apply lock and the saved plan, which are local
state, stay out of the commit and out of the build. This file keeps the two
files out of a new service only. If a service committed them before, remove
them from the index once and commit the change:

```bash
git rm --cached .stego/apply.lock .stego/plan.yaml
git commit -m "Stop tracking the local STEGO state"
```

If only one of the two files is in the commit, give only that file.
Commit `.stego/config.yaml`
(the registry pin) and `.stego/state.yaml` (the record). Each shipped archetype generates
the service on the runtime data path: generated commands and queries carry all
data access, in the transaction of one tenant, and each operation is decided
for the verified caller. Until the former data path is removed, STEGO reads the
record of the repository's HEAD commit to choose the data path, and it refuses
a project with no record outside a Git repository.

Consumers can compose a pinned common registry with local application
extensions (a retired record, in git history). Common component copies are not
required. Application output paths use `component_namespaces`; offline builds
can select an explicit verified vendored checkout.
Use the path of your STEGO checkout for `/path/to/stego`.
This creates a `service.yaml` scaffold and a `fills/` directory. Edit
`service.yaml` with your entities and operations:

```yaml
kind: service
name: my-service
archetype: rest-crud
language: go
base_path: /api/my-svc/v1
error_type_base: https://api.example.com/errors/

tenancy:
  kind: pooled
  key: tenant_id
  claim: tenant
  provisioning: {resource_type: service, provision: provision, retire: retire}
authorization:
  provider: spicedb
  schema_path: authz/schema.zed
  subject_from_identity: user
audit:
  level: change
overrides:
  postgres-adapter:
    schema_generation: tables-v1
    writer_lease: "off"

entities:
  - name: Todo
    resource_type: todo
    permissions: {create: public, read: view, list: view, update: edit, delete: edit}
    public_reasons: {create: a new todo has no resource to decide on before it exists}
    fields:
      - { name: title, type: string, min_length: 1, max_length: 255 }
      - { name: completed, type: bool, default: false }

collections:
  todos:
    entity: Todo
    operations: [create, read, update, delete, list]
```

The service needs pooled tenancy, SpiceDB authorization and the audit log. Each entity names
its resource type and the permission of each operation. A create of a
top-level entity is public, with a reason. A pooled service also names its tenant
provisioning: the resource type, with a definition of its own, on which an operator
is allowed to provision and retire tenants.

A collection is served at a path that STEGO makes from its entity name, not
from the collection name: the base path, then the entity name in lower case
and in the plural. Thus the collection `todos` of the entity `Todo` is served
at `/api/my-svc/v1/todos`, and a collection of the entity `StockItem` is served
at `/api/my-svc/v1/stockitems`, also when the collection has the name
`stock-items`. A collection that is scoped to a parent entity puts the path of
the parent's collection and the scope parameter before it. To use another
path, give the collection `path_prefix`, for example `path_prefix:
/stock-items`, which comes after the base path.

A route of a scoped collection names each ancestor of the row, such as
`/workspaces/{workspace_id}/projects/{project_id}/tickets/{id}`. The handler
decides a permission only on the deepest resource that the route names: the
row for a read, an update, a patch and a delete, and the parent for a list, a
create and an upsert. For each ancestor above it, the handler checks only that
the row exists in the tenant and is of the ancestor above it in the path. A
missing, wrong or denied path gives the same 404. Thus a project that is shared
with a user is open to that user through these routes, also when the user may
not view the workspace. When a row must also need the permission of its parent,
write that rule in the schema as an intersection, for example
`permission view = viewer & parent->view`. A caller with no grant on the deepest
resource learns nothing of the ancestors. A caller with a grant on it learns no
field of an ancestor, but on a path of two or more levels it can find out, from
200 against 404, whether an ancestor above the parent is of the next ancestor,
and the `href` of the row names each ancestor.

Put the SpiceDB schema at `authz/schema.zed`:

```zed
definition user {}

definition service {
	relation operator: user
	permission provision = operator
	permission retire = operator
}

definition todo {
	relation owner: user
	relation viewer: user
	permission view = owner + viewer
	permission edit = owner
}
```

A denied operation answers 404, the same answer as a missing row, also when the
caller may read the row. For example, a viewer who reads a todo with 200 gets 404
on its delete. So an answer never tells a caller that a row it may not change
exists. The owner decided this rule for each operation on 2026-11-11
(ST-SUP-RC29-OC02, ST-SUP-RP11-LC06). A command or a query refusal that the
declaration names keeps its own status, such as 403.

Generate and build:

```bash
stego validate      # check service.yaml against registry
stego plan          # see what will be generated
stego apply         # generate code into out/
stego deps          # resolve and check project dependencies
go build -o service ./out
```

Run the following STEGO commands from the project directory that contains
`service.yaml`. The build command keeps that working directory unchanged.

`stego validate` prints every fault of the declaration, or that there is none.
With `--json` it prints one JSON document for an agent: `accepted`, the
`faults` (each with its rule, path, value found, allowed values and message),
and the `settings` that checking resolved. Each setting has its `path`, its
`value` and `default`, which is `true` when checking filled the value in
because the declaration leaves it out, such as `idempotency.retention` with
`24h`, or `tenancy.kind` with `pooled` and `audit.level` with `change` for a
declaration that says nothing of tenancy or audit. A default that the
declaration leaves is a setting that is not a default, such as `tenancy.kind`
with `single`, and its reason is the setting `tenancy.reason` or
`audit.reason`. Each command gets `commands[i].retry_on_conflict` with `0` and
`commands[i].lock_timeout` with `2s` when it leaves them out, each slot
binding gets `slots[i].runs_as` with `tenant` when it does not run as system, and each
observation gets `observations[i].counts` with `attempt`. A declaration that
names no modes gets `modes` with `api`, `worker` and `controller`, and each worker
with an `every` trigger gets `workers[i].single` with `true` and, when it is
single, `workers[i].lease_ttl` with `15s`. Each schema change with a step gets
`schema_changes[i].phase`, the phase of its last step in `schema_history`:
`expand` while it is open, `contract` when it is finished.
It also holds the `warnings`: notices about parts of the declaration that
checking accepts, each with the parts of a fault, such as the mixin
`event-publisher`, a deprecated alias of `durable-events` and `kafka-events`.
Without `--json`, the warnings come after the faults. A warning does not change
`accepted` or the exit code.

Run `stego deps` after apply and after changes to application imports. It runs Go
module resolution, module verification, and package dependency checks. It uses
temporary module files, then saves `go.mod` and `go.sum` with transaction recovery.
Commit both files. A command failure or an input change stops the update.
The command has a five-minute limit. It checks up to 10,000 Go source and module
files, with a total size limit of 256 MiB. Local replacement directories must
exist. Their Go source and module files are checked for changes too.

Dependency resolution uses `GOWORK=off` and clears `GOFLAGS` for its Go commands.
Other Go environment settings, including private module and proxy settings,
remain in effect. Plan and apply do not resolve or download dependencies.
These checks do not replace application tests or dependency security review.

Adapter version 4 requires verified database TLS by default. Configure
`sslmode=verify-full` and a trusted server certificate. The
database TLS contract (a retired record, in git history) describes private trust roots,
host fallbacks, and the explicit loopback test exception.

Set `DATABASE_URL` or `DATABASE_URL_FILE` for the service database. The file
setting supports mounted secrets with bounded reads and private file permissions.
See database secret sources (a retired record, in git history) for the source
rules and restart behavior.

The generated HTTP server uses a five-second header deadline, 30-second read and
write deadlines, a 60-second idle deadline, and a 32 KiB header limit setting.
SIGINT and SIGTERM start shutdown. Active requests have ten seconds to finish.
After that interval, the server closes remaining connections and reports an
error. Deferred resource cleanup runs before process exit. These defaults serve
the current request-response API. Long-lived streams need a separate deadline
policy. Network deadlines do not stop application code that ignores cancellation.

The service of this quick start is on the runtime data path. Before you start
it, set the `STEGO_AUTHN_` settings above. Incomplete settings stop startup.

Add business logic via fills:

```bash
stego fill create admin-policy --slot before_create --collection todos
stego apply         # generate the shared slot contracts
# implement fills/admin-policy/fill.go and add its tests
stego deps          # include dependencies used by the fill
stego test          # run fill tests and record their evidence
stego fill sign admin-policy   # sign off the evidence with your key
stego fill status   # show each bound fill and its qualification
# add the binding shown below to service.yaml, then apply again
stego apply
```

```yaml
slots:
  - collection: todos
    slot: before_create
    gate: [admin-policy]
```

New fill methods return an error until you implement them. They use the generated
slot types and include a constructor and an interface check. STEGO does not
replace existing fill files.

The fill of a command, a query or a worker implements the slot
`command.command`, `query.query` or `worker.worker`. Declare the command, query
or worker with its `fill` first. Then `stego fill create` writes its fill, also
before the apply that writes its contract:

```bash
stego fill create place-order --slot command.command   # New() gives the fill
stego fill create open-orders --slot query.query       # the function Fill
stego fill create fulfill-orders --slot worker.worker  # New(), for the trigger
stego apply         # write the contracts in out/commands, out/queries, out/workers
stego deps          # resolve the modules, then check the fills again
go build ./out
```

Each unfinished fill returns `ErrNotImplemented`, and its test expects that
error. Change the test when you write the fill.

A fill must be qualified before a binding can use it. `stego test` runs the
fill tests and, when they pass, records the evidence in the fill's `fill.yaml`:
the digests of the fill's files and of its slot contract, and the compiler that
ran the tests. `stego fill sign <name>` signs that evidence with your own key,
which git config names (`user.signingkey`, with `gpg.format ssh` for an SSH
key). STEGO never makes or uses a key of its own. The project's `SIGNERS` file
lists the keys that may sign, in the SSH allowed signers format. A fill is
qualified when its evidence matches its current files and contract and a key
in `SIGNERS` signed it. A change to a fill's files or to its contract makes the
fill stale, so run `stego test` and `stego fill sign` again.

Apply saves a transaction record before it changes output. If apply stops before
completion, run `stego recover`. Recovery verifies the record and all affected
files before it completes the saved changes. A conflicting file stops recovery.
Plan and drift refuse to report complete output while a transaction is pending.
Source edits made after an interruption are preserved for the next plan.

Do not remove `.stego/apply.lock` while a STEGO process runs. The file remains
after normal use; the operating system releases its lock when the process stops.
One apply can change up to 64 MiB of file contents. Its saved transaction record
has a 128 MiB limit. Recovery tests cover process interruption on Linux. Other
programs can still observe separate file replacements during apply.

## Run the example

A complete example with fills is included in `examples/user-management/`.
It runs on the runtime data path and demonstrates the rest-crud archetype:

- **Tenancy and authorization**: each operation runs in the caller's tenant and
  is decided in SpiceDB (`authz/schema.zed`), and each write leaves an audit row
- **Collections** with multi-path access (org-scoped users + global user list)
- **base_path** for route prefixing (`/api/user-mgmt/v1`)
- **RFC 9457** error responses with `application/problem+json`
- **Envelope** response format with cursor pagination (`items`, `next`)
- **Patch** operation with pointer-field request struct for partial updates
- **Upsert** with natural-key conflict resolution
- **List search** (`?search=field = 'value' and ...` on list endpoints)
- **Implicit fields** under path prefixes (`/user-activity`, `/org-activity`)
- **OpenAPI validation** middleware via kin-openapi
- **Slot wiring**: gate (RBAC policy), fan-out (welcome message), and
  short-circuit chain (org name validation + provisioning)
- **Change events**: the event-publisher mixin sends the change event of each
  write to Kafka through the outbox

```bash
export STEGO_REGISTRY=/path/to/stego/registry
export PATH="/path/to/stego:$PATH"
cd /path/to/stego/examples/user-management
stego validate
stego plan
stego apply
stego deps
go build -o service ./out
```

## Commands

This table comes from the CLI command records of the spec. Do not edit it by
hand; see [Specification checks](#specification-checks).

<!-- stego:generated cli-commands -->
| Command | Usage |
|---|---|
| `stego apply` | `stego apply [--saved-plan] [--replace-hand-edits] [--base <ref>] [--strict]` |
| `stego architecture export` | `stego architecture export --format structurizr\|mermaid [--view <view>]` |
| `stego architecture facts` | `stego architecture facts --topic personal-data\|tenant-isolation\|access\|audit\|data-protection\|owner-code\|retention\|configuration [--json]` |
| `stego assets` | `stego assets --directory <folder> --output <zip>` |
| `stego build` | `stego build --source <repo> --revision <commit> [--module <path>] --target <path> --go <sdk>/bin/go --work <folder> --output <folder> --module-cache <folder>` |
| `stego build download` | `stego build download --source <repo> --revision <commit> [--module <path>] --target <path> --go <sdk>/bin/go --output <folder> [--runtime-folder <stego checkout>/registry/runtime]` |
| `stego build verify` | `stego build verify --record <file> --record-sha256 <digest> --artifact <file>` |
| `stego build verify-source` | `stego build verify-source --record <file> --record-sha256 <digest> --source <folder>` |
| `stego compat` | `stego compat --base <ref> [--json]` |
| `stego deps` | `stego deps` |
| `stego drift` | `stego drift` |
| `stego env check` | `stego env check --requirements <platform-requirements.yaml> <environment-file>` |
| `stego fill create` | `stego fill create <name> --slot <slot> [--component <component>] [--collection <collection>]` |
| `stego fill sign` | `stego fill sign <name>` |
| `stego fill status` | `stego fill status` |
| `stego fleet` | `stego fleet --digest <digest> <checkout>...` |
| `stego fleet vex` | `stego fleet vex --registry <folder> <checkout>...` |
| `stego image build` | `stego image build (--build-record <file> --build-record-sha256 <digest> --application <file> \| --after <file> --build-result <folder>) --trust-store <file> --trust-store-sha256 <digest> [--entrypoint <name>] --output <folder>` |
| `stego image export` | `stego image export --record <file> --record-sha256 <digest> --image <folder> --build-record <file> --work <folder>` |
| `stego image publish` | `stego image publish (--record <file> --record-sha256 <digest> --image <folder> --build-record <file> \| --after <file> --image <folder>) --spdx-bundle <file> --cyclonedx-bundle <file> --signer-issuer <url> --signer-subject <identity> --gh <path> --source-repository <owner/name> --repository <repo> --registry-ca <file> --registry-ca-sha256 <digest> --credentials <file> [--token-origin <origin>]... [--blob-origin <origin>]... --work <folder>` |
| `stego image retrieve` | `stego image retrieve --record <file> --record-sha256 <digest> --build-record <file> --repository <repo> --registry-ca <file> --registry-ca-sha256 <digest> --credentials <file> [--token-origin <origin>]... [--blob-origin <origin>]... --work <folder> --spdx-bundle <file> --cyclonedx-bundle <file> --signer-issuer <url> --signer-subject <identity> --gh <path> --source-repository <owner/name>` |
| `stego image sign` | `stego image sign --image <folder> (--record-sha256 <digest> \| --after <file>) --spdx-bundle <file> --cyclonedx-bundle <file> --signer-issuer <url> --signer-subject <identity> --gh <path> --source-repository <owner/name>` |
| `stego image verify` | `stego image verify --record <file> --record-sha256 <digest> --image <folder> --build-record <file> --work <folder> --spdx-bundle <file> --cyclonedx-bundle <file> --signer-issuer <url> --signer-subject <identity> --gh <path> --source-repository <owner/name>` |
| `stego init` | `stego init --archetype <name> [--empty]` |
| `stego lifecycle supersede` | `stego lifecycle supersede --registry <folder> (--component <name> \| --runtime <module>) --digest <digest> --successor <digest> [--successor-component <name>] --fails-after <YYYY-MM-DD> --needs-declaration-change yes\|no --needs-fill-change yes\|no --summary <text> [--reference <url>]...` |
| `stego lifecycle withdraw` | `stego lifecycle withdraw --registry <folder> (--component <name> \| --runtime <module>) --digest <digest> --fails-after <YYYY-MM-DD> --reason <text> [--reference <url>]...` |
| `stego plan` | `stego plan [--show] [--base <ref>] [--strict]` |
| `stego recover` | `stego recover` |
| `stego registry compose` | `stego registry compose` |
| `stego registry consumers` | `stego registry consumers <component> <checkout>...` |
| `stego registry fills` | `stego registry fills --slot <slot> [--component <component>]` |
| `stego registry inspect` | `stego registry inspect <component> \| --archetype <name> [--json]` |
| `stego registry search` | `stego registry search [--provides <port>] [--requires <port>] [--slot <slot>]` |
| `stego registry sunsets` | `stego registry sunsets <checkout>...` |
| `stego test` | `stego test [--] [<test arguments>...]` |
| `stego validate` | `stego validate [--json] [--strict]` |
| `stego version` | `stego version [--json]` |
<!-- stego:end -->

The environment file of `stego env check` has a schema that STEGO owns: see
[docs/environment-file.md](docs/environment-file.md), with one example file.

## Concepts

Six nouns: **Archetype**, **Component**, **Mixin**, **Service Declaration**, **Collection**, **Fill**.
Seven operators: `use`, `with`, `mixin`, `gate`, `chain`, `fan-out`, `map`.

The full specification is the Lathe model under `data/`; `npx lathe serve`
browses it. The prose specs are retired; each record that came from one cites it at the commit that last held it.

### Specification checks

`lathe check` loads the model and fails on any error: data that breaks the
model, a test file without the ID of its test record, a manual test in STEGO's
own spec, a component or runtime package no aggregate lists. The workflow
`spec.yml` runs it on every pull request with the Lathe commit it pins, which
moves together with the schema in `lathe.config.json`. Write the data only
through `lathe <aggregate> <command>` or the Lathe plugin's tools, never by
editing `data/*.jsonl`. For merges, enable Lathe's row-by-row driver once per
clone:

```sh
git config merge.lathe.driver './node_modules/.bin/lathe merge-data %O %A %B %P'
git config merge.lathe.name 'Lathe row-by-row merge of data/*.jsonl'
```

The user doc standard (ST-STD-DOCS) keeps this README current. The
docs it holds are the files that `internal/assure/testdata/user-docs.txt`
lists. Each `stego` command and flag in them must exist in the spec and be
built, each relative link must resolve, and each whole service declaration must
pass `stego validate`. The change gate refuses a change that alters a command
that a doc names, unless the same change updates that doc. The command table
above is generated; after a change to a CLI command record, write it again
and commit the result:

```sh
go test ./internal/assure -run TestOwnTree -update-docs
```

## Release notes

### Nested routes: a grant on the deepest resource is enough; check your schema

Before this release, a route of a scoped collection also decided the read
permission of each ancestor in the path. Now it decides a permission only on
the deepest resource that the route names (ST-SUP-Q156). For each ancestor
above it, it checks only that the row exists and is of the ancestor above it.

This can give more access to an existing service. For example, with
`permission edit = owner + organization->edit` on a member, an owner of the
member who may not view the organization could not change the member before.
Now that owner can. `stego validate` and `stego plan` warn about each such
permission (the rule `scoped-access`). Each warning names the entity, the
permission and the parent, and gives the intersection that keeps the earlier
access, such as:

```zed
permission edit = (owner + organization->edit) & organization->view
```

Read each warning, and change the schema where the earlier access is the one
you want.

The user-management examples do this for `member`: its view is now
`organization->view` and its edit is
`(owner + organization->edit) & organization->view`. Thus an owner of a member
who may not view its organization no longer finds that member in the list
`all-users` either.

## License

Apache License 2.0. See [LICENSE](LICENSE).
