// Package kafkainbox is the Kafka inbox of the outbox runtime (ST-SUP-RP01): it reads the change events of another
// service from one Kafka topic in a consumer group, checks each record, and writes each record that it takes into the
// outbox of the service as a message of the destination that one consuming worker consumes, with an ID that a
// redelivery of the record gives again, so the record has its effect once (ST-SUP-RP01-LC35 to ST-SUP-RP01-LC38). It
// commits the offset of a record only after the transaction that wrote the record committed. A record that fails a
// check is kept in the table of refused records with a fixed reason and an alert, and is never applied.
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop beyond Run.
package kafkainbox
