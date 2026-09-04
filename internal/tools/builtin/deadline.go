package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/shreyasprasad/agentd/internal/tools"
)

// DeadlineName is the date-math tool's name.
const DeadlineName = "compute_deadline"

// ComputeDeadline adds a number of days to a date, optionally counting only
// weekdays. It exists so the M1 loop has a real, deterministic tool with
// arguments worth validating; the sandboxed python tool in M2 subsumes it for
// anything more elaborate (court holidays, jurisdiction rules).
type ComputeDeadline struct{}

// DeadlineArgs is the tool input.
type DeadlineArgs struct {
	StartDate    string `json:"start_date"`
	Days         int    `json:"days"`
	SkipWeekends bool   `json:"skip_weekends"`
}

// DeadlineResult is the tool output.
type DeadlineResult struct {
	StartDate       string `json:"start_date"`
	Days            int    `json:"days"`
	SkipWeekends    bool   `json:"skip_weekends"`
	Deadline        string `json:"deadline"`
	DeadlineWeekday string `json:"deadline_weekday"`
	WeekendsSkipped int    `json:"weekend_days_skipped"`
}

func (ComputeDeadline) Name() string { return DeadlineName }

func (ComputeDeadline) Description() string {
	return "Compute a deadline by adding `days` to `start_date` (YYYY-MM-DD). " +
		"Set `skip_weekends` to count only Monday through Friday. Negative `days` counts backward. " +
		"Returns the resulting date and its weekday."
}

func (ComputeDeadline) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "start_date":    {"type": "string", "pattern": "^\\d{4}-\\d{2}-\\d{2}$", "description": "Start date, YYYY-MM-DD."},
    "days":          {"type": "integer", "minimum": -3650, "maximum": 3650, "description": "Days to add; negative counts backward."},
    "skip_weekends": {"type": "boolean", "description": "Count only weekdays.", "default": false}
  },
  "required": ["start_date", "days"],
  "additionalProperties": false
}`)
}

func (ComputeDeadline) TrustTier() tools.TrustTier { return tools.Builtin }

func (ComputeDeadline) Invoke(_ context.Context, inv tools.Invocation) (tools.Result, error) {
	var args DeadlineArgs
	if err := json.Unmarshal(inv.Args, &args); err != nil {
		return tools.Result{}, err
	}
	start, err := time.Parse("2006-01-02", args.StartDate)
	if err != nil {
		return tools.Result{}, fmt.Errorf("start_date: %w", err)
	}

	res := DeadlineResult{StartDate: args.StartDate, Days: args.Days, SkipWeekends: args.SkipWeekends}
	d := computeDeadline(start, args.Days, args.SkipWeekends, &res.WeekendsSkipped)
	res.Deadline = d.Format("2006-01-02")
	res.DeadlineWeekday = d.Weekday().String()

	out, err := json.Marshal(res)
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Content: out}, nil
}

func computeDeadline(start time.Time, days int, skipWeekends bool, skipped *int) time.Time {
	if !skipWeekends {
		return start.AddDate(0, 0, days)
	}
	step := 1
	if days < 0 {
		step, days = -1, -days
	}
	d := start
	for counted := 0; counted < days; {
		d = d.AddDate(0, 0, step)
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			*skipped++
			continue
		}
		counted++
	}
	return d
}
