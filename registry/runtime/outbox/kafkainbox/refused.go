package kafkainbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Schema and Table name the table of the refused records: refused in the schema stego_inbox of the service database.
const (
	Schema = "stego_inbox"
	Table  = "refused"
)

// RefusedRetention is how long a refused record stays in the table before the purge timer of the outbox removes it
// (ST-SUP-RP01-LC37).
const RefusedRetention = 30 * 24 * time.Hour

// The fixed reasons of a refused record (ST-SUP-RP01-LC36). A row holds one of them and no text of the record.
const (
	ReasonMissingHeader    = "missing-header"
	ReasonBadEventID       = "bad-event-id"
	ReasonBadKind          = "bad-kind"
	ReasonForeignKind      = "foreign-kind"
	ReasonBadContentType   = "bad-content-type"
	ReasonTooLarge         = "too-large"
	ReasonNotJSON          = "not-json"
	ReasonBadTenant        = "bad-tenant"
	ReasonKeyMismatch      = "key-mismatch"
	ReasonUnknownTenant    = "unknown-tenant"
	ReasonRetiredTenant    = "retired-tenant"
	ReasonBadResourceKey   = "bad-resource-key"
	ReasonDuplicateHeader  = "duplicate-header"
	ReasonNotOutboxMessage = "not-outbox-message"
	// ReasonEventIDReused is a record whose event ID the inbox holds with another kind or payload: the first stays.
	ReasonEventIDReused = "event-id-reused"
	// ReasonKindMismatch is a record whose payload names another entity or action than its kind.
	ReasonKindMismatch = "kind-mismatch"
)

// reasons are the fixed reasons that the table takes.
var reasons = []string{ReasonMissingHeader, ReasonBadEventID, ReasonBadKind, ReasonForeignKind, ReasonBadContentType,
	ReasonTooLarge, ReasonNotJSON, ReasonBadTenant, ReasonKeyMismatch, ReasonUnknownTenant, ReasonRetiredTenant,
	ReasonBadResourceKey, ReasonDuplicateHeader, ReasonNotOutboxMessage, ReasonEventIDReused, ReasonKindMismatch}

// roleName is a role name that the migration text takes: a lower-case SQL name of at most 63 characters.
var roleName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Migration gives the statements of the table of the refused records for workerRole, the role that the worker mode
// connects as (ST-SUP-RP01-LC37). The owner role of the migration job runs them, so it owns the schema and the
// table. The worker role gets USAGE on the schema and SELECT, INSERT and DELETE on the table, for the write of a
// refused record and its purge, and no UPDATE or TRUNCATE; the public role gets nothing. The table has no tenant
// wall: a refused record can name no tenant of the tenant list, so only the worker role and the owner read it. Each
// statement runs again with no error on a database that it made.
func Migration(workerRole string) (string, error) {
	if !roleName.MatchString(workerRole) {
		return "", errors.New("kafkainbox: the worker role is not a lower-case SQL name of at most 63 characters")
	}
	role := `"` + workerRole + `"`
	table := Schema + "." + Table
	quoted := make([]string, len(reasons))
	for i, r := range reasons {
		quoted[i] = "'" + r + "'"
	}
	return strings.Join([]string{
		"CREATE SCHEMA IF NOT EXISTS " + Schema,
		"REVOKE ALL ON SCHEMA " + Schema + " FROM PUBLIC",
		"CREATE TABLE IF NOT EXISTS " + table + ` (
	subscription  text        NOT NULL CHECK (subscription ~ '^[a-z][a-z0-9-]{0,62}$'),
	topic         text        NOT NULL CHECK (octet_length(topic) BETWEEN 1 AND 249),
	partition     integer     NOT NULL CHECK (partition >= 0),
	record_offset bigint      NOT NULL CHECK (record_offset >= 0),
	reason        text        NOT NULL CHECK (reason IN (` + strings.Join(quoted, ", ") + `)),
	event_id      uuid,
	tenant_id     text        CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
	kind          text        CHECK (kind ~ '^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$'),
	refused_at    timestamptz NOT NULL DEFAULT pg_catalog.now(),
	PRIMARY KEY (subscription, topic, partition, record_offset)
)`,
		"CREATE INDEX IF NOT EXISTS refused_age ON " + table + " (refused_at)",
		"REVOKE ALL ON TABLE " + table + " FROM PUBLIC",
		"REVOKE ALL ON TABLE " + table + " FROM " + role,
		"GRANT USAGE ON SCHEMA " + Schema + " TO " + role,
		"GRANT SELECT, INSERT, DELETE ON TABLE " + table + " TO " + role,
	}, ";\n") + ";\n", nil
}

// Refusal is one refused record: where it is in Kafka, the fixed reason, and the event ID, the tenant ID and the kind
// only when each passed its form check.
type Refusal struct {
	Subscription, Topic string
	Partition           int32
	Offset              int64
	Reason              string
	EventID, Tenant     string
	Kind                string
}

// nullable gives NULL for an empty text.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// storeRefusal writes r in a transaction of its own on db. A redelivery of the same record writes nothing new.
func storeRefusal(ctx context.Context, db *sql.DB, r Refusal) error {
	_, err := db.ExecContext(ctx, "INSERT INTO "+Schema+"."+Table+
		" (subscription, topic, partition, record_offset, reason, event_id, tenant_id, kind) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)"+
		" ON CONFLICT (subscription, topic, partition, record_offset) DO NOTHING",
		r.Subscription, r.Topic, r.Partition, r.Offset, r.Reason, nullable(r.EventID), nullable(r.Tenant), nullable(r.Kind))
	if err != nil {
		return fmt.Errorf("kafkainbox: write the refused record: %w", err)
	}
	return nil
}

// purgeRefused removes at most limit refused records older than retention, the oldest first.
func purgeRefused(ctx context.Context, db *sql.DB, limit int, retention time.Duration) (int64, error) {
	result, err := db.ExecContext(ctx, "DELETE FROM "+Schema+"."+Table+" WHERE ctid IN (SELECT ctid FROM "+Schema+"."+Table+
		" WHERE refused_at < pg_catalog.now() - $1 * interval '1 millisecond' ORDER BY refused_at LIMIT $2)",
		retention.Milliseconds(), limit)
	if err != nil {
		return 0, fmt.Errorf("kafkainbox: purge the refused records: %w", err)
	}
	return result.RowsAffected()
}
