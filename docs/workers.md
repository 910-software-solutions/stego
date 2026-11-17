# Worker grants in each tenant

To declare a worker and write its fill, see
[Commands, queries and workers](commands-queries-workers.md#declare-a-worker).

A worker of a pooled service with tenant provisioning acts in a tenant through
its own relation on the tenant resource. The schema of the service gives the
type `tenant` one relation `worker_<worker name in snake case>` of the subject
type `service_account` for each declared worker (ST-SUP-RC35-OC13).

## Who writes the tuples

- Provision writes the tuple of each declared worker in the new tenant.
- Retire deletes the tuple of each declared worker in the retired tenant. It
  always does this.
- The controller mode runs the job `worker-tuples` at start and then once an
  hour. The job writes the missing tuples of the declared workers in each active
  tenant. It deletes the tuples of each relation `worker_<name>` that no
  declared worker gives. A tenant whose tuples are correct gets no write.

Do not write these tuples by hand. A relation of the type `tenant` whose name
starts with `worker_` is reserved for the declared workers.

## The release identity row

The migration job writes one release identity row last: the release (the name
of the last migration) and the workers digest. The workers digest is the
SHA-256 of the sorted worker names. `stego plan` shows it in a `note:` line.

The job `worker-tuples` changes tuples only when this row is the identity of
its own binary. Retire deletes the tuples of undeclared workers only under the
same condition. During a rollout, or after a rollback, a controller of another
release writes nothing and logs one notice
(`command.worker_tuples.fenced`) with both identities. Thus an old controller
never deletes the tuple of a worker that the new release adds.

## One service owns its schema

One service owns its SpiceDB schema and its definition `tenant`. Two services
must not share one definition `tenant`: the reconcile of each service would
delete the worker tuples of the other service.

## Remove a worker

SpiceDB refuses a schema that removes a relation that has tuples. Remove a
worker in two releases:

1. Remove the worker from the declaration, and keep its relation
   `worker_<name>` in the schema. The reconcile of this release deletes the
   tuples of the worker in each active tenant.
2. In a later release, remove the relation from the schema.

## Worker lists

A worker fill reads a set of rows through a declared list. Declare each list
under `lists:` of the worker (ST-DECL-INV94):

```yaml
entities:
  - name: Ticket
    lock_at_most: 50        # needed only for a list with lock: true
    fields:
      - {name: status, type: enum, values: [open, closed]}
      - {name: due_at, type: timestamp, client_set: true}
workers:
  - name: stale-scan
    trigger: every 1m
    writes: [Ticket]
    fill: stale-scan
    lists:
      - name: stale-tickets
        entity: Ticket       # an entity that the worker writes
        lock: true           # optional: ListForUpdate locks the rows
        filter:              # one to eight terms
          - {field: status, op: equals, value: open}
          - {field: due_at, op: less_than, now: true}
```

- `op` is `equals`, `not_equals`, `less_than`, `less_than_or_equals`,
  `greater_than`, `greater_than_or_equals` or `is_set`.
- `value` is a constant of the field type. `now: true` is the time of the run:
  the due tick time of a worker with an `every` trigger, and the start of the run
  of a consuming worker. Use `now` only on a timestamp field.
- `is_set` takes no value.
- `lock: true` needs `lock_at_most` on the entity, a whole number from 1 to 1000.

`stego apply` writes one typed list for each declared list in `out/workers`,
named `<Worker><List>` in mixed case, such as `StaleScanStaleTickets`. The
generator builds its statements from the entity of the release: the columns of
the entity's scan, the tenant, and the filter as bound values. When a schema
change adds a field, the next release writes the list again with the new column.
The release before it still reads its own columns.

The fill uses the list through the repository of the entity:

```go
tickets, err := workers.TicketRepository(h)
if err != nil {
	return err
}
stale, err := tickets.ListForUpdate(ctx, workers.StaleScanStaleTickets, nil)
```

A declared list takes no filter values from the fill.

### Move from command.ListSpec

Before runtime v0.125.0, a worker fill gave its own SQL text in
`command.ListSpec` and made its list with `command.NewList`. That SQL named the
columns of the scan, so each added column broke it (walkthrough 6, K8). Since
v0.125.0 the handle of a worker takes only the lists that its declaration
declares (ST-SUP-RP10-LC38). A fill that uses its own list gets this internal
error, and its run rolls back:

```text
command: internal error: the worker stale-scan reads the list stale of Ticket,
which its declaration does not declare: since runtime v0.125.0 a worker fill
gives no SQL text, so declare the list under lists: of the worker and use the
generated list of the worker contract (ST-SUP-RP10-LC38)
```

To move a fill:

1. Write the `WHERE` terms of the `Read` statement as `filter` terms of a list
   under `lists:` of the worker. Add `lock: true` when the fill used `Lock`.
2. Run `stego apply`.
3. In the fill, remove the `command.ListSpec` and the `command.NewList` call,
   and use the generated list. Remove the filter values from the `List` and
   `ListForUpdate` calls.
