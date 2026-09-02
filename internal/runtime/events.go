package runtime

// Event types written to run_events. The log is the source of truth for run
// state; everything else in the runs table is a derived counter.
const (
	EventRunStarted      = "run_started"
	EventModelRequested  = "model_requested"
	EventModelResponded  = "model_responded"
	EventToolRequested   = "tool_requested"
	EventToolSucceeded   = "tool_succeeded"
	EventToolFailed      = "tool_failed"
	EventBudgetExceeded  = "budget_exceeded"
	EventCancelRequested = "cancel_requested"
	EventRunFinished     = "run_finished"
)

// TerminalEventTypes are the events after which no further events can be
// appended to a run. The SSE handler closes a stream once one arrives.
func IsTerminal(eventType string) bool {
	return eventType == EventRunFinished
}
