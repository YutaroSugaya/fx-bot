// Package command holds state-changing usecases (CQRS Command side).
//
// Each command:
//   - Has a single Execute method
//   - Takes a typed Input struct and returns a typed Output + error
//   - Depends on port interfaces only (no adapter import)
//   - Wraps DB multi-writes in a transaction
//   - Trips emergency_stop via safety.Trip / safety.TripWithDetail on critical
//     failures (network/DB inconsistency that can't be auto-recovered).
//
// See /docs/architecture/layers/usecase.md for the full CQRS contract.
package command
