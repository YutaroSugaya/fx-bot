// Package handler holds HTTP handlers split by resource.
//
// Each *Handler is a struct with:
//   - dependencies as fields (port interfaces / command-query usecases / Logger)
//   - public method(s) implementing http.HandlerFunc
//   - companion <name>_test.go using httptest
//
// Routes are registered by the parent app/APIServer behind its BasicAuth
// (newAuthMiddleware), CORS and CSRF middleware. Handlers do not know about
// auth — that's APIServer's job.
//
// See /docs/architecture/layers/handler.md for the layering contract.
package handler
