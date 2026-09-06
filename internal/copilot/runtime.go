package copilot

import (
	"context"
	"fmt"

	sdk "github.com/github/copilot-sdk/go"
)

type skillInfo struct {
	Name   string
	Path   string
	Source string
}

type runtimeFactory interface {
	Start(context.Context, *sdk.ClientOptions) (runtimeClient, error)
}

type runtimeClient interface {
	Version(context.Context) (string, error)
	SubscriptionAuthenticated(context.Context) (bool, error)
	CreateSession(context.Context, *sdk.SessionConfig) (runtimeSession, error)
	Stop() error
}

type runtimeSession interface {
	LoadedSkills(context.Context) ([]skillInfo, error)
	InvokedSkills(context.Context) ([]string, error)
	SendAndWait(context.Context, sdk.MessageOptions) (*sdk.SessionEvent, error)
	Abort(context.Context) error
	Disconnect() error
}

type sdkRuntimeFactory struct{}

func (sdkRuntimeFactory) Start(ctx context.Context, options *sdk.ClientOptions) (runtimeClient, error) {
	client := sdk.NewClient(options)
	if err := client.Start(ctx); err != nil {
		return nil, err
	}
	return &sdkRuntimeClient{client: client}, nil
}

type sdkRuntimeClient struct {
	client *sdk.Client
}

func (r *sdkRuntimeClient) Version(ctx context.Context) (string, error) {
	status, err := r.client.GetStatus(ctx)
	if err != nil {
		return "", err
	}
	return status.Version, nil
}

func (r *sdkRuntimeClient) SubscriptionAuthenticated(ctx context.Context) (bool, error) {
	status, err := r.client.GetAuthStatus(ctx)
	if err != nil {
		return false, err
	}
	return status.IsAuthenticated, nil
}

func (r *sdkRuntimeClient) CreateSession(ctx context.Context, config *sdk.SessionConfig) (runtimeSession, error) {
	session, err := r.client.CreateSession(ctx, config)
	if err != nil {
		return nil, err
	}
	return &sdkRuntimeSession{session: session}, nil
}

func (r *sdkRuntimeClient) Stop() error { return r.client.Stop() }

type sdkRuntimeSession struct {
	session *sdk.Session
}

func (s *sdkRuntimeSession) LoadedSkills(ctx context.Context) ([]skillInfo, error) {
	if _, err := s.session.RPC.Skills.EnsureLoaded(ctx); err != nil {
		return nil, err
	}
	result, err := s.session.RPC.Skills.List(ctx)
	if err != nil {
		return nil, err
	}
	skills := make([]skillInfo, 0, len(result.Skills))
	for _, skill := range result.Skills {
		path := ""
		if skill.Path != nil {
			path = *skill.Path
		}
		skills = append(skills, skillInfo{Name: skill.Name, Path: path, Source: string(skill.Source)})
	}
	return skills, nil
}

func (s *sdkRuntimeSession) InvokedSkills(ctx context.Context) ([]string, error) {
	result, err := s.session.RPC.Skills.GetInvoked(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(result.Skills))
	for _, skill := range result.Skills {
		if skill.Name == "" {
			return nil, fmt.Errorf("runtime returned an invoked skill without a name")
		}
		names = append(names, skill.Name)
	}
	return names, nil
}

func (s *sdkRuntimeSession) SendAndWait(ctx context.Context, options sdk.MessageOptions) (*sdk.SessionEvent, error) {
	return s.session.SendAndWait(ctx, options)
}

func (s *sdkRuntimeSession) Abort(ctx context.Context) error { return s.session.Abort(ctx) }

func (s *sdkRuntimeSession) Disconnect() error { return s.session.Disconnect() }
