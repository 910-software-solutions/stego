// Package txhold gives the packages of the runtime module the transaction behind a tenant.Querier, which a worker
// function or a provisioning hook gets (ST-SUP-RP04-LC07). The Querier hides the *sql.Tx, so the code that a
// service generates cannot commit or roll back a transaction of the tenant runtime; this package is internal to the
// module, so only the runtime's own packages, such as the command runtime, which stages audit rows and outbox
// messages in the transaction, can reach it.
package txhold

import "database/sql"

// of is the accessor that the tenant package registers when it starts.
var of func(q any) (*sql.Tx, bool)

// Register sets the accessor. Only the tenant package calls it, once, from its init.
func Register(accessor func(q any) (*sql.Tx, bool)) {
	if of == nil {
		of = accessor
	}
}

// Of gives the transaction behind q, a tenant.Querier, and false for any other value.
func Of(q any) (*sql.Tx, bool) {
	if of == nil || q == nil {
		return nil, false
	}
	return of(q)
}
