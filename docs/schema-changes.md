# Schema changes

After the first release of a service, its tables can have rows. STEGO cannot
count rows, so it takes each table of the base as a table with rows. A table
with rows changes only by a declared schema change, one phase for each
release.

## Declare a schema change

A schema change has an ID, a kind and the entity and the field it changes.
The kinds are `add`, `rename`, `retype`, `require`, `drop`, `unique` and
`relax`. The
schema history puts the steps of the changes in order. A step moves one change
to `expand` or to `contract`.

```yaml
schema_changes:
  - {id: note-title-rename, kind: rename, entity: Note, field: heading, from: title}
schema_history:
  - {change: note-title-rename, phase: expand}
```

The release with the expand step adds. The release after it, with the contract
step, removes what only the old release needed. The declaration keeps every
schema change, also a finished one.

A schema change does not change the API of the service. After a rename, also
after its contract step, the API and the OpenAPI document keep the old name
of the field: the `from` of its first rename. In the example above, a client
still sends and reads `title`, and the column of the table has the new name
`heading` (ST-GEN-UC12-R04). A retype keeps the name of the field in the API
too. A new name in the API is a breaking change for each client, and a
schema change never makes one.

Checking holds each schema change against its field as its own step leaves
it: the declared field with each later step undone, the last first
(ST-DECL-Q17). So one field can change more than once. A retype from `int32`
to `string` and then a retype from `string` to `int64` are each checked from
their own `from` to the type that their own step gives, and a relax before a
retype or a rename names the field as it is at the relax. A change that does
not suit the shape at its step is refused, and the fault names the step, such
as `schema_history[2]`, and the shape of the field after it. Only one change
that moves the rows of a field can be at expand at one time, so put the
contract step of the first change before the expand step of the next. An add
finishes at its expand step, so a later change of the field that it adds is
not refused for that reason.

## Add steps

`stego apply` writes the add steps for you. Before it plans, it puts one
schema change of kind `add`, with its expand step, at the end of the schema
history for each new entity, and for each new field that is optional or has a
default on an entity of the base. Their IDs are `add-<entity>-<field>` and
`add-<entity>` in lower kebab case. Apply names each step that it writes;
commit them with the record. `stego plan` lists the steps that apply writes
and saves no plan until they are in the declaration. Apply writes no other
kind of step: declare a rename, a retype, a require, a drop or a unique
yourself.
A generated file that is missing is written again by `stego apply` with no
flag; only a generated file that was edited by hand stops the apply.

The other flags of plan and apply, such as `--strict`, do not change this
check: `--strict` holds the secret sources of the environment variables, as
[delivery](delivery.md) tells.

Plan and apply check the declaration against the base before they plan: the
ref that `--base` names, or the default branch of the repository. The default
branch is the branch of `origin` that `refs/remotes/origin/HEAD` names, else a
local `main`, else `master`. When the check refuses the declaration, plan and
apply write nothing. When the service folder is in no Git repository, there is
no check, and plan and apply say so. A new repository with no commit holds no
base: when the service has no record either, plan and apply take it as the
first release, with no check, and say so. When the service has a record, a
release may have run, so plan and apply stop: commit the service folder with
its record. `stego init` tells you to commit before the first release, also
the example content it writes (and `stego init --empty`, which writes none),
and the `go.mod` that it writes with the module path of `--module`. A
repository is new only when HEAD names no commit, it has no ref, it is not
shallow, it has no `FETCH_HEAD`, and its object store holds no commit. A
checkout by commit, such as `git fetch <url> <ref>` and
`git checkout FETCH_HEAD`, has commits but no branch. Such a repository, any
other repository with commits but no such branch, or one that git cannot read,
stops plan and apply: name the base with `--base`.

## Relax steps

A relax changes one field in place, in one release, with no new column and no
copy of the rows (decision on finding F12 of the schema-change research,
owner, 2026-11-13). It has an expand step only, and that step finishes it.
Declare it with the field and a `definition` that says what the field was
before the change, one item for each attribute that the change relaxes:

| Change | Item | Example |
| --- | --- | --- |
| A longer `max_length` | `max_length N`, the old length | `max_length 40` |
| More enum values | `values V1 V2 ...`, the old values | `values calm busy` |
| A default where there was none | `no default` | `no default` |
| A required field made optional | `required` | `required` |
| An enum made a string with no `max_length` | `type enum, values V1 V2 ...` | `type enum, values a b` |

```yaml
schema_changes:
  - {id: note-title-relax, kind: relax, entity: Note, field: title, definition: max_length 40}
  - {id: note-size-relax, kind: relax, entity: Note, field: size, definition: "required, no default"}
schema_history:
  - {change: note-title-relax, phase: expand}
  - {change: note-size-relax, phase: expand}
```

Every value that the release before writes stays valid, and it can read every
value that the new release writes, so both releases run on the same schema.
The check against the base refuses a narrowing: a shorter `max_length`, an
enum value taken away, a field made required, and a type change that
PostgreSQL cannot make without a rewrite, such as an enum made a string with a
`max_length`. Those still need a new field, a copy and a drop. `stego compat`
names the relax to declare when you relax a field with no schema change.

The step's own migration runs one `ALTER TABLE` that reads no row under its
`ACCESS EXCLUSIVE` lock: the new `CHECK` of the field `NOT VALID` as
`<table>_<column>_relax`, a longer `character varying`, `DROP NOT NULL` and
`SET DEFAULT`. `ALTER COLUMN TYPE` scans the table again for each validated
`CHECK` on the column, so when the type changes, the old `CHECK` goes in the
same statement; the new `CHECK` holds each new write. In the same transaction,
after the `ALTER TABLE`, the migration gives the column a comment, the relax
marker, that states the exact form after the step of each attribute that it
relaxes: the type, nullable, the default and the `CHECK`. A second migration
validates the new `CHECK`, which blocks no read and no write, and a third drops
the old `CHECK`, when it is still there, and gives the new one the name
`<table>_<column>_check`.

The schema check of each release accepts on a marked column, for each
attribute, only the form of its own manifest or the exact form that the marker
states. Any other form stays a fault: a longer type, a dropped `NOT NULL`, a
default or a `CHECK` that the relax did not declare is refused on a marked
column as on any other. A release whose manifest states the relaxed form
accepts only that form, so for it the marker no longer counts. A role that owns
the table can write such a marker by hand, as it can change the table itself.
A relax needs the runtime library v0.108.0 or later in the release before:
`stego compat` refuses a relax on a base that pins an older one, and names the
version to deploy first.

## Within steps

A reference can be held within a field: `within: project_id` on the reference
`sprint_id` of a ticket means that the ticket and its sprint must have the
same project (ST-DECL-INV93, ST-DECL-Q42). The tables hold this rule with a
composite key: a unique key of the tenant column, `project_id` and `id` on the
sprints, and the foreign key of the tenant column, `project_id` and
`sprint_id` of the tickets onto it (ST-SUP-RC01-OC46). A first release has the
key in `001_initial`. When the tables of the release before have rows, add
`within` with a within step, which has an expand step only:

```yaml
schema_changes:
  - {id: ticket-sprint-within, kind: within, entity: Ticket, field: sprint_id}
schema_history:
  - {change: ticket-sprint-within, phase: expand}
```

The step builds the unique key `CONCURRENTLY` in its index migration, adds the
foreign key `NOT VALID` in its keys migration, `<step>_keys`, after the index,
and validates the key in its validate migration, which blocks no read and no
write. A row that breaks the rule, such as a ticket of one project in a sprint
of another, stops the migration job at the validate migration. Its refusal
names the step, the table and the key, and no value of the row. Correct or
remove the rows that break the key, then run the migration job again: it
continues at the validate migration. `stego compat` names the within step to
declare when you add `within` to a reference of a table with rows. A change or
a removal of `within` has no step yet, so the check against the base refuses
it.

## Drop steps

A drop removes a field or an entity in two releases (ST-GEN-Q47). The release
with the expand step stops reading and writing the old column or table, and
the release before it keeps running on the same database. A later release,
with the contract step, drops the old column or table.

### Drop a field

Remove the field from the entity, and declare a drop with the field and its
Definition. The Definition gives the field as the entity declared it, so that
the first migration `001_initial` stays the same:

```yaml
entities:
  - name: Product
    fields:
      - { name: name, type: string, max_length: 100 }
      - { name: status, type: enum, values: [draft, active] }
schema_changes:
  - {id: product-legacy-note-drop, kind: drop, entity: Product, field: legacy_note, definition: "string, optional, max_length 200, after name"}
schema_history:
  - {change: product-legacy-note-drop, phase: expand}
```

The Definition starts with the type. Then it gives one item for each
attribute that the field had, separated by commas:

| Item | Means |
|---|---|
| `min_length N`, `max_length N` | the length limits of a string |
| `min N`, `max N` | the limits of a number |
| `values V1 V2 ...` | the values of an enum; an enum must have them |
| `default D` | the default of the field |
| `optional` | the field was optional |
| `unique` | the field was unique |
| `pii` or `secret` | the sensitivity of the field |
| `after F` or `first` | the place of the field: after the field `F`, or the first field |
| `pattern P` | the pattern; it comes last, because a pattern can hold a comma |

The place is required. Give the field before it in the entity, as the entity
is at the step of the drop: a field that an earlier step dropped is not
there. An item that the Definition does not name, the field did not have.
`stego compat` names the Definition to declare when a field of the base is
gone: for example, `the Definition string, max_length 200, optional, after
name`. When you drop more than one field of an entity, declare the drops in
the order that `stego compat` names them: then each place is right. When the Definition does not give the field back exactly, the first
migration of the head is not the one of the base, and `stego compat` refuses
it.

The expand migration keeps the old column, its CHECK and its indexes:

- For a required field, it drops the NOT NULL of the column. When the field
  had no default, it gives the column the `fill_value` of the drop as its
  DEFAULT, or else the zero value of the type (the empty string, 0 or
  false). So each row that the new release writes has a value that the
  release before can read. When the zero value does not suit the field, as for
  an enum, a `min_length` or a `min` above 0, checking refuses the drop until
  you give a `fill_value` that suits. A timestamp, a jsonb or a bytes field
  has no zero value, so it needs a `fill_value`.
- For an optional field, it sets a DEFAULT only from a `fill_value`, because
  NULL is a value of the old field.
- It marks the column with the relaxed form of the change (as a relax step
  does), so that the schema check of the release before accepts the column.
  This needs the runtime library v0.108.0 or later in the release before.
- It makes the table of the release reports, `stego_schema.releases`.

Each statement takes a short lock and reads no row.

#### A client that still sends the dropped field

The API of the expand release does not know the field. When a create or an
update body holds it, the API ignores that member, as it ignores each member
that the entity does not declare, and answers as for a body without it: a
create gives 201, and the row has no value of the field. The API sends no
message about it.

This is intentional. In the time between the two releases, the release before
and the new release serve the same routes, and a client that the release
before knows still sends the field. A refusal (400) from the new release would
make the writes of such a client fail by chance, at each request that the new
release gets. The release before keeps the field: it writes the value, and
the old column keeps it until the contract step.

Thus, before the contract step, change each client to stop sending the field.
A value that a client sends to the new release is lost. If a client must not
lose it, keep the field in the API until each client stops sending it, and
only then declare the drop.

### Remove an entity

Keep the entity and the collections over it as they are, and add
`dropped_by` with the ID of a drop that names the entity and no field:

```yaml
entities:
  - name: Promotion
    dropped_by: promotion-drop
    resource_type: promotion
    fields:
      - { name: product_id, type: ref, to: Product }
      - { name: percent_off, type: int32, min: 1, max: 90 }
schema_changes:
  - {id: promotion-drop, kind: drop, entity: Promotion}
schema_history:
  - {change: promotion-drop, phase: expand}
```

A drop of an entity takes no Definition and no `fill_value`. From the expand
step, STEGO generates no API, no client, no repository, no handler and no
schema manifest entry for the entity and its collections. Nothing else of the
declaration may name it: no reference, command, query, worker, subscription
or invariant. The declaration keeps it, so that the first migration, the add
step that made its table and the contract step come from the declaration
alone. The expand migration changes no table and makes only the table of the
release reports.

Keep the definition of the resource type in your `schema.zed`, with the
relations of its references. The release before still writes its
relationships, and SpiceDB refuses a schema that removes a definition with
relationships. Checking refuses a schema that does not hold it. The
relationships stay in SpiceDB while the entity stays declared (ST-SUP-RC29-OC25).

### The contract step of a drop

Put the contract step of the drop in a later release, after the release of
its expand step:

```yaml
schema_history:
  - {change: product-legacy-note-drop, phase: expand}
  - {change: product-legacy-note-drop, phase: contract}
```

The migration first checks, in its own transaction, that no process of a
release that does not know the expand migration of the drop runs, as the
contract step of a rename does (see "Contract steps"). Such a release still
reads the old column or table. When a process of it reports itself, the job
exits non-zero, writes no ledger row, changes no schema and names the change,
the expand migration and the version label of that release. Stop the release
before, and run the job again.

Then the step runs each statement in its own short transaction, under a lock
timeout of 1 s, and again after a pause when a lock wait ends at that
timeout, at most 10 times:

- for a field, `ALTER TABLE ... DROP COLUMN IF EXISTS`, which also drops the
  CHECK and the indexes of the column;
- for an entity, first each foreign key of the table, the tenant key too,
  each in its own transaction, and then `DROP TABLE IF EXISTS`. So no
  statement holds the lock of a busy table that the dropped table refers to
  and the lock of the dropped table at one time.

Each statement first takes the lock of the table and then checks the running
releases again in its transaction. So when a process of the release before
reports itself during the step, the step stops and leaves the table with all
its keys.

A drop cannot be undone: the contract step deletes the values of the column or
the rows of the table, and no step gives them back. Take a backup before you
release it. The step counts a process of the release before as stopped when
its last report is older than three of its report intervals: 30 s with the
default interval of 10 s, and at most 180 s. A process that is still alive but
cannot write its report for that time, for example during a database failover
or on a paused node, does not stop the step. So stop and remove every pod of the
release before (its Deployment or ReplicaSet) before you run the migration job
of the contract step. A run that stops part way recovers on the next run,
and the ledger row comes last. The release of the expand step keeps running
before, during and after the contract step.

Limits: a drop of a reference, and a drop of a unique field that is required
or gets a default, are not built yet; generation refuses them and names the
reason. A table that another live table refers to cannot be dropped: remove
the reference first.

## The migrations of the steps

`001_initial` holds the tables of the first release: the declaration with
every schema step undone. Each step is one more migration, after the
migrations of the components, in the order of the schema history. Its name
is `step_<position>_<change>_<phase>`, such as
`step_0001_add-note-body_expand`. The migration of an add creates the table
of the new entity, with the row-level security of an owned table in a pooled
service, or adds the column of the new field with its checks, its foreign
key and its indexes. It changes nothing that is there. `stego plan` lists
each new step with its change, kind, phase, migration, trigger and backfill.
A new database and a database that each release migrated in turn get the
same schema.

A step on a table with rows takes only short locks. Its own migration adds
each CHECK and foreign key `NOT VALID`, so it reads no row while it holds
its lock. Each index of the step is a migration of its own after it,
`step_<position>_<change>_<phase>_index_<nn>`, which builds the index with
`CREATE INDEX CONCURRENTLY` outside a transaction. A build that fails drops
its invalid index at once, and the next job builds it again. When that drop
fails, the job tries it again, at most 5 times; when it still fails, the job
fails and names the invalid index, which you must drop by hand. Before the
job records an index build, it checks that the index is valid and has the
definition of the step: a table or an index of another definition that has
the name of the index fails the job. A rename then has the migration
`..._sync`, which adds the sync trigger after the indexes, so the unique
index of the new column is valid before the trigger copies a value into
it. The last migration of the step, `..._validate`, validates the constraints in its own
transaction, which blocks no read and no write. Each migration of a step
waits at most 1 s for a lock, and runs for at most 15 s, or 1 h for an index
build or a validation. When the lock wait ends, the job tries again after a
short random pause, at most 10 times, so a request that queues behind the
step waits at most 1 s. An index build has no lock timeout, because
`CREATE INDEX CONCURRENTLY` waits for each older transaction and blocks no
read and no write while it waits; its statement timeout of 1 h holds.

An add of a field that is required and has no default is refused, because
the rows that the table holds cannot take it. Make the field optional or
give it a default. An entity that refers to an entity that a later step adds
is refused too: put the step that adds the other entity first.

The marker of the database holds the digest of the stored shape of the
first release, from postgres-adapter 6.2.0. A change of a collection or a
permission does not change it. The first migration job of postgres-adapter
6.2.0 or later on a database that an earlier release made writes the new
digest into the marker, once. A pod of the earlier release that restarts
after that job fails its marker check: during that rolling deploy, do not
restart the pods of the earlier release. A change of the indexes of a collection over
an entity of the base is refused, because no schema step builds such an
index yet. See [stego compat](compat.md).

The release before a step must run postgres-adapter 6.1.0 or later, because
an earlier ledger refuses the row of a step that it does not know. The check
against the base refuses a new step on such a base and names the version to
deploy first.

## Retype and require

A retype gives a field a new type. Declare the conversion to the new type
and the inverse conversion back, by the names that the database component
declares, and the type before the change in `from`:

```yaml
schema_changes:
  - {id: orgsetting-rank-retype, kind: retype, entity: OrgSetting, field: rank,
     from: int64, conversion: int64_to_text, inverse_conversion: text_to_int64}
```

The postgres-adapter declares these conversions: `integer_to_int64`,
`int64_to_integer`, `integer_to_text`, `int64_to_text`, `bool_to_text`,
`text_to_integer`, `text_to_int64` and `text_to_bool` (`integer` is
`int32`). A conversion is total, or total only under a constraint of the
field: a text converts to an int64 only under one of the patterns
`^[0-9]{1,9}$`, `^-?[0-9]{1,9}$`, `^[0-9]{1,18}$` and `^-?[0-9]{1,18}$`.
Checking refuses a conversion that the component does not declare for the
two types, a conversion that is not total over the field before the change
and has no `fill_value`, and an inverse conversion that is not total over
the field after the change (ST-DECL-INV62, ST-DECL-INV63). Checking also
refuses a retype where the CHECK of one column could refuse a converted
value of the other: between two integer types the fields before and after
the change have the same `min` and `max`, and between a number or a boolean
and a text neither field has a `min`, a `max`, a `min_length`, a
`max_length` or `values`. Give the field
before the change, with its constraints, in `definition`, such as
`string, pattern ^[0-9]{1,9}$`; a pattern is the last item, as it can hold
a comma.

The field keeps its name in the API, but its JSON type in the API changes with
its type: a retype from `int64` to `string` changes the value `14` to `"14"`,
and a retype from `string` to `int64` changes `"14"` to `14`. The OpenAPI
contract of the release after the change gives the new type. Thus a client
that reads the field must accept both types while the two releases run.
Its values go to a new column,
`<field>_<type>`, such as `rank_string`, which stays its column after the
contract step. The expand step adds the column, nullable, its CHECK
`NOT VALID` and its indexes, and then the sync trigger, which converts each
write of the release before into the new column and each write of the new
release back into the old one. A value that does not convert gets the fill
value. The migration job of the
new release converts the rows there before the rollout, under the lease of
the change, and the contract step waits for a backfill with no failed row.
Only one rename or retype can be at expand on a table at one time. A retype
of a field in a unique key or that is unique after it, of a reference, of a
string with a `max_length`, and of the counter `generation` is not built,
and neither is a require of a unique field, whose rows would all get the
fill value. The contract step counts the rows with the old column set and
the new one empty last, and waits while one is left.

A require adds a required field with no default. Declare the value that
each row there gets in `fill_value`:

```yaml
  - {id: user-locale-require, kind: require, entity: User, field: locale, fill_value: en}
```

Its expand step adds the column with the fill value as its default, and
nullable. PostgreSQL keeps the default in the catalog and rewrites no row,
so each row there and each insert of the release before read the fill
value at once. A require has no sync trigger and no backfill. Its contract
step makes the column NOT NULL in short steps (a CHECK `NOT VALID`, its
validation, `SET NOT NULL`, the drop of the CHECK) and keeps the default.

A retype whose conversion is not total declares its fill value with its
expand step. A later release cannot add or change it, because the expand
step holds it and its ledger digest covers it: the check against the base
refuses the change (ST-GEN-Q24).

## Restarts during a rolling deploy

A pod of the release before a step restarts on the database after the
step's migration. Its schema check accepts what a later step adds, each only
in the exact form that the step gives it, on the table of a declared entity:

- the foreign key of an add of a reference field: on the new nullable column,
  to the primary key of the table of an entity that it knows, with the tenant
  column first when the referenced entity is owned, with no action,
  validated or not yet validated, not deferrable, and named
  `<table>_<column>_fkey`
  (ST-SUP-RP15-LC03). A foreign key on one column refers only to a shared
  entity, whose rows each tenant may read;
- the default of the new column of an add: a constant of a built-in type,
  and no function call (ST-GEN-Q15);
- the CHECK of the new column of an add, validated or not yet validated,
  named `<table>_<column>_check` (ST-SUP-RP15-LC01). Only the owner role,
  which has the DDL rights, can add such a CHECK, so the CPU that an
  accepted CHECK can use is not an attack surface;
- the unique index of an add of a unique field: the tenant column first,
  then the key columns, of which at least one is new, named
  `<table>_<columns>_key` (ST-SUP-RP15-LC01). On a table with row-level
  security every other unique index that the manifest does not name fails
  the check, because a write that fails on it would tell one tenant that
  another tenant holds the value;
- the sync trigger of the expand step of a rename or a retype:
  a BEFORE INSERT OR UPDATE row trigger whose function is the fixed
  template of this release, in the schema `stego_steps`, for one column
  that the release before knows and one new column (ST-SUP-RP15-LC02).
  The old column may be in one unique key: the tenant column and that
  column alone, a unique index that the manifest names. Then the new column
  must have the same unique key, valid, named `<table>_<column>_key`, and no
  other (ST-GEN-Q18). A rename of a field in a unique key of more than one
  field, or of a reference, is refused.

The sync trigger takes its direction from the writer, never from a NULL.
The release of the step sets `stego.schema_step` to the IDs of its open
changes with `SET LOCAL` in each transaction that writes. When that marker
names the change, the trigger sets the old column from the new one; else it
sets the new column from the old one. A conversion of an old value that does
not convert gives the fill value of the change, or NULL, and never refuses a
write of the release before. A write that sets both columns to values that
differ is refused. The backfill passes the trigger of its own change with
`stego.backfill_change`, only as the owner role.

A column that the manifest does not list, on any table of the manifest,
must be of a built-in base type of `pg_catalog`, such as `text`, `uuid` or
`integer`: a domain, an enum, a range or a composite type can run a function
on each insert, so it fails the check. Every other change to such a table still fails the check. A release before
this rule refuses these forms: during the first rolling deploy of a step
after it, do not restart the pods of a release that does not have it.

## Contract steps

The contract step of a rename comes in the release after its expand step.
Put it at the end of the schema history:

```yaml
schema_history:
  - {change: note-title-rename, phase: expand}
  - {change: note-title-rename, phase: contract}
```

`stego plan` lists its one migration,
`step_<position>_<change ID>_contract`. The migration job runs it, and the
migration first checks, in its own transaction:

- the backfill row of the change is complete, with no failed row;
- the completeness CHECK of the expand step is there with its exact
  definition and validated, so no row of any tenant has the old column set
  and the new one empty;
- while the sync trigger of the change is there, it is enabled (not
  disabled and not for a replica only) and runs the sync function of the
  change, so each write of the release before is copied;
- no process of a release that does not know the change runs. Each process
  of a service reports its release in `stego_schema.releases` every 10 s and
  deletes its report when it stops. A release that does not register the
  expand migration of the change reads the old column, so the step waits for
  it. A report that is older than three of its intervals does not count.

When a check fails, the job exits non-zero, writes no ledger row, changes no
schema and logs why: the change, the rows left in the tenants of the tenant
list, or the version label (`STEGO_SERVICE_VERSION`) and the count of the
processes that still run. Run the migration job of the release before until
its backfill completes, or stop the release before, and run the job again.

Then the step runs each statement in its own short transaction, under a lock
timeout of 1 s, and again after a pause when a lock wait ends at that
timeout, at most 10 times. For a required field it adds
`CHECK (<new> IS NOT NULL) NOT VALID`, validates it, sets NOT NULL and drops
the CHECK. Then it makes the old column nullable. Then one transaction
checks the step again, drops the sync trigger, the completeness CHECK and the
old column with its CHECK and its indexes. Thus a process of the release
before that starts after the check gets an error on its first write of the
old column, and loses no edit. Last, the step drops the function of the
trigger. When a run finds the trigger gone and the old column still there,
for example after a change by hand, it does all the checks again. A run that
stops part way recovers on the next run, and the ledger row comes last. The release of the expand step accepts the schema between
each two statements, so its pods keep running and can restart. The release
before the expand step can no longer run its migration job.

The migration job, as the owner role, deletes the reports that are older
than one day. It passes over a report that a session holds, so a session of
the runtime role cannot stop the job or the reports of other processes.

The runtime role cannot read the reports, but the text of a statement shows
in `pg_stat_activity`, which other sessions of the same role can read. The
runtime library sends the holder of a report as a parameter of the extended
query protocol, so `pg_stat_activity` shows `$1` and not the holder. Do not
set `default_query_exec_mode=simple_protocol` (for example, for PgBouncer in
transaction mode): with the simple protocol, the holders show in
`pg_stat_activity`, and another session of the runtime role can delete the
report of a process with `release_stop`. Use the extended protocol.

Limits: a release on a runtime library before v0.103.0 writes no report,
and an expand step from a postgres-adapter before 6.4.0 made no table of the
reports (ST-SUP-Q126). A contract step of a rename of a field with a default
is not built yet.

## The tenant key of each owned table

In a pooled service, each owned table has a foreign key, the tenant key,
from its tenant column to `stego_tenants (tenant_id)`, ON DELETE NO ACTION
and ON UPDATE NO ACTION. Thus a row of a tenant that the tenant list does
not hold cannot be written, and the row of a tenant that has rows cannot be
deleted from the list (retire a tenant; do not delete it). The schema
manifest names the key, `<table>_<tenant column>_fkey`.

The migration job adds each key after the tenant list (step `tenant_keys`),
NOT VALID, under a lock timeout of 1 s and its retry, so it scans no row
under its short lock. Then it validates each key (step
`tenant_keys_validate`), which blocks no read and no write. Under FORCE ROW
LEVEL SECURITY, PostgreSQL validates a foreign key as the owner role with a
query that sees only the rows of one tenant. Thus, in the same transaction,
the job revokes SELECT on the table from the owner role, so PostgreSQL reads
each row past row-level security, and grants it again. No other session
sees this.

When a row of a tenant that the tenant list does not hold is in the table,
the validation fails, and the job exits non-zero and logs which table and
key. It names no tenant. Provision the tenant, or remove its rows, then run
the job again. A database of a release before the key can hold such rows,
because the list was not written then. To find them, as a role that may
read past row-level security:

```sql
SELECT DISTINCT o.tenant_id FROM public.<table> o
WHERE NOT EXISTS (SELECT 1 FROM public.stego_tenants t WHERE t.tenant_id = o.tenant_id);
```

A release before the tenant key does not accept it in its schema check, so
after the job of a release with the key, roll back only to a release that
has it too.

## The job lock of the migration job

The migration job holds a session advisory lock, the job lock, from its
start to its backfill step. A second job of the same database waits for the
first. PostgreSQL gives an advisory lock to each role that can connect to
the database, with no other privilege. Thus each role with CONNECT on the
database, the runtime role too, can take the job lock and stop the migration
jobs until it releases the lock. This is a known risk (review of PR #295,
I1), and STEGO does not prevent it.

To make the risk small:

- Give CONNECT on the database of the service only to the roles that must
  connect to it: the login of the migration job, the runtime role and the
  worker role. Revoke CONNECT from PUBLIC.
- Do not give CONNECT to the confined roles of STEGO, such as the outbox
  relay role and the idempotency purge role, on a database that they do not
  use.
- A job that waits for the job lock longer than its time limit fails with
  "another migration job holds the job lock". Find the session that holds
  the lock, and stop it if it is not a migration job:

  ```sql
  SELECT a.pid, a.usename, a.application_name, a.backend_start
  FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
  WHERE l.locktype = 'advisory' AND l.granted
    AND l.classid = 1937007983 AND l.objid = 1785684555 AND l.objsubid = 2;
  ```

## The check against the base

`stego compat --base <ref>` checks the declaration against the declaration at
the base ref. The check takes the declaration at the base ref as the version
that runs now. A release that is not deployed before the next one is the
operator's risk. See [stego compat](compat.md).
