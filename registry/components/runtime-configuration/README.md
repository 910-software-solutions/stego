# Runtime configuration

This component generates typed environment settings. Applications declare field
names, environment names, types, limits, and defaults. STEGO supplies parsing,
validation, private errors, and complete-result semantics. The component uses
only the Go standard library.

Add `runtime-configuration` to the application archetype. For example:

```yaml
overrides:
  runtime-configuration:
    groups:
      - name: Worker
        fields:
          - name: Address
            env: WIDGET_ADDRESS
            type: string
            min_length: 1
            max_length: 256
            required: true
            description: "The address of the widget service."
          - name: Resync
            env: WIDGET_RESYNC
            type: duration
            min: 1s
            max: 1h
            default: 30s
            required: false
            description: "The time between two full reads of the widgets. When it has no value, it is 30 seconds."
          - name: WatchLimit
            env: WIDGET_WATCH_LIMIT
            type: integer
            min: "0"
            max: "1000"
            default: "0"
            required: false
            description: "The most widgets that one watch reads. When it has no value, there is no limit."
          - name: Enabled
            env: WIDGET_ENABLED
            type: boolean
            default: "false"
            required: false
            description: "Turns the widget worker on."
          - name: Token
            env: WIDGET_TOKEN
            type: string
            min_length: 1
            max_length: 4096
            required: true
            secret: true
            description: "The access token of the widget service."
```

`LoadWorker()` returns `(Worker, error)`. Call it before provider setup. Pass the
typed fields to provider constructors. `ReadWorker(lookup)` accepts an explicit
lookup function for tests or an operator-supplied snapshot. The lookup must be
trusted and return promptly. A group reads each declared field at most once. It
does not enumerate the process environment or read another group's settings.

Each field states `description` and `required` (ST-SUP-RC22-OC09). The description
is one or two sentences of ASD-STE100 Simplified Technical English, each ending
with a full stop, of at most 200 characters on one line. A required field states
no default, and an optional field states its default. A field whose value is a
secret states `secret: true`; it is required and states no default. Apply lists
each field in the service's environment variable list
(`deploy/platform-requirements.yaml` and `deploy/environment-variables.md`), and
a secret field by its name only. Version 2.0.0 refuses a field that leaves out a
fact; version 1.0.0 did not ask for them.

A missing optional variable uses its declared default. A missing required
variable is an error (ST-SUP-RC22-OC02).
An explicitly empty variable is a value; it does not select a default. A string
can permit empty values with `min_length: 0`. All defaults must be strings in the
declaration and must satisfy the same rules as runtime input.

String limits count UTF-8 bytes, with a maximum of 4,096 bytes. Invalid UTF-8 and
control characters are rejected. Values are not trimmed or expanded. Integers
use signed 64-bit decimal form, with no leading zeros or plus sign. Durations use
Go duration syntax and cannot exceed 64 input bytes. Integer and duration fields
require explicit inclusive `min` and `max` values. Boolean values must be exactly
`true` or `false`. No type conversion depends on an ambient locale.

Invalid declarations fail before output. A component can declare at most 32
groups, 64 fields per group, and 256 fields in total. Group and field names are
limited to 64 bytes. The encoded declaration cannot exceed 128 KiB. Names are
exported ASCII Go identifiers. Environment names use uppercase ASCII letters,
digits, and underscores, with a letter first, and cannot exceed 128 bytes.
Duplicate fields, duplicate
environment names within a group, and generated symbol collisions are rejected.
Different groups can use the same environment name when they state it with the same
type, required flag, default, secret flag and limits; else apply refuses.

A failed read returns the zero settings value and an error that identifies only
the declared group, field, and fixed reason. It retains no input value or parser
error. `errors.Is(err, ErrConfiguration)` identifies configuration errors.
`errors.As` can obtain `*Error` and its `Group`, `Field`, and `Reason` methods.
Reasons are `missing`, `invalid`, or `lookup` for a nil lookup callback.

Formatting a complete settings value redacts its contents. Implicit JSON export
fails. Field access is explicit and returns the actual value; applications must
not log private fields. Use Secret file references instead of embedding secret
values as declaration defaults. Redaction does not prevent explicit reflection
or copying fields to another object.

Provider constructors still validate endpoint, certificate, credential, and
domain contracts. This component does not read Secret files, open connections,
grant access, or infer dependencies. Groups are independent. A process-wide
environment change during a read is not an atomic snapshot. Select immutable
configuration before startup when a shared snapshot is required.
