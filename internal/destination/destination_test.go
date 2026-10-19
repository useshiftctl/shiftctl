package destination_test

import (
	"testing"

	"github.com/useshiftctl/shiftctl/internal/destination"
)

func TestForName(t *testing.T) {
	tests := []struct {
		input       string
		wantID      string
		wantNil     bool
	}{
		{"GitLab", "gitlab", false},
		{"gitlab", "gitlab", false},
		{"GITLAB", "gitlab", false},
		{"  GitLab  ", "gitlab", false},
		{"GitHub", "github", false},
		{"github", "github", false},
		{"GITHUB", "github", false},
		{"Bitbucket", "bitbucket", false},
		{"bitbucket", "bitbucket", false},
		{"Azure DevOps", "azuredevops", false},
		{"azure devops", "azuredevops", false},
		{"AzureDevOps", "azuredevops", false},
		{"ado", "azuredevops", false},
		{"ADO", "azuredevops", false},
		{"", "", true},
		{"unknown", "", true},
		{"Heroku", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := destination.ForName(tt.input)
			if tt.wantNil {
				if got != nil {
					t.Errorf("ForName(%q) = %v, want nil", tt.input, got.ID())
				}
				return
			}
			if got == nil {
				t.Fatalf("ForName(%q) = nil, want %q", tt.input, tt.wantID)
			}
			if got.ID() != tt.wantID {
				t.Errorf("ForName(%q).ID() = %q, want %q", tt.input, got.ID(), tt.wantID)
			}
		})
	}
}
