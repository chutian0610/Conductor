// Package protocol defines the wire types shared across Conductor
// modules. These are the canonical shapes for:
//
//   - agent specs and provider configuration
//   - events streamed from a running agent session
//   - workflow run state and stage outputs
//   - references to on-disk and remote resources
//
// The package is intentionally JSON-friendly (all fields are
// exported and use primitive types or types that marshal as JSON).
// A future Phase 2 may add JSON-Schema codegen from these structs.
package protocol
