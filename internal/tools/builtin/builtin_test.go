package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/shreyasprasad/agentd/internal/tools"
)

func TestComputeDeadline(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		want    DeadlineResult
		wantErr bool
	}{
		{
			name: "calendar days",
			args: `{"start_date":"2026-09-03","days":30}`,
			want: DeadlineResult{StartDate: "2026-09-03", Days: 30, Deadline: "2026-10-03", DeadlineWeekday: "Saturday"},
		},
		{
			name: "weekdays only",
			// 2026-09-03 is a Thursday. 30 weekdays later is 2026-10-15 (Thursday), crossing six weekends.
			args: `{"start_date":"2026-09-03","days":30,"skip_weekends":true}`,
			want: DeadlineResult{StartDate: "2026-09-03", Days: 30, SkipWeekends: true, Deadline: "2026-10-15", DeadlineWeekday: "Thursday", WeekendsSkipped: 12},
		},
		{
			name: "weekdays backward across a weekend",
			// Monday 2026-09-07 minus 1 weekday is Friday 2026-09-04.
			args: `{"start_date":"2026-09-07","days":-1,"skip_weekends":true}`,
			want: DeadlineResult{StartDate: "2026-09-07", Days: -1, SkipWeekends: true, Deadline: "2026-09-04", DeadlineWeekday: "Friday", WeekendsSkipped: 2},
		},
		{
			name: "zero days",
			args: `{"start_date":"2026-09-05","days":0,"skip_weekends":true}`,
			want: DeadlineResult{StartDate: "2026-09-05", SkipWeekends: true, Deadline: "2026-09-05", DeadlineWeekday: "Saturday"},
		},
		{name: "bad date", args: `{"start_date":"2026-13-40","days":1}`, wantErr: true},
		{name: "not json", args: `nope`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ComputeDeadline{}.Invoke(context.Background(), tools.Invocation{Args: json.RawMessage(tc.args)})
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got DeadlineResult
			if err := json.Unmarshal(res.Content, &got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %+v\nwant %+v", got, tc.want)
			}
			if res.Terminal {
				t.Fatal("deadline must not be terminal")
			}
		})
	}
}

func TestFinish(t *testing.T) {
	res, err := Finish{}.Invoke(context.Background(), tools.Invocation{Args: json.RawMessage(`{"answer":"42"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Terminal {
		t.Fatal("finish must be terminal")
	}
	if got := FinalAnswer(res.Content); got != "42" {
		t.Fatalf("FinalAnswer = %q", got)
	}
	if _, err := (Finish{}).Invoke(context.Background(), tools.Invocation{Args: json.RawMessage(`{"answer":""}`)}); err == nil {
		t.Fatal("empty answer should error")
	}
}

func TestBuiltinsRegister(t *testing.T) {
	r := tools.NewRegistry()
	if err := r.Register(Finish{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(ComputeDeadline{}); err != nil {
		t.Fatal(err)
	}
	if got := r.Names(); len(got) != 2 || got[0] != DeadlineName || got[1] != FinishName {
		t.Fatalf("Names = %v", got)
	}
}
