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
`stego validate` and `stego plan` also warn when the sign-off of a fill counts only
because of a key that the change adds to `SIGNERS`; with `--strict` this is a fault.

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

Keep this order: first the worker, then its relation. The authorization relay
writes only the relations of the schema of its own release. When a release
removes the relation while the relation still has tuples, the relay of that
release refuses the deletes of the reconcile and of a Retire, and the outbox
parks them. Thus run the release of step 1 until the reconcile deleted the
tuples in each active tenant, then deploy the release of step 2.
