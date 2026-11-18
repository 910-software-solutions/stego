package outbox

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// Resolver resolves parked messages in a process that does not deliver them, such as the api mode of a service,
// which serves the resolve route of an operator (ST-SUP-RP14-LC10). It holds the decision port, the auditor and the
// state hook of each destination that has one, and no handler and no claim: it never delivers a message. It
// resolves by the rules that the row of the message recorded at enqueue, must land and idempotent, which are the
// rules that the relay function resolve holds too.
type Resolver struct {
	queue   *Queue
	decider Decider
	auditor Auditor
	hooks   map[string]StateHook
}

// NewResolver returns the resolver on queue with the decision port decider, the auditor auditor and the state hook
// of each destination that has one, by name. It refuses a missing part and a hook of a name that is not a label.
func NewResolver(queue *Queue, decider Decider, auditor Auditor, hooks map[string]StateHook) (*Resolver, error) {
	switch {
	case queue == nil || queue.db == nil:
		return nil, errors.New("outbox: the resolver needs a queue")
	case decider == nil:
		return nil, errors.New("outbox: the resolver needs a decision port")
	case auditor == nil:
		return nil, errors.New("outbox: the resolver needs an auditor")
	}
	own := make(map[string]StateHook, len(hooks))
	for name, hook := range hooks {
		if !labelPattern.MatchString(name) || hook == nil {
			return nil, errors.New("outbox: a state hook of the resolver needs a label name and a function")
		}
		own[name] = hook
	}
	return &Resolver{queue: queue, decider: decider, auditor: auditor, hooks: own}, nil
}

// Resolve applies a person's resolution of one parked message in the caller's transaction tx, as Worker.Resolve
// does (ST-SUP-RP01-LC13, ST-SUP-RP01-LC17): it asks the decision port before it reads the message, so a deny is
// ErrNotAllowed for every message; then ErrNotFound for a message that the tenant of tx does not hold under the
// destination, ErrNotParked for a message that is not parked, and an error that matches ErrRefusedOutcome for an
// outcome that the destination refuses. The state hook of the destination and the audit row are written in tx with
// the state change. The caller commits tx with tenant.Commit.
func (r *Resolver) Resolve(ctx context.Context, tx *sql.Tx, request ResolveRequest) error {
	rules := func(destination string, row resolveRow) (resolveRules, error) {
		return resolveRules{mustLand: row.mustLand, idempotent: row.idempotent, hook: r.hooks[destination]}, nil
	}
	_, _, err := resolveCore(ctx, tx, request, r.decider, r.auditor, r.queue.now(), rules, nil)
	return err
}

// Allowed asks the decision port whether principal may resolve the messages of destination in tenant t, as
// Resolve does first (ST-SUP-RP01-LC17), for a caller that lists the parked messages of a destination
// (ST-SUP-RP14-LC09). A deny is false with no error; a destination name that is not a label is a deny.
func (r *Resolver) Allowed(ctx context.Context, t tenant.Tenant, principal Principal, destination string) (bool, error) {
	if !labelPattern.MatchString(destination) {
		return false, nil
	}
	subject, ok := seal.SubjectOf(principal)
	if !ok || !validText(subject, MaxKeyBytes) || t.IsZero() {
		return false, errors.New("outbox: a decision needs a principal of the authorization runtime and a tenant")
	}
	return r.decider.Decide(ctx, DecisionRequest{Subject: subject, Permission: "resolve", Resource: resource + destination,
		Tenant: t})
}

// MaxParkedPage is the most messages of one page of the parked messages of a destination.
const MaxParkedPage = 1000

// ParkedMessage is one parked message of a destination as an operator sees it (ST-SUP-RP14-LC09): its ID, its
// destination, its failure code, its attempt count and the times it was made and parked, and never its payload, its
// tenant data or an error text. Position is its place in the queue, after which the next page starts.
type ParkedMessage struct {
	ID          uuid.UUID
	Destination string
	FailureCode string
	Attempts    int64
	CreatedAt   time.Time
	ParkedAt    time.Time
	Position    int64
}

// Parked lists at most limit parked messages of destination in the tenant of tx, in the order of the queue, after
// the position after (0 is the start), and tells whether more follow. It runs under the tenant policy in the
// caller's transaction, which tenant.Begin opened, so it lists no message of another tenant (ST-SUP-RP01-LC19).
func (q *Queue) Parked(ctx context.Context, tx *sql.Tx, destination string, after int64, limit int) ([]ParkedMessage, bool, error) {
	switch {
	case tx == nil:
		return nil, false, errors.New("outbox: Parked needs a transaction of one tenant")
	case limit < 1 || limit > MaxParkedPage || after < 0:
		return nil, false, errors.New("outbox: the page of the parked messages is out of its bounds")
	case !labelPattern.MatchString(destination):
		return nil, false, errors.New("outbox: the destination name is not a label")
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	rows, err := tx.QueryContext(ctx, `SELECT id, destination, failure_code, attempts, created_at, parked_at, sequence
		FROM stego_outbox.messages WHERE state = 'parked' AND destination = $1 AND sequence > $2
		ORDER BY sequence LIMIT $3`, destination, after, limit+1)
	if err != nil {
		return nil, false, canceled(ctx, err)
	}
	defer rows.Close()
	var page []ParkedMessage
	for rows.Next() {
		var m ParkedMessage
		if err := rows.Scan(&m.ID, &m.Destination, &m.FailureCode, &m.Attempts, &m.CreatedAt, &m.ParkedAt, &m.Position); err != nil {
			return nil, false, canceled(ctx, err)
		}
		page = append(page, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, canceled(ctx, err)
	}
	if len(page) > limit {
		return page[:limit], true, nil
	}
	return page, false, nil
}
