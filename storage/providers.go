package storage

import (
	"context"
	"errors"
	"os"
)

const GitHubTokenEnvironment = "SYNTROPH_GITHUB_TOKEN"

// CredentialProvider is deliberately opaque: adapters receive an authenticated
// client while credentials remain owned by the selected transport.
type CredentialProvider interface {
	Client(context.Context) (GitHubClient, error)
}

type GitHubTokenProvider struct {
	Factory func(string) (GitHubClient, error)
}

func (p GitHubTokenProvider) Client(ctx context.Context) (GitHubClient, error) {
	if p.Factory == nil {
		return nil, errors.New("GitHub token provider requires a client factory")
	}
	token := os.Getenv(GitHubTokenEnvironment)
	if token == "" {
		return nil, errors.New("SYNTROPH_GITHUB_TOKEN is empty")
	}
	return p.Factory(token)
}

type MCPProvider struct {
	ClientFactory func(context.Context) (GitHubClient, error)
}

func (p MCPProvider) Client(ctx context.Context) (GitHubClient, error) {
	if p.ClientFactory == nil {
		return nil, errors.New("mcp provider requires a client factory")
	}
	return p.ClientFactory(ctx)
}
