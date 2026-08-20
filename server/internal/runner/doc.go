// Package runner is the glue between spec storage and the codex
// provider. One Invoke call = one spec invocation = one Codex
// Session = one prompt → one AgentTurnResult.
//
// What Invoke does NOT do in Phase 1:
//
//   - Persist timeline / run state to disk (that's internal/storage,
//     a separate package — Phase 1 step 8).
//   - Resume a previous session by content (the CLI takes a
//     --resume flag, but only the simple "thread/resume by id"
//     path is wired up).
//   - Run multiple turns against the same Session. Each Invoke
//     creates a fresh Session; multi-turn conversation goes via
//     Phase 2's runner state machine.
//
// Phase 1 is the smallest useful surface: load spec, run prompt,
// stream events back, return final result. That's enough to
// exercise every layer end-to-end and unblock the run.go CLI.
package runner
