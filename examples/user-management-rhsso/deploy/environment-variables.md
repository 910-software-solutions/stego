# Environment variables of the service user-management

stego apply writes this file from the declaration and the registry, with the list in
platform-requirements.yaml. Do not edit it: change the declaration or the registry.

A required variable has no default: the service does not start when it has no value. A secret variable
shows its name only: give its value through a secret of the platform.

## audit-log

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `STEGO_AUDIT_SINK_AUTH_HEADER` | string | no | none | no | - | `X-Api-Key` | The header that carries the token of STEGO_AUDIT_SINK_TOKEN_FILE as the file holds it. When it has no value, the token goes in the Authorization header as a bearer token. |
| `STEGO_AUDIT_SINK_CA_FILE` | path | no | none | no | - | `/etc/stego/audit/ca.crt` | A PEM file of certificate authorities that the audit sink adds to the system roots. When it has no value, the sink uses the system roots. |
| `STEGO_AUDIT_SINK_CLIENT_CERT_FILE` | path | no | none | no | - | `/etc/stego/audit/tls.crt` | The file of the PEM client certificate of mutual TLS for the audit sink. Give it with STEGO_AUDIT_SINK_CLIENT_KEY_FILE. |
| `STEGO_AUDIT_SINK_CLIENT_KEY_FILE` | path | no | none | no | - | `/etc/stego/audit/tls.key` | The file of the PEM client key of mutual TLS for the audit sink. Give it with STEGO_AUDIT_SINK_CLIENT_CERT_FILE. |
| `STEGO_AUDIT_SINK_TOKEN_FILE` | path | no | none | no | - | `/etc/stego/audit/token` | The file of the token for the audit sink at STEGO_AUDIT_SINK_URL. When it has no value, the sink must use a client certificate. |
| `STEGO_AUDIT_SINK_URL` | url | no | none | no | - | `https://collector.example.com/v1/logs` | The HTTPS URL of the OTLP/HTTP logs endpoint that gets the audit records. When it has no value, the audit records go to a table in the service database. |

## command

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `STEGO_MODE` | list | no | `all` | no | api, worker, controller, all | `api,worker` | The modes that the process runs when no --mode option is given. When it has no value, the process runs all modes. |
| `STEGO_SERVICE_VERSION` | string | no | none | no | - | `1.4.2` | The version of the release, which each audit row and the controller lease carry. When it has no value, they carry an empty version. |

## domain-probe

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `STEGO_PROBE_CONSOLE` | string | no | `off` | no | json, logfmt, off | `json` | The format of the domain probe values on standard output. When it has no value, the probes write nothing to standard output. |

## health-check

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `PORT` | integer | no | `8080` | no | - | `8443` | The TCP port where the HTTP server listens. When it has no value, the server listens on port 8080. |
| `STEGO_HTTP_REQUIRE_TLS` | boolean | no | `0` | no | 0, 1 | - | Set to 1 to make the HTTP server use TLS. When it has no value, the server uses TLS only when it has a certificate and a key. |
| `STEGO_HTTP_TLS_CERT` | path | no | none | no | - | `/etc/stego/tls/tls.crt` | The file of the PEM TLS certificate of the HTTP server. When it and STEGO_HTTP_TLS_KEY have no value, the server uses plain HTTP. |
| `STEGO_HTTP_TLS_KEY` | path | no | none | no | - | `/etc/stego/tls/tls.key` | The file of the PEM TLS private key of the HTTP server. When it and STEGO_HTTP_TLS_CERT have no value, the server uses plain HTTP. |

## kafka-producer

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `STEGO_KAFKA_BROKERS` | list | no | none | no | - | `kafka-0.example.com:9093,kafka-1.example.com:9093` | The seed brokers, 1 to 16 host:port items, or STEGO_KAFKA_BROKERS_FILE in its place. The worker does not start when the two have no value. |
| `STEGO_KAFKA_BROKERS_FILE` | path | no | none | no | - | `/etc/stego/kafka/brokers` | The file that holds the list of seed brokers. Give it or STEGO_KAFKA_BROKERS, not the two. |
| `STEGO_KAFKA_BROKER_HOSTS` | list | no | none | no | - | `kafka-0.example.com,kafka-1.example.com` | The hosts that the worker may dial, at most 64, or STEGO_KAFKA_BROKER_HOSTS_FILE in its place. When the two have no value, the worker dials each broker that a broker names. |
| `STEGO_KAFKA_BROKER_HOSTS_FILE` | path | no | none | no | - | `/etc/stego/kafka/broker-hosts` | The file that holds the list of the hosts that the worker may dial. Give it or STEGO_KAFKA_BROKER_HOSTS, not the two. |
| `STEGO_KAFKA_CA_FILE` | path | no | none | no | - | `/etc/stego/kafka/ca.crt` | A PEM file of certificate authorities that the worker adds to the system roots. When it has no value, the worker uses the system roots. |
| `STEGO_KAFKA_CLIENT_CERT_FILE` | path | no | none | no | - | `/etc/stego/kafka/tls.crt` | The file of the PEM client certificate of mutual TLS. Give it with STEGO_KAFKA_CLIENT_KEY_FILE. |
| `STEGO_KAFKA_CLIENT_KEY_FILE` | path | no | none | no | - | `/etc/stego/kafka/tls.key` | The file of the PEM client key of mutual TLS. Give it with STEGO_KAFKA_CLIENT_CERT_FILE. |
| `STEGO_KAFKA_INSECURE_LOOPBACK` | boolean | no | `0` | no | 0, 1 | - | Set to 1 to let loopback brokers go without TLS, for a local test only. When it has no value, each broker needs TLS. |
| `STEGO_KAFKA_PASSWORD_FILE` | path | no | none | no | - | `/etc/stego/kafka/password` | The file of the SCRAM password. The mechanism SCRAM-SHA-512 needs it. |
| `STEGO_KAFKA_SASL_MECHANISM` | string | no | none | no | SCRAM-SHA-512, OAUTHBEARER | `SCRAM-SHA-512` | The SASL mechanism. When it has no value, the worker uses no SASL and needs a client certificate. |
| `STEGO_KAFKA_TOKEN_FILE` | path | no | none | no | - | `/etc/stego/kafka/token` | The file of the OAUTHBEARER token. The mechanism OAUTHBEARER needs it. |
| `STEGO_KAFKA_TOPIC` | string | no | none | no | - | `orders.changes` | The topic of every message, or STEGO_KAFKA_TOPIC_FILE in its place. When the two have no value, each message goes to the topic of its event type. |
| `STEGO_KAFKA_TOPIC_FILE` | path | no | none | no | - | `/etc/stego/kafka/topic` | The file that holds the topic of every message. Give it or STEGO_KAFKA_TOPIC, not the two. |
| `STEGO_KAFKA_USERNAME` | string | no | none | no | - | `orders-worker` | The SCRAM user name. Give it or STEGO_KAFKA_USERNAME_FILE when the mechanism is SCRAM-SHA-512. |
| `STEGO_KAFKA_USERNAME_FILE` | path | no | none | no | - | `/etc/stego/kafka/username` | The file that holds the SCRAM user name. Give it or STEGO_KAFKA_USERNAME, not the two. |

## otel-tracing

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `OTEL_EXPORTER_OTLP_CERTIFICATE` | path | no | none | no | - | `/etc/stego/otel/ca.crt` | A PEM file of certificate authorities that verify the collector. When it has no value, the export uses the system roots. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | url | no | none | no | - | `https://collector.example.com:4317` | The HTTPS origin of the OpenTelemetry collector. When it has no value, the service exports no telemetry. |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | string | no | `grpc` | no | grpc | - | The OTLP protocol of the export, which can only be grpc. When it has no value, the export uses grpc. |
| `OTEL_LOGS_EXPORTER` | string | no | `otlp` | no | otlp, none | - | Set to none to stop the export of logs. When it has no value, the service exports logs with OTLP. |
| `OTEL_METRICS_EXPORTER` | string | no | `otlp` | no | otlp, none | - | Set to none to stop the export of metrics. When it has no value, the service exports metrics with OTLP. |
| `OTEL_METRIC_EXPORT_INTERVAL` | integer | no | `10000` | no | 1000 to 60000 milliseconds | `30000` | The time between two exports of metrics, in milliseconds. When it has no value, the time is 10000 milliseconds. |
| `OTEL_SERVICE_NAME` | string | no | none | no | - | `orders-api` | The service name in the telemetry. When it has no value, the telemetry uses the declared service name. |
| `OTEL_TRACES_SAMPLER_ARG` | number | no | `0.1` | no | 0 to 1 | `0.5` | The ratio of traces that the service samples. When it has no value, the ratio is 0.1. |
| `STEGO_OTEL_TOKEN_FILE` | path | no | none | no | - | `/etc/stego/otel/token` | The file of the bearer token for the collector at OTEL_EXPORTER_OTLP_ENDPOINT. When it has no value, the export sends no token. |

## postgres-adapter

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `DATABASE_URL` | string | no | - | yes | - | - | The PostgreSQL connection string of the runtime role, or DATABASE_URL_FILE in its place, not the two. The process does not start when the two have no value. |
| `DATABASE_URL_FILE` | path | no | none | no | - | `/etc/stego/database/url` | The file that holds the PostgreSQL connection string of the runtime role. Give it or DATABASE_URL, not the two. |
| `STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK` | boolean | no | `0` | no | 0, 1 | - | Set to 1 to let a loopback database connection go without TLS, for a local test only. When it has no value, each connection needs TLS. |
| `STEGO_DATABASE_CONNECTION_MAX_IDLE_TIME` | duration | no | `5m` | no | 1s to 24h | `10m` | The longest time that the pool keeps an idle database connection. When it has no value, the limit is 5 minutes. |
| `STEGO_DATABASE_CONNECTION_MAX_LIFETIME` | duration | no | `30m` | no | 1s to 24h | `1h` | The longest time that the pool keeps one database connection. When it has no value, the limit is 30 minutes. |
| `STEGO_DATABASE_MAX_IDLE_CONNECTIONS` | integer | no | none | no | 0 to 1024 | `8` | The largest number of idle connections in the database pool, at most the open limit. When it has no value, the limit is 4, or the open limit when it is less. |
| `STEGO_DATABASE_MAX_OPEN_CONNECTIONS` | integer | no | `16` | no | 1 to 1024 | `32` | The largest number of open connections in the database pool. When it has no value, the pool opens at most 16 connections. |
| `STEGO_MIGRATION_DATABASE_URL` | string | no | - | yes | - | - | The PostgreSQL connection string of the migration job, or STEGO_MIGRATION_DATABASE_URL_FILE in its place, not the two. The process does not start when the two have no value. |
| `STEGO_MIGRATION_DATABASE_URL_FILE` | path | no | none | no | - | `/etc/stego/database/migration-url` | The file that holds the PostgreSQL connection string of the migration job. Give it or STEGO_MIGRATION_DATABASE_URL, not the two. |
| `STEGO_RELAY_DATABASE_URL` | string | no | - | yes | - | - | The PostgreSQL connection string of the relay role, or STEGO_RELAY_DATABASE_URL_FILE in its place, not the two. The process does not start when the two have no value. |
| `STEGO_RELAY_DATABASE_URL_FILE` | path | no | none | no | - | `/etc/stego/database/relay-url` | The file that holds the PostgreSQL connection string of the relay role. Give it or STEGO_RELAY_DATABASE_URL, not the two. |
| `STEGO_WORKER_DATABASE_URL` | string | no | - | yes | - | - | The PostgreSQL connection string of the worker role, or STEGO_WORKER_DATABASE_URL_FILE in its place, not the two. The process does not start when the two have no value. |
| `STEGO_WORKER_DATABASE_URL_FILE` | path | no | none | no | - | `/etc/stego/database/worker-url` | The file that holds the PostgreSQL connection string of the worker role. Give it or STEGO_WORKER_DATABASE_URL, not the two. |

## query

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `STEGO_QUERY_CURSOR_KEY_FILE` | path | yes | none | no | - | `/etc/stego/query/cursor-key` | The file of the key of the list cursors, at least 32 bytes, the same in each replica. The service does not start when it has no value. |

## rest-api

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `CORS_ALLOWED_HEADERS` | list | no | `Content-Type,Authorization` | no | - | `Content-Type,Authorization,X-Request-Id` | The request headers that cross-origin requests may send. When it has no value, the API allows Content-Type and Authorization. |
| `CORS_ALLOWED_METHODS` | list | no | `GET,POST,PATCH,PUT,DELETE,OPTIONS` | no | - | `GET,POST` | The HTTP methods that cross-origin requests may use. When it has no value, the API allows GET, POST, PATCH, PUT, DELETE and OPTIONS. |
| `CORS_ALLOWED_ORIGINS` | list | no | none | no | - | `https://app.example.com,https://admin.example.com` | The origins that may send cross-origin requests. When it has no value, the API sets no CORS header. |

## rh-sso-auth

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `AUTH_ENABLED` | boolean | no | `true` | no | true | - | Tells that SSO authentication is on, and the service refuses each value other than true. When it has no value, authentication is on. |
| `JWK_CA_FILE` | path | no | none | no | - | `/etc/stego/auth/ca.crt` | A PEM file of certificate authorities for the key set URL. When it has no value, the verifier uses the jwk_ca_file setting. |
| `JWK_CERT_FILE` | path | no | none | no | - | `/etc/stego/auth/jwks.json` | The file of the key set of Red Hat SSO. When it has no value, the verifier uses the jwk_cert_file setting. |
| `JWK_CERT_URL` | url | no | `https://sso.redhat.com/auth/realms/redhat-external/protocol/openid-connect/certs` | no | - | `https://sso.redhat.com/auth/realms/redhat-external/protocol/openid-connect/certs` | The HTTPS URL of the key set of Red Hat SSO. When it has no value, the verifier uses the jwk_cert_url setting. |
| `STEGO_AUTHN_AUDIENCE` | string | no | `user-management` | no | - | `my-service` | The audience that each bearer token must name, which must be equal to the audience setting. When it has no value, the verifier uses the audience setting. |
| `STEGO_AUTHN_CA_FILE` | path | no | none | no | - | `/etc/stego/authn/ca.crt` | A PEM file of certificate authorities that the verifier adds to the system roots, unless the component states a CA file. When it has no value, the verifier uses the system roots. |
| `STEGO_AUTHN_ISSUER` | url | no | `https://sso.example.com/auth/realms/example` | no | - | `https://sso.example.com/realms/main` | The issuer that each bearer token must name, which must be equal to the issuer setting. When it has no value, the verifier uses the issuer setting. |
| `STEGO_AUTHN_KEYS_FILE` | path | no | none | no | - | `/etc/stego/authn/keys.json` | The file of the key set of the issuer. Give it or STEGO_AUTHN_KEYS_URL when the component states no key file and no discovery. |
| `STEGO_AUTHN_KEYS_URL` | url | no | none | no | - | `https://sso.example.com/realms/main/certs` | The HTTPS URL of the key set of the issuer. Give it or STEGO_AUTHN_KEYS_FILE when the component states no key file and no discovery. |
| `STEGO_AUTH_AUDIENCE` | string | no | `user-management` | no | - | `my-service` | The audience that each token must name. When it has no value, the verifier uses the audience setting. |
| `STEGO_AUTH_ISSUER` | url | no | `https://sso.example.com/auth/realms/example` | no | - | `https://sso.redhat.com/auth/realms/redhat-external` | The issuer that each token must name. When it has no value, the verifier uses the issuer setting. |
| `STEGO_AUTH_PUBLIC_KEY_FILE` | path | no | none | no | - | `/etc/stego/auth/public.pem` | The file of the RSA public key of the shared JWT verifier. This component sets no default, so the shared verifier does not start when it has no value. |
| `STEGO_AUTH_ROLES_CLAIM` | string | no | none | no | - | `realm_access.roles` | The claim of the token that holds the roles of the caller, for the shared JWT verifier. When it has no value, the verifier reads no roles claim. |

## spicedb-authz

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `STEGO_SPICEDB_ENDPOINT` | address | yes | none | no | - | `spicedb.example.svc:50051` | The host and port of SpiceDB. The service does not start when it has no value. |
| `STEGO_SPICEDB_INSECURE_LOOPBACK` | boolean | no | `0` | no | 0, 1 | - | Set to 1 to let a loopback SpiceDB address go without TLS, for a local test only. When it has no value, the connection needs TLS. |
| `STEGO_SPICEDB_KEY_FILE` | path | yes | none | no | - | `/etc/stego/spicedb/key` | The file of the preshared key of SpiceDB. The service does not start when it has no value. |
