# RPC service

This archetype gives a Go service that serves its commands and queries over
gRPC and over HTTP. It has pooled tenancy, SpiceDB authorization, the audit
log, PostgreSQL and the deployment files. It serves no collection: a service
with no rest-api creates and changes rows through commands, and reads them
through queries.

## From init to the first build

1. Make the project:

   ```bash
   stego init --archetype rpc-service
   ```

   The scaffold holds one query, `get-greeting`, its fill and the fill's test,
   one proto file and the schema `authz/schema.zed`.

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
relation and the number of persons. Provision refuses with 400 and the field
`admins`:

- a body with no `admins`, or with 0 or more than 10 persons,
- the same person two times,
- an empty `subject` or `issuer`, one longer than 1024 bytes, or one with a
  control character,
- `admins` when the provisioning declares no `admin`.

The same `Idempotency-Key` with other admins is refused with 422.

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
