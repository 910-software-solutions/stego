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
  retired, is refused. The inbox writes a row to `stego_inbox.refused` with a
  fixed reason, such as `unknown-tenant`, and the warn event
  `kafka.consumer.refused`. The row holds the topic, the partition, the
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
older record could apply that record two times. For a topic that keeps its
records without end, set `STEGO_KAFKA_CONSUMER_ACCEPT_INFINITE_RETENTION=1`.
The readiness of the worker mode then names the stopped inbox, and the log has
the event `kafka.consumer.start_refused` at the stage
`kafka-consumer.retention`.

The worker mode reads these settings, with the connection settings
`STEGO_KAFKA_*` of the Kafka destination:

| Variable | What it is |
|---|---|
| `STEGO_KAFKA_CONSUMER_TOPIC` | The topic of the source service. The worker mode does not start without it or its `_FILE` form. |
| `STEGO_KAFKA_CONSUMER_GROUP` | The consumer group. The default is `<service>.<subscription>`. |
| `STEGO_KAFKA_CONSUMER_ACCEPT_INFINITE_RETENTION` | `1` to read a topic with an infinite retention. |

The platform must let only the source service write the topic, because the
inbox trusts the header `stego-tenant` only for this reason.
