// Package ledger holds the posting engine: the only package permitted to
// run balance-mutating queries. The RPC layer never touches internal/store
// directly; it goes through the engine.
//
// Ticket #2 lands only the package skeleton; the posting engine itself
// (zero-sum validation, precision math, pending/posted/voided transitions)
// arrives with the posting-engine ticket.
package ledger
