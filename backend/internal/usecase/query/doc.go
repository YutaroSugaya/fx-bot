// Package query holds read-only usecases (CQRS Query side).
//
// Each query:
//   - Has a single Execute method
//   - Returns a typed Output (View DTO) + error
//   - Does NOT modify DB / broker state
//   - May invoke external read-only operations (file read, CLI for Q&A, etc.)
//
// See /docs/architecture/layers/usecase.md for the full CQRS contract.
package query
