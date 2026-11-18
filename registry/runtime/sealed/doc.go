// Package sealed is the sealing runtime of the runtime library (ST-SUP-RP16): the key ring of the secret fields of a
// service, the sealed form of a readable secret, which is AES-256-GCM with the place of the value as its associated
// data, the Argon2id hash of a compare-only secret and its check in constant time, and the Secret and Compared values,
// which never print, log or encode their text (decision ST-DECL-Q44).
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code and the command runtime call its functions
// and types, and the package calls back into no generated code and owns no main loop.
package sealed
