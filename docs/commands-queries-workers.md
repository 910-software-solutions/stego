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
      - { name: status, type: enum, values: [draft, placed, fulfilled], default: draft }
      - { name: total_units, type: int32, min: 0, max: 1000000, default: 0 }

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
}

definition service_account {}

definition tenant {
	relation worker_fulfill_orders: service_account
	relation worker_low_stock_scan: service_account
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

### Idempotency

Each request of a command must have the header `Idempotency-Key`.

- With no key, or a key that is not valid, the answer is 400 with the problem
  type `idempotency-key-missing`.
- A request again with the same key, method, path and body gets the stored
  response again. The fill does not run again.
- A request with the same key and another body gets 422 with the problem type
  `idempotency-key-reused`.
- A request with the same key while the first request runs gets 409 with the
  problem type `idempotency-key-in-progress`.

The keys of each tenant and each caller are separate. Thus the same key from
another caller, or in another tenant, is a new request.

The runtime keeps each key for the time in `idempotency.retention`. The
default is `24h`.

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
status and the code of the refusal. Any other error is 500 with the code
`internal`, and the transaction is rolled back.

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
other entity.

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

`query` is the package `github.com/jsell-rh/stego/registry/runtime/query`, and
`Item` is a type of the fill. The handler writes `Items` as JSON.

The request of a get query has one field for the member: `req.OrderID` for
`orderId`. The response of a get query is any value that the handler can
write as JSON.

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

Each parameter name is exact. A collection list ignores `limit`, and a list
query ignores `pageSize`. Give the `next` value of a response as the `cursor`
of the next request. A cursor is valid only for the same list, tenant and
caller. Another cursor gives 400.

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

The component `worker` is at the stage alpha. Accept it in the declaration
with a date before its sunset (2027-04-30) and a reason:

<!-- excerpt -->
```yaml
experimental_acceptances:
  - {component: worker, accepted_until: 2027-03-31, reason: the fulfillment worker reacts to placed orders}
```

### The two triggers

- `every 1m`: the runtime runs the fill once in each tenant at each tick, in
  one transaction of that tenant.
- `consumes fulfillment`: the runtime runs the fill once for each message of
  the destination `fulfillment`, in the transaction of the tenant of the
  message. A command or a worker stages these messages as an effect.

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

Validate examines the permission on `resource_type` only. If another type in
`writes` does not give the permission through the worker relation, the
runtime refuses each read and write of the worker on those rows.

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

An interval worker gets the time of the tick:

```go
// New gives the fill of low-stock-scan.
func New() workers.LowStockScanFill {
	return func(ctx context.Context, tick time.Time, h *workers.LowStockScanHandle) error {
		// Read the stock items of this tenant and emit low_stock for each low item.
	}
}
```

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

2. Make the fill folder:

   ```bash
   stego fill create place-order --slot command.command
   ```

   This is to write `fills/place-order/fill.yaml`, a fill that compiles
   against the contract, and a test. At this time, `stego fill create` does
   not know the slots of commands, queries and workers. Until the fix is
   merged, write `fill.yaml` by hand. See [Known limits](#known-limits).

3. Generate the contract, and resolve the dependencies:

   ```bash
   stego apply
   stego deps
   ```

   Apply writes the contract of the fill in `out/commands`, `out/queries` or
   `out/workers`. Commit `go.mod` and `go.sum`.

4. Write the fill in `fills/place-order/fill.go`, and write its tests. Then
   run the tests:

   ```bash
   stego test
   ```

5. Apply again and build:

   ```bash
   stego apply
   go build -o service ./out
   ```

The file `fill.yaml` names the fill and its slot:

```yaml
kind: fill
name: place-order
implements: command.command
```

The slot of a command fill is `command.command`, of a query fill
`query.query`, and of a worker fill `worker.worker`.

### Known limits

- `stego fill create` does not know the slots `command.command`,
  `query.query` and `worker.worker` at this time. It stops with `slot
  "command.command" not found in any component`. Write `fill.yaml` by hand as
  above. Then run `stego apply`. A fix is in progress.
- If the project has a `go.mod` and apply says that the fill has no Go
  package, add a file `fill.go` that holds only the package clause, such as
  `package placeorder`. Then run `stego apply` again.
- If `stego deps` stops with `updates to go.mod needed`, run `go mod tidy`
  once. Then run `stego deps` again.

### Security gap: these fills are not qualified

**Warning:** At this time, STEGO does not qualify the fills of commands,
queries and workers. `stego test` records no evidence for them, and
`stego fill sign` and `stego fill status` do not show them. Validate, plan
and apply accept these fills with no sign-off. The service then runs code
that no listed key signed, with write access to the entities in `writes`.

This is a known gap, and it is being closed: these fills must be tested and
signed as each slot fill is (decision ST-EXT-Q30). Until then, review each
change to these fills before you apply it.
