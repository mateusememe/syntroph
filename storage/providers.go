package storage

import (
	"context"
	"errors"
	"os"
)

type Provider string

const (
	ProviderEnvToken Provider = "env-token"
	ProviderMCP      Provider = "mcp"
)

// CredentialProvider is deliberately opaque: adapters receive an authenticated
// client, while this boundary lets configuration select env-token or MCP.
type CredentialProvider interface {
	Client(context.Context) (GitHubClient, error)
}

type EnvTokenProvider struct {
	TokenEnv string
	Factory  func(string) (GitHubClient, error)
}

func (p EnvTokenProvider) Client(ctx context.Context) (GitHubClient, error) {
	if p.TokenEnv == "" || p.Factory == nil {
		return nil, errors.New("env-token provider requires token_env and factory")
	}
	token := os.Getenv(p.TokenEnv)
	if token == "" {
		return nil, errors.New("configured GitHub token environment variable is empty")
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
