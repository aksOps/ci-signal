package gitlab

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"ci-signal/internal/config"
	"ci-signal/internal/review"
	gitlabapi "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	defaultRetryMax  = 2
	defaultRetryWait = 250 * time.Millisecond
	maximumRetryWait = 5 * time.Second
)

type clientOptions struct {
	httpClient   *http.Client
	retryMax     int
	retryMin     time.Duration
	retryMaxWait time.Duration
}

type Option func(*clientOptions) error

func WithHTTPClient(client *http.Client) Option {
	return func(options *clientOptions) error {
		if client == nil {
			return errors.New("GitLab HTTP client cannot be nil")
		}
		options.httpClient = client
		return nil
	}
}

func WithRetryPolicy(maximum int, minimumWait, maximumWait time.Duration) Option {
	return func(options *clientOptions) error {
		if maximum < 0 || minimumWait < 0 || maximumWait < minimumWait {
			return errors.New("invalid GitLab retry policy")
		}
		options.retryMax = maximum
		options.retryMin = minimumWait
		options.retryMaxWait = maximumWait
		return nil
	}
}

type Client struct {
	job     *gitlabapi.Client
	api     *gitlabapi.Client
	baseURL string
	project string
	mrIID   int64
	logger  *slog.Logger

	capabilityMu sync.RWMutex
	jobDenied    map[CredentialEndpoint]struct{}
	authorMu     sync.RWMutex
	authors      map[int64]authorLookup
}

type authorLookup struct {
	kind       AuthorKind
	diagnostic ReadDiagnostic
	err        error
}

func New(loaded config.Loaded, logger *slog.Logger, supplied ...Option) (*Client, error) {
	settings := loaded.Config.GitLab
	base, apiPrefix, err := validateTarget(settings)
	if err != nil {
		return nil, err
	}
	apiToken := loaded.Secrets.APIToken.Value()
	if apiToken == "" {
		return nil, errors.New("GitLab API token is required")
	}
	apiTimeout := loaded.Config.Limits.APITimeout.Value()
	if apiTimeout <= 0 {
		return nil, errors.New("positive GitLab API timeout is required")
	}

	options := clientOptions{
		httpClient:   http.DefaultClient,
		retryMax:     defaultRetryMax,
		retryMin:     defaultRetryWait,
		retryMaxWait: maximumRetryWait,
	}
	for _, option := range supplied {
		if option == nil {
			return nil, errors.New("nil GitLab client option")
		}
		if err := option(&options); err != nil {
			return nil, err
		}
	}
	httpClient := guardedHTTPClient(options.httpClient, base, apiPrefix, apiTimeout)
	clientOptions := []gitlabapi.ClientOptionFunc{
		gitlabapi.WithBaseURL(settings.BaseURL),
		gitlabapi.WithHTTPClient(httpClient),
		gitlabapi.WithCustomRetryMax(options.retryMax),
		gitlabapi.WithCustomRetryWaitMinMax(options.retryMin, options.retryMaxWait),
		gitlabapi.WithOnlyIdempotentRetries(),
	}
	apiClient, err := gitlabapi.NewClient(apiToken, clientOptions...)
	if err != nil {
		return nil, fmt.Errorf("create GitLab API-token client: %w", err)
	}
	var jobClient *gitlabapi.Client
	if token := loaded.Secrets.JobToken.Value(); token != "" {
		jobClient, err = gitlabapi.NewJobClient(token, clientOptions...)
		if err != nil {
			return nil, fmt.Errorf("create GitLab job-token client: %w", err)
		}
	}
	return &Client{
		job:       jobClient,
		api:       apiClient,
		baseURL:   strings.TrimSuffix(settings.BaseURL, "/"),
		project:   settings.Project,
		mrIID:     int64(settings.MRIID),
		logger:    logger,
		jobDenied: make(map[CredentialEndpoint]struct{}),
		authors:   make(map[int64]authorLookup),
	}, nil
}

func (c *Client) TargetIdentity() review.MRIdentity {
	return review.MRIdentity{BaseURL: c.baseURL, Project: c.project, MRIID: int(c.mrIID)}
}

func validateTarget(settings config.GitLab) (*url.URL, string, error) {
	base, err := url.Parse(settings.BaseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, "", errors.New("GitLab base URL must be an absolute HTTP or HTTPS URL")
	}
	if base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, "", errors.New("GitLab base URL cannot contain credentials, query, or fragment")
	}
	if strings.TrimSpace(settings.Project) == "" || settings.MRIID < 1 {
		return nil, "", errors.New("GitLab project and positive merge-request IID are required")
	}
	apiPrefix := strings.TrimSuffix(path.Clean("/"+strings.TrimPrefix(base.Path, "/")), "/")
	if !strings.HasSuffix(apiPrefix, "/api/v4") {
		apiPrefix = strings.TrimSuffix(apiPrefix, "/") + "/api/v4"
	}
	return base, apiPrefix + "/", nil
}

func guardedHTTPClient(source *http.Client, base *url.URL, apiPrefix string, timeout time.Duration) *http.Client {
	clone := *source
	next := clone.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	clone.Transport = guardedTransport{next: next, base: base, apiPrefix: apiPrefix}
	priorRedirect := clone.CheckRedirect
	clone.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if err := validateRequestURL(request.URL, base, apiPrefix); err != nil {
			return err
		}
		if priorRedirect != nil {
			return priorRedirect(request, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 GitLab redirects")
		}
		return nil
	}
	if timeout > 0 {
		clone.Timeout = timeout
	}
	return &clone
}

type guardedTransport struct {
	next      http.RoundTripper
	base      *url.URL
	apiPrefix string
}

func (transport guardedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := validateRequestURL(request.URL, transport.base, transport.apiPrefix); err != nil {
		return nil, err
	}
	return transport.next.RoundTrip(request)
}

func validateRequestURL(target, base *url.URL, apiPrefix string) error {
	if target == nil || !strings.EqualFold(target.Scheme, base.Scheme) || !strings.EqualFold(target.Host, base.Host) {
		return errors.New("GitLab request or redirect left the configured origin")
	}
	if target.User != nil || !strings.HasPrefix(path.Clean(target.Path)+"/", apiPrefix) {
		return errors.New("GitLab request or redirect left the configured API path")
	}
	return nil
}

func (c *Client) jobCapabilityDenied(endpoint CredentialEndpoint) bool {
	c.capabilityMu.RLock()
	defer c.capabilityMu.RUnlock()
	_, denied := c.jobDenied[endpoint]
	return denied
}

func (c *Client) rememberJobDenial(endpoint CredentialEndpoint) {
	c.capabilityMu.Lock()
	c.jobDenied[endpoint] = struct{}{}
	c.capabilityMu.Unlock()
}

func (c *Client) logRead(diagnostic ReadDiagnostic) {
	if c.logger != nil {
		c.logger.Info("GitLab read route", "endpoint", diagnostic.Endpoint, "credential_source", diagnostic.Source, "fallback_reason", diagnostic.Fallback)
	}
}

func (c *Client) logWrite(operation string) {
	if c.logger != nil {
		c.logger.Info("GitLab mutation route", "operation", operation, "credential_source", CredentialAPIToken)
	}
}

func (c *Client) logAuthorClassificationUnavailable(userID int64) {
	if c.logger != nil {
		c.logger.Warn("GitLab author classification unavailable", "user_id", userID, "credential_source", CredentialAPIToken)
	}
}
