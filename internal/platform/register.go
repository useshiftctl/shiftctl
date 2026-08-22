package platform

import (
	"github.com/shiftctl/shiftctl/internal/platform/azuredevops"
	"github.com/shiftctl/shiftctl/internal/platform/bitbucket"
	"github.com/shiftctl/shiftctl/internal/platform/github"
	"github.com/shiftctl/shiftctl/internal/platform/gitlab"
)

// GitHub returns the GitHub Platform implementation.
func GitHub() Platform { return github.New() }

// GitLab returns the GitLab Platform implementation.
func GitLab() Platform { return gitlab.New() }

// Bitbucket returns the Bitbucket Platform implementation.
func Bitbucket() Platform { return bitbucket.New() }

// AzureDevOps returns the Azure DevOps Platform implementation.
func AzureDevOps() Platform { return azuredevops.New() }
