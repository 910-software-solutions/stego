package outbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// Resolver resolves parked messages in a process that does not deliver them, such as the api mode of a service,
// which serves the resolve route of an operator (ST-SUP-RP14-LC10). It holds the decision port, the auditor, the
// declared rules of each destination of the release and the state hook of each destination that has one, and no
// handler and no claim: it never delivers a message. It resolves by the stricter of the rules that the row of the
// message recorded at enqueue and the rules that the release declares, so a release that makes a destination
// stricter also holds the older parked messages of it; it refuses each message of a destination that the release
// does not declare (security review of PR #421, L3).
type Resolver struct {
	queue    *Queue
	decider  Decider
	auditor  Auditor
	declared map[string]Rules
	hooks    map[string]StateHook
}

// Rules are the declared rules of one destination of a release: its name, whether its messages must land and
// whether it is idempotent, as the destination is registered on the outbox worker of the release.
type Rules struct {
	Name       string
	MustLand   bool
	Idempotent bool
}

// NewResolver returns the resolver on queue with the decision port decider, the auditor auditor, the declared rules
// of each destination of the release and the state hook of each destination that has one, by name. It refuses a
// missing part, a rule whose name is not a label or is declared twice, and a hook of a destination that the rules do
// not declare.
func NewResolver(queue *Queue, decider Decider, auditor Auditor, declared []Rules, hooks map[string]StateHook) (*Resolver, error) {
	switch {
	case queue == nil || queue.db == nil:
		return nil, errors.New("outbox: the resolver needs a queue")
	case decider == nil:
		return nil, errors.New("outbox: the resolver needs a decision port")
	case auditor == nil:
		return nil, errors.New("outbox: the resolver needs an auditor")
	}
	rules := make(map[string]Rules, len(declared))
	for _, d := range declared {
		if !labelPattern.MatchString(d.Name) {
			return nil, fmt.Errorf("outbox: the declared destination %q of the resolver is not a label", d.Name)
		}
		if _, ok := rules[d.Name]; ok {
			return nil, fmt.Errorf("outbox: the resolver gets the rules of the destination %s twice", d.Name)
		}
		rules[d.Name] = d
	}
	own := make(map[string]StateHook, len(hooks))
	for name, hook := range hooks {
		if !labelPattern.MatchString(name) || hook == nil {
			return nil, errors.New("outbox: a state hook of the resolver needs a label name and a function")
		}
		if _, ok := rules[name]; !ok {
			return nil, fmt.Errorf("outbox: the resolver gets a state hook of %s, which the rules do not declare", name)
		}
		own[name] = hook
	}
	return &Resolver{queue: queue, decider: decider, auditor: auditor, declared: rules, hooks: own}, nil
}

// stricter gives the rules of a resolution of a message of destination: the stricter of what the row recorded and
// what the release declares. Acted is refused when either is idempotent, and discarded when either need not land
// (ST-SUP-RP01-LC13). A destination that the release does not declare refuses every outcome.
func stricter(destination string, row resolveRow, declared Rules, ok bool) (resolveRules, error) {
	if !ok {
		return resolveRules{}, fmt.Errorf("%w: the release does not declare the destination %s", ErrRefusedOutcome, destination)
	}
	return resolveRules{mustLand: row.mustLand && declared.MustLand, idempotent: row.idempotent || declared.Idempotent}, nil
}

func (r *Resolver) Resolve(ctx context.Context, tx *sql.Tx, request ResolveRequest) error {
	rules := func(destination string, row resolveRow) (resolveRules, error) {
		declared, ok := r.declared[destination]
		rules, err := stricter(destination, row, declared, ok)
		rules.hook = r.hooks[destination]
		return rules, err
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
