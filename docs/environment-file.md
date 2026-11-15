# The environment file

An environment file tells what one environment of a service supplies: the image, the destination, and a value for
each platform requirement. `stego env check` compares the file with the platform requirements that `stego apply`
writes to `deploy/platform-requirements.yaml`:

```sh
stego env check --requirements deploy/platform-requirements.yaml env/staging.yaml
```

The check prints each requirement that the file does not meet, each value of the wrong type, and each value that
no requirement names. It exits with status 1 when one requirement is not met or one value has the wrong type. It
reads the key names of a secret, never a secret value, and it changes no file.

STEGO owns the schema of the file (ST-DLV-TM02). The schema is strict: a field that the schema does not have, a
YAML anchor, an alias or a merge key is an error, and the check names the line and the field.

## Fields

| Field | Value |
|---|---|
| `version` | The schema version, the number `1`. |
| `service` | The name of the service. It must be the service of the platform requirements. |
| `environment` | The name of the environment, such as `staging`. |
| `image` | The image of the service, by its digest: `repository@sha256:<64 hexadecimal digits>`. A tag is the wrong type. |
| `destination` | A map of `cluster` (the API server URL), `namespace` and `overlay_path`, each one text. |
| `secrets` | A map of secret names to maps of key names. The check reads only which keys have a value. |
| `configuration` | A map of environment variable names to one value each. |
| `backing_stores` | A map of names to `{engine}`, such as `engine: postgres`. |
| `topics` | A map of names to `{topic}`, the topic name of the environment. |
| `identity_issuers` | A map of names to `{issuer}`, an HTTPS URL with a host and no user, query or fragment. |
| `ingresses` | A map of names to `{port, host}`. The host is a host name, with no scheme, port or path. |
| `egresses` | A map of names to `{address, port}`. The address is a host name or an IP address, with no scheme, port or path. |
| `cluster_permissions` | A map of names to `{api_group, resource, verbs}`. `api_group` is `""` for the core group. |
| `pod_security` | A map of names to `{level}`: `privileged`, `baseline` or `restricted`. |

A port is a number from 1 to 65535. The verbs are `get`, `list`, `watch`, `create`, `update`, `patch`, `delete` and
`deletecollection`.

## How the check compares the values

- Each requirement of the platform requirements must have an entry of its kind and its name, with each part that its
  shape names: the keys of a secret, the engine of a backing store, the port of an ingress or an egress, the verbs of
  a cluster permission. A pod security level stricter than the requirement meets it.
- Each value under `configuration` must have the type of its environment variable in the `environment` list of the
  platform requirements (`string`, `integer`, `number`, `duration`, `boolean`, `url`, `path`, `address` or `list`),
  and be one of its allowed values or inside its range. A `url` must be an HTTPS URL. A `path` is an absolute file
  path. An `address` is a host and a port, such as `spicedb.example.com:50051`. A `duration` is a Go duration, such
  as `30s`.
- A required environment variable must have a value under `configuration`, or a secret of its name when the variable
  is secret.
- A secret environment variable under `configuration` is the wrong type: give it as a secret.
- An entry that no requirement and no listed variable names is listed as unused. It does not fail the check.

`deploy/environment-variables.md` lists each environment variable with its type, its default and its allowed values.

## Example

The environment file `env/staging.yaml` of a service `tasks` that uses PostgreSQL, SpiceDB and a JWT issuer:

```yaml
version: 1
service: tasks
environment: staging
image: quay.io/example/tasks@sha256:3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a
destination:
  cluster: https://api.staging.example.com:6443
  namespace: tasks-staging
  overlay_path: overlays/staging
secrets:
  tasks-database:
    url: <the connection string of the runtime role>
    migration-url: <the connection string of the migration job>
    worker-url: <the connection string of the worker role>
  tasks-database-relay:
    url: <the connection string of the relay role>
  tasks-spicedb:
    preshared-key: <the preshared key of SpiceDB>
  tasks-query-cursor:
    key: <at least 32 random bytes>
configuration:
  PORT: 8080
  STEGO_SPICEDB_ENDPOINT: spicedb.staging.example.com:50051
  STEGO_SPICEDB_KEY_FILE: /etc/tasks/spicedb/key
  STEGO_QUERY_CURSOR_KEY_FILE: /etc/tasks/cursor/key
  STEGO_AUTHN_KEYS_URL: https://sso.staging.example.com/realms/main/certs
backing_stores:
  tasks-db:
    engine: postgres
identity_issuers:
  tasks-issuer:
    issuer: https://sso.staging.example.com/realms/main
ingresses:
  http:
    port: 8080
    host: tasks.staging.example.com
egresses:
  tasks-db-network:
    address: postgres.staging.example.com
    port: 5432
  tasks-issuer-keys:
    address: sso.staging.example.com
    port: 443
  spicedb:
    address: spicedb.staging.example.com
    port: 50051
pod_security:
  pod-security:
    level: restricted
```

The names under `secrets`, `backing_stores`, `identity_issuers`, `ingresses`, `egresses` and `pod_security` are the
names that `deploy/platform-requirements.yaml` gives. Keep the secret values out of the repository: the check needs
only the key names, so a file in the repository can give each key a placeholder value.
