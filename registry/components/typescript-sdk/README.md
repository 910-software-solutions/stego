# TypeScript browser SDK

This component generates a browser client from captured OpenAPI 3.0 contracts.

To get the client of the service's own API, bind the mixin `typescript-sdk` in
the service declaration, such as `mixins: [typescript-sdk]`. The rest-crud
archetype lists it, and an rpc-service can bind it too. With no `document`
setting, the client is made from the OpenAPI document of the service in the
same run: the document that the rest-api component writes, or, with no
rest-api component, the document of the command and query routes that the
command component writes. The output is `out/tssdk`. To make a client of
another API, set `document` under `overrides.typescript-sdk` to the root
contract, and list its other files in `references`.

The client sends each request to the current browser origin, so a browser
service whose API proxy forwards the API prefix of the service serves it, as
the staff portal of a browser-service does. Give that prefix to the browser
backend (its `apis` setting) with the origin of the service.

The output contains an ES module, TypeScript declarations, and package metadata.
It has no npm runtime dependency. Import `createBrowserClient` from the output.
The client uses the current HTTPS origin and the separate STEGO browser backend.
The browser supplies cookies. Application code does not supply OAuth tokens.

Methods use the contract's `operationId`. Each method accepts an input object
with path and query fields and, where declared, `body`. A header parameter
of the operation is a field too, in lower camel case: `idempotencyKey`,
`ifMatch`, `ifNoneMatch`, `authzRevocationAfter` and `prefer`. These are the
headers that the API proxy of the browser backend forwards; a document with
another header parameter is refused. Each value is at most 1024 visible ASCII
characters. A command requires `idempotencyKey`, such as
`{idempotencyKey: JSON.stringify(crypto.randomUUID())}`. The client never makes
a key and never sends a request again: to send a write again after an unknown
outcome, send it with the same key. The optional second
argument supplies an abort signal and trace context. Results contain `status`,
`body`, and `etag`.
`RequestSchemas` contains request models. `Schemas` contains response models.
Runtime checks enforce the supported schema constraints in each direction.

Call `session()` for public session data. Call `logout()` for console sign-out.
Use the backend's `/auth/logout` page for the provider sign-out confirmation.
The client gets a fresh session CSRF token before each mutation.
Call `login()` to navigate to the same-origin login route. The client retains
a bounded local return path. It does not navigate after an API error unless
the application calls this method.

Declare `error_codes` for public API error codes that the UI needs. Use at most
64 unique names with lowercase letters, digits, underscores, and dots. Each name
starts with a letter and has at most 128 characters. Thus you can list the code
of a declared refusal of a command, such as `shipment.not_draft`:

```yaml
overrides:
  typescript-sdk:
    error_codes: [shipment.not_draft, shipment.not_found]
```

`SDKError.apiCode` contains only a declared code. The client reads the member
`code` of an error answer of the media type `application/json` or
`application/problem+json`, as a command refusal gives it. Error bodies and
private diagnostic fields stay hidden.

See the contract and test record (a retired record, in git history) for limits
and open work. Browser telemetry and the full Hypershell UI migration remain
open. The generated Go backend supplies its existing server telemetry.

For a traced operation, pass `{traceparent}` in the method's request options.
The client captures it before the operation starts. It also applies to the
operation's CSRF session request. Reused clients do not retain trace context.
The value must use W3C version `00`, nonzero lowercase trace and span IDs, and
flags `00` or `01`. Invalid values fail before a network request. The client
does not accept arbitrary headers, `tracestate`, or `baggage`. A trace context
does not grant API access.
