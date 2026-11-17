# Subscriptions

A service can take the change events that another STEGO service sends to
Kafka (the mixin `kafka-events`). Declare a subscription and one worker that
consumes it:

```yaml
subscriptions:
  - name: order-changes
    source: orders
    events: [orders.order.created]

workers:
  - name: ship-orders
    trigger: consumes order-changes
    writes: [Shipment]
    resource_type: shipment
    permission: ship
    fill: ship-orders

experimental_acceptances:
  - {component: kafka-consumer, accepted_until: 2027-03-31, reason: fulfilment ships each new order}
```

| Key | What it is |
|---|---|
| `name` | The name, in lower case with hyphens. It is the destination that the worker consumes. No command, query, worker or controller can have this name. |
| `source` | The name of the service that sends the events. It cannot be the service itself. |
| `events` | 1 to 32 event kinds of the source, each `<source>.<entity in lower case with hyphens>.<created, updated or deleted>`. |

A service has one subscription while the component `kafka-consumer` is alpha.
A subscription selects `kafka-consumer`, so accept it in the declaration.

The inbox writes each record into the outbox, so `kafka-consumer` needs the
port `durable-outbox`. The archetypes `rest-crud` and `rpc-service` bind this
port to the component `outbox`, so you do not bind it in `overrides`. The
archetype `browser-service` has no outbox, and thus a browser service cannot
declare a subscription.

In the worker mode, the Kafka inbox reads the topic in the consumer group
`<service>.<subscription>`. For each record it checks the headers, the key and
the payload, and it looks up the tenant of the header `stego-tenant` in the
tenant list. Then it writes the record into the outbox as a message for the
worker, in the transaction of that tenant. It commits the offset of a record
only after that transaction commits. The ID of the message comes from the
subscription, the tenant and the event ID. Thus when Kafka delivers a record
again, after a crash or a rebalance, the inbox writes nothing new, and the
worker runs once for each event.

- A record of a kind of the source that the subscription does not name is
  skipped.
- A record that fails a check, or whose tenant is not provisioned or is
  retired, is refused. A check also refuses a payload whose entity and action
  are not those of the kind (`kind-mismatch`), and an event ID that comes
  again with another payload (`event-id-reused`): the first message stays. The inbox writes a row to `stego_inbox.refused` with a
  fixed reason, such as `unknown-tenant`, and the warn event
  `kafka.consumer.refused`. The event comes at most once in 10 seconds for
  each reason, with the number of refusals that it held back in `suppressed`;
  the counter `kafka.consumer.records` counts each refusal. The row holds the topic, the partition, the
  offset and the reason, and no payload. The record is never applied, and it
  does not stop the records after it. The purge removes a row after 30 days.
  When you correct the cause, such as when you provision the tenant, reset the
  offset of the group to send the record again. The inbox writes no record
  two times.
- The worker acts with its own service account, as for each other message.
  It writes only the entities that it declares. A fill error that occurs 5
  times in a row parks the message as poison (see [A failed run of a consuming worker](commands-queries-workers.md#a-failed-run-of-a-consuming-worker)).
- The payload of a change event holds only the entity, the action and the row
  ID. To read the row, call the API of the source service.

The inbox does not start when the topic keeps its records longer than the
outbox keeps a done message (7 days by default), because an offset reset to an
older record could apply that record two times. A compacted topic
(`cleanup.policy` with `compact`) keeps the last record of each key without
end, so the inbox does not start on it either. For a topic that keeps its
records without end, set `STEGO_KAFKA_CONSUMER_ACCEPT_INFINITE_RETENTION=1`.
The inbox then writes the info event `kafka.consumer.retention_accepted` once.
When the inbox does not start, the readiness of the worker mode,
`/readyz/modes`, answers 503 with `worker: not ready: the component
inbox-<subscription> stopped`. It is the readiness probe of the worker
Deployment of the base (see [Probes and modes](delivery.md#probes-and-modes)).
The stage `kafka-consumer.retention` is not in that text: it is in the event
`kafka.consumer.start_refused`. `/readyz` stays 200, because it is the
readiness of the API.

The worker mode reads these settings, with the connection settings
`STEGO_KAFKA_*` of the Kafka destination:

| Variable | What it is |
|---|---|
| `STEGO_KAFKA_CONSUMER_TOPIC` | The topic of the source service. The worker mode does not start without it or its `_FILE` form. |
| `STEGO_KAFKA_CONSUMER_GROUP` | The consumer group. The default is `<service>.<subscription>`. |
| `STEGO_KAFKA_CONSUMER_ACCEPT_INFINITE_RETENTION` | `1` to read a topic with an infinite retention. |

When the sessions of the inbox fail 5 times in a row, for example because the
topic is missing or the user may not read it, the readiness of the worker mode
names the inbox and a fixed failure class, such as
`broker:TOPIC_AUTHORIZATION_FAILED`, and the gauge `kafka.consumer.failing` is
1. A session that reads the topic again clears both. In Kubernetes, the worker
Deployment of the base is then not ready.

## Access to the topic

The inbox trusts the header `stego-tenant` only because the platform lets
only the source service write the topic. Thus:

- Only the Kafka user of the source service may write the topic. The topic
  need of the source service has `access: write` in its
  `deploy/platform-requirements.yaml`, and the need of the consumer has
  `access: read`. Give each service only that access.
- Give the consumer a Kafka user of its own, with read access to the topic and
  to its consumer group only. While the consumer reads the connection settings
  `STEGO_KAFKA_*`, a service that both writes its own events and reads a
  subscription uses one user for the two; give that user write access to its
  own topic and read access to the source topic only.
