package tenant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// MaxGateAge is the oldest answer of the tenant list that a Gate gives from its cache (ST-SUP-RC27-OC11).
const MaxGateAge = 5 * time.Second

// MaxGateEntries is the most tenants that the cache of a Gate holds. When it is full, the next lookup that misses
// clears it, so the claims of many tenants cannot grow the memory of a service without a bound.
const MaxGateEntries = 10000

// ErrNotProvisionedCode is the fixed code of a request whose verified claim names a tenant that the tenant list does
// not hold, or holds as retired (ST-SUP-RC27-OC11).
const ErrNotProvisionedCode = "tenant.not_provisioned"

// gateEntry is one cached answer of the tenant list, or only the generation of a tenant whose answer Forget dropped
// (cached false). Each Forget of the tenant increments generation, so a lookup that read the list before the Forget
// does not store its answer after it.
type gateEntry struct {
	active     bool
	cached     bool
	at         time.Time
	generation uint64
}

// Gate tells whether a tenant is an active tenant of the tenant list stego_tenants of a schema: a tenant that the
// list holds with no retire time (ST-SUP-RC27-OC11, ST-SUP-RP04-LC07). It reads the list as the role of its pool, the
// runtime role in a service, and keeps each answer for at most its age, so a Provision or a Retire on another
// replica takes effect within that age. It caches no error.
type Gate struct {
	db     *sql.DB
	query  string
	maxAge time.Duration
	now    func() time.Time
	// read, when set by a test, runs after each read of the list and before its answer is stored.
	read func(id string)

	mu      sync.Mutex
	entries map[string]gateEntry
	// epoch counts the clears of entries, which drop the generations too, so a lookup in flight across a clear does
	// not store its answer.
	epoch uint64
}

// NewGate returns the gate of the tenant list of schema on db, whose cached answers are at most maxAge old. It
// refuses no database, a schema that quoteName refuses, and an age that is not positive or is above MaxGateAge.
func NewGate(db *sql.DB, schema string, maxAge time.Duration) (*Gate, error) {
	if db == nil {
		return nil, errors.New("tenant: the gate needs a database")
	}
	if maxAge <= 0 || maxAge > MaxGateAge {
		return nil, fmt.Errorf("tenant: the age of the gate's answers must be more than 0 and at most %s", MaxGateAge)
	}
	quotedSchema, err := quoteName("schema", schema)
	if err != nil {
		return nil, err
	}
	return &Gate{db: db, maxAge: maxAge, now: time.Now, entries: map[string]gateEntry{},
		query: "SELECT retired_at IS NULL FROM " + quotedSchema + ".stego_tenants WHERE tenant_id = $1"}, nil
}

// Active reports whether t is an active tenant of the tenant list: one that the list holds with no retire time. It
// refuses the zero tenant. A failed read is an error, which the caller refuses, so the gate fails closed.
func (g *Gate) Active(ctx context.Context, t Tenant) (bool, error) {
	if t.IsZero() {
		return false, ErrZero
	}
	now := g.now()
	g.mu.Lock()
	entry, ok := g.entries[t.id]
	epoch := g.epoch
	g.mu.Unlock()
	if ok && entry.cached && now.Sub(entry.at) >= 0 && now.Sub(entry.at) < g.maxAge {
		return entry.active, nil
	}
	var active bool
	err := g.db.QueryRowContext(ctx, g.query, t.id).Scan(&active)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		active = false
	case err != nil:
		return false, fmt.Errorf("tenant: read the tenant list for %s: %w", t.id, err)
	}
	if g.read != nil {
		g.read(t.id)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// The answer is stored only when no Forget of the tenant and no clear came after the lookup began: the read may
	// be older than the change that the Forget follows.
	if current := g.entries[t.id]; g.epoch != epoch || current.generation != entry.generation {
		return active, nil
	}
	if _, held := g.entries[t.id]; !held && len(g.entries) >= MaxGateEntries {
		clear(g.entries)
		g.epoch++
	}
	g.entries[t.id] = gateEntry{active: active, cached: true, at: now, generation: entry.generation}
	return active, nil
}

// Forget drops the cached answer of the tenant ID id and increments its generation, so the next lookup reads the
// list, and a lookup that began before Forget, whose read may be older than the change, gives its answer but does
// not store it. Provision and Retire call it after their commit, so on the replica that served them no lookup after
// the commit answers from a read before it. Another replica answers from its cache for at most MaxGateAge.
func (g *Gate) Forget(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry, held := g.entries[id]
	if !held && len(g.entries) >= MaxGateEntries {
		clear(g.entries)
		g.epoch++
	}
	g.entries[id] = gateEntry{generation: entry.generation + 1}
}

// ValidID checks a tenant ID by the rule of New and of the CHECK of stego_tenants: from 1 to 255 bytes of valid
// UTF-8, and no character of the Unicode general categories Cc, Cf, Zl or Zp (ST-SUP-RP04-LC07). A Provision or
// Retire request checks the ID that it names with it before any decision, so a malformed ID reaches no provider and
// no statement.
func ValidID(id string) error { return validID(id) }
