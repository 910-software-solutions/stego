# Commands, queries and workers

This page tells you how to declare commands, queries, workers and
observations in `service.yaml`, and how to write their fills. It also tells
you how to give the protobuf messages of the commands (`proto_files`).

- A **command** is a change that your own code makes in one transaction, such
  as "place an order". The runtime decides the permission of the caller
  before your fill runs.
- A **query** is a read that your own code makes, in a read-only transaction.
- A **worker** is background work that your own code does at an interval, or
  for each message that a command or another worker stages.
- An **observation** is a typed signal that a fill can emit, such as "the
  stock of an item is low".

Each command, query and worker names one fill. The fill is the Go package in
`fills/<fill name>/`. STEGO generates the HTTP handler, the transaction, the
permission decision, the audit row and the typed contract that the fill
implements.

## The example service

The example is the service `inventory`. It has warehouses, stock items and
orders. The create of a warehouse is `public`, with a reason: this is the
choice of the example, because a new warehouse has no parent resource to
decide on. It is not the usual setting. Give each operation the permission
that your schema needs. The example declares:

- The command `place-order`, which reserves stock for a draft order and stages
  a message for the fulfillment worker.
- The command `grant-clerk`, which writes a relationship in SpiceDB.
- The list query `open-orders` and the get query `order-summary`.
- The worker `fulfill-orders`, which consumes the messages of `place-order`.
- The worker `low-stock-scan`, which runs every minute and emits the
  observation `low_stock`.
- The worker `ship-orders`, which runs every minute, creates one shipment for
  each fulfilled order and marks the order `shipped`.

A test of the STEGO repository runs `stego validate` on this declaration,
with the schema and the proto file below (ST-STD-DOCS-R02). Each smaller
part of a declaration on this page is a part of this declaration.

```yaml
kind: service
name: inventory
archetype: rest-crud
language: go
base_path: /api/inventory/v1
error_type_base: https://errors.example.test/inventory/

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

entities:
  - name: Warehouse
    resource_type: warehouse
    permissions: {create: public, read: view, list: view, update: manage, delete: manage}
    public_reasons: {create: a new warehouse has no resource to decide on before it exists}
    relationships:
      - {relation: manager, subject_source: caller, subject_type: user}
    fields:
      - { name: code, type: string, unique: true, min_length: 2, max_length: 16 }
  - name: StockItem
    resource_type: stock_item
    permissions: {create: public, read: view, list: view, update: edit, delete: manage}
    public_reasons: {create: a new stock item is decided on edit of its warehouse through the reference warehouse_id}
    references:
      warehouse_id: {relation: warehouse, permission: edit}
    fields:
      - { name: warehouse_id, type: ref, to: Warehouse }
      - { name: sku, type: string, min_length: 3, max_length: 40 }
      - { name: quantity, type: int32, min: 0, max: 1000000 }
      - { name: reserved, type: int32, min: 0, max: 1000000, default: 0 }
  - name: Order
    resource_type: order
    permissions: {create: public, read: view, list: view, update: edit, delete: manage}
    public_reasons: {create: a new order is decided on edit of its warehouse through the reference warehouse_id}
    references:
      warehouse_id: {relation: warehouse, permission: edit}
    fields:
      - { name: warehouse_id, type: ref, to: Warehouse }
      - { name: status, type: enum, values: [draft, placed, fulfilled, shipped], default: draft }
      - { name: total_units, type: int32, min: 0, max: 1000000, default: 0 }
  - name: Shipment
    resource_type: shipment
    permissions: {create: ship, read: view, list: view, update: manage, delete: manage}
    references:
      order_id: {relation: order, permission: ship}
    fields:
      - { name: order_id, type: ref, to: Order, unique: true }
      - { name: units, type: int32, min: 0, max: 1000000 }

collections:
  warehouses:
    entity: Warehouse
    operations: [create, read, list]
  warehouse-stock:
    entity: StockItem
    scope: { warehouse_id: Warehouse }
    operations: [create, read, update, list]
  warehouse-orders:
    entity: Order
    scope: { warehouse_id: Warehouse }
    operations: [create, read, list]

commands:
  - name: place-order
    writes: [Order, StockItem]
    resource_type: order
    permission: edit
    resource_member: order_id
    fill: place-order
    route: POST /orders/{order_id}/place
    effects:
      - {name: fulfill, destination: fulfillment}
    refusals:
      - {name: short_stock, code: stock.short, status: 409}
      - {name: not_draft, code: order.not_draft, status: 409}
    lists:
      - name: stock-of-warehouse
        entity: StockItem
        filter:
          - {field: warehouse_id, op: equals, param: warehouse_id}
  - name: grant-clerk
    writes: [Warehouse]
    resource_type: warehouse
    permission: manage
    resource_member: warehouse_id
    fill: grant-clerk
    route: POST /warehouses/{warehouse_id}/clerks
    relationships_written: [warehouse#clerk]

queries:
  - name: open-orders
    reads: [Order]
    resource_type: order
    permission: view
    fill: open-orders
    route: GET /open-orders
  - name: order-summary
    reads: [Order]
    resource_type: order
    permission: view
    resource_member: orderId
    fill: order-summary
    route: GET /orders/{orderId}/summary

workers:
  - name: fulfill-orders
    trigger: consumes fulfillment
    writes: [Order, StockItem]
    resource_type: order
    permission: fulfill
    fill: fulfill-orders
  - name: low-stock-scan
    trigger: every 1m
    writes: [StockItem]
    resource_type: stock_item
    permission: scan
    fill: low-stock-scan
    lists:
      - name: low-items
        entity: StockItem
        filter:
          - {field: quantity, op: less_than, value: 10}
  - name: ship-orders
    trigger: every 1m
    writes: [Order, Shipment]
    resource_type: order
    permission: ship
    fill: ship-orders
    lists:
      - name: fulfilled-orders
        entity: Order
        filter:
          - {field: status, op: equals, value: fulfilled}
      - name: shipments-of-order
        entity: Shipment
        filter:
          - {field: order_id, op: equals, param: order_id}

observations:
  - name: low_stock
    kind: event
    severity: warn
    fields:
      - {name: level, type: "enum(low | empty)"}
      - {name: free, type: "measure(1, 0..1000000)"}
    emitted_by: [low-stock-scan]

experimental_acceptances:
  - {component: worker, accepted_until: 2027-03-31, reason: the fulfillment worker reacts to placed orders}

overrides:
  postgres-adapter: { schema_generation: tables-v1 }
  command: { proto_files: [{path: proto/inventory.proto, import_path: inventory.proto}] }
```

The SpiceDB schema is at `authz/schema.zed`:

```zed
definition user {}

definition service {
	relation operator: user
	permission provision = operator
	permission retire = operator
}

definition warehouse {
	relation manager: user
	relation clerk: user
	permission view = manager + clerk
	permission edit = manager + clerk
	permission manage = manager
}

definition stock_item {
	relation warehouse: warehouse
	permission view = warehouse->view
	permission edit = warehouse->edit
	permission manage = warehouse->manage

	relation tenant: tenant
	permission fulfill = tenant->worker_fulfill_orders
	permission scan = tenant->worker_low_stock_scan
}

definition order {
	relation warehouse: warehouse
	permission view = warehouse->view
	permission edit = warehouse->edit
	permission manage = warehouse->manage

	relation tenant: tenant
	permission fulfill = tenant->worker_fulfill_orders
	permission ship = tenant->worker_ship_orders
}

definition shipment {
	relation order: order
	permission view = order->view
	permission edit = order->edit
	permission manage = order->manage

	relation tenant: tenant
	permission ship = tenant->worker_ship_orders
}

definition service_account {}

definition tenant {
	relation worker_fulfill_orders: service_account
	relation worker_low_stock_scan: service_account
	relation worker_ship_orders: service_account
}

definition outbox_destination {
	relation operator: user
	permission resolve = operator
}
```

The messages of the commands are at `proto/inventory.proto`:

```proto
syntax = "proto3";

package inventory.v1;

// PlaceOrderRequest places a draft order: it reserves the stock of one item.
message PlaceOrderRequest {
  string order_id = 1;
  string stock_item_id = 2;
  int32 quantity = 3;
}

message PlaceOrderResponse {
  string order_id = 1;
  string status = 2;
  int32 total_units = 3;
}

// GrantClerkRequest makes a user a clerk of a warehouse.
message GrantClerkRequest {
  string warehouse_id = 1;
  string user = 2;
}

message GrantClerkResponse {
  string warehouse_id = 1;
}
```

## The messages of the commands: proto_files

The request and the response of a command are protobuf messages. For the
command `place-order`, STEGO looks for the messages `PlaceOrderRequest` and
`PlaceOrderResponse`: the command name in upper camel case, then `Request` or
`Response`.

Put the messages in one or more proto files in your project. Name each file
in the setting `proto_files` of the component `command`:

<!-- excerpt -->
```yaml
  command: { proto_files: [{path: proto/inventory.proto, import_path: inventory.proto}] }
```

- `path` is the path of the file from the project folder.
- `import_path` is the name that other proto files use to import this file.

The rules:

- Each file is proto3.
- A file holds messages only. It has no `service`. `stego validate` refuses a
  file with a service: `the proto file inventory.proto declares a service;
  the command messages hold messages only`.
- Two files do not declare a message with the same name.
- Do not give `proto_files` when the declaration has no command. Validate
  refuses it.
- You do not need `option go_package`. STEGO puts the Go code of the messages
  in the package `out/commands`, beside the command contract.

The body of a command request is the JSON form of the request message
(protojson). The JSON field names are in lower camel case, such as
`{"stockItemId": "...", "quantity": 2}`. protojson also accepts the proto
field names, such as `stock_item_id`.

### A secret member

Mark each member that holds a secret, such as a PIN in a request or an API
key in a response, with the standard protobuf option `debug_redact`. Without
the mark, STEGO does not know that the member is a secret: the generated CLI
gives it a flag, which puts the value in the shell history and the process
list. `stego validate` warns (`unmarked-secret-member`) of a request member
that has the name of a secret field of an entity that the command writes and
has no mark.

For example, write `string pin = 2 [debug_redact = true];` in the request
message `IssueKeyRequest` and `string api_key = 1 [debug_redact = true];` in
the response message `IssueKeyResponse`. The fill makes the API key with
`command.NewToken(32)` (from runtime v0.163.0), never from an ID or the time
(see [The imports of a fill](fills.md#the-imports-of-a-fill)). A secret in a
response is only for a secret that the command issues: the stored answer
keeps it for the retention of the idempotency key, sealed with the key ring,
and a retry with the same key gets it again. Never put a secret field of an
entity in a response.

From rest-api 4.22.0, cli-application 1.13.0 and command 1.16.0:

- The OpenAPI document gives the member `x-sensitive: true`, and the format
  `password` for a string. A member of a request body is also `writeOnly`.
- The generated CLI gives no flag to a secret request member. Give it in
  `--body FILE`. A command whose answer holds a secret member is Sensitive:
  it writes the answer only to `--output-file FILE`.
- The service reads the key ring (`STEGO_SECRET_KEY_RING_FILE`), as for a
  secret field. The ring keys the fingerprints of the idempotency keys and
  seals the stored answers. Thus a stored API key and the fingerprint of a
  body with a PIN are not in clear in the table `idempotency_keys`.

From rest-api 4.23.0, cli-application 1.15.0 and runtime v0.160.0:

- A command whose answer holds a secret member at any depth is Sensitive,
  also when the member is deeper than the OpenAPI document shows.
- A secret member cannot be a path parameter. Generation refuses the route,
  because the URL goes in access logs. Give the member in the body.
- A Sensitive command refuses `--output-file -`: its answer never goes to
  stdout.
- The CLI refuses a `--body FILE` that holds a secret member when the group
  or other users have access to the file. Use `chmod 600 FILE`.
- The runtime refuses to serve a command with a secret member when the
  service has no key ring.

The mark also works in nested messages, lists and maps. Other tools, such as
the debug output of C++ and Java, also hide a member with this mark.

## Declare a command

<!-- excerpt -->
```yaml
commands:
  - name: place-order
    writes: [Order, StockItem]
    resource_type: order
    permission: edit
    resource_member: order_id
    fill: place-order
    route: POST /orders/{order_id}/place
    effects:
      - {name: fulfill, destination: fulfillment}
    refusals:
      - {name: short_stock, code: stock.short, status: 409}
      - {name: not_draft, code: order.not_draft, status: 409}
```

| Key | What it is |
|---|---|
| `name` | The name, in lower case with hyphens. It is unique among the commands, queries and workers. |
| `writes` | The entities that the fill can read and write. The fill cannot reach other entities. |
| `resource_type` | The SpiceDB type of the row on which the runtime decides the permission. One entity in `writes` has this type. |
| `permission` | The permission that the caller must have on that row. |
| `resource_member` | The field of the request message that holds the ID of that row. |
| `route` | `POST` and a path under `base_path`. |
| `fill` | The name of the one fill. |
| `refusals` | The refusals that the fill can return. Optional. |
| `effects` | The messages that the fill can stage, each with a destination and a name. Optional. |
| `relationships_written` | The SpiceDB relations that the fill can write, as `resource_type#relation`. Optional. |
| `retry_on_conflict` | How many times the runtime runs the fill again after a write conflict: 0 to 3. The default is 0. |
| `lock_timeout` | How long a statement waits for a row lock: from `100ms` to `10s`. The default is `2s`. |

### The route and the resource member

Each path parameter of the route is a string field of the request message.
The runtime copies the path value into that field. The status of a success
comes from the last segment of the path:

- When the last segment is a parameter, such as `/orders/{order_id}`, or has
  a colon, such as `/orders/{order_id}/stock:reserve`, the command answers 200.
- When the last segment is a plain word, such as `place` in
  `/orders/{order_id}/place`, the command creates a resource and answers 201.

The resource member is a string field of the request message. Give the proto
field name (`order_id`) or the JSON name (`orderId`). Both are accepted.

Before the fill runs, the runtime reads the row with this ID and decides the
permission of the caller on it. If the caller does not have the permission,
the answer is 404, and the fill does not run.

Note: a query takes its resource member in lower camel case only. See
[Declare a query](#declare-a-query).

### A command with no resource member

A command whose `resource_type` is the resource type of the tenant
provisioning, such as `service`, and that names no `resource_member`, creates
rows with no parent. The runtime decides its permission on the service-level
resource of the tenant of the request. Its SpiceDB ID is tenant-qualified:

```text
service:<base64url of the tenant ID>/<base64url of the service name>
```

base64url has no padding. For the tenant `acme` of the service `fulfilment`,
the ID is `service:YWNtZQ/ZnVsZmlsbWVudA`. A grant on `service:fulfilment`,
with no tenant, is not a grant in a tenant: the command answers 404. Only
Provision and Retire are decided on `service:<service name>`.

SpiceDB knows a person by the user ID
`<base64url of the issuer>|<base64url of the sub claim>`.

The first grant in a new tenant comes from Provision. Declare the relation
with `admin` in `tenancy.provisioning`, such as
`{resource_type: service, provision: provision, retire: retire, admin: admin}`.
Then Provision takes the first administrators of the tenant in its body:

```bash
curl -X POST https://<host>/api/inventory/v1/stego/tenants \
  -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H 'Idempotency-Key: "provision-acme"' \
  -d '{"tenant_id":"acme","admins":[{"subject":"dora"}]}'
```

Provision writes the grant `admin` of each person on the tenant-qualified ID,
in its transaction. The service serves these tenant routes under its base
path:

| Route | What it does | Permission on `<resource type>:<service name>` |
|---|---|---|
| `POST <base path>/stego/tenants` | Provision: adds the tenant, with its first admins and workers. | `provision` |
| `POST <base path>/stego/tenants/{tenantId}/retire` | Retire: stops the tenant. The rows stay. | `retire` |
| `GET <base path>/stego/tenants` | Lists the tenants, with `limit` and `cursor`. Served only when the schema declares `permission list_tenants`. | `list_tenants` |

For example, to retire the tenant `acme`:

```bash
curl -X POST https://<host>/api/inventory/v1/stego/tenants/acme/retire \
  -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H 'Idempotency-Key: "retire-acme"'
```

A retire is for good. The tenant ID stays in the tenant list as retired, so
a later provision of the same ID answers 409 with the code `tenant.retired`,
and no route, worker or subscription of the service acts in that tenant
again. (A provision of an ID that is active answers 409 with the code
`tenant.provisioned`.) To serve the same customer again, provision a new
tenant ID. A retire changes only the service that gets it: retire the tenant
in each service of the chain.

With more than one API replica, a Provision or a Retire reaches the other
replicas within 5 seconds (`tenant.MaxGateAge`): in that time another replica
can still refuse the new tenant with 403 `tenant.not_provisioned`, or admit
a request of the retired tenant, which then runs to its end. Thus a retire
is done 5 seconds plus the longest request time after it answered. See
[Provision or retire a tenant on more than one replica](operations.md#provision-or-retire-a-tenant-on-more-than-one-replica).

The
[rpc-service guide](../registry/archetypes/rpc-service/README.md#tenants-and-the-first-grant)
gives the rules of `admins`, the operator's own grant and how to write later
grants.

### Idempotency

Each request of a command must have the header `Idempotency-Key`.

- With no key, the answer is 400 with the problem type
  `idempotency-key-missing`.
- With a key that is not one structured string of 1 to 255 characters, such
  as `abc` with no double quotes, the answer is 400 with the problem type
  `idempotency-key-invalid`. Put the key in double quotes, such as `"k-1"`.
- A request again with the same key, method, path and body gets the stored
  response again. The fill does not run again.
- A request with the same key and another body gets 422 with the problem type
  `idempotency-key-reused`.
- A request with the same key while the first request runs gets 409 with the
  problem type `idempotency-key-in-progress`.

The keys of each tenant and each caller are separate. Thus the same key from
another caller, or in another tenant, is a new request.

The header `Idempotency-Kept: 1` is for HTTP only. A command over gRPC gets no
such metadata (ST-SUP-Q199 (d)). After a failure, a gRPC client sends the
command again only with the same key: the service then gives the stored
answer, or runs the command once.

The runtime keeps only an answer that commits. It does not keep an error
answer: a 404, a 400, a declared refusal, an answer of 5xx, an answer of
429, or an answer 409 with the code `conflict`. For each of these, it rolls
back the transaction of the command, and the key with it. A request again
with the same key then runs the fill again, and can get another answer. The
rows that the fill writes and the effects that it stages in the
outbox are in that transaction, so only the run that commits keeps them.
One answer is not an error in this sense: a before slot of a collection that
halts the command (its result sets `Halt` with a status). The command then
commits with no write, and the runtime keeps the answer of the halt with the
key, also when its status is a 4xx. A request again with the same key gets
that stored answer, and the slot does not run again. But a halt with a status
of 500 or more, or 429, is not kept: the command rolls back as for an error
answer, and a request again with the same key runs the slot again. A before
slot that refuses the request (its result is not `Ok`) rolls back as an error
does.
Anything that the fill does outside the transaction, such as a log line,
occurs again at each request. It can also occur twice in one request: the
runtime runs a command again once, in a new transaction, when the database
refuses a prepared statement that a change of a table made stale
(ST-SUP-RP04-LC11). Thus, send each change to another system as an
effect through the outbox (see [Effects](#effects)), never from the fill
directly. A worker then sends it after the commit.

A worker can send one effect more than one time, for example after a crash
before it records that the send is done (see [The fill of a
worker](#the-fill-of-a-worker)). Thus the system that gets the effect must
find a second copy and ignore it: give the effect a stable ID in its
payload, such as the ID of the row and the change, and let the receiver keep
the IDs that it applied.

The runtime keeps each key for the time in `idempotency.retention`. The
default is `24h`.

To send the same answer again, the runtime keeps the whole answer of each
key: its status, its headers and its body. The body holds the fields of the
row, also a `pii` field, such as an email. From runtime v0.138.0, a service
with a `pii` or a `secret` field seals the headers and the body of each
stored answer with its key ring, with the tenant, the principal and the key
as the place of the sealed value (ST-SUP-Q182 (b)). A replay opens them and
gives back the first answer byte for byte. Such a service needs its key ring
(see [Rotate and back up the key ring of the secret
fields](operations.md#rotate-and-back-up-the-key-ring-of-the-secret-fields)).
A service with no `pii` and no `secret` field keeps its answers in clear for
the retention of the key, 24 hours by default: such an answer holds no
personal data. Do not make the retention longer than your clients need.

A key row that a release before v0.138.0 stored in clear does not match a
retry of a release that seals, and a sealed row does not match a retry of a
release before: the retry gets `idempotency-key-reused`, and the client sends
it again with a new key. The runtime never answers the sealed bytes as a
body.

### Refusals

A refusal is an answer that the fill gives when a business rule stops the
command. Each refusal has:

- `name`, in lower snake case.
- `status`: 400, 403, 404, 409, 412 or 422.
- `code`: a fixed code, such as `stock.short`. The codes `internal` and
  `conflict` are reserved.

The contract has one value for each refusal, named with the command and the
refusal: `commands.PlaceOrderShortStock` and `commands.PlaceOrderNotDraft`.
The fill returns this value as its error. The handler then answers with the
status and the code of the refusal. From runtime v0.139.0, the error
`command.ErrNotFound`, which a read of the handle gives for a row that is not
there or that the caller may not read, is 404 with the code `not_found`, and
`NotFound` over gRPC. Any other error is 500 with the code `internal`, and the
transaction is rolled back. The log then has one line `command.fill.failed`
with the command and a fixed cause class, and no error text. To tell the
client which ID is wrong, map `command.ErrNotFound` to a declared refusal, as
[the developer reference of fills](fills.md#map-an-error-of-the-handle-to-a-refusal)
shows.

### The answers of a command over gRPC

A command over gRPC gives each answer that is not a success as a gRPC
status. The message of the status is the fixed code or the problem type
alone (walkthrough 28, GRPC-CODES):

| Answer | HTTP | gRPC status | Message |
|---|---|---|---|
| No verified identity | 401 | `Unauthenticated` | `unauthenticated` |
| No idempotency key, where the command requires one | 400 | `InvalidArgument` | `idempotency-key-missing` |
| A key that is not one structured string of 1 to 255 characters | 400 | `InvalidArgument` | `idempotency-key-invalid` |
| The same key with another request | 422 | `FailedPrecondition` | `idempotency-key-reused` |
| The same key while the first request runs | 409 | `Aborted` | `idempotency-key-in-progress` |
| A message that cannot be read | 400 | `InvalidArgument` | `bad_request` |
| An `authz-revocation-after` value that is not a revocation, or more than one `authz-revocation-after` value | 400 | `InvalidArgument` | `bad_request` |
| A call that its client cancelled, or whose deadline passed, during the wait for a revocation | none | `Canceled` or `DeadlineExceeded`, the status of the context | the text of the context error |
| A value that the checks of the command refuse | 400 | `InvalidArgument`, with a `google.rpc.BadRequest` detail that names the field | `invalid` |
| The caller does not have the permission on the row, or the row is not there | 404 | `NotFound` | `denied` |
| The fill returns `command.ErrNotFound` | 404 | `NotFound` | `not_found` |
| A declared refusal of status 400 or 422 | 400, 422 | `InvalidArgument` | the code of the refusal |
| A declared refusal of status 403 | 403 | `PermissionDenied` | the code of the refusal |
| A declared refusal of status 404 | 404 | `NotFound` | the code of the refusal |
| A declared refusal of status 409, or a write conflict | 409 | `Aborted` | the code of the refusal, or `conflict` |
| A declared refusal of status 412 | 412 | `FailedPrecondition` | the code of the refusal |
| A decision that the authorization provider cannot make now, or a database that cannot serve now | 503 | `Unavailable` | `undecided` or `unavailable` |
| Any other error | 500 | `Internal` | `internal` |

Thus a client that must tell a reused key from a declared refusal of
status 412 reads the message, not only the status code.

### Effects

An effect is a message that the fill stages in the outbox, in the transaction
of the command. A worker that consumes the destination gets the message after
the commit. The fill stages it with `h.Effects().Stage`:

```go
err := h.Effects().Stage(ctx, "fulfillment", "fulfill", updated, payload)
```

The arguments are the destination, the effect name, the loaded row that the
message is about, and the payload as JSON.

### A relationship that a command writes

The command `grant-clerk` makes a user a clerk of a warehouse:

<!-- excerpt -->
```yaml
  - name: grant-clerk
    writes: [Warehouse]
    resource_type: warehouse
    permission: manage
    resource_member: warehouse_id
    fill: grant-clerk
    route: POST /warehouses/{warehouse_id}/clerks
    relationships_written: [warehouse#clerk]
```

Each relation in `relationships_written` is a relation of the SpiceDB type of
an entity that the command writes. A command cannot write the relation
`tenant`, a relation `worker_<name>`, or a relation that the runtime writes
itself.

For each relation, the contract has two helpers:
`commands.GrantClerkRelateClerk` and `commands.GrantClerkUnrelateClerk`. Each
takes a loaded row of the handle, the subject type and the subject ID. The
runtime writes the tuple to SpiceDB after the commit.

The runtime forms the SpiceDB ID of a user as the base64url form of the
issuer, a `|`, and the base64url form of the `sub` claim. The contract has no
helper that makes this ID for another user. The fill of the example makes it
from the issuer part of the caller's own ID.

### The fill of a command

The fill package exports `New`, which gives the fill function of the command
type `commands.<Command>Fill`. This is a part of the fill of `place-order`:

```go
// New gives the fill of place-order.
func New() commands.PlaceOrderFill {
	return func(ctx context.Context, req *commands.PlaceOrderRequest, h *commands.PlaceOrderHandle) (*commands.PlaceOrderResponse, error) {
		orders, err := commands.OrderRepository(h)
		if err != nil {
			return nil, err
		}
		order, err := orders.GetForUpdate(ctx, req.OrderId)
		if err != nil {
			return nil, err
		}
		if order.Value().Status != "draft" {
			return nil, commands.PlaceOrderNotDraft
		}
		// Reserve the stock, update the order and stage the effect.
	}
}
```

The handle gives a repository for each entity in `writes`, such as
`commands.OrderRepository(h)`. It gives no database, no network client and no
other entity. [The developer reference of fills](fills.md) gives each method of
a repository and the decision that it makes.

### The lists of a command

A command fill reads a set of rows through a declared list, as a worker fill
does (ST-DECL-INV94). `place-order` reads the stock items of the warehouse of
its order. The warehouse comes from the order, so the term gives a parameter
in place of a value:

<!-- excerpt -->
```yaml
    lists:
      - name: stock-of-warehouse
        entity: StockItem
        filter:
          - {field: warehouse_id, op: equals, param: warehouse_id}
```

`stego apply` writes the list `commands.PlaceOrderStockOfWarehouse` in
`out/commands`. Because the list has a parameter, it also writes the function
`commands.PlaceOrderStockOfWarehouseWith(warehouseID string)`. The fill gives
the value of the parameter to this function:

```go
items, err := commands.StockItemRepository(h)
if err != nil {
	return nil, err
}
stock, err := items.List(ctx, commands.PlaceOrderStockOfWarehouseWith(order.Value().WarehouseID), 100)
```

The generator builds the statements of the list from the entity of the
release. When a schema change adds or renames a field, the next release
writes the list again with the new columns. The handle binds each parameter as
a SQL parameter, never as text. A parameter of a reference field is a key: a
value that the key check of the entity refuses gives an empty list. A command
on the service-level resource has no list.

A list of a child entity reads only the rows of one decided parent. Its parent
term is `{field: order_id, op: equals, parent: true}`, and its function takes the
loaded parent row, such as `OfOrderWith(order LoadedOrder)`. The handle takes
only a row that this handle read. Before it reads a row, it decides the
permission of the reference on the parent, so a caller that may act on one
order never lists the rows of another order.

## Declare a query

<!-- excerpt -->
```yaml
queries:
  - name: open-orders
    reads: [Order]
    resource_type: order
    permission: view
    fill: open-orders
    route: GET /open-orders
  - name: order-summary
    reads: [Order]
    resource_type: order
    permission: view
    resource_member: orderId
    fill: order-summary
    route: GET /orders/{orderId}/summary
```

| Key | What it is |
|---|---|
| `name` | The name, in lower case with hyphens. |
| `reads` | The entities that the fill can read. |
| `resource_type` | The SpiceDB type of the rows that the query reads. One entity in `reads` has this type. |
| `permission` | The permission that the caller must have on each row. |
| `resource_member` | For a get query only: the request member that holds the ID of the one row. |
| `route` | `GET` and a path under `base_path`. |
| `fill` | The name of the one fill. |
| `refusals` | The refusals that the fill can return, as for a command. Optional. |

### A get query and a list query

A query with a `resource_member` is a **get query**. It reads one row. A query
with no `resource_member` is a **list query**. It reads one page of rows.

For a get query:

- The resource member is in lower camel case, such as `orderId`. A name with
  an underscore, such as `order_id`, is refused: `the query order-summary
  names the resource member "order_id", which is not a member name in lower
  camel case`.
- The only path parameter that the route can have is the resource member. If
  the route has no parameter, the request gives the ID in the URL query, such
  as `?orderId=...`.
- The runtime decides the permission on the row before the fill runs.

For a list query:

- The route has no path parameter. A path parameter is refused: `the query
  open-orders has the path parameter {warehouseId}, which is not its resource
  member`. Thus a list query cannot take the ID of a parent from the path. The
  request of a list query holds only the cursor and the page size, so the fill
  filters the rows only by values that the fill itself sets.
- The runtime keeps only the rows on which the caller has the permission.
- The request pages with `limit` and `cursor`. See [Paging](#paging).

A list query whose `method` is a gRPC server stream, such as `rpc
ListAccounts(ListAccountsRequest) returns (stream Account)`, is a **stream
query**. It is served over gRPC only: the stream sends each row that the
caller may read, and has no page. A stream query declares no `route`.
Checking refuses a route on it: `query "list-accounts" has the route "GET
/accounts" and the gRPC method billing.v1.Billing/ListAccounts, which is a
server stream; a stream is served over gRPC only, so remove the route`. To
list the same rows over HTTP, declare a second list query with a `route` and
a unary method, or with no method. The Product owner decided that an HTTP
stream waits until an HTTP client needs one (ST-SUP-RC33-OC08). A route that
no handler serves would answer 404 or 405 with no fault, so checking refuses
it.

### The fill of a query

The fill package exports a function with the name `Fill` of the type
`queries.<Query>Fill`. It does not export `New`. This is the fill of
`open-orders`, with no change:

```go
// Fill lists one page of the placed orders.
func Fill(ctx context.Context, req queries.OpenOrdersRequest, h *queries.OpenOrdersHandle) (queries.OpenOrdersResponse, error) {
	orders, err := queries.OpenOrdersOrders(h)
	if err != nil {
		return queries.OpenOrdersResponse{}, err
	}
	page, err := orders.List(ctx, queries.ListRequest{Limit: req.Limit, After: req.After,
		Where: []query.Equal{{Field: "status", Value: "placed"}}})
	if err != nil {
		return queries.OpenOrdersResponse{}, err
	}
	items := make([]Item, 0, len(page.Items))
	for _, o := range page.Items {
		v := o.Value()
		items = append(items, Item{ID: o.ID(), WarehouseID: v.WarehouseID, TotalUnits: v.TotalUnits})
	}
	return queries.OpenOrdersResponse{Items: items, Next: page.Next}, nil
}
```

`query` is the package `github.com/910-software-solutions/stego/registry/runtime/query`, and
`Item` is a type of the fill. The handler writes `Items` as JSON.

The request of a get query has one field for the member: `req.OrderID` for
`orderId`. The response of a get query is any value that the handler can
write as JSON.

Do not put a secret in the answer of a query. The read-only value of an
entity has no `secret` field, so a query fill cannot read a stored secret.
The OpenAPI document gives a query answer as any JSON value, with no
`x-sensitive` mark, and the generated CLI writes it to stdout. A call that
makes a secret, such as a new API key, is a command with a `debug_redact`
member in its response message (see [A secret member](#a-secret-member);
ST-SUP-Q199 (a)).

### An optional field in a command and in a query

The Go type of an optional field is not the same in the two packages:

- In `out/commands`, an optional field that is not bytes or jsonb is a
  pointer, such as `*string`. A command writes the row, so `nil` sets the
  column to NULL, and a pointer to `""` sets it to the empty text.
- In `out/queries`, the value that a fill reads has the field as a value,
  such as `string`. A NULL column gives the zero value: `""`, `0` or `false`.

Thus a command fill uses `v.Carrier != nil` and `*v.Carrier`, and a query
fill uses `v.Carrier` with no `*`. Do not copy the code that reads a field from
a command fill into a query fill.

The rest-api handlers read the type `<Entity>View`, in which an optional field
is a pointer, so an answer leaves out a NULL field (decision ST-SUP-Q128,
option B). The value that a query fill reads did not change, so no fill had to
change. Thus a query fill cannot tell NULL from the zero value. A pointer in
the query value needs a major version of each archetype that has a query
(ST-SUP-Q128, option A), and it is not built.

## Paging

A collection list and a list query page in different ways:

| | Collection list | List query |
|---|---|---|
| Page size parameter | `pageSize` | `limit` |
| Default page size | 100 | 100 |
| Largest page size | 1000. A larger value gives 1000. | 1000. A larger value gives 400. |
| Value less than 1 | 400 | 400 |
| Next page | `cursor` | `cursor` |
| Response | `items` and `next` | `items` and `next` |

Each parameter name is exact. A collection list reads only `pageSize`,
`cursor`, `orderBy`, `fields` and `search`. It answers 400 for each other
parameter, such as `limit` or `PageSize`, and the detail names the parameter
(rest-api 4.14.0). A list query ignores `pageSize`. Give the `next` value of a response as the `cursor`
of the next request. A cursor is valid only for the same list, tenant and
caller. Another cursor gives 400.

## Commands and queries in the OpenAPI document

From rest-api 4.17.0, the OpenAPI document of the service
(`out/internal/api/openapi.json`, also served at its discovery route) holds
each command and each get or list query that the service serves over HTTP, at
its route. Its operationId is the name in lower camel case, such as
`dischargePatient`. A command has its path parameters, the required header
`Idempotency-Key`, the request and the response message in their protobuf
JSON form, and a response for each refusal. A list query has `cursor` and
`limit`. A stream query is served over gRPC only, so it is not in the
document.

The protobuf JSON form names each member in lower camel case, such as
`closedRecords`, and gives a 64-bit integer as a string. A request can give a
member by either name, such as `insurance_number` or `insuranceNumber`. Thus
the request validation of the document does not check the body of a command;
the command runtime decodes it.

A service with both commands and collections, such as a rest-crud service,
answers in two forms. A client must read each answer in its own form:

| | A command route | A collection route (rest-api) |
| --- | --- | --- |
| Member names | Lower camel case, such as `walletId` | The declared field name, in lower snake case, such as `wallet_id` |
| A 64-bit integer (`int64`) | A JSON string, such as `"balance": "120"` | A JSON number, such as `"balance": 120` |
| A member with the zero value, such as 0 or `""` | Not in the answer, as the protobuf JSON form gives it | In the answer, for a required field |
| The status of a success | 201 when the last segment of the path is a plain word, else 200 (see [The route and the resource member](#the-route-and-the-resource-member)) | 201 for a create (`POST` to the collection), 200 for a read, a list and an update, and 204 for a delete |

Thus a client of its own, such as a script with `curl` and `jq`, reads
`.walletId // ""` and converts the string of a 64-bit integer in a command
answer, and reads `.wallet_id` and a number in a collection answer.

A service with no rest-api component, such as an rpc-service, has no such
document; the command component writes the document of its command and query
routes, in the same form, in `out/commands/openapi.json`.

The generated clients come from this document. Bind the mixin `go-sdk` for a
Go client (`out/sdk`) and the mixin `cli-application` for a CLI
(`out/cli/cmd`), with `mixins: [go-sdk, cli-application]`. See the READMEs of [go-sdk](../registry/components/go-sdk/README.md) and
[cli-application](../registry/components/cli-application/README.md).

## Declare a worker

<!-- excerpt -->
```yaml
workers:
  - name: fulfill-orders
    trigger: consumes fulfillment
    writes: [Order, StockItem]
    resource_type: order
    permission: fulfill
    fill: fulfill-orders
  - name: low-stock-scan
    trigger: every 1m
    writes: [StockItem]
    resource_type: stock_item
    permission: scan
    fill: low-stock-scan
    lists:
      - name: low-items
        entity: StockItem
        filter:
          - {field: quantity, op: less_than, value: 10}
```

| Key | What it is |
|---|---|
| `name` | The name, in lower case with hyphens. |
| `trigger` | `every <interval>`, in whole seconds, minutes or hours (`s`, `m`, `h`) from `10s` to `24h`, or `consumes <destination>`. |
| `writes` | The entities that the fill can read and write. |
| `resource_type` | A SpiceDB type of the rows that the worker reads and writes. |
| `permission` | The permission that the worker needs on each row that it reads or writes. |
| `fill` | The name of the one fill. |
| `effects` | The messages that the fill can stage, as for a command. Optional. |
| `single` | For an `every` worker: when true, one instance of the service runs the worker at a time, under a lease. The default is true. |
| `lease_ttl` | The time that the lease holds, from `15s` to `5m`. The default is `15s`. |
| `lists` | The lists of rows that the fill reads. See [The lists of a worker](#the-lists-of-a-worker). Optional. |

The component `worker` is at the stage alpha. Accept it in the declaration
with a date before its sunset (2027-04-30) and a reason:

<!-- excerpt -->
```yaml
experimental_acceptances:
  - {component: worker, accepted_until: 2027-03-31, reason: the fulfillment worker reacts to placed orders}
```

### The two triggers

- `every 1m`: the runtime runs the fill once in each tenant at each tick, in
  one transaction of that tenant. The ticks are at the multiples of the
  interval since the Unix epoch, in UTC: `every 1h` runs at the start of
  each hour, and `every 24h` at 00:00 UTC. The first tick is the first such
  time after the worker starts, not the start: an `every 24h` worker that
  starts at 10:00 UTC runs first at 00:00 UTC the next day. A tick that is
  due while the service is stopped, or while a tick of the worker runs, is
  not run later.
- `consumes fulfillment`: the runtime runs the fill once for each message of
  the destination `fulfillment`, in the transaction of the tenant of the
  message. A command or a worker stages these messages as an effect.

### A job that runs once a month

The longest interval of `every` is `24h`. A worker has no calendar trigger,
such as "on the first day of each month". For a job of each month, run the
worker each hour, and let the fill find out if the month has a run already:

1. Declare the worker with `trigger: every 1h`.
2. Declare an entity that records each run, with the period as a field, such
   as `MonthlyClose` with `period` (`pattern: "^[0-9]{4}-[0-9]{2}$"`). Put it
   in `writes`.
3. Declare in the worker a list of the runs of one period: the list
   `closes-of-period` of the entity `MonthlyClose`, with the filter term
   `{field: period, op: equals, param: period}`.

4. In the fill, find the period from the tick, which is the time of the run.
   Return with no change when the list holds a row of the period. Else do the
   job, and create the row of the period in the same transaction:

   ```go
   first := time.Date(tick.Year(), tick.Month(), 1, 0, 0, 0, 0, time.UTC)
   period := first.AddDate(0, -1, 0).Format("2006-01") // the month before the tick
   closes, err := workers.MonthlyCloseRepository(h)
   if err != nil {
   	return err
   }
   done, err := closes.List(ctx, workers.MonthlyCloseClosesOfPeriodWith(period), 1)
   if err != nil || len(done) > 0 {
   	return err
   }
   // Close the month here, then record it.
   _, err = closes.Create(ctx, workers.MonthlyClose{Period: period})
   return err
   ```

The first run of each month closes the month before it. When the service is
stopped at the start of a month, the first run after the start does the
job. A run that fails commits nothing, so the next run does the job again.
Each run reads one list in each tenant, which costs little. Use the first day
of the month in UTC, as above, and not `tick.AddDate(0, -1, 0)`: on 31 March
that gives 3 March. A daily job is `every 24h`. A worker that must run at one
time of the day uses the same form: `every 1h` and a check of the hour.

A job of each week uses the same pattern with the ISO week as the period
(walkthrough 27, W-WEEK). There is no weekly trigger, because `every` stops
at `24h`. Declare the period as `pattern: "^[0-9]{4}-W[0-9]{2}$"`, such as
`2026-W47`, and find the week before the tick from its ISO year and week:

```go
year, week := tick.AddDate(0, 0, -7).ISOWeek() // the week before the tick
period := fmt.Sprintf("%04d-W%02d", year, week)
```

Use `ISOWeek` and not `tick.Year()`: the first days of January can be in
the last week of the year before, such as 1 January 2027 in `2026-W53`. An
ISO week starts on Monday in UTC, so the first run on Monday closes the
week before it.

### The permission of a worker

A worker acts as the service account `service_account:<worker name>`, not as
a user. Each tenant gives this account one relation on the SpiceDB type
`tenant`. The runtime writes these tuples. See
[Worker grants in each tenant](workers.md).

Do these steps in the schema for each worker:

1. Give the type `tenant` the relation `worker_<worker name in snake case>`
   of the subject type `service_account`:

   <!-- excerpt -->
   ```zed
   definition tenant {
   	relation worker_fulfill_orders: service_account
   	relation worker_low_stock_scan: service_account
   	relation worker_ship_orders: service_account
   }
   ```

   Validate refuses a schema without this relation.

2. Give each type of row that the worker reads or writes the relation
   `tenant` and the permission that the worker names, through the worker
   relation. The runtime relates each row to its tenant when it creates the
   row.

   <!-- excerpt -->
   ```zed
   	relation tenant: tenant
   	permission fulfill = tenant->worker_fulfill_orders
   ```

   The worker `fulfill-orders` writes `Order` and `StockItem`, so the types
   `order` and `stock_item` both have the permission `fulfill`.

Validate refuses a type in `writes` or `reads` that does not have the
permission. From rest-crud 3.274.0, validate and plan also give the warning
`worker-relation` for each such type, and for `resource_type`, whose
permission does not reach the worker relation (walkthrough 28, CHK-WREL).
Without that path, the worker has the permission only on a row that gives
it to `service_account:<worker name>` in another way, such as an owner
relation of a row that the worker created. The runtime refuses each read
and write of the worker on the other rows, and a consuming worker parks
each such event after its retries.

3. When the worker creates a row with no parent, give the type `tenant` the
   permission of the worker too. A row has no parent when its entity has a
   resource type and no required reference to an entity with a resource type,
   such as the local copy of an event of another service. The runtime decides
   such a create on the tenant resource with the permission of the worker. A
   consuming worker also needs the permission of the stager there.

   The SpiceDB ID of the tenant resource is tenant-qualified, as each other ID
   of a tenant is: `tenant:<base64url of the tenant ID>/<base64url of the
   tenant ID>`, with no padding. For the tenant `acme`, it is
   `tenant:YWNtZQ/YWNtZQ`. A grant on `tenant:acme` is not a grant in the
   tenant `acme`, and it gives nothing. This command gives the ID:

   ```bash
   b64() { printf %s "$1" | base64 | tr '+/' '-_' | tr -d '=\n'; }
   TENANT=acme
   t=$(b64 "$TENANT"); echo "tenant:$t/$t"
   ```

   For a worker `record-orders` with the permission `record`, the type
   `tenant` holds `relation worker_record_orders: service_account` and
   `permission record = worker_record_orders`.

   Generation refuses a schema whose type `tenant` has no such permission,
   and validate gives the warning `worker-root-create` before that.

   A person reads such a row only through a relation of the type `tenant`.
   Give the type `tenant` the relation `service` of the provisioning resource
   type, such as `relation service: service`. Then each Provision relates the
   tenant resource to the service-level resource of the same tenant, which
   holds the admins of the Provision, and each Retire deletes the relation
   (ST-SUP-RC27-OC18). The schema then decides who reads the row, such as
   `permission audit = service->admin` on the type `tenant` and
   `permission view = tenant->audit` on the type of the row. Validate refuses
   the relation `service` when the declaration has no tenant provisioning, or
   when the relation admits another subject type. See [the developer
   reference of fills](fills.md#rows-with-no-parent-and-who-reads-them).

### The fill of a worker

The fill package exports `New`, as for a command. The type of the fill
function depends on the trigger.

A consuming worker gets the message as JSON and the effect name:

```go
// New gives the fill of fulfill-orders.
func New() workers.FulfillOrdersFill {
	return func(ctx context.Context, message json.RawMessage, effect string, h *workers.FulfillOrdersHandle) error {
		var f Fulfillment
		if err := json.Unmarshal(message, &f); err != nil || effect != "fulfill" {
			return workers.Refused(ErrBadMessage)
		}
		// Read the order and the stock item, then update them.
	}
}
```

- When the fill returns nil, the transaction commits.
- When the fill returns `workers.Refused(err)`, the runtime parks the message.
  It does not deliver the message again.
- When the fill returns another error, the transaction is rolled back and the
  runtime delivers the message again. Thus a fill can get one message two
  times. Make the fill safe for this: `fulfill-orders` does nothing for an
  order that is not `placed`.

A worker writes rows through the repository of each entity in its `writes`,
as a command does. To create a row with no parent, such as the local copy of
an event of another service, give `Create` the value alone:

```go
accounts, err := workers.AccountRepository(h)
if err != nil {
	return err
}
_, err = accounts.Create(ctx, workers.Account{ProjectRef: e.ID, Plan: "free"})
```

The runtime decides that create on the tenant resource with the declared
`permission` of the worker (ST-SUP-Q164). Thus the definition of the tenant in
the schema must give that permission to the worker relation, such as
`permission bill = worker_bill_projects`. Generation refuses a schema that
does not. To create a row with a parent, give the loaded parent after the
value, such as `charges.Create(ctx, charge, account)`, where `account` is a row
that the fill read through its repository.

An interval worker gets the time of the tick:

```go
// New gives the fill of low-stock-scan.
func New() workers.LowStockScanFill {
	return func(ctx context.Context, tick time.Time, h *workers.LowStockScanHandle) error {
		// Read the stock items of this tenant and emit low_stock for each low item.
	}
}
```

### The lists of a worker

A worker fill reads a set of rows through a declared list (ST-DECL-INV94).
`low-stock-scan` reads the stock items with fewer than 10 units:

<!-- excerpt -->
```yaml
    lists:
      - name: low-items
        entity: StockItem
        filter:
          - {field: quantity, op: less_than, value: 10}
```

| Key | What it is |
|---|---|
| `name` | The name of the list, unique in its worker. |
| `entity` | An entity that the worker writes. |
| `filter` | One to eight terms. Each term has a `field` of the entity and an `op`. |
| `op` | `equals`, `not_equals`, `less_than`, `less_than_or_equals`, `greater_than`, `greater_than_or_equals` or `is_set`. |
| `value` | A constant of the field type. `is_set` takes no value. |
| `now` | `true` in place of `value`: the time of the run. This is the time of the tick of an `every` worker, and the start of the run of a consuming worker. Use it only on a timestamp field. |
| `param` | A name in lower snake case, in place of `value`: a parameter whose value the fill gives at call time, such as a field of the message. Each parameter has its own name in the list. `is_set` takes no parameter. |
| `parent` | `true` in place of `value`, with `equals`, on the reference of a child entity: the fill gives the loaded parent row, such as a `LoadedOrder`. A child entity has no resource type and a reference to an entity with one. A list of a child needs exactly one parent term, and a list of any other entity has none. |
| `lock` | Optional. When true, `ListForUpdate` locks the rows. The entity must declare `lock_at_most`, a whole number from 1 to 1000. |

`stego apply` writes one typed list for each declared list in `out/workers`.
When a generated file of `out/workers` is missing, `stego apply` writes it
again with no flag; it refuses a generated file that was edited by hand.
Its name is the worker name and the list name in mixed case, such as
`LowStockScanLowItems`. The generator builds the statements of the list from
the entity of the release: the columns of the entity, the tenant, and the
filter as bound values. When a schema change adds a field, the next release
writes the list again with the new column, and the release before it still
reads its own columns.

The fill reads the list through the repository of the entity:

```go
items, err := workers.StockItemRepository(h)
if err != nil {
	return err
}
low, err := items.List(ctx, workers.LowStockScanLowItems, 100)
```

A declared list takes no filter values from the fill. A list with a parameter
has a generated function `<Worker><List>With`, which takes each parameter as a
typed Go value, in the order of the terms. A consuming worker can give a field
of its message as a parameter.

A list keeps only the rows on which the worker has its permission in SpiceDB
(ST-SUP-RP10-LC17). Thus a row with no tuple is not in the list: a row that a
person or a script wrote past the service, or a new row whose tuples the relay
did not write yet. A worker that checks a rule over all the rows, such as a
sum of the balances, does not see such a row. Write the rows only through the
service. [The developer reference of fills](fills.md) gives the decision of
each method of the handle.

### A worker that creates a row from another row

A worker can handle one row two times. A consuming worker can get one
message two times, and an interval worker lists the same rows at each tick.
When the fill creates a row from another row, such as `ship-orders`, which
creates a shipment for each fulfilled order, a check in the fill alone does
not stop a second row. The list of a worker keeps only the rows whose tuples
the relay wrote (see [The lists of a worker](#the-lists-of-a-worker)). Thus a
new shipment is not in the list of the shipments of its order for a short
time, and a run in that time creates a second shipment. In a trial run, a
worker that ran again in this time made three invoices for one job.

Declare a unique key on the reference that the new row takes from the other
row. Then the database refuses the second row:

<!-- excerpt -->
```yaml
  - name: Shipment
    resource_type: shipment
    permissions: {create: ship, read: view, list: view, update: manage, delete: manage}
    references:
      order_id: {relation: order, permission: ship}
    fields:
      - { name: order_id, type: ref, to: Order, unique: true }
      - { name: units, type: int32, min: 0, max: 1000000 }
```

<!-- excerpt -->
```yaml
  - name: ship-orders
    trigger: every 1m
    writes: [Order, Shipment]
    resource_type: order
    permission: ship
    fill: ship-orders
    lists:
      - name: fulfilled-orders
        entity: Order
        filter:
          - {field: status, op: equals, value: fulfilled}
      - name: shipments-of-order
        entity: Shipment
        filter:
          - {field: order_id, op: equals, param: order_id}
```

The fill marks each order `shipped` in the transaction that creates its
shipment, so the order leaves the list of fulfilled orders when the run
commits. Without the mark, the list gives the same first 100 fulfilled orders
at each tick, and a later order never ships. The fill also keeps its check, so
a run after the relay creates no row and fails on no key:

```go
// New gives the fill of ship-orders.
func New() workers.ShipOrdersFill {
	return func(ctx context.Context, tick time.Time, h *workers.ShipOrdersHandle) error {
		orders, err := workers.OrderRepository(h)
		if err != nil {
			return err
		}
		shipments, err := workers.ShipmentRepository(h)
		if err != nil {
			return err
		}
		fulfilled, err := orders.List(ctx, workers.ShipOrdersFulfilledOrders, 100)
		if err != nil {
			return err
		}
		for _, order := range fulfilled {
			shipped, err := shipments.List(ctx, workers.ShipOrdersShipmentsOfOrderWith(order.ID()), 1)
			if err != nil {
				return err
			}
			if len(shipped) == 0 {
				shipment := workers.Shipment{OrderID: order.ID(), Units: order.Value().TotalUnits}
				if _, err := shipments.Create(ctx, shipment, order); err != nil {
					return err
				}
			}
			// The order leaves the list of fulfilled orders in the same transaction.
			v := order.Value()
			v.Status = "shipped"
			if _, err := orders.Update(ctx, order, v); err != nil {
				return err
			}
		}
		return nil
	}
}
```

Only the worker creates a shipment: the create permission and the reference
`order_id` take the permission `ship`, which the schema gives only to the
worker relation. A user who could create a shipment first would take the key,
and the worker could then not ship the order.

The mark does not stop two runs at one time, such as on two instances of a
worker with `single: false`, or a second delivery of a message to a consuming
worker. When a run creates a shipment that is already there, `Create` gives
`command.ErrConflict` and the fill returns it. The run commits nothing, and
`worker.run.failed` has the cause `conflict`. A later run, after the relay
wrote the tuples of the first shipment, finds that shipment, creates none and
marks the order.
The run record of [A job that runs once a month](#a-job-that-runs-once-a-month)
takes the same key: `unique: true` on `period`.

A key of the reference alone permits one row for each referenced row. When
an order can have one shipment for each warehouse, make the key of the
reference and the warehouse with `unique_composite`. To make a field of an
entity of the base unique, declare a unique step: see
[Unique steps](schema-changes.md#unique-steps).

### A failed run of a consuming worker

Each attempt of a consuming worker that does not commit writes one log line
`command.consumer.run_failed` at the level warn. The line gives the worker, the
destination, the event ID of the message, the attempt number, the outcome and
the cause class. It never gives the error text or a value of the message. The
attempt is also sent as `worker.run.failed` with the worker, the outcome and
the cause class.

An error of the fill that is not an error of the handle gets one of these
cause classes. The runtime reads the type of each error in the chain of the
error (`errors.As`), never its text:

| Cause | What the chain of the error holds |
|---|---|
| `fill_json` | An `encoding/json` `SyntaxError`, `UnmarshalTypeError` or `InvalidUnmarshalError`, such as from a decode of the payload. |
| `fill_parse` | A `strconv` `NumError` or a `time` `ParseError`. |
| `fill_canceled` | `context.Canceled`. |
| `fill_error` | Another error. |

The text of a fill error can hold a value of the message or of a row, so no
log line and no observation gives it. To tell an operator more, the fill logs
its own context with `log/slog`, which the fill allow list takes, before it
returns the error: names and counts, never a value. For example:

```go
	if err := json.Unmarshal(payload, &reply); err != nil {
		slog.WarnContext(ctx, "apply-partner-replies: the reply does not decode", "bytes", len(payload))
		return fmt.Errorf("decode the reply: %w", err)
	}
```

When the fill fails with the same outcome and cause class in 5 attempts in a
row, the runtime parks the message as poison (failure code `poison`) and
raises the alert of a parked message. Only the errors that the fill causes are
counted: an error of the fill (`fill_error`, `fill_json`, `fill_parse` and
`fill_canceled`, but not a fill canceled because the worker stops), a row that is not there, an invalid value, a
refused resource, a unique violation and a panic. A provider that cannot decide
now, a conflict, a failed database statement, a timeout and a lost connection
are not counted, and they start the count again. A parked message stays in the
outbox, and a person can send it again or discard it. Until a person resolves
it, a parked message holds back each later message of the same destination,
tenant and resource key, so the later events of the same row wait.

A row that a worker writes gets its relations in SpiceDB from the relay, a
moment after the run commits. Until then the worker is denied on the row, so a
list does not give it and a read is refused. Thus the run of an update event
can miss the row that the run of its create event made. When the failure of a
run comes from such a deny, the outcome is `undecided` and the cause is
`relations_pending`: the refused error of the row, also inside a declared
refusal, or a not-found error, a declared refusal or a fill error of a run
whose list left out such a row. The message goes back to pending with the
failure code `relations-pending.<n>.<time>`, the poison count does not count
it, and it does not park. A later attempt sees the row (runtime v0.186.0).
Any other failure of the run counts as before. The wait has a bound: after 10
such attempts (`command.RelationsPendingAttempts`), or 5 minutes after the
first (`command.RelationsPendingAge`), the next attempt counts as if the row
had its relations, so a row whose relations never come parks its message or
counts for poison. That attempt writes the warn line
`command.consumer.relations_pending_past_bound` with the worker, the
destination and the event ID. A run that commits does not change, so a fill that makes a
row when a list does not give it can still make a second row: declare a unique
key on the reference that it makes the row from.

The log line and the alert of a parked message are also written to standard
output as one JSON line each, also when no OpenTelemetry collector is set. The
runtime writes each log record at the level warn or higher in this way, at most
10 lines of one event name in each minute, and then one line with the event
name and the number of lines that it dropped.

### See and resolve the parked messages

In the api mode, the service serves two routes for an operator, behind the
same authentication as the other routes:

- `GET <base path>/stego/outbox/{destination}/parked` lists the parked
  messages of the destination in the tenant of the caller: the message ID, the
  destination, the failure code, the attempt count and the times when the
  message was made and parked. It never gives the payload. It pages with the
  `limit` and `cursor` parameters, as the other lists do.
- `POST <base path>/stego/outbox/{destination}/parked/{id}:resolve` with the
  body `{"resolution": "not_acted"}` resolves one message. `not_acted` sends
  it again with the same identity, `acted` marks it done, and `discarded`
  discards it. The answer is 204, and the audit row
  `resolve-parked-message` is written in the same transaction.

Each park writes one log line `outbox.parked` at the level WARN, and adds one
to the metric `outbox.parked`. Both name the destination, the state that the
message left and the cause, one of these fixed classes (ST-SUP-RP01-LC12):

- `refused`: the destination refused the message for good.
- `not_understood`: the worker of no release could read the message within
  one hour (ST-SUP-RP01-LC40). A worker of an older release leaves such a
  message in pending for a worker of a newer release, and counts it in the
  metric `outbox.not_understood`, and writes the WARN line
  `outbox.not_understood` on its first hold, at most one line a minute for
  each destination.
- `attempts_exhausted`, `lookups_exhausted`: the attempts or the status
  lookups of the message are at their limit.
- `outcome_unknown`: the outcome of the send is not known, and the
  destination cannot find it.
- `tenant_not_active`: the tenant of the message is not active.

The line also has the failure code of the message, such as
`delivery-refused-permanent` or `poison`, which the list route gives too.
Neither holds an error text or a value of the message.

The destination decides which resolutions it accepts (ST-SUP-RP01-LC13):

- An idempotent destination refuses `acted` with 409. To send such a message
  again is safe, and `acted` would lose what the delivery writes. These
  destinations are idempotent: `authz`, the audit sink, the change events, a
  subscription that a worker consumes, and each destination of an effect whose
  component declares it with `idempotent: true` in its setting
  `destinations`. For them, correct the cause and send the message again with
  `not_acted`, or use `discarded`. A destination with `idempotent: false`
  accepts `acted`: use it when the receiver already did the work.
- A destination that need not land refuses `discarded` with 409.

The caller needs the permission `resolve` on `outbox_destination:<destination>`
in its tenant. Declare the type in the authorization schema, as the schema of
this page does:

<!-- excerpt -->
```zed
definition outbox_destination {
	relation operator: user
	permission resolve = operator
}
```

A caller without the permission gets 403, the same for each destination and
each message. A message that the tenant does not hold gets 404. A message that
is not parked, or a resolution that the destination refuses, gets 409. Another
body gets 400.

Checking refuses a declaration that binds authorization when its schema does
not define `outbox_destination` with the permission `resolve`, also when the
declaration binds no mixin: the runtime serves these routes in each service on
the runtime data path. Checking also refuses a `resolve` that reaches a
wildcard subject such as `user:*`, and an entity with the resource type
`outbox_destination`.

Give `resolve` to the operators only. Checking cannot see a `resolve` that
reaches each member of a tenant, such as through a relation that every member
holds. Such a member can send a parked message again or discard it. Do not
write `resolve = nil` either: then no operator can resolve a message.

**Warning:** Do not discard a parked message of the destination `authz`
unless you apply its change in another way. The messages of `authz` are the
relationship writes and deletes of SpiceDB. When you discard a message that
removes an access, SpiceDB keeps that access: the revocation is never
applied. Send the message again with `not_acted`. The destination `authz` is
idempotent, so it refuses `acted`.

### Take the change events of another service

A worker can consume the change events of another service through a
subscription. A service has 1 to 8 subscriptions, and one worker consumes
each. See [Subscriptions](subscriptions.md).

### A reference held within a field

A reference can be held within a field that the row and the referenced row
must have equal. Declare it with `within` on the reference. For a Ticket of a
Project that refers to a Sprint of a Project, the reference is
`sprint_id: {relation: sprint, permission: edit, within: project_id}`.

The field must be a required reference to the same entity on both entities,
or the scope field of a collection over each entity (ST-DECL-INV93). Use
`within: none` when the reference crosses that entity on purpose, such as a
link between issues of two projects. Checking warns about a reference with no
`within` when both entities refer to the same entity.

A create, and an update that changes the reference or the field, compares the
field of the new row with the loaded referenced row before the row statement.
A difference is refused with 400, the code `invalid` and the field of the
reference, and nothing is written (ST-SUP-RP10-LC37). An update that changes
only the field must also give the loaded row of the reference that stays; the
generated CRUD handlers do this. The database holds the same rule with a
composite key (ST-SUP-RC01-OC46), so a race, a worker that gives no parent and
a move of the referenced row are refused too: a move of a sprint to another
project while a ticket of the first project refers to it gives 400 `invalid`
with the field `project_id`.
When the field that the reference is held within is optional, the database
also holds a CHECK: a row that sets the reference and leaves that field empty
gives 400 `invalid` with the field of the reference.

### A failed statement

When a statement of a command, a worker or a consumer fails and the failure is
not a conflict, the runtime writes one log line `command: a statement failed`
at the level error. The line gives the command or worker, the step, the cause
class and the SQLSTATE class. It never gives a value or the database text. The
cause classes are:

| Cause | What it is |
|---|---|
| `data` | A value of the row does not fit, such as a foreign key or a check (SQLSTATE classes 22 and 23). |
| `not_granted` | The role lacks a privilege (42501). |
| `schema` | The statement does not fit the database, such as an undefined column (the other values of class 42). |
| `statement` | Another SQLSTATE. |
| `scan` | The row has other columns than the generated Scan of the entity reads. |
| `other` | An error with no SQLSTATE, such as a lost connection. |

The runtime writes at most one line for each command or worker, cause and
SQLSTATE class in each 10 seconds, with the number of lines that it held back.

From runtime v0.165.0, a statement that the database refuses with SQLSTATE
0A000 `cached plan must not change result type` is not a failed statement.
This occurs when a person drops a column and adds it again while the service
runs: the prepared statement of a connection is then stale. The runtime makes
each connection of the pool prepare its statements again, and runs the
command once more with no wait. A query answers 500 one time, and the next
call passes (ST-SUP-RP04-LC11).

### A value that breaks a declared constraint

Each entity checks the declared constraints of its fields before a create or
an update sends a statement: `min_length`, `max_length`, `pattern`, `min`,
`max` and the values of an enum. An optional field is checked only when it is
set. A value that breaks one is answered 400 with the code `invalid` and the
field, or `InvalidArgument` over gRPC. The reason names the constraint and its
declared bound, never the value.

When the table refuses a value that the check does not cover, the CHECK
(SQLSTATE 23514), the NOT NULL (23502) or the foreign key (23503) of a
declared field gives the same 400 with the field. A unique violation (23505)
stays 409 `conflict`. When the violated index is a declared unique key, a
`unique` field or a `unique_composite`, the answer names the fields of the key
in `field` and never a value: the detail of a generated CRUD write is `the
Organisation was not written: another Organisation has the same name, which
must be unique`, and over gRPC the status `Aborted` carries a `BadRequest`
detail with the fields. Any other conflict keeps the detail `the
<Entity> changed during the request`.

The 409 names the key. Thus a caller who may create or update the entity
learns that a row with that value of the key exists in the tenant, also a row
that the caller may not read. A unique violation told this before too, with no
name of the key. When the existence of a value must not be known across
parents, such as the name of a project in another team, do not make the field
unique in the whole tenant. Make it unique under its parent with
`unique_composite`, which holds the reference to the parent and the field,
such as `{ name: name, type: string, unique_composite: [team_id, name] }`. Then
the conflict tells only of the rows under the parent of the write. A get query of a key that is not a UUID is a missing
row: 404, or `NotFound` over gRPC, with no statement.

### Outage answers

When the authorization provider does not answer in the decision time limit,
2 seconds by default, or the database cannot serve now, a command or a query
answers 503 with `Retry-After`, or `Unavailable` over gRPC. It never waits
for a provider that does not answer. The code of the answer is `undecided` for
the provider and `unavailable` for the database. The database cannot serve
now when it refuses, resets or loses the connection, or gives SQLSTATE class
08, 57P01, 57P02, 57P03 or 53300. Each such step writes one warn line
`command: the database cannot serve now` with the command and the step, at
most one line in each 10 seconds. A COMMIT whose outcome is not known stays
500, because the client must not take it as not done.

### Read your own write over gRPC

A gRPC command whose transaction wrote a tuple answers the header metadata
`authz-revocation: pending` and `authz-revocation-location` with the path
`/authz/revocations/{id}`, as the HTTP headers do. To read the write at once,
send the request metadata `authz-revocation-after` with that path or the
identity, and `prefer: wait=N` when you want a shorter wait. The query or the
stream waits at most N seconds, the wait cap (10 seconds) and half the time
that the deadline of the call leaves. It answers the last state in the header
metadata `authz-revocation`, and `unknown` for a revocation that is not yours.
A bad value is `InvalidArgument`.

### Write a child at once after its parent

A create answers `Authz-Revocation: pending` and `Authz-Revocation-Location`
until the relay writes its tuples to SpiceDB, often for less than one second.
Until then, a write that is decided on the new row, such as the create of a
child under it, gets 404. To create the child at once, send the
`Authz-Revocation-Location` of the parent's create in the header
`Authz-Revocation-After` of the child's create, and `Prefer: wait=N` when you
want a shorter wait:

```bash
curl -X POST https://<host>/api/projects/v1/projects/$PROJECT/tasks \
  -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: \"$KEY\"" \
  -H "Authz-Revocation-After: $LOCATION" \
  -d '{"title":"first task"}'
```

The write waits for that revocation before it decides, as a read does: at
most N seconds, the wait cap (10 seconds) and half the time that the deadline
leaves. Over gRPC, send the request metadata `authz-revocation-after`. Each
generated CRUD write and each declared command takes it. Its `Authz-Revocation`
names the revocation of its own write, not of the wait. From runtime v0.189.0,
a write that waited with `Prefer: wait=N` answers `Preference-Applied:
wait=<seconds>`, with the whole seconds that the wait allowed, as the status
route does.
A revocation of another tenant, or one that is still pending at the end of the
wait, does not fail the write: the write is then decided as with no header. A
value that is not a revocation is 400, or `InvalidArgument` over gRPC. The
browser proxy of a browser-service sends the header to the API as it is.

A Provision that grants admins or workers answers `Authz-Revocation: pending`
with no location. Send `Prefer: wait=N` with the Provision to wait for the
grants. When the answer says `complete`, the first command of an admin is
decided with the grant. A wait that stays pending for the whole wait cap
writes the warn line `authz.revocation.pending_past_cap`: check that the
worker mode, which runs the relay, runs.

In one command, the fill cannot do this yet. When a fill creates a row and
then a row that refers to it, with the loaded value that the first `Create`
returned, the runtime decides the reference at SpiceDB. SpiceDB does not
hold the tuples of the new row before the relay writes them after the
commit, so the decision is a deny: `Create` gives `command.ErrNotFound`,
the command answers 404 `not_found`, and the transaction commits nothing
(ST-SUP-RP10-LC05-T26). Today, create the row in one call and the rows that
refer to it in a second call that sends `Authz-Revocation-After`, and make
each call idempotent.

ST-SUP-Q221 decided a declared opt-in, `creates_with_children: true` on a
command or a worker (ST-SUP-RP10-LC59, ST-DECL-INV108). It is not built.
With it, the `Create` of a row that refers to a row that a `Create` of the
same attempt wrote is decided with no call of SpiceDB, only when validate
proved from the schema that the decisions and tuples of the first create
give the permission of the reference:

- **The union-subset rule.** Holding D proves X when D is a term of X's
  union, directly or through more unions only; arrows are followed when
  they are plain (`->` or `.any()`). `view = edit + reader` proves `view`
  from `edit`, and so does `view = edit2 + reader` with
  `edit2 = edit + member`. Any intersection (`&`), exclusion (`-`),
  `.all()`, caveat or expiration on that path makes it unprovable, and so
  does a subset that is not a term: `edit = owner + admin` does not prove
  `view = owner + admin + member`.
- **What counts as held.** Only what the runtime decided: a command decides
  the permission that the reference names, or its own; a worker decides its
  own permission, whatever the reference names; a consuming worker also
  decides the stager's, which is proved only from the stager's decisions. A
  caller relationship of the new row counts for the caller. An optional
  reference is never a witness.
- **What it does not cover.** The command's own permission on the new row
  (a `Get`, `Update`, `Relate` of it after its create), a move to a new
  parent, and an update or a delete of a new child are decided at SpiceDB
  as today. So is every reference while the schema that SpiceDB serves is
  not the one compiled into the pod's own release (a rolling deploy of a
  newer schema: up to about 7 s after it is served, an older pod can still
  use its proof), or when SpiceDB does not answer.

Validate refuses the opt-in when no reference has a proof, and warns
`new-reference-decided` of each reference that has none. Each such
decision writes the info line `command.reference.decided_by_create`.

### Act at once on a row that another user created

A second user of the same tenant can also act at once on a new row, such as
accounts receivable that converts a job that a sales user just created. Give
the second user the `Authz-Revocation-Location` of the create, such as in the
page or the message that names the new row. The second user sends it in
`Authz-Revocation-After`, with `Prefer: wait=N` if needed, and the command or
the read waits for the revocation before it decides (runtime v0.187.0;
before, it was decided as with no header and got 404 until the relay wrote
the tuples, in walkthrough 22 for 0.2 to 0.4 s):

```bash
curl -X POST https://<host>/api/erp/v1/jobs/$JOB/convert \
  -H "Authorization: Bearer $SECOND_USER_TOKEN" \
  -H "Idempotency-Key: \"$KEY\"" \
  -H "Authz-Revocation-After: $LOCATION" \
  -H "Prefer: wait=5" \
  -d '{}'
```

The wait gives the second user nothing but time. The decision is the
permission of the second user: a user with no permission on the row still
gets 404 after the wait. A read gives `Authz-Revocation: unknown` for a
revocation that the reader did not make, also after the wait, and the status
route `/authz/revocations/<id>` answers 404 to each user but the maker. A
revocation of another tenant is not found, and the request does not wait.
Thus the location never reads data of another tenant and never tells that a
row exists. The length of the wait still tells the holder of the location
when the revocation of the create completed, because the wait ends then. This
is true in the same tenant only. Give the location only to users who may know
when the create completed.
This is the recommendation (a) of the open question ST-SUP-Q206.

### Release notes for runtime v0.208.0

- A refusal before the router gives its fixed problem code to the access
  line of the request (`otel.SetErrorCode`): the 401 of the authentication
  its code, such as `SVC-AUT-001`, and the tenant check
  `tenant.not_provisioned` for 403, `unavailable` for 503 and `internal` for
  500 (erp-demo production run, ST-SUP-Q210). The code is the fixed code of
  the problem, never a token, a claim or the tenant. From otel-tracing
  1.26.0, the local access line of each request with a status of 400 or
  more holds it as `stego.error_code`; the line of an answer before the
  router still has no `http.route`.
- The telemetry runtime that otel-tracing generates imports no package of
  the runtime library: it gives each request a place under the key
  `otel.SharedErrorCodeKey`, an unnamed struct type, which
  `otel.SetErrorCode` fills and `otel.ErrorCode` reads too.
- `otel.SetErrorCode` also takes upper-case letters, for the fixed codes of
  the problem documents such as `SVC-AUT-001`. A code with any other
  character, such as a space, is still ignored.
- rest-crud 3.277.0, rpc-service 1.277.0 and browser-service 1.277.0 pin
  this runtime. It holds each change of v0.206.0 too.
- apidiff against v0.206.0: compatible (added `otel.SharedErrorCodeKey`).

### Release notes for runtime v0.203.0

- `STEGO_SPICEDB_CA_FILE` names a PEM file of certificate authorities that
  the connection to SpiceDB adds to the system roots, for a SpiceDB whose
  certificate a private authority signs (jsell-rh/stego#514, decision
  ST-SUP-Q217 (a), ST-SUP-RP14-LC14). `ReadSettings` and
  `ReadMigrationSettings` read it with the checks of the key files, and
  `DialSpiceDB` verifies SpiceDB with `SpiceDB.Roots`. With no value the
  roots are the system roots, as before. spicedb-authz 1.7.0 states the
  variable; rest-crud 3.271.0, rpc-service 1.271.0 and browser-service
  1.271.0 pin this runtime. It holds each change of v0.202.0 too.
- From rest-crud 3.272.0, rpc-service 1.272.0 and browser-service 1.272.0, the
  module path is `github.com/910-software-solutions/stego/registry/runtime`.
  The tag `registry/runtime/v0.203.0` of the repository
  github.com/910-software-solutions/stego holds the new path.
- apidiff against v0.202.0: compatible (added `service.SpiceDB.Roots` and
  `service.SpiceDBCAFileSetting`).

### Release notes for runtime v0.202.0

- The WARN line `command.database.unavailable` names `sqlstate_class`, the
  class of the SQLSTATE of the error, such as `53` for a full disk (53100)
  or no memory (53200), `57` for a server that shuts down and `08` for a
  lost connection, or `none` for an error with no SQLSTATE (security review
  of PR #499, L1). It never names the full code or the message.
- apidiff against v0.198.0: compatible (no change of the exported API).

### Release notes for runtime v0.198.0

- A database that takes no new connection is unavailable (walkthrough 26,
  U-503). `tenant.Unavailable` now counts SQLSTATE 55000 when the
  connection is made, such as after `ALTER DATABASE ... ALLOW_CONNECTIONS
  false`, 57P05 (an idle session that the server ended) and 53000, 53100
  and 53200 beside 53300. Thus such a request gets 503 with `Retry-After`
  and the code `unavailable`, not 500 `internal`. A 55000 of a statement,
  such as `currval` before `nextval`, stays 500.
  The code `unavailable` comes from the tenant check, which reads the tenant
  list. The tenant check keeps each answer for at most 5 s
  (`tenant.MaxGateAge`). In that time a request of a collection route passes
  the tenant check, and its handler answers the 503 with the code
  `<SVC>-INT-001` of the CRUD routes, with the same type and `Retry-After`
  (walkthrough 27, U-503). Thus an alert or a client reads the type
  `service-unavailable` or the status 503, not only the code.
- Each 503 has the one problem type `<error_type_base>service-unavailable`,
  from the command runtime, the query runtime, the tenant check and the
  CRUD routes. Before, a 503 of the command or query runtime or of the
  tenant check had the type of its code, `.../unavailable` or
  `.../undecided`. The codes do not change.
- `http.route` may hold a colon, so a declared command route such as
  `/jobs/{job_id}/status:convert` has its template on the span, the metric
  and the access line (walkthrough 26, L-ROUTE).
- apidiff against v0.196.0: compatible (no change of the exported API).

### Release notes for runtime v0.196.0

- Each line of one process names one `service.instance.id` (core49
  follow-up). Before, the HTTP telemetry of otel-tracing and the runtime of
  the service each made an instance identity, so the access lines and the
  lines of the runtime of one process named two. `otel.NewWithInstance`
  starts the telemetry runtime with the identity that it is given, and
  refuses an identity that is not a UUID v4 in lower case.
  `service.Parts.Instance` gives it to `service.New`; with no value the
  runtime makes an identity of its own, as before. The generated
  `NewService` of rest-crud 3.262.0, rpc-service 1.262.0 and
  browser-service 1.262.0 takes the runtime of otel-tracing and gives the
  runtime of the service its identity.
- apidiff against v0.195.0: compatible (added `otel.NewWithInstance` and
  `service.Parts.Instance`).

### Release notes for runtime v0.195.0

- Each JSON line of the runtime has one form (walkthrough 25, L-1; see
  [Read the logs](operations.md#read-the-logs)): `timestamp` in UTC,
  `severity`, `service.name`, `service.instance.id`, `event.name`,
  `message`, `trace_id` and `span_id`, and `attributes`, each key with the
  prefix `stego.` or a name of the semantic conventions for HTTP and RPC.
  The console of the log provider now writes `service.name`, the instance,
  the trace and the span, and gives each attribute the prefix, such as
  `stego.worker` for `worker`. The probe console puts the fields of a
  built-in event under `attributes` as `stego.<field>` (before, under
  `fields` with no prefix), the fields of a declared observation as
  `stego.field.<field>`, and `stego.tenant`, `stego.principal`,
  `stego.operation` and `stego.value` there too, and gives each line a
  fixed message. A count line holds `stego.dropped`. The log/slog handler
  names the service and the instance (`otel.SlogHandler.SetIdentity`, which
  the start and the migration job call), and each log/slog line of the
  runtime names an event of its own, such as `command.database.unavailable`
  or `lease.release_report.failed` in place of `stego.log`. A line of code
  outside the runtime may name only an event that `otel.GeneratedEvent`
  gives, and only from the generated package that writes it: main or the
  gRPC transport for `service.listening`, the runtime wiring for
  `service.audit_sink.selected`. A line names each key once.
- The probe console closes the windows of its rate limit at a tick, so the
  count line of the dropped lines of a window comes at the end of the
  window also when no later event of its name comes (walkthrough 25, L-2).
- `worker.run.failed` of a consumer run with the cause `relations_pending`
  (`probe.RelationsPendingCause`) is a WARN, as the outbox dispatches the
  message again; each other cause stays ERROR (walkthrough 25, L-3).
- `schema.backfill.throttled` is sent with its reason
  (`probe.BackfillThrottleReasons`). Before, each was dropped and counted.
- OpenTelemetry Go v1.47.0 (the OTLP log exporter v0.23.0), which fixes
  GO-2026-6974: a span or log attribute of U+FFFD runes could pass the limit
  of the attribute value length. The code of otel-tracing 1.22.0 requires
  the same versions.
- apidiff against v0.194.0: compatible (added `otel.SlogHandler.SetIdentity`,
  `otel.GeneratedEvent`, `probe.RelationsPendingCause` and
  `probe.BackfillThrottleReasons`).

### Release notes for runtime v0.194.0

- No change of the code. The tests of the relate job are now named for the
  record of the relate job: ST-SUP-RP10-LC58-T01 and T02 (before,
  ST-SUP-RP10-LC50-T06 and T07), so the sum of the module moves.
- apidiff against v0.193.0: compatible (no change of the exported API).

### Release notes for runtime v0.193.0

- A Provision or a Retire of a tenant (`command.ServeTenant`) whose change
  of the tenant list fails before the COMMIT on a database that cannot
  serve now (`tenant.Unavailable`, or `command.ErrUnavailable`), such as a
  server in recovery that gives SQLSTATE 57P03, answers 503 with the code
  `unavailable` and `Retry-After`, and its outcome row is `failed` with the
  code `unavailable` (walkthrough 24, D-k). Before, it answered 500
  `internal`. An error of the COMMIT itself stays 500, because its outcome
  may not be known. A retry with the same `Idempotency-Key` gives the stored
  answer when the first call committed.
- apidiff against v0.192.0: compatible (no change of the exported API).

### Release notes for runtime v0.192.0

- New `command.ResetAttemptsByID`: the reset of a count of wrong attempts
  for the ID of a row, which the fill does not read. The runtime only checks
  that the row is there, gives the fill no value of it, and decides the
  reset permission and the reset guards on the row, not the read permission
  of the entity. A row that is not there and a deny both give
  `command.ErrNotFound`. So a caller with only the reset permission can
  reset (decision ST-SUP-Q211 (a), walkthrough 24, U-R). The generator of
  command 1.28.0 gives the helper `<Command>Reset<Entity><Field>AttemptsByID`.
  The audit row is that of `ResetAttempts`. A refused reset writes no reset
  row, but the outcome row of the call (`denied`, or `failed` with
  `not_found`) stays.
- The relate step (decision ST-SUP-Q212 (a), walkthrough 24, P2-R2): new
  `command.RelateChange`, `command.NewRelateJob` and `command.RelateJob`, and
  the field `service.Parts.Relate`. The controller mode runs the job
  `relate` at start and each 10 minutes. For each relate change it holds the
  lease `stego.backfill.<change ID>` and stages the tuple of the relation of
  each row through the authz outbox. For a field or a reference relation, it
  first deletes each tuple of the relation on the row, so a former subject
  loses its access. A pass completes only after no process of a release
  before the step reports itself. Each batch writes one audit row
  `backfill-relation`, and each run writes the line `command.relate.pass`.
  See [Give the rows that are there a new relation](upgrade.md#give-the-rows-that-are-there-a-new-relation).
- A start that cannot use SpiceDB fails at the stage `service.start.spicedb`
  with the fault `spicedb.connection` or `spicedb.settings`, the object
  `spicedb`, a fixed remedy and a cause class, such as `unavailable` or
  `not_serving` (walkthrough 24, D-m). Before, the failure record had only
  the stage `component.stego-runtime.constructor[0]`. The error text names
  only the setting, never the endpoint, the key or the text of the cause.
- A start whose migration ledger lacks a migration of the release, or holds
  another digest of one, names the fault `migration.pending` or
  `migration.changed`, the object `migration_ledger`, a fixed remedy and a
  cause class (`ledger_missing`, `ledger_not_granted`,
  `migration_missing` or `digest_changed`) in its failure record
  (walkthrough 24, D-l). Before, the reason was only in a separate log line.
- Not in the runtime, in the same release: `apply` of the CLI
  (cli-application 1.30.0) leaves each secret field of a resource that
  exists out of the write and names it as not compared, so a document with
  a required secret applies again. `--set-secret FIELD` sends a new value
  from a private file, only of a field that the document holds. A command
  of the CLI names the required field that it lacks.
- New constants: `command.RelateCommand`, `command.RelateEvent`, the kinds
  `command.RelateTenant`, `RelateField` and `RelateReference`,
  `service.RelateJobName`, `service.SpiceDBConnectionFault`,
  `service.SpiceDBSettingsFault`, `service.MigrationPendingFault`,
  `service.MigrationChangedFault` and the four `service.MigrationCause...`
  classes.
- apidiff against v0.191.0: compatible (21 additions, no other change).

### Release notes for runtime v0.191.0

- New setting `STEGO_LOG_LEVEL` (`otel.LogLevelSetting`): `debug`, `info`,
  `warn` or `error`, and `info` when it has no value. `otel.ParseLogLevel`
  reads it, and `otel.NewSlogHandlerAt` gives the log/slog handler of a
  level; `service.UseJSONLog` uses it. `probe.Start` and `service.Migrate`
  refuse another value with fixed text. The console writes a built-in
  worker event of the level or higher; the service events are on the
  console at each level (recommendation of ST-SUP-Q210).
- New built-in event `worker.run.completed` (INFO, the field `worker`) for a
  run that committed, from `probe.Worker.RunCompleted`. The ticker and
  `command.ObserveConsumer` send it through `command.CompletedObserver`.
- `command.Runtime.RecordFailure` writes the INFO line `command.refused`
  (`command.RefusedEvent`) for an answer of 400 to 499: the action, the
  resource type, the status, the fixed code and the declared field. It holds
  no tenant, principal, resource ID, request value or error text.
- `service.Migrate` writes the lines `migrate.job.started`,
  `migrate.step.started`, `migrate.step.completed` and
  `migrate.job.completed` (`service.JobStartedEvent` and the others).
- New `otel.Security(ctx)`: the log/slog handler writes a line of INFO or
  higher of a context that it marks at each level of `STEGO_LOG_LEVEL`. The
  runtime marks the lines of the security class: another entry under the ID
  of an audit entry, a stored answer that does not open, a decision that the
  authorization provider could not make, and the seal and re-seal pass
  lines (security review of PR #492, L3).
- New `probe.BuiltinReserve`: an INFO worker event never takes the last 16
  places of the queue of the built-in events, so a burst of
  `worker.run.completed` never drops `worker.run.failed` or a service event
  (security review of PR #492, L1).
- apidiff against v0.190.0: compatible.

### Release notes for runtime v0.190.0

- New field `command.CounterSpec.Reset`: true when a command of the service
  declares the reset of the counter (ST-SUP-RC32-OC24). `Matches` of a value
  of such a counter checks it only when the command decided its own
  permission on the row of the value in the same attempt: on its resource, in
  a locked read or list (`GetForUpdate`, `GetForDelete`), or for a write of
  the row. On any other row it gives `false`, charges nothing and fails the
  call with an internal error that names the counter (ST-SUP-RP10-LC54;
  decision ST-SUP-Q209 (d); the residual
  sec-489c of the second security review of PR #489). Before, a caller who
  held the reset and the permission of a command decided on another row or
  another resource type could read the row with no lock and check it with no
  bound, a reset after each limit. A counter with no `Reset`, as the code of
  a generator before gives it, is held too when a command declares its
  reset: `NewCommand` marks each counter that `CommandSpec.Resets` names. A
  locked read or list counts for its row only once its lock holds, and a
  command on the service-level resource counts for no row, also through an
  effect or a write (security review of PR #491, L1, L2 and I1).
- `command.CommandSpec.ResetGuards` can hold the permissions of commands of
  another resource type that write the entity: the generator of command
  1.26.0 gives them, and the runtime decides each on the row of the reset.
- apidiff against v0.189.0: compatible.

### Release notes for runtime v0.189.0

- In a rolling release, the relay of the release before no longer parks a
  message that only the new release can read (walkthrough 23, P2-R;
  recommendation (a) of ST-SUP-Q208, open for the Product owner). New mark
  `outbox.NotUnderstood`: the worker takes the attempt back, keeps the
  message pending with the wait of an unknown destination, so a worker of
  the new release delivers it in the order of its key, and parks it with the
  cause `not_understood` only when it is older than
  `Destination.NotUnderstoodBound` (one hour by default, at most seven days).
  The relay gives the mark for a tuple of a relation that is not on its allow
  list (`authz-relation-unknown`), a payload of a later version or with a
  field or an operation that it does not know (`authz-payload-newer`), and a
  delete of a retired tenant of a resource type outside
  `RelayConfig.Types` (`authz-type-unknown`). It writes nothing for them.
  The metric `outbox.not_understood` counts them, and the first hold of each
  writes a WARN line `outbox.not_understood`, at most one a minute for each
  destination. The bound of `authz` is one hour (`authz.NotUnderstoodBound`).
  A payload of a later version is held for each operation, also for a
  filtered delete and the delete of a tenant.
- The metric and the log line `outbox.parked` name the fixed cause class
  (`refused`, `not_understood`, `attempts_exhausted`, `lookups_exhausted`,
  `outcome_unknown` or `tenant_not_active`), and the line names the failure
  code.
- The failure record of the migration Job names the fault, the object and
  the remedy of the steps `open`, `authorization_schema`,
  `authorization_schema_again` and `authorization_schema_served`
  (`service.StepError.FaultClass`, `FaultObject` and `FaultRemedy`), and a
  refused key source of a start names its fault (`authn.Keys*Fault`).
- A unique violation of an index that the entity does not declare names the
  declared fields of its key when the name of the index is
  `<table>_<columns>_key` and each part is a column of the entity, such as
  the index of a unique step of a newer release.
- New `command.Runtime.AwaitRevocationOn`: as `AwaitRevocation`, and it sets
  `Preference-Applied` when a request with `Prefer: wait=N` waited. `Serve`
  calls it.

### Release notes for runtime v0.188.0

- A count of wrong attempts can decay (ST-SUP-RP10-LC54, decisions
  ST-SUP-Q205 (c) and ST-SUP-Q207 (a)). New field
  `command.CounterSpec.UnlockAfter`, zero for no decay, or from
  `command.MinUnlockAfter` (1 minute) to `command.MaxUnlockAfter` (24 hours)
  in whole seconds. The charge counts again from 1 when the last charge of the
  row is the window or longer before on the clock of the database, and changes
  nothing at the limit. New method `command.(*Counter).UnlockAfter`.
- The checks of the windows before, `failures`, never decay: with the checks
  of the window, they count the checks since the last match or new value,
  also those of a pod of a release before. New function `command.FailureCap`: min(10 x limit, 100). At the cap
  the row stays locked until a new value or a reset.
- New function `command.ResetAttempts` and new field
  `command.CommandSpec.Resets`: a command that declares the reset of a field
  sets the counts of a row to 0 when it commits, after a decision of its own
  permission on that row, and writes an audit row with the marker
  `attempts_reset` (`command.AttemptsResetMarker`). It gives only an error.
  `Matches` on that field in that command fails the call. New field
  `command.CommandSpec.ResetGuards`: the permissions of the other commands
  that write the entity; a caller who holds one of them on the row gets
  `ErrDenied`, and the counts stay.
- The migration of the key table adds the nullable columns `charged_at` and
  `failures` to `attempt_counts`, only when the catalog does not hold them.
  The manifest of a release before still holds the table. A charge gives a
  count with no time the time of the charge.
- A charge that waits for a slot counts the waiting charges of all the
  tenants only for the line `command.attempts.pool_saturated` (security review
  of PR #488, L2).
- apidiff against v0.187.0: compatible.

### Release notes for runtime v0.187.0

- `AwaitRevocation`, `AwaitRevocationRPC`, `AwaitWrite` and `AwaitWriteRPC`
  wait for a revocation that any principal of the caller's tenant made, so a
  second user who has the location of a create acts at once on the new row
  (walkthrough 22, P3-A; recommendation (a) of ST-SUP-Q206, open for the
  Product owner). New method `authz.Runtime.WaitInTenant`, which tells
  whether the caller made the write. A read answers `unknown` for a
  revocation of another principal, also after the wait. `WaitFor`, `Status`
  and the status route do not change. A revocation of another tenant is not
  found at once.
- `command.attempts.pool_saturated` comes when a charge of a counted check
  must wait for a slot while the charges hold each shared slot of the pool of
  the charges (ST-SUP-RP10-LC54; walkthrough 22, P1-PS). Before, the line
  could not come: the slots keep the connections in use under the size of
  the pool. The line gives `stego.pool.in_use` (the slots in use),
  `stego.pool.max_open` and `stego.pool.waiting` (the charges that wait), and
  no value and no tenant. A charge that waits only for the limit of its own
  tenant does not write it.
- New function `authz.StatusClass`: the class of a gRPC status code, and for
  a refusal the fixed class of its reason after a period, such as
  `invalid_argument.relationships_exist`. The failure record of the migration
  Job gives it as its cause (ST-SUP-RP14-LC03).

### Release notes for runtime v0.186.0

- A consumer run that fails after a decision denied a row with an empty
  authorization token, whose relations the relay has not written yet, is a
  transient refusal with the outcome `undecided`, the new cause class
  `relations_pending` in `probe.WorkerCauses` and the failure code
  `relations-pending.<n>.<time>` (ST-SUP-RP10-LC17), only when its failure
  comes from that deny. The poison count does not count it, and a declared
  refusal or a deny of the staging principal of such a run does not park the
  message, for at most `command.RelationsPendingAttempts` (10) attempts and
  `command.RelationsPendingAge` (5 minutes); then the attempt is classed as
  before and writes the warn line `command.consumer.relations_pending_past_bound`
  (`command.RelationsPastBoundEvent`). A panic stays a panic, and a run that
  commits does not change. Walkthrough 21: a consuming worker
  failed about one first attempt in three with `fill_error`, as the list of
  an update event did not give the row of its create event.
- `command.NewStruct` refuses a value of more than `command.MaxStructNodes`
  (100,000) values in all, counted at each place that holds a shared map or
  list, before the conversion walks it (ST-SUP-RP10-LC57).

### Release notes for runtime v0.185.0

- The runtime counts each charge of a check of a counted compare-only secret
  that cannot commit, so the call answers 503, in the counter
  `command.attempts.failed` of the meter provider of the service, with the
  unit `{charge}` and the attributes `action`, `entity` and `cause`. It has no
  tenant and no value. The counter counts each failed charge, also when the
  rate limit holds back the log line of the same name (ST-SUP-RP10-LC54,
  security review of PR #482, I4). New field `command.Config.Meter`; with no
  value, the runtime records no metric.
- `kafkainbox.CheckSettings` names the setting of a subscription that it
  cannot read: the topic or the group setting or its `_FILE` form, such as
  `STEGO_KAFKA_CONSUMER_ORDER_CHANGES_TOPIC_FILE`. Before, it named the group
  setting for each such setting (ST-SUP-RP14-LC08, security review of PR #483,
  L1). The new type `kafkaoutbox.SettingError` names the setting of each
  refusal of the Kafka settings; its text does not change.

### Release notes for runtime v0.184.0

- `service.New` gives the log provider of the service to the command runtime
  of its commands and collections too, not only to the runtime of its
  workers and its provisioning. So the lines `command.attempts.failed` and
  `command.attempts.pool_saturated` of a command are JSON lines with
  `stego.*` attributes, and an outcome row that is not written is logged
  (ST-SUP-RP10-LC54; walkthrough 21, U-E).
- New `otel.SlogHandler` and `service.UseJSONLog`: each `log/slog` line of a
  process, such as the lines of the seal and re-seal jobs of the controller
  and the line of a refused step of the migration job, is one JSON line on
  standard error, with the timestamp, the severity, the event name, the
  message and the attributes under `stego.`. The event name comes from the
  attribute `event.name` (`otel.SlogEventKey`), from the message when it is an
  event name, or is `stego.log`. New event names `command.seal.pass`,
  `command.reseal.pass` and `migrate.step.refused`. The generated code calls
  `service.UseJSONLog` first in `NewService` and in `Migrate`.
- A start whose database pool did not open gives the failure record of the
  process its fault class (`database.connection`, `database.settings` or
  `database.runtime_role`), the object `database`, a remedy and a cause class:
  `connection_closed` for a connection that ended in the middle of a message,
  as when the database server restarts, else the class of the SQLSTATE or the
  network, or `invalid_setting`, `refused` or `connection_failed`. New
  `tenant.ErrRuntimeRole` (walkthrough 21, U-F).

### Release notes for runtime v0.182.0

- The worker mode runs the Kafka inbox of each declared subscription, each in
  its own consumer group and with its own Kafka client (ST-SUP-RP14-LC08,
  decision on ST-SUP-Q204). `service.WorkerParts` has the new field
  `Inboxes`: a function that gives the inbox of each subscription. The field
  `Inbox` is deprecated; the worker mode takes it as one inbox when `Inboxes`
  is nil. The purge timer of the outbox purges the refused records once, and
  the readiness of the worker mode names each inbox that stopped or that fails.
- The inbox of a subscription reads its settings by the name of the
  subscription: the new `kafkainbox.SubscriptionTopicSetting`,
  `SubscriptionGroupSetting`, their `_FILE` forms and
  `SubscriptionInfiniteRetentionSetting` are patterns with `{subscription}`,
  and `kafkainbox.Setting` gives the name of one subscription, such as
  `STEGO_KAFKA_CONSUMER_ORDER_CHANGES_TOPIC`. The new
  `kafkaoutbox.ReadSubscriptionConsumer` takes the subscription. `Settings`
  has the field `Subscription`, and the new `RetentionFitsFor` takes the name
  of the retention setting for its error (ST-SUP-RP01-LC38). The fixed names
  `TopicSetting` and the others, `kafkaoutbox.ReadConsumer` and
  `RetentionFits` stay, deprecated, for the API of the runtime.
- `kafkainbox.ReadSettings` and the new `kafkainbox.CheckSettings` refuse a
  value of a fixed name of `kafka-consumer` before 0.5.0, such as
  `STEGO_KAFKA_CONSUMER_TOPIC`, with an error that names it and the name to
  set. `CheckSettings`, which the start of the worker mode calls, and the new
  `kafkainbox.CheckGroups` also refuse two subscriptions with one consumer
  group, naming the group setting of the second. The start of the worker mode
  stops at the stage `service.start.setting.kafka-consumer.<variable>`.
- The events `kafka.consumer.failed` and `kafka.consumer.start_refused` of an
  inbox have the attribute `subscription`.

### Release notes for runtime v0.181.0

- The charges of the checks of counted compare-only secrets of one tenant use
  at most half of the connections of the pool of the charges at one time, and
  at least one. The charges of all the tenants use at most all but one
  connection, and only a tenant with no charge in flight can take the last
  one. A charge waits for a slot, not for a connection that another tenant
  needs. Before, the checks of one tenant could fill the pool of 4
  connections, and the checks of the other tenants waited up to 5 s and
  answered 503 (ST-SUP-RP10-LC54, security review of PR #481, L1, and of PR
  #482, L1). The runtime writes the warn line `command.attempts.pool_small`
  once for a pool of the charges of fewer than 3 connections or with no
  bound: in `command.New` for `Config.ChargeDB`, and else at the first charge.
- The error line `command.attempts.failed` comes at most once in 10 s for
  each action, entity and cause class, with the new attribute
  `stego.held_back` (`held_back` with no log provider), the count of the lines
  that it held back. Before, each failed charge wrote a line (security review
  of PR #481, I1).
- `sealed.CountedBy` is deprecated and binds nothing: it gives its value with
  no change. A counter binds only with the store key of a runtime, through
  `sealed.CountedByKey`, so code with no store key, such as a fill, cannot
  make a counted value. The function stays, so the API stays compatible
  within v0 (ST-STD-RUNTIME-R03, security review of PR #481, I2).

### Release notes for runtime v0.180.0

- The runtime writes a counted compare-only value that a read gave only into
  its own field and row, in the attempt that read it. A fill that gives it to
  a compare-only field with no limit, or to another row, gets an internal
  error, and the command commits nothing. Thus a fill cannot get the stored
  hash back through a field with no limit, and cannot give the hash a new
  count of 0 in a new row (ST-SUP-RP10-LC54, security review of PR #479, L1).
  New methods `sealed.Compared.IsCounted` and `sealed.Compared.CounterBy`.
- A rerun of a command in the same request does not charge again the checks
  that an earlier attempt charged only for the same stored value. A new PIN
  that a reset wrote between two attempts gets its own charges (security
  review of PR #479, L2).
- The charge of a check waits at most 1 s for a lock, such as behind the reset
  of a command that has not committed; then the command answers 503, and the
  line `command.attempts.failed` has the cause `lock_timeout`. When each
  connection of the pool of the charges is in use, the runtime writes the warn
  line `command.attempts.pool_saturated` through its log provider, with the
  entity and the counts of the pool and no value, at most once in 10 s
  (security review of PR #479, L3). From runtime v0.187.0 it writes the line
  when a charge waits for a slot (see the release notes of v0.187.0).

### Release notes for runtime v0.178.0

- The charge of a check of a counted compare-only secret runs on a small pool
  of its own, of at most 4 connections, apart from the main pool. Before, it
  took a second connection of the main pool, so a full main pool made the
  check wait for 5 s and the command answered 503. New field
  `command.Config.ChargeDB`; with no value, the charges use the audit pool
  (ST-SUP-RP10-LC54, security review of PR #451, L2).
- A rerun of a command in the same request, such as after a conflict, a stale
  prepared statement or a seal switch, does not charge again the checks that an
  earlier attempt charged (security review of PR #451, I1 of the second
  review).
- `sealed.Compared.Stored` and `StoredContext` give the new error
  `sealed.ErrCounted` for a counted stored value, and no hash. The new
  `sealed.Compared.StoredBy`, `sealed.StoreKey` and `sealed.NewStoreKey` give
  the hash only to the runtime that bound the counter with the new
  `sealed.CountedByKey`. `sealed.CountedBy` binds no store key (and binds
  nothing as of runtime v0.181.0), and both keep the counter of a value that
  has one (ST-SUP-RP16-LC03, security review of PR
  #451, L1).
- `lease.SealsText` adds the columns and the default of `stego_schema.seals`
  only when they are not there, so a run again takes no `ACCESS EXCLUSIVE` lock
  on the table (security review of PR #472, I2).

### Release notes for runtime v0.177.0

- The cause of `worker.run.failed` and of the log line
  `command.consumer.run_failed` is `fill_json`, `fill_parse` or
  `fill_canceled` for a fill error whose chain holds an `encoding/json` error,
  a `strconv` `NumError` or a `time` `ParseError`, or `context.Canceled`, and
  `fill_error` for another fill error. The runtime reads the type of the
  error, never its text. The three new classes count for the poison bound as
  `fill_error` does (ST-SUP-RC35-OC06, decision ST-SUP-Q203 (b)). New values
  of `probe.WorkerCauses`. See [A failed run of a consuming
  worker](#a-failed-run-of-a-consuming-worker).
- The failure record of a migration Job step that SpiceDB failed gives the
  cause class of the gRPC status code, such as `unavailable`, never the text
  (ST-SUP-RP14-LC03). New method `service.StepError.CauseClass`, new function
  `authz.CodeClass`. See [When the Job fails](delivery.md#when-the-job-fails).

### Release notes for runtime v0.176.0

- A worker can declare `reads: [Entity]`. Its fill gets a read-only
  repository `<Entity>Reader`, of the new type `command.Reader`, with `Get` and
  `List` only, and values `<Entity>View` with no secret field
  (ST-SUP-RP10-LC53, ST-SUP-RC35-OC19, decision ST-SUP-Q186 (a)). New field
  `command.WorkerSpec.Reads`, new field `command.EntitySpec.ReadOnly`, new
  function `command.ReadRepo` and new field
  `service.WorkerParts.ConsumerReadTables`, which the generated code fills. The
  migration job gives the worker role of a consuming worker only `SELECT` on
  the columns that the view reads.
- A compare-only secret with `attempts: {limit: N}` has a limit of wrong
  checks. The runtime charges each check in its own table `attempt_counts`
  before it checks the value, so the count stays after a refusal and parallel
  checks stop at the limit. A charge that cannot commit answers 503
  (ST-SUP-RP10-LC54, decision ST-SUP-Q185 (a)). New `command.CounterSpec`,
  `command.NewCounter`, `command.CountedAt`, `command.HashCountedOf`,
  `idempotency.AttemptsTable`, `sealed.Counter`, `sealed.CountedBy` and
  `sealed.Compared.IsNew`. See [Limit the wrong
  attempts](fills.md#limit-the-wrong-attempts).
- An entity with `events: {fields: [...]}` puts the declared fields of the row,
  as `data`, and their version, as `data_version`, in each change event
  (ST-SUP-RP10-LC55, decision ST-SUP-Q184 (c)). New fields
  `command.EntitySpec.EventData` and `EventVersion`, and the functions
  `command.EventValue` and `command.JSONValue`. See [The fields of a change
  event](fills.md#the-fields-of-a-change-event).

### Release notes for runtime v0.139.0

- A fill that returns `command.ErrNotFound` answers 404 with the code
  `not_found`, the audit outcome `failed`, and `NotFound` over gRPC
  (ST-SUP-RP10-LC51). The answer is the same for a row that is not there and
  for a row that the caller may not read. A fill that maps the error to a
  declared refusal keeps its refusal. New constant `command.ErrorCodeNotFound`.
- Each other 500 of a fill writes one error line `command.fill.failed` with
  the command and a fixed cause class: `fill_error`, `closed`, `internal`,
  `sealed`, `panic` or `deadline`, unless a line of a failed statement or of
  an internal error already names the cause. The line holds no error text. It
  writes at most one line for each command and cause class in ten seconds,
  with `held_back`.
- Provision relates the tenant resource to the service-level resource of the
  tenant when the schema declares the relation `service` on the type `tenant`,
  and Retire deletes it (ST-SUP-RC27-OC18, ST-SUP-RP10-LC52). New field
  `command.TenantSpec.ServiceRelation`, constant
  `command.TenantServiceRelation`, and field `service.Parts.Relations`, which
  the generated code fills.
- Provision and Retire store a keyed fingerprint and a sealed answer in the
  key table when the service has a key ring, as each other command does
  (ST-SUP-RP16-LC05).
- The start of a service refuses a database whose migration ledger holds
  another digest of a migration of the release, at the stage
  `service.start.migration`, with `service.ErrMigrationChanged`
  (ST-SUP-RP14-LC13). New field `service.Parts.Migrations`, which the
  generated code fills from `MigrationDigests` of postgres-adapter 6.15.0.

### Release notes for runtime v0.136.0

- A command waits for the caller's revocation before it decides
  (ST-SUP-RP10-LC47): `command.Serve` and `command.ServeRPC` read
  `Authz-Revocation-After`, or the metadata `authz-revocation-after`, and wait
  as a read does. New methods `command.Runtime.AwaitRevocation` and
  `command.Runtime.AwaitRevocationRPC`, which the generated CRUD writes call
  before `command.Run`. See [Write a child at once after its
  parent](#write-a-child-at-once-after-its-parent).
- A unique violation of a declared key names the fields of the key
  (ST-SUP-RP10-LC48): `command.OutcomeOf` gives the fields in `Field` and the
  reason `command.ReasonUnique`. New field `command.EntitySpec.Unique`, which the
  generated code fills, and function `command.UniqueKey`.

### Release notes for runtime v0.134.0

- A consumer runs no fill before SpiceDB holds the tuple of its own worker in
  the tenant of the message (ST-SUP-RC35-OC18). A message whose tuple is
  missing goes back to pending with the failure code `worker-tuple-missing`
  and the cause `worker_tuple_missing`, and the poison count does not count
  it. Thus a subscription that a release adds no longer parks its backlog
  before the controller of the release writes the tuple. New field
  `command.Config.WorkerTuples`, constants `command.WorkerTupleMissingCode`
  and `command.HeldTupleAge`, and the class `worker_tuple_missing` in
  `probe.WorkerCauses`. A read of the tuple that fails is undecided, with the
  cause `undecided`. A tuple that stays missing for 15 minutes
  (`command.WorkerTupleAlertAfter`) logs the warn line
  `command.consumer.worker_tuple_missing` with the worker, at most once in
  each 15 minutes.
- The key set reads the refresh age of a key document from its
  `Cache-Control` max-age, from `authn.MinAge` (1 minute) to `authn.MaxAge`
  (5 minutes) (ST-SUP-RP13-LC23). From half the refresh age a known key ID
  starts one load beside the request; at the refresh age it waits for the
  load, so a key that the issuer removed stops within the refresh age. New
  function `authn.RefreshAge`.
- A cursor key ring: `query.Config.ExtraCursorKeys` opens cursors and seals
  none, and the api mode reads the new optional variable
  `STEGO_QUERY_CURSOR_EXTRA_KEY_FILE` (ST-SUP-RP11-LC23). Rotate the key in
  three rollouts: give the new key as the extra key; give the new key as the
  key and the old key as the extra key; after the longest list session,
  remove the extra key. No cursor is refused.
- The tenant check does not run for a request that the main router matches
  to no route: such a request gets 404, or 405, and not 403
  `tenant.not_provisioned` (ST-SUP-RC27-OC14).
- Retire stages, for each tenant-qualified type of the schema, the delete of
  the tuples of the rows of the tenant (ST-SUP-RC27-OC15). The relay deletes
  them by resource ID prefix in parts (`authz.TenantDeleteLimit`, 1000, and
  `authz.TenantDeleteCalls`, 10, in one attempt), with no lock of the floor
  row, raises the floor after each attempt, and logs
  `authz.tenant_tuples.deleted` with the type and the count. New operation
  `authz.OperationDeleteTenant`, function `authz.TenantPrefix`, method
  `authz.Handle.UnrelateTenant` and interface `authz.PrefixDeleter`.
- One problem form (ST-SUP-Q171 (a)): the type of each problem of the command
  runtime, the query runtime, the idempotency check, the tenant check and the
  provisioning routes is `<error_type_base><path>`, where the path is the code
  with `.` and `_` as `-`, such as `tenant-not-provisioned`, or the name of the
  status when there is no code, such as `not-found`. With no
  `error_type_base` the type is `about:blank`. The codes do not change. A
  client that reads the type, not the code, must read the new types. New
  function `authn.ProblemTypeOf`, field `query.Config.ProblemBase` and method
  `query.Runtime.WriteProblem`.
- The tenant list of the operator (ST-SUP-Q172 (a)): `GET <base
  path>/stego/tenants`, when the schema declares `permission list_tenants`
  on the resource type of the provisioning. It pages with `limit` and
  `cursor`, answers `items` and `next`, and writes the audit row
  `list-tenants`. New function `command.ServeTenantList`, method
  `service.Service.TenantList`, function `tenant.Entries` and method
  `authz.Handle.DecideService`. Add `permission list_tenants = operator` to
  the definition of the resource type of the provisioning to serve it.
- Upgrade note: a message that an older runtime counted keeps its attempt
  count, and so its back-off, after the upgrade. The runtime does not reset
  it, because the outbox row holds no runtime version, and a new column would
  need a schema step of every service. The back-off has the bound
  `RetryMax` (5 minutes by default), so the next attempt comes at most 5
  minutes after the upgrade. A failure code of an older form starts the
  poison count at 1. When such a message is parked, resolve it with
  `not_acted` to send it again.

### Release notes for runtime v0.133.0

- Each call of the decision provider has a time limit, 2 seconds by default
  (`authz.DefaultDecisionTimeout`, `authz.Config.DecisionTimeout`). A call
  past it is transient: 503 with `Retry-After`, or `Unavailable` over gRPC.
  New cause `authz.CauseStoreUnavailable` for a revocation floor that the
  database cannot read now.
- New function `tenant.Unavailable`. `command.ErrUnavailable` and the code
  `command.ErrorCodeUnavailable` answer a database that cannot serve now with
  503; `query.ErrorCodeUnavailable` does the same for a query, and
  `query.Transient` also reports such an error. `command.RPCCode` maps 503 to
  `Unavailable`.
- `command.EntitySpec` has the new fields `Check` and `Constraints`, and the
  new helpers `command.MinLength`, `MaxLength`, `OneOf` and `MustPattern`.
  `query.Spec` has the new field `ValidID`, with `query.ValidUUID`.
- A worker's create of a row with no parent is decided on the tenant resource
  with the permission of the worker, and in a consumer also as the staging
  principal (ST-SUP-Q164 (b)). New error `command.ErrNoParent` and function
  `command.NoParentEntity` stay for the API; no call gives the error now. The
  class `create_without_parent` is in `probe.WorkerCauses`.
- `command.ServeRPC` sends the revocation metadata of a write.
  `query.Runtime.AwaitWriteRPC` waits for the revocation of the request
  metadata, which `query.ServeRPC` and `query.ServeStream` call;
  `query.StreamRPC` has the new field `Runtime`. New constants
  `authz.RevocationMetadata`, `RevocationLocationMetadata`,
  `RevocationAfterMetadata` and `PreferMetadata`.
- `command.ServeTenant` answers `Authz-Revocation: pending` for a Provision
  that staged a tuple, and waits with `Prefer: wait=N`.
- `authz.Config.PrincipalWaiters` (default `authz.DefaultPrincipalWaiters`, 4)
  limits the waits of one principal. A wait that the cap ends while the
  revocation is pending logs `authz.revocation.pending_past_cap`.
- The status route and the generated wait code give no internal code when the
  client goes away during a wait.
- New function `service.ReadModeSettings`: only the api mode reads
  `STEGO_QUERY_CURSOR_KEY_FILE`.
- The failure class of the Kafka inbox names the operation that failed:
  `broker:<CODE> (describe-configs)`, `(fetch)` or `(commit)`. New function
  `kafkainbox.ValidateMigration` and the step `kafka_inbox_validate`, which
  validates the CHECK of the refused reasons in a transaction of its own.

### Release notes for runtime v0.132.0

- New function `outbox.NewDeclaredResolver` takes the declared rules of each
  destination (`outbox.Rules`); `outbox.NewResolver` is deprecated. A resolution takes the stricter of the row and the
  declared rules, and a message of a destination that the release does not
  declare answers 409. `service.Parts` has the new field `Destinations`, which
  generated code sets. New functions `authz.Rules`, `auditoutbox.Rules` and
  `kafkaoutbox.Rules`.
- The Kafka inbox reads the tenant again in the transaction of each record,
  under the lock of the tenant list (new function `tenant.HoldList`), so a
  record of a tenant that a Retire retired after the lookup is refused as
  `retired-tenant`. `kafkainbox.Config` and `service.InboxEnv` have the new
  field `TenantSchema`. The step `kafka_inbox` grants the worker role SELECT
  on the tenant list.
- The console of the log provider keeps a rate-limit budget of its own for
  the lines of severity ERROR and FATAL. The probe console holds the built-in
  worker events, such as `worker.run.failed`, to the same rate limit.
- `authz.RelayConfig` has the new field `Relations`, the allow list of the
  relations that the relay writes; `NewRelay` refuses no list. Generated code
  sets it from the authorization schema.
- An attempt of the Kafka destination that no broker answers logs
  `kafka.delivery_failed` with the class `broker-unavailable`. After 5 such
  attempts in a row, `/readyz/modes` of the worker mode answers 503 with
  `the destination kafka fails: broker-unavailable`. `outbox.Destination` has
  the new field `Failing`, and `outbox.Worker` the new function `Failing`.
- A Retire deletes the tuples of the admin relation of the tenant.
- The migration of the refused records of the Kafka inbox replaces the CHECK
  of the reasons when the list of reasons changes.
- New function `pgtest.ReapNetwork` (test support).

### Release notes for runtime v0.131.0

- The log provider of the telemetry runtime writes each record at the level
  warn or higher as one JSON line on standard output, with or without an
  OTLP endpoint (ST-SUP-RP03-LC16). `otel.Runtime.LoggerProvider` is no longer
  nil without a collector. New functions `ConsoleDropped` and
  `ConsoleFailures`.
- New parked-message routes of the api mode (ST-SUP-RP14-LC09,
  ST-SUP-RP14-LC10): `service.ParkedRoutes`, `Service.RegisterParked`,
  `outbox.Resolver`, `outbox.Queue.Parked`, `outbox.ErrRefusedOutcome`,
  `authz.Runtime.StateHook`, `query.KeyCursor` and `query.Cursor.Key`.
- New function `command.Snapshot`: the generated contract package takes a
  copy of each declared list when it starts, and the handle refuses a list
  that a fill changed after that.
- `command.ListValue` has the new field `KeyOf` (`command.KeyUUID`), and `command.Reference`
  the new field `Check`. Generated code sets them; code that does not set them
  needs no change.
- The Kafka inbox reads the retention and the cleanup policy of its topic
  again each minute, refuses a topic with no cleanup policy and a record whose
  key names another row than its payload.
- A worker can create a row that has a reference: the authorization
  migration gives the worker role the same grants as the runtime role on the
  revocation floor and records. A refused statement of the authorization
  handle keeps its SQLSTATE, so a missing grant parks a consumed message
  (cause `not_granted`) and does not retry it without end.
- A provision of a retired tenant ID answers 409 with the code
  `tenant.retired`; a provision of an active ID keeps `tenant.provisioned`.
  New error `tenant.ErrRetired` and code `command.ErrorCodeRetired`.
- The problem types `idempotency-key-missing`, `idempotency-key-in-progress`
  and `idempotency-key-reused` of a command sit under the `error_type_base`
  of the service. `command.Config` has the new field `ProblemBase`.
- The readiness of the worker mode names the stage of a stopped Kafka inbox,
  such as `the component inbox-order-changes stopped at the stage
  kafka-consumer.retention`.

### Release notes for runtime v0.130.0

- New package `outbox/kafkainbox`: the Kafka inbox of a subscription
  (ST-SUP-RP01-LC35 to ST-SUP-RP01-LC38). New functions
  `outbox.Queue.Receive`, `outbox.Worker.AddPurge`, `outbox.Worker.Retention`,
  `kafkaoutbox.Connection` and `kafkaoutbox.ReadConsumer`. `service.WorkerParts` has the new field
  `Inbox`, and `service.Migration` has the new field `Inbox` with the step
  `kafka_inbox`. Code that does not set them needs no change.

### Release notes for runtime v0.127.0

- A command fill gives no SQL text. Before v0.127.0, a command fill that read
  a set of rows gave its own SQL text in `command.ListSpec` and made its list
  with `command.NewList`. That SQL named the columns of the entity, so the
  first schema change broke it with 500 `internal` (walkthrough 7, B1). Now
  every handle takes only the lists that its declaration declares
  (ST-SUP-RP10-LC38). The fill still compiles, but each call that uses its own
  list fails with this error:

  ```text
  command: internal error: the command place-order reads the list of-order of
  Line, which its declaration does not declare: since runtime v0.127.0 a fill
  gives no SQL text, so declare the list under lists: of the command and use
  the generated list of the command contract (ST-SUP-RP10-LC38)
  ```

  To move a fill, write the `WHERE` terms of its `Read` statement as `filter`
  terms under `lists:` of the command. Write a term whose value came from the
  fill as `param:`. Run `stego apply`. Then remove the `command.ListSpec` and
  the `command.NewList` call from the fill, and call the generated
  `<Command><List>With` function in place of the filter values.
- A list term can give a parameter (`param:`), for a command and for a worker.
- A list of a child entity takes its decided parent row (`parent: true`), and
  a parameter of a reference is checked with the key check of the entity.
- The generated upsert of a collection finds its row through a generated list.
- A failed statement writes one log line with its cause class and SQLSTATE
  class (see "A failed statement").
### Release notes for runtime v0.126.0

- A reference can be held within a field (ST-SUP-RP10-LC37). `Reference` has
  the new fields `Field`, `Within` and `Key`, and `EntitySpec` has `Within`
  and `Referrers`. Generated code sets them; code that builds an `EntitySpec`
  by hand needs no change while no reference declares `within`.
- A foreign key violation of a composite key of `within` is `ErrInvalid` that
  names the reference, not an internal error.

### Release notes for runtime v0.125.0

- A worker fill gives no SQL text. Before v0.125.0, a worker fill gave its
  own SQL text in `command.ListSpec` and made its list with `command.NewList`.
  That SQL named the columns of the entity, so each added column broke it.
  Now the handle of a worker takes only the lists that its declaration
  declares (ST-SUP-RP10-LC38). The fill still compiles, but each run that
  uses its own list rolls back with this error:

  ```text
  command: internal error: the worker low-stock-scan reads the list low of
  StockItem, which its declaration does not declare: since runtime v0.125.0 a
  worker fill gives no SQL text, so declare the list under lists: of the
  worker and use the generated list of the worker contract (ST-SUP-RP10-LC38)
  ```

  To move a fill, write the `WHERE` terms of its `Read` statement as `filter`
  terms under `lists:` of the worker, and add `lock: true` when the fill used
  `Lock`. Run `stego apply`. Then remove the `command.ListSpec` and the
  `command.NewList` call from the fill, use the generated list, and remove the
  filter values from the `List` and `ListForUpdate` calls.
- The outbox worker gives each destination claim slots of its own, so a slow
  or refused destination does not delay another destination. It runs
  `Concurrency` slots, or one slot for each destination when there are more
  destinations. Each slot holds one database connection, so
  `outbox.Worker.Run` refuses a pool that holds fewer connections than the
  slots plus 2. The worker mode of a generated service sets its pool to this
  size. A program that builds its own outbox worker must give its pool at least
  that many connections.

## Declare an observation

An observation is the only correct path for a fill to emit a log line or a
domain signal.

<!-- excerpt -->
```yaml
observations:
  - name: low_stock
    kind: event
    severity: warn
    fields:
      - {name: level, type: "enum(low | empty)"}
      - {name: free, type: "measure(1, 0..1000000)"}
    emitted_by: [low-stock-scan]
```

| Key | What it is |
|---|---|
| `name` | The name, in lower snake case. Each signal has the name `domain.<name>`. |
| `kind` | `event`, `counter` or `histogram`. |
| `severity` | `debug`, `info`, `warn` or `error`. |
| `fields` | Up to eight fields, each with a name in lower snake case and a type. Optional. |
| `value` | For a histogram only: the number that each emission records, as `name: unit`, such as `wait: ms`. |
| `emitted_by` | The commands, queries and workers whose fills can emit it, and the slot bindings as `collection.slot`. |
| `counts` | `attempt` or `committed`. The default is `attempt`. |

The type of a field is one of these:

- `enum(a | b)`: one to 32 values in lower snake case.
- `bool`.
- `measure(<unit>, <min>..<max>)`: a whole number in a range. The unit is
  `s`, `ms`, `By` or `1`.
- `Entity.field`: a field of a declared entity. At this time a worker cannot
  use this type: validate refuses it with `the entity StockItem needs exactly
  one Go model provider, and has 0`.

Validate refuses free text, a plain `int` and all other types.

The probe of each operation in `emitted_by` has one method for each
observation. The fill calls it through the handle:

```go
h.Probe().LowStock("low", free)
```

The arguments are the fields, in the order of the declaration. A fill imports
only its slot contract, so it cannot import the package `out/probes`, which
holds the enum type `LowStockLevel`. Give an enum value as a string constant,
such as `"low"` or `"empty"`.

## The fill workflow

Do these steps for each command, query and worker. The example is the command
`place-order`. For a query, give the slot `query.query`. For a worker, give
the slot `worker.worker`.

1. Declare the command in `service.yaml`, and add its messages to the proto
   file. For a worker, add the schema relations. Then run:

   ```bash
   stego validate
   ```

   Validate names each fill that is missing and its slot, such as `write the
   fill of the slot command.command there`.

2. Make the fill. You can do this before the first apply, because the
   contract need not exist yet:

   ```bash
   stego fill create place-order --slot command.command
   ```

   The output is:

   ```text
   Created fill "place-order" of the command place-order for slot "command.command"
     fills/place-order/fill.yaml
     fills/place-order/fill.go
     fills/place-order/fill_test.go
   Run stego apply to write the contract of the fill, and stego deps to resolve its modules; then write the fill.
   Until it is written, the fill returns ErrNotImplemented.
   ```

   The new fill exports `New`, or `Fill` for a query, and returns
   `ErrNotImplemented`. Its test passes while the fill refuses each call. When
   you write the fill, the test fails and tells you to replace it with the
   tests of what the fill does. Fill create refuses a fill that no command, query or worker
   of `service.yaml` names, and a fill that exists.

3. Write the fill in `fills/place-order/fill.go`, and write its tests. Then
   test the fill and sign its evidence with your own key:

   ```bash
   stego test
   stego fill sign place-order
   stego fill status
   ```

   `stego test` tests the fill against the contract that apply plans to write,
   so you can do this before the first apply. The project's files do not
   change. When a fill records no evidence, `stego test` names it and exits
   non-zero. `stego fill status` shows the fill as `qualified`.

4. Generate the contract, resolve the dependencies and build:

   ```bash
   stego apply
   stego deps
   go build -o service ./out
   ```

   Plan and apply refuse a fill of a command, query or worker that is not
   qualified, and name it and what to run. `stego validate` gives only a
   warning for a fill that has no evidence or no sign-off, such as
   `fill "place-order" is untested ...; run stego test, then stego fill sign
   place-order; plan, apply and build refuse the fill until then`. A fill folder that holds only
   `fill.yaml` does not stop apply: apply writes the contract, then names the
   fill to write.

The file `fill.yaml` names the fill and its slot, and no collection: the
command, query or worker that names the fill is its binding.

```yaml
kind: fill
name: place-order
implements: command.command
```

The slot of a command fill is `command.command`, of a query fill
`query.query`, and of a worker fill `worker.worker`.

### Qualification of these fills

These fills ship in the service with write access to the entities in
`writes`, so they are qualified as each slot fill is (decision ST-EXT-Q30).
The contract digest of such a fill covers the part of the generated package
(`out/commands`, `out/queries` or `out/workers`) that the fill compiles
against: the declarations of its own command, query or worker, and the
declarations that the fill uses. A change to the fill, or to that part, makes
the fill stale: run `stego test`, then `stego fill sign <name>` again. A new or
changed command does not make the other command fills stale, unless it changes
a declaration that they use.

`stego deps` and `stego build` refuse such a fill that is not qualified, as
plan and apply do. `stego build` also compares the contract digest of the
evidence with the contract that the source revision holds. `stego test` passes
only `-v`, `-race`, `-count` (1 or more), `-timeout` and `-failfast` to
`go test`, and records evidence only when at least one test of the fill ran
and passed.

Upgrade: a project whose fills of commands, queries or workers have no
signed evidence is refused at plan. Run `stego test`, then
`stego fill sign <name>` for each fill that plan names. `stego fill sign` keeps a
sign-off that already covers the current evidence with a key that `SIGNERS`
lists, and writes nothing; give `--force` to sign it again with your key. When
your key is not in `SIGNERS`, `stego fill sign` warns and names the fingerprint
to list; the fill stays not qualified until a listed key signs.
