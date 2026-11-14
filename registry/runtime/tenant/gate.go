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

// gateEntry is one cached answer of the tenant list.
type gateEntry struct {
	active bool
	at     time.Time
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

	mu      sync.Mutex
	entries map[string]gateEntry
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
	g.mu.Unlock()
	if ok && now.Sub(entry.at) >= 0 && now.Sub(entry.at) < g.maxAge {
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
	g.mu.Lock()
	if _, held := g.entries[t.id]; !held && len(g.entries) >= MaxGateEntries {
		clear(g.entries)
	}
	g.entries[t.id] = gateEntry{active: active, at: now}
	g.mu.Unlock()
	return active, nil
}

// Forget drops the cached answer of the tenant ID id, so the next lookup reads the list. Provision and Retire call it
// after their commit, so the replica that served them answers from the list at once.
func (g *Gate) Forget(id string) {
	g.mu.Lock()
	delete(g.entries, id)
	g.mu.Unlock()
}

// ValidID checks a tenant ID by the rule of New and of the CHECK of stego_tenants: from 1 to 255 bytes of valid
// UTF-8, and no character of the Unicode general categories Cc, Cf, Zl or Zp (ST-SUP-RP04-LC07). A Provision or
// Retire request checks the ID that it names with it before any decision, so a malformed ID reaches no provider and
// no statement.
func ValidID(id string) error { return validID(id) }
