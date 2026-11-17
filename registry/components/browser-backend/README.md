# Browser backend

Use this component in a separate browser service. The `browser-service`
archetype adds the common database pool, health checks, and telemetry.
The application supplies its browser assets and its domain API.

Use PostgreSQL adapter 4.4.1 or later and the compiler's independent factory
selection. This supplies the shared verified-TLS policy, connection deadline,
and bounded pool even when the browser service has no entities. Configure its
pool with the same deployment settings (a retired record, in git history)
as the API. With telemetry 1.13.0, pool metrics are automatic. The browser and
API processes have separate pool budgets and runtime identities.

Declare `api_prefix`, `routes`, and `assets`. Each asset has a `source` path
relative to the service project and a public `path`. Public assets use
`/index.html` or `/assets/`. Routes must include `/`. A route can contain
`{id}`. An optional `roles_claim` selects a dotted identity claim path.
The compiler captures the declared asset files and checks their sizes.

Run the service binary with the one argument `migrate` before service startup
(version 4.6.0). The Kubernetes base writes this migration Job with the Secret
`<service>-database-owner`, as for a rest-crud service. The mode migrate reads
the name of the runtime login from `STEGO_RUNTIME_DATABASE_ROLE` (from the
ConfigMap) and opens the owner connection of `STEGO_MIGRATION_DATABASE_URL`.
Only this mode reads the two, and the Job holds no credential of the runtime
login. When the migration login is not the database owner, it takes the owner
role, so the login must be a member of it. Then it runs
`browser/schema.Bootstrap` for the runtime login, which checks the owner, the
login, the table, the columns, the constraints, the indexes and the grants of
that login. It exits 0 only when Bootstrap passes. The start of the service runs
`browser/schema.Verify` as the runtime login.
You write no Go code or SQL to make the session table.

The database administrator makes these before the first migration job: a
NOLOGIN owner role that owns the database, a runtime login with only CONNECT on
the database and USAGE on the schema public, and a migration login that is a
member of the owner role. Revoke the default PUBLIC grants on the database and
on the schema public. The schema check refuses other attributes and grants, so
use this SQL as it is. Replace `<service>` with the service name, with each `-`
as `_`. Generation refuses a service name that gives a role name longer than
the 63 bytes of a PostgreSQL name, because PostgreSQL cuts a longer name with
no error.

```sql
-- As a database administrator, connected to any database of the server:
CREATE ROLE <service>_owner NOLOGIN NOINHERIT;
CREATE ROLE <service>_runtime LOGIN NOINHERIT;
CREATE ROLE <service>_migrator LOGIN IN ROLE <service>_owner;
CREATE DATABASE <service> OWNER <service>_owner;
REVOKE ALL ON DATABASE <service> FROM PUBLIC;
GRANT CONNECT ON DATABASE <service> TO <service>_runtime;
```

Then give each login a new secret in `psql`. First make sure that the server
keeps secrets as SCRAM: `SHOW password_encryption` must give `scram-sha-256`.
If it gives `md5`, do not go on; run `SET password_encryption =
'scram-sha-256';` in the session first. The `\password` command asks for the
secret and sends only its SCRAM verifier. The secret is then not in the SQL or
the shell history, and the server log can hold only its SCRAM verifier:

```psql
SHOW password_encryption;
\password <service>_runtime
\password <service>_migrator
```

PostgreSQL gives CONNECT on the databases `postgres` and `template1` to PUBLIC.
The block below revokes it, so that the runtime login cannot open them. It
changes the server for each login: before you run it, grant CONNECT on these
databases to each other login that uses them, such as a monitoring login or
the `auth_query` login of PgBouncer. A database of another application where
PUBLIC still has CONNECT stays open to the runtime login. The real limit is a
`pg_hba.conf` rule such as `host <service> <service>_runtime <cidr>
scram-sha-256`, with no other line that matches the role.

```sql
-- As a database administrator, after the grants to the other logins:
REVOKE CONNECT ON DATABASE postgres FROM PUBLIC;
REVOKE CONNECT ON DATABASE template1 FROM PUBLIC;
```

```sql
-- As a database administrator, connected to the database <service>:
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO <service>_runtime;
```

The check needs these details, which PostgreSQL does not give by default:

- The owner role and the runtime login are NOINHERIT. PostgreSQL makes a role
  INHERIT when you do not say so, and the check refuses that with `fault: owner`
  or `fault: login`.
- Only the owner and the runtime login hold a grant on the database. Do not
  grant CONNECT to the migration login: it connects through its membership of
  the owner role. A grant to it gives `fault: grant`.
- The migration login is INHERIT, the default, so that it can connect as a
  member of the owner role.

The Job takes `STEGO_RUNTIME_DATABASE_ROLE=<service>_runtime`, and the owner
Secret holds the connection string of `<service>_migrator`.

When Verify refuses the database, at startup or in the mode migrate, the
failure record names the stage `database.schema`, the field `fault` (owner,
login, table, column, constraint, index or grant) and the field `object` (the
generated table, column, constraint or index). It holds no value, password or
database error text. The stage `database.access` is kept for a connection that
did not open or a login that could not be read.

The generated `browser/schema.Bootstrap` function is also available to your own
code. Version 2 requires a separate schema owner and runtime login. An older deployment
that used the table owner as its runtime login must change its grants and table
ownership before it uses this version. Startup rejects unsafe permissions.

With `postgres-client`, provision a database with `ManagedSchema: true`, then call
`WithDatabaseOwner`. In its callback, pass the connection and `identity.User` to
`browser/schema.Bootstrap`. Keep this operation in the controller. The generated
browser process uses only the limited runtime login. The schema package is a
separate Go package and does not include browser assets or the browser runtime.

Bootstrap installs the generated `browser/schema.sql` only in an empty public
namespace. It checks an existing schema without a repair. All DDL and grants use
one transaction. A retry retains the table and session data. The runtime verifies
the owner, login, table, columns, constraints, indexes, and grants before it opens
the session store. Unexpected triggers, rules, row policies, or grants stop
startup. The runtime has SELECT, INSERT, UPDATE, and DELETE. It has no schema
CREATE, database TEMPORARY, or table TRUNCATE permission.

This schema check supports PostgreSQL 16 through 18. It checks the enforcement
flag on PostgreSQL 18. PostgreSQL 18 also stores table NOT NULL constraints in
`pg_constraint`; earlier supported versions use `pg_attribute` for these checks.
See the [PostgreSQL constraint catalog](https://www.postgresql.org/docs/18/catalog-pg-constraint.html).
The focused CI schema test uses PostgreSQL 18. Do not treat this as live evidence
for other server versions.
Set these values in the service environment:

- `DATABASE_URL`: the database connection with verified TLS.
- `STEGO_HTTP_TLS_CERT` and `STEGO_HTTP_TLS_KEY`: the service certificate files.
- `STEGO_BROWSER_ORIGIN`: the public HTTPS origin without a path.
- `STEGO_BROWSER_API_URL`: the separate API HTTPS origin without a path.
- `STEGO_BROWSER_API_CA_FILE`: the API trust root file, if required.
- `STEGO_BROWSER_ISSUER`: the OIDC issuer URL.
- `STEGO_BROWSER_ISSUER_CA_FILE`: the issuer trust root file, if required.
- `STEGO_BROWSER_CLIENT_ID`: the confidential OIDC client ID.
- `STEGO_BROWSER_CLIENT_SECRET_FILE`: the protected client secret file.
- `STEGO_BROWSER_SESSION_KEY_FILE`: a protected file with 32 random bytes
  encoded as standard base64.

Use different host names for the console, its API, and its identity provider.
The API and identity provider can share a host with each other. They must not
share the console host, even on another port. Use ASCII host names, including
punycode for international domain names. Cookies do not isolate ports on one
host, as described in [RFC 6265](https://www.rfc-editor.org/rfc/rfc6265#section-8.5).

Register the exact `STEGO_BROWSER_ORIGIN` plus `/auth/callback` as the client
redirect URI. The identity provider must support authorization codes, S256
PKCE, and token revocation. Grant the access token the audience and roles
required by the domain API. ID tokens must identify the browser client.

The service exposes GET `/auth/login`, GET `/auth/callback`, GET
`/auth/session`, and GET and POST `/auth/logout`. The session response includes
`authenticated`, `expires_at`, `roles`, `user`, and `csrf_token` when signed
in. It does not include OAuth tokens. Mutations require the exact Origin and
`X-CSRF-Token` headers. The API prefix forwards the user's server-held access
token to the configured API. It does not accept bearer tokens from the browser.

The component does not supply domain pages, API authorization rules, identity
provider administration, ingress certificates, or production database setup.
See the runtime contract (a retired record, in git history) for limits and
acceptance evidence. A browser user interface and deployment still need their
own application checks.

GET `/auth/logout` displays a confirmation form. It does not remove the session
or revoke a token. The form requires the session CSRF value and an exact Origin
match. POST without a form still uses `X-CSRF-Token` and returns 204 after local
session removal and token revocation. Failed revocation returns an error; the
local session stays removed.

Set `logout_scope: identity_provider` to send a confirmed browser form to the
identity provider's discovered logout endpoint after local sign-out. The default
is `console`. Provider sign-out requires a separate HTTPS cookie host and an
endpoint without query parameters. Register the exact console origin plus
`/auth/logout` as a permitted post-logout redirect URI. The generated request
uses `client_id` and `post_logout_redirect_uri`. It does not send an ID token
through browser HTML or a URL. The provider must ask for confirmation without
an ID token hint, as specified by
[OpenID Connect RP-Initiated Logout](https://openid.net/specs/openid-connect-rpinitiated-1_0.html#RPLogout).
The console confirmation page permits form navigation to this provider origin.
Other pages retain the same-origin form policy. A return to the console confirms
only local sign-out; it does not assert that the provider completed sign-out.

An invalid or rejected API session returns HTTP 401 with `error: reauth_required`,
`login_url: /auth/login`, and `statusCode: 401`. The browser can restart login
and set a permitted `return_to` route. Storage failure remains HTTP 503. API
permission denial remains HTTP 403 and does not remove a valid session.

For a production browser build, run this command from the service project:

```sh
stego assets --directory build/client --output ui/assets.zip
```

Declare `asset_bundle: ui/assets.zip` instead of `assets`. Keep the bundle
outside generated output.
The command accepts `index.html` and supported files below `assets/`. It rejects
symbolic links, invalid paths, and files that exceed the limits. The archive
has stable file order and metadata. An invalid input cannot replace an existing
bundle.

Limits are 128 files, 4 MiB per expanded file, 16 MiB in total, and 4 MiB for
the captured ZIP. The compiler reads only captured input bytes. It does not
read a build directory during generation. The generated server permits exact
SHA-256 hashes for inline scripts in `index.html`. Other responses do not get
these script permissions. It does not permit `unsafe-inline` or `unsafe-eval`.

HTML and JavaScript inputs are trusted application code. The HTML checks find
unsupported constructs. They are not an HTML sanitizer. The application must
check its dependencies, rendered pages, accessibility, and browser behavior.

When `telemetry_service_name` is set, the compiler requires one explicit HTML
head element. It rejects an existing `stego-runtime-config` metadata element.
The backend inserts this metadata with the active runtime's public signal
settings. Collector addresses and credentials are not included. The metadata
is HTML-escaped data. It does not require an inline script or a change to the
script policy. The response ETag includes the settings.

Concurrent requests wait for an active token renewal across backend instances.
The wait has a two-second deadline and uses the caller's cancellation signal.
Only the request that claims the stored session can renew its token. A stalled
renewal returns temporary unavailability and keeps the session cookie. A lost
or uncertain renewal still requires a new login. Sign-out takes precedence.
A cancelled renewal releases its stored claim with a separate five-second
cleanup deadline. Known tokens that were not saved are revoked within that
budget. The cancelled request cannot prevent this cleanup.

The generated `sessionclient` directory supplies a browser JSON client for an
upstream API. It uses the same request, session, CSRF, cancellation, and input
limits as the generated TypeScript SDK. Its API prefix is fixed at generation.
It rejects requests outside that prefix, redirects, unsafe paths, and caller
credentials. Writes first read the current session. Cookies stay in the browser.
Sign-out opens the generated confirmation page.

The client returns an `unknown` JSON body. The application must validate its
own response shapes, or use an OpenAPI SDK for that validation. The session
client does not claim to validate the upstream application's domain contract.
