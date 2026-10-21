package server

import (
	"testing"

	"github.com/useshiftctl/shiftctl/internal/execstate"
)

func makeState(stages []execstate.StageState) *execstate.State {
	return &execstate.State{
		MigrationName: "test",
		Stages:        stages,
	}
}

func TestDeriveScreen(t *testing.T) {
	tests := []struct {
		name        string
		stages      []execstate.StageState
		wantScreen  string
		wantStage   int
	}{
		{
			// With zero stages the "all verified" loop is vacuously true. decommission.
			// In practice deriveScreen is never called with an empty stage list.
			name:       "no stages. vacuously all verified",
			stages:     []execstate.StageState{},
			wantScreen: "decommission",
			wantStage:  0,
		},
		{
			name: "stage 1 pending. approval",
			stages: []execstate.StageState{
				{Number: 1, Status: execstate.StatusPending},
			},
			wantScreen: "approval",
			wantStage:  1,
		},
		{
			name: "stage 1 approved. execution",
			stages: []execstate.StageState{
				{Number: 1, Status: execstate.StatusApproved},
			},
			wantScreen: "execution",
			wantStage:  1,
		},
		{
			name: "stage 1 executing. execution",
			stages: []execstate.StageState{
				{Number: 1, Status: execstate.StatusExecuting},
			},
			wantScreen: "execution",
			wantStage:  1,
		},
		{
			name: "stage 1 interrupted. execution",
			stages: []execstate.StageState{
				{Number: 1, Status: execstate.StatusInterrupted},
			},
			wantScreen: "execution",
			wantStage:  1,
		},
		{
			name: "stage 1 verifying. verifying",
			stages: []execstate.StageState{
				{Number: 1, Status: execstate.StatusVerifying},
			},
			wantScreen: "verifying",
			wantStage:  1,
		},
		{
			name: "stage 1 verified stage 2 pending. approval for stage 2",
			stages: []execstate.StageState{
				{Number: 1, Status: execstate.StatusVerified},
				{Number: 2, Status: execstate.StatusPending},
			},
			wantScreen: "approval",
			wantStage:  2,
		},
		{
			name: "stage 1 pending stage 2 pending. approval for stage 1 (sequential)",
			stages: []execstate.StageState{
				{Number: 1, Status: execstate.StatusPending},
				{Number: 2, Status: execstate.StatusPending},
			},
			wantScreen: "approval",
			wantStage:  1,
		},
		{
			name: "all stages verified. decommission",
			stages: []execstate.StageState{
				{Number: 1, Status: execstate.StatusVerified},
				{Number: 2, Status: execstate.StatusVerified},
				{Number: 3, Status: execstate.StatusVerified},
			},
			wantScreen: "decommission",
			wantStage:  0,
		},
		{
			name: "stage 1 verified stage 2 approved. execution for stage 2",
			stages: []execstate.StageState{
				{Number: 1, Status: execstate.StatusVerified},
				{Number: 2, Status: execstate.StatusApproved},
			},
			wantScreen: "execution",
			wantStage:  2,
		},
		{
			name: "executing takes priority over pending",
			stages: []execstate.StageState{
				{Number: 1, Status: execstate.StatusExecuting},
				{Number: 2, Status: execstate.StatusPending},
			},
			wantScreen: "execution",
			wantStage:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := makeState(tt.stages)
			gotScreen, gotStage := deriveScreen(s)
			if gotScreen != tt.wantScreen {
				t.Errorf("screen = %q, want %q", gotScreen, tt.wantScreen)
			}
			if gotStage != tt.wantStage {
				t.Errorf("activeStage = %d, want %d", gotStage, tt.wantStage)
			}
		})
	}
}
