# Go SDK

This component generates typed Go clients from OpenAPI 3.0 contracts.

To get the client of the service's own API, bind the mixin `go-sdk` in the
service declaration, such as `mixins: [go-sdk]`. The rest-crud archetype
lists it. With no `document` setting, the client is made from the OpenAPI document that
the rest-api component writes for the service in the same run
(`out/internal/api/openapi.json`). Thus it has a method for each collection
route, each command and each get or list query, and it never differs from the
service. The package is `out/sdk`. To make a client of another API, set
`document` under `overrides.go-sdk` to the root contract in the project, and
list its other files in `references`.

A command, and a create of a service with the idempotency component, require
the header `Idempotency-Key`. Each other create and each patch take it, not
required: the service keeps the key and gives the stored answer to a second
request with the same key and body. Give it in the parameters of the method,
such as `DischargePatientParams{IdempotencyKey: key}` or
`CreateApplicantsParams{IdempotencyKey: &key}`: a structured string, such as a
quoted UUID. The SDK sends the key that you give, and never sends a request
again. To send a write again after an unknown outcome, send it with the same
key.

A secret member of a request, such as a PIN, is never in an answer. When the
request requires it, it is a plain field, such as `PortalPin string`; when the
request does not require it, such as on an update that keeps the stored value,
it is a pointer.

STEGO supplies a bounded HTTPS transport. Select `otel-tracing` to include shared
logs, metrics, and traces. The application supplies no client transport code.
Call `sdk.NewClient` with a base URL, bearer token, and optional CA file and
request timeout. Call `Close` to release the client.

See the input and runtime contract (a retired record, in git history) for limits,
unsupported features, response handling, and acceptance requirements.

Version 2 preserves omitted fields, explicit JSON null, and values for nullable
properties. Nullable model fields use `nullable.Nullable[T]`. Use `Set`,
`SetNull`, and `SetUnspecified` to select the state. This replaces pointer
fields from version 1 and requires caller source changes.
