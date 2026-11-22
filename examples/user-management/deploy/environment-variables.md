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
| `GODEBUG` | string | no | none | no | - | `panicnil=1` | Read by the Go runtime and standard library: settings that change their behavior. The service does not start when it holds a tls or x509 setting, because that can turn off a TLS check. |
| `GOGC` | string | no | `100` | no | - | `200` | Read by the Go runtime: the garbage collection target as a percent, or off. When it has no value, it is 100. |
| `GOMAXPROCS` | integer | no | none | no | 1 to 1024 | `2` | Read by the Go runtime: the most operating system threads that run Go code at one time. When it has no value, the runtime uses the CPU limit. |
| `GOMEMLIMIT` | string | no | none | no | - | `512MiB` | Read by the Go runtime: the soft memory limit of the process, such as 512MiB. When it has no value, there is no limit. |
| `GOTRACEBACK` | string | no | `single` | no | none, single, all, system, crash | - | Read by the Go runtime: how much a crash report shows. When it has no value, it shows the goroutine that failed. |
| `SSL_CERT_DIR` | string | no | none | no | - | - | Read by the Go TLS library for folders of system certificate authorities. The service does not start when it has a value: give a certificate authority in the setting of each connection. |
| `SSL_CERT_FILE` | path | no | none | no | - | - | Read by the Go TLS library for the system certificate authorities. The service does not start when it has a value: give a certificate authority in the setting of each connection. |
| `STEGO_MODE` | list | no | `all` | no | api, worker, controller, all | `api,worker` | The modes that the process runs when no --mode option is given: some of the modes that the declaration states, or all. When it has no value, the process runs the declared modes. |
| `STEGO_SERVICE_VERSION` | string | no | none | no | - | `1.4.2` | The version of the release, which each audit row and the controller lease carry. When it has no value, they carry an empty version. |
| `TZ` | string | no | none | no | - | `UTC` | Read by the Go time library: the local time zone of the process. When it has no value, it uses the system time zone. |

## domain-probe

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `STEGO_PROBE_CONSOLE` | string | no | `off` | no | json, logfmt, off | `json` | The format of the domain probe values on standard output. When it has no value, the probes write nothing to standard output. |

## health-check

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `PORT` | integer | no | `8080` | no | 1 to 65535 | `8443` | The TCP port where the HTTP server listens. When it has no value, the server listens on port 8080. |
| `STEGO_HTTP_REQUIRE_TLS` | boolean | no | `0` | no | 0, 1 | - | Set to 1 to make the HTTP server use TLS. When it has no value, the server uses TLS only when it has a certificate and a key. |
| `STEGO_HTTP_TLS_CERT` | path | no | none | no | - | `/etc/stego/tls/tls.crt` | The file of the PEM TLS certificate of the HTTP server. When it and STEGO_HTTP_TLS_KEY have no value, the server uses plain HTTP. |
| `STEGO_HTTP_TLS_KEY` | path | no | none | no | - | `/etc/stego/tls/tls.key` | The file of the PEM TLS private key of the HTTP server. When it and STEGO_HTTP_TLS_CERT have no value, the server uses plain HTTP. |

## jwt-auth

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `STEGO_AUTHN_AUDIENCE` | string | no | none | no | - | `my-service` | The audience that each bearer token must name, which must be equal to the audience setting. When it has no value, the verifier uses the audience setting. |
| `STEGO_AUTHN_CA_FILE` | path | no | none | no | - | `/etc/stego/authn/ca.crt` | A PEM file of certificate authorities that the verifier adds to the system roots, unless the component states a CA file. When it has no value, the verifier uses the system roots. |
| `STEGO_AUTHN_ISSUER` | url | no | none | no | - | `https://sso.example.com/realms/main` | The issuer that each bearer token must name, which must be equal to the issuer setting. When it has no value, the verifier uses the issuer setting. |
| `STEGO_AUTHN_KEYS_FILE` | path | no | none | no | - | `/etc/stego/authn/keys.json` | The file of the key set of the issuer. Give it or STEGO_AUTHN_KEYS_URL when the component states no key file and no discovery. |
| `STEGO_AUTHN_KEYS_URL` | url | no | none | no | - | `https://sso.example.com/realms/main/certs` | The HTTPS URL of the key set of the issuer. Give it or STEGO_AUTHN_KEYS_FILE when the component states no key file and no discovery. |
| `STEGO_AUTH_AUDIENCE` | string | no | none | no | - | `my-service` | The audience that each token must name, which must be equal to the audience setting when the declaration states one. When it has no value, the verifier uses the audience setting. |
| `STEGO_AUTH_ISSUER` | url | no | none | no | - | `https://sso.example.com/realms/main` | The issuer that each token must name, which must be equal to the issuer setting when the declaration states one. When it has no value, the verifier uses the issuer setting. |
| `STEGO_AUTH_PUBLIC_KEY_FILE` | path | no | none | no | - | `/etc/stego/auth/public.pem` | The file of the RSA public key that verifies tokens. When it has no value, the verifier uses the public_key_file setting. |
| `STEGO_AUTH_ROLES_CLAIM` | string | no | none | no | - | `realm_access.roles` | The claim of the token that holds the roles of the caller. When it has no value, the verifier uses the roles_claim setting. |

## kafka-producer

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `STEGO_KAFKA_BROKERS` | list | one of `kafka-brokers` | none | no | - | `kafka-0.example.com:9093,kafka-1.example.com:9093` | The seed brokers, 1 to 16 host:port items, or STEGO_KAFKA_BROKERS_FILE in its place. The worker does not start when the two have no value. |
| `STEGO_KAFKA_BROKERS_FILE` | path | one of `kafka-brokers` | none | no | - | `/etc/stego/kafka/brokers` | The file that holds the list of seed brokers. Give it or STEGO_KAFKA_BROKERS, not the two. |
| `STEGO_KAFKA_BROKER_HOSTS` | list | no | none | no | - | `kafka-0.example.com,kafka-1.example.com` | The hosts that the worker may dial, at most 64, or STEGO_KAFKA_BROKER_HOSTS_FILE in its place. When the two have no value, the worker dials each broker that a broker names. |
| `STEGO_KAFKA_BROKER_HOSTS_FILE` | path | no | none | no | - | `/etc/stego/kafka/broker-hosts` | The file that holds the list of the hosts that the worker may dial. Give it or STEGO_KAFKA_BROKER_HOSTS, not the two. |
| `STEGO_KAFKA_CA_FILE` | path | no | none | no | - | `/etc/stego/kafka/ca.crt` | A PEM file of certificate authorities that the worker adds to the system roots. When it has no value, the worker uses the system roots. |
| `STEGO_KAFKA_CLIENT_CERT_FILE` | path | no | none | no | - | `/etc/stego/kafka/tls.crt` | The file of the PEM client certificate of mutual TLS. Give it with STEGO_KAFKA_CLIENT_KEY_FILE. |
| `STEGO_KAFKA_CLIENT_KEY_FILE` | path | no | none | no | - | `/etc/stego/kafka/tls.key` | The file of the PEM client key of mutual TLS. Give it with STEGO_KAFKA_CLIENT_CERT_FILE. |
| `STEGO_KAFKA_INSECURE_LOOPBACK` | boolean | no | `0` | no | 0, 1 | - | Set to 1 to let loopback brokers go without TLS, for a local test only. When it has no value, each broker needs TLS. |
| `STEGO_KAFKA_PASSWORD_FILE` | path | no | none | no | - | `/etc/stego/kafka/password` | The file of the SCRAM password. The mechanism SCRAM-SHA-512 needs it. |
| `STEGO_KAFKA_SASL_MECHANISM` | string | no | none | no | SCRAM-SHA-512, OAUTHBEARER | `SCRAM-SHA-512` | The SASL mechanism. When it has no value, the worker uses no SASL and needs a client certificate. |
| `STEGO_KAFKA_TOKEN_FILE` | path | no | none | no | - | `/etc/stego/kafka/token` | The file of the OAUTHBEARER token. The mechanism OAUTHBEARER needs it. |
| `STEGO_KAFKA_TOPIC` | string | one of `kafka-topic` | none | no | - | `orders.changes` | The topic of every message, which must be the topic of the topic need; give it or STEGO_KAFKA_TOPIC_FILE. stego env check refuses another topic in this variable, and does not read the file. |
| `STEGO_KAFKA_TOPIC_FILE` | path | one of `kafka-topic` | none | no | - | `/etc/stego/kafka/topic` | The file that holds the topic of every message. Give it or STEGO_KAFKA_TOPIC, not the two. |
| `STEGO_KAFKA_USERNAME` | string | no | none | no | - | `orders-worker` | The SCRAM user name. Give it or STEGO_KAFKA_USERNAME_FILE when the mechanism is SCRAM-SHA-512. |
| `STEGO_KAFKA_USERNAME_FILE` | path | no | none | no | - | `/etc/stego/kafka/username` | The file that holds the SCRAM user name. Give it or STEGO_KAFKA_USERNAME, not the two. |

## otel-tracing

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `GRPC_ALTS_MAX_CONCURRENT_HANDSHAKES` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_BINARY_LOG_FILTER` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_ENABLE_TXT_SERVICE_CONFIG` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_ENFORCE_ALPN_ENABLED` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_EXPERIMENTAL_ALTS_HANDSHAKER_KEEPALIVE_PARAMS` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_EXPERIMENTAL_ENABLE_DEFAULT_PORT_FOR_PROXY_TARGET` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_EXPERIMENTAL_ENABLE_PRIORITY_LB_CHILD_POLICY_CACHE` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_EXPERIMENTAL_PF_WEIGHTED_SHUFFLING` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_EXPERIMENTAL_RING_HASH_SET_REQUEST_HASH_KEY` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_EXPERIMENTAL_XDS_AUTHORITY_REWRITE` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_EXPERIMENTAL_XDS_BOOTSTRAP_CALL_CREDS` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_EXPERIMENTAL_XDS_DUALSTACK_ENDPOINTS` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_EXPERIMENTAL_XDS_EXT_PROC_ON_CLIENT` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_EXPERIMENTAL_XDS_GCP_AUTHENTICATION_FILTER` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_EXPERIMENTAL_XDS_HTTP_CONNECT` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_EXPERIMENTAL_XDS_MTLS_SPIFFE` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_EXPERIMENTAL_XDS_ORCA_LRS_PROPAGATION` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_EXPERIMENTAL_XDS_SNI` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_EXPERIMENTAL_XDS_SYSTEM_ROOT_CERTS` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_GCP_OBSERVABILITY_CONFIG` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_GCP_OBSERVABILITY_CONFIG_FILE` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_GO_EXPERIMENTAL_CASE_SENSITIVE_BALANCER_REGISTRIES` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_GO_EXPERIMENTAL_CONTROL_BUFFER_THROTTLE_LIMIT` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_GO_EXPERIMENTAL_ENABLE_8KB_DEFAULT_HEADER_LIST_SIZE` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_GO_EXPERIMENTAL_ENABLE_RECEIVE_BUFFER_COMPACTION` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_GO_EXPERIMENTAL_HTTP_FRAMER_READ_BUFFER_POOLING` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_GO_EXPERIMENTAL_XDS_RESOURCE_PANIC_RECOVERY` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_GO_IGNORE_TXT_ERRORS` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_GO_LOG_FORMATTER` | string | no | none | no | json | - | Read by the gRPC library: the format of its log messages. When it has no value, it writes plain text. |
| `GRPC_GO_LOG_SEVERITY_LEVEL` | string | no | `error` | no | error, ERROR, warning, WARNING, info, INFO | - | Read by the gRPC library: the lowest level of its log messages. When it has no value, it logs errors only. |
| `GRPC_GO_LOG_VERBOSITY_LEVEL` | integer | no | `0` | no | 0 to 99 | - | Read by the gRPC library: how much detail its info messages give. When it has no value, it is 0. |
| `GRPC_GO_SERVER_GOROUTINE_LABELS` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_RING_HASH_CAP` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_TEST_ONLY_GOOGLE_C2P_RESOLVER_TRAFFIC_DIRECTOR_URI` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_XDS_BOOTSTRAP` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_XDS_BOOTSTRAP_CONFIG` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `GRPC_XDS_ENDPOINT_HASH_KEY_BACKWARD_COMPAT` | string | no | none | no | - | - | Read by the gRPC library. The service does not start when it has a value, because it can change how gRPC connects or what it logs. |
| `OTEL_EXPORTER_OTLP_CERTIFICATE` | path | no | none | no | - | `/etc/stego/otel/ca.crt` | A PEM file of certificate authorities that verify the collector. When it has no value, the export uses the system roots. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | url | no | none | no | - | `https://collector.example.com:4317` | The HTTPS origin of the OpenTelemetry collector. When it has no value, the service exports no telemetry. |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | string | no | `grpc` | no | grpc | - | The OTLP protocol of the export, which can only be grpc. When it has no value, the export uses grpc. |
| `OTEL_LOGS_EXPORTER` | string | no | `otlp` | no | otlp, none | - | Set to none to stop the export of logs. When it has no value, the service exports logs with OTLP. |
| `OTEL_METRICS_EXPORTER` | string | no | `otlp` | no | otlp, none | - | Set to none to stop the export of metrics. When it has no value, the service exports metrics with OTLP. |
| `OTEL_METRIC_EXPORT_INTERVAL` | integer | no | `10000` | no | 1000 to 60000 milliseconds | `30000` | The time between two exports of metrics, in milliseconds. When it has no value, the time is 10000 milliseconds. |
| `OTEL_SERVICE_NAME` | string | no | none | no | - | `orders-api` | The service name in the telemetry. When it has no value, the telemetry uses the declared service name. |
| `OTEL_TRACES_SAMPLER_ARG` | number | no | `0.1` | no | 0 to 1 | `0.5` | The ratio of traces that the service samples. When it has no value, the ratio is 0.1. |
| `STEGO_LOG_LEVEL` | string | no | `info` | no | debug, info, warn, error | `warn` | The lowest severity of the log lines on standard output and standard error: debug, info, warn or error. When it has no value, the level is info. |
| `STEGO_OTEL_TOKEN_FILE` | path | no | none | no | - | `/etc/stego/otel/token` | The file of the bearer token for the collector at OTEL_EXPORTER_OTLP_ENDPOINT. When it has no value, the export sends no token. |

## postgres-adapter

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `DATABASE_URL` | string | one of `database-url` | - | yes | - | - | The PostgreSQL connection string of the runtime role, or DATABASE_URL_FILE in its place, not the two. The process does not start when the two have no value. |
| `DATABASE_URL_FILE` | path | one of `database-url` | none | no | - | `/etc/stego/database/url` | The file that holds the PostgreSQL connection string of the runtime role. Give it or DATABASE_URL, not the two. |
| `HOME` | path | no | none | no | - | - | Read by the pgx driver for its default pass, service and certificate files. The database opener gives each of those settings itself, so no file under HOME changes the connection. |
| `PGAPPNAME` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGCHANNELBINDING` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGCONNECT_TIMEOUT` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGDATABASE` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGHOST` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGMAXPROTOCOLVERSION` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGMINPROTOCOLVERSION` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGOPTIONS` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGPASSFILE` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGPASSWORD` | string | no | - | yes | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGPORT` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGREQUIREAUTH` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGSERVICE` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGSERVICEFILE` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGSSLCERT` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGSSLKEY` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGSSLMODE` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGSSLNEGOTIATION` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGSSLPASSWORD` | string | no | - | yes | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGSSLROOTCERT` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGSSLSNI` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGTARGETSESSIONATTRS` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGTZ` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `PGUSER` | string | no | none | no | - | - | Read by the pgx driver for a part that the connection string does not give. The service does not start when it has a value: give the part in DATABASE_URL. |
| `STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK` | boolean | no | `0` | no | 0, 1 | - | Set to 1 to let a loopback database connection go without TLS, for a local test only. When it has no value, each connection needs TLS. |
| `STEGO_DATABASE_CONNECTION_MAX_IDLE_TIME` | duration | no | `5m` | no | 1s to 24h | `10m` | The longest time that the pool keeps an idle database connection. When it has no value, the limit is 5 minutes. |
| `STEGO_DATABASE_CONNECTION_MAX_LIFETIME` | duration | no | `30m` | no | 1s to 24h | `1h` | The longest time that the pool keeps one database connection. When it has no value, the limit is 30 minutes. |
| `STEGO_DATABASE_MAX_IDLE_CONNECTIONS` | integer | no | none | no | 0 to 1024 | `8` | The largest number of idle connections in the database pool, at most the open limit. When it has no value, the limit is 4, or the open limit when it is less. |
| `STEGO_DATABASE_MAX_OPEN_CONNECTIONS` | integer | no | `16` | no | 1 to 1024 | `32` | The largest number of open connections in the database pool. When it has no value, the pool opens at most 16 connections. |
| `STEGO_MIGRATION_DATABASE_URL` | string | one of `migration-database-url` | - | yes | - | - | The PostgreSQL connection string of the migration job, or STEGO_MIGRATION_DATABASE_URL_FILE in its place, not the two. The process does not start when the two have no value. |
| `STEGO_MIGRATION_DATABASE_URL_FILE` | path | one of `migration-database-url` | none | no | - | `/etc/stego/database-owner/url` | The file that holds the PostgreSQL connection string of the migration job. Give it or STEGO_MIGRATION_DATABASE_URL, not the two. |
| `STEGO_RELAY_DATABASE_URL` | string | one of `relay-database-url` | - | yes | - | - | The PostgreSQL connection string of the relay role, or STEGO_RELAY_DATABASE_URL_FILE in its place, not the two. The process does not start when the two have no value. |
| `STEGO_RELAY_DATABASE_URL_FILE` | path | one of `relay-database-url` | none | no | - | `/etc/stego/database/relay-url` | The file that holds the PostgreSQL connection string of the relay role. Give it or STEGO_RELAY_DATABASE_URL, not the two. |
| `STEGO_WORKER_DATABASE_URL` | string | one of `worker-database-url` | - | yes | - | - | The PostgreSQL connection string of the worker role, or STEGO_WORKER_DATABASE_URL_FILE in its place, not the two. The process does not start when the two have no value. |
| `STEGO_WORKER_DATABASE_URL_FILE` | path | one of `worker-database-url` | none | no | - | `/etc/stego/database/worker-url` | The file that holds the PostgreSQL connection string of the worker role. Give it or STEGO_WORKER_DATABASE_URL, not the two. |

## query

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `STEGO_QUERY_CURSOR_EXTRA_KEY_FILE` | path | no | none | no | - | `/etc/stego/query/cursor-extra-key` | The file of a second key of the list cursors, which opens a cursor and seals none: the new key before it seals, or the old key after a rotation. It must differ from the key. |
| `STEGO_QUERY_CURSOR_KEY_FILE` | path | no | none | no | - | `/etc/stego/query/cursor-key` | The file of the key of the list cursors, at least 32 bytes, the same in each replica. Only the api mode reads it, and the api mode does not start when it has no value. |

## rest-api

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `CORS_ALLOWED_HEADERS` | list | no | `Content-Type,Authorization` | no | - | `Content-Type,Authorization,X-Request-Id` | The request headers that cross-origin requests may send. When it has no value, the API allows Content-Type and Authorization. |
| `CORS_ALLOWED_METHODS` | list | no | `GET,POST,PATCH,PUT,DELETE,OPTIONS` | no | - | `GET,POST` | The HTTP methods that cross-origin requests may use. When it has no value, the API allows GET, POST, PATCH, PUT, DELETE and OPTIONS. |
| `CORS_ALLOWED_ORIGINS` | list | no | none | no | - | `https://app.example.com,https://admin.example.com` | The origins that may send cross-origin requests. When it has no value, the API sets no CORS header. |

## spicedb-authz

| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |
|---|---|---|---|---|---|---|---|
| `STEGO_SPICEDB_ENDPOINT` | address | yes | none | no | - | `spicedb.example.svc:50051` | The host and port of SpiceDB. The service does not start when it has no value. |
| `STEGO_SPICEDB_INSECURE_LOOPBACK` | boolean | no | `0` | no | 0, 1 | - | Set to 1 to let a loopback SpiceDB address go without TLS, for a local test only. When it has no value, the connection needs TLS. |
| `STEGO_SPICEDB_KEY_FILE` | path | yes | none | no | - | `/etc/stego/spicedb/key` | The file of the preshared key of SpiceDB. The service does not start when it has no value. |

## Groups of one of

The service needs exactly one variable of each group: it does not start when the group has no value, or
more than one. stego env check fails in the same cases.

| Group | Variables |
|---|---|
| `database-url` | `DATABASE_URL`, `DATABASE_URL_FILE` |
| `kafka-brokers` | `STEGO_KAFKA_BROKERS`, `STEGO_KAFKA_BROKERS_FILE` |
| `kafka-topic` | `STEGO_KAFKA_TOPIC`, `STEGO_KAFKA_TOPIC_FILE` |
| `migration-database-url` | `STEGO_MIGRATION_DATABASE_URL`, `STEGO_MIGRATION_DATABASE_URL_FILE` |
| `relay-database-url` | `STEGO_RELAY_DATABASE_URL`, `STEGO_RELAY_DATABASE_URL_FILE` |
| `worker-database-url` | `STEGO_WORKER_DATABASE_URL`, `STEGO_WORKER_DATABASE_URL_FILE` |

The key of the Secret of each of these variables is the one value of its group:
`DATABASE_URL`, `STEGO_MIGRATION_DATABASE_URL`, `STEGO_RELAY_DATABASE_URL`, `STEGO_WORKER_DATABASE_URL`.
The base gives it as the variable, and the Component secret-files gives it as the file of the *_FILE form. Give
the Secret in the environment file, with or without the Component, and never the *_FILE variable: the base never
wires it.

## Secrets

The Secret and the key that give each secret variable, or the file of a path variable, names only. The migration Job
alone reads a variable of the mode migrate; the serving Deployment never references its Secret.

| Variable | Secret | Key | Read by |
|---|---|---|---|
| `DATABASE_URL` | `user-management-database` | `url` | the Deployment |
| `STEGO_KAFKA_CA_FILE` | `user-management-kafka-client` | `ca-crt` | the Deployment of the worker mode only (mode worker) |
| `STEGO_KAFKA_PASSWORD_FILE` | `user-management-kafka-client` | `password` | the Deployment of the worker mode only (mode worker) |
| `STEGO_MIGRATION_DATABASE_URL` | `user-management-database-owner` | `url` | the migration Job only (mode migrate) |
| `STEGO_QUERY_CURSOR_KEY_FILE` | `user-management-query-cursor` | `key` | the Deployment of the api mode only (mode api) |
| `STEGO_RELAY_DATABASE_URL` | `user-management-database-relay` | `url` | the Deployment of the worker mode only (mode worker) |
| `STEGO_SPICEDB_KEY_FILE` | `user-management-spicedb` | `preshared-key` | the Deployment and the migration Job |
| `STEGO_WORKER_DATABASE_URL` | `user-management-database` | `worker-url` | the Deployment of the worker mode only (mode worker) |
