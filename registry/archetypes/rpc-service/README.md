# RPC service

This archetype gives a Go service that serves its commands and queries over
gRPC and over HTTP. It has pooled tenancy, SpiceDB authorization, the audit
log, PostgreSQL and the deployment files. It serves no collection: a service
with no rest-api creates and changes rows through commands, and reads them
through queries.

## From init to the first build

1. Make the project:

   ```bash
   stego init --archetype rpc-service --module github.com/my-org/greeter
   ```

   Init writes `go.mod` with that module path, and each later command reads
   it from there. The scaffold holds one query, `get-greeting`, its fill and the fill's test,
   two proto files and the schema `authz/schema.zed`: `proto/messages.proto`
   holds the request and response messages, and `proto/service.proto` holds the
   gRPC service, which imports them.

2. Check the declaration:

   ```bash
   stego validate
   ```

   Validate accepts the scaffold. It gives one warning: the example fill
   `get-greeting` has no evidence and no sign-off.

3. Test the fill and sign its evidence with your own key. List the public key
   in the file `SIGNERS` first, one line `<email> <public key>`:

   ```bash
   stego test
   stego fill sign get-greeting
   ```

   Plan, apply, build and deps refuse a fill with no evidence or no sign-off.
   `stego test` exits non-zero and names each fill that records no evidence.
   `stego fill sign` keeps a sign-off that already covers the current evidence
   with a key that `SIGNERS` lists; give `--force` to sign again. With a key
   that `SIGNERS` does not list, it signs, prints a warning with the key
   fingerprint, and plan, apply, build and deps refuse the fill.
   `stego validate` and `stego plan` also warn when the sign-off counts only
   because of a key that the change adds to `SIGNERS`; with `--strict` this is
   a fault.

4. Generate, resolve the dependencies and build:

   ```bash
   stego plan
   stego apply
   stego deps
   go build -o service ./out
   ```

The scaffold is an example. Change it to your service before the first
`stego apply`: apply writes the record of the service, and plan and apply
check each later release against it and against the default branch. Then an
entity that a release removes, such as `Greeting`, is a schema change. To try
the scaffold as it is, build it in a folder of its own.

`stego apply` writes `deploy/environment-variables.md`, which names each
setting of the binary. The binary with the one argument `migrate` is the
migration job: it writes the tables and the SpiceDB schema before the
service starts.

## Generated clients

Each command and each get or list query with a `route` is served over HTTP
too. The command component writes the OpenAPI document of these routes in
`out/commands/openapi.json`. A stream query is served over gRPC only, so it is
not in the document.

To get a Go client or a CLI of these routes, bind their mixins:

```yaml
mixins: [go-sdk, cli-application]
```

The Go SDK (`out/sdk`) and the CLI (`out/cli/cmd`) are made from that
document, as for a rest-crud service, so you give no `document` and no
`factory_package`. Give the base URL of the service with no base path, such as
`https://underwriting.example.com`: the document holds the base path in each
route. See the READMEs of
[go-sdk](../../components/go-sdk/README.md) and
[cli-application](../../components/cli-application/README.md).

## Tenants and the first grant

The service holds no data until an operator provisions a tenant. Each
permission is decided in SpiceDB. Thus a new tenant needs one person who has
a grant in it. Provision gives this grant.

### The operator

The operator holds the relation `operator` on the service-level resource of
the whole service, `service:<service name>`, such as `service:fulfilment`.
This ID has no tenant. No STEGO path writes this grant. The platform operator
writes it to SpiceDB directly, one time for each service:

```bash
zed relationship create service:fulfilment operator "user:<user ID of the operator>"
```

`zed` writes with the SpiceDB preshared key. This key can write every
relation of every tenant. Thus only the platform operator runs `zed`, and
only from a controlled host. A `zed` write gets no STEGO audit row.

### The user ID

SpiceDB knows a person by a user ID that STEGO makes from the token:

```text
<base64url of the issuer>|<base64url of the sub claim>
```

base64url has no padding (`=`). For the issuer
`https://idp.example/realms/stego` and the subject `dora`, the user ID is
`aHR0cHM6Ly9pZHAuZXhhbXBsZS9yZWFsbXMvc3RlZ28|ZG9yYQ`. In a shell:

```bash
b64url() { printf %s "$1" | base64 -w0 | tr '+/' '-_' | tr -d '='; }
echo "$(b64url https://idp.example/realms/stego)|$(b64url dora)"
```

### The first administrators

Declare the relation that Provision gives to the first administrators of a
new tenant, with `admin` in the provisioning:

```yaml
tenancy:
  kind: pooled
  key: tenant_id
  claim: tenant
  provisioning: {resource_type: service, provision: provision, retire: retire, admin: admin}
```

Add the relation to the resource type of the provisioning. Give it the
permissions of the commands on the service-level resource:

```text
definition service {
	relation operator: user
	relation admin: user
	relation dispatcher: user
	permission provision = operator
	permission retire = operator
	permission create_shipment = admin + dispatcher
}
```

Then the operator provisions the tenant and names its first administrators,
1 to 10 persons. Give the `sub` claim of each person. Give `issuer` only when
the person's tokens come from another issuer than the operator's:

```bash
curl -X POST https://<host>/api/fulfilment/v1/stego/tenants \
  -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H 'Idempotency-Key: "provision-acme"' \
  -H 'Content-Type: application/json' \
  -d '{"tenant_id":"acme","admins":[{"subject":"dora"}]}'
```

Provision writes, in its transaction, the grant
`service:<base64url of the tenant>/<base64url of the service name>#admin@user:<user ID>`
for each person. The grant is in the new tenant only. It does not let the
person provision or retire a tenant. The audit row of Provision names the
relation and the number of persons.

Provision first decides the permission of the caller. A caller who may not
provision gets 404 and learns nothing of the admins. Then Provision refuses
with 400 and the field `admins`:

- a body with no `admins`, or with 0 or more than 10 persons,
- the same person two times,
- an empty `subject` or `issuer`, one longer than 1024 bytes, or one with a
  control character,
- an `issuer` that is not the issuer whose tokens the service accepts,
- `admins` when the provisioning declares no `admin`.

A replay with the same `Idempotency-Key` must send the same body: the same
`admins` list, in the same order and in the same form. For example, an
`issuer` that is the same as the default counts as another form. Another body
with that key is refused with 422.

### A command that writes the first row

A command whose `resource_type` is the resource type of the provisioning, and
that names no `resource_member`, is decided on the service-level resource of
the tenant of the request. It creates rows with no parent:

```yaml
commands:
  - name: create-shipment
    writes: [Shipment]
    resource_type: service
    permission: create_shipment
    route: POST /shipments
    method: fulfilment.v1.Fulfilment/CreateShipment
    fill: create-shipment
```

A command reads its request and response messages from a proto file with no
service, so put them in `proto/messages.proto` and list that file under the
setting `proto_files` of the component `command` too; add the method to the
service in `proto/service.proto`:

```yaml
overrides:
  command:
    proto_files:
      - {path: proto/messages.proto, import_path: greeter/v1/messages.proto}
```

The runtime decides `create_shipment` on
`service:<base64url of the tenant>/<base64url of the service name>`. A grant
on `service:fulfilment`, with no tenant, is not a grant in a tenant: the
command answers 404.

### Later grants

No STEGO path gives a further person a relation on the service-level resource
of a tenant yet (open question ST-SUP-Q160). Until then, the operator writes
the grant to SpiceDB directly, with the tenant-qualified ID:

```bash
TENANT=$(b64url acme); SERVICE=$(b64url fulfilment)
zed relationship create "service:$TENANT/$SERVICE" dispatcher "user:<user ID>"
```

The rules of the operator's `zed` writes apply: only the platform operator
writes, from a controlled host, and the write gets no STEGO audit row.
ST-SUP-Q160 tracks a path that STEGO decides and audits.
