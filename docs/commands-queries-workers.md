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

### A failed run of a consuming worker

Each attempt of a consuming worker that does not commit writes one log line
`command.consumer.run_failed` at the level warn. The line gives the worker, the
destination, the event ID of the message, the attempt number, the outcome and
the cause class. It never gives the error text or a value of the message. The
attempt is also sent as `worker.run.failed` with the worker, the outcome and
the cause class.

When the fill fails with the same outcome and cause class in 5 attempts in a
row, the runtime parks the message as poison (failure code `poison`) and
raises the alert of a parked message. Only the errors that the fill causes are
counted: an error of the fill, a row that is not there, an invalid value, a
refused resource, a unique violation and a panic. A provider that cannot decide
now, a conflict, a failed database statement, a timeout and a lost connection
are not counted, and they start the count again. A parked message stays in the
outbox, and a person can send it again or discard it.

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
   `ErrNotImplemented`. Its test expects that error. Change the test when you
   write the fill. Fill create refuses a fill that no command, query or worker
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
   change. `stego fill status` shows the fill as `qualified`.

4. Generate the contract, resolve the dependencies and build:

   ```bash
   stego apply
   stego deps
   go build -o service ./out
   ```

   Plan and apply refuse a fill of a command, query or worker that is not
   qualified, and name it and what to run. A fill folder that holds only
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
`stego fill sign <name>` for each fill that plan names.
