package gitlab

import (
	"errors"
	"fmt"

	gitlabapi "gitlab.com/gitlab-org/api/client-go/v2"
)

type CredentialSource string

const (
	CredentialJobToken CredentialSource = "job_token"
	CredentialAPIToken CredentialSource = "api_token"
)

type FallbackReason string

const (
	FallbackNone            FallbackReason = ""
	FallbackJobTokenAbsent  FallbackReason = "job_token_absent"
	FallbackUnauthorized    FallbackReason = "job_token_unauthorized"
	FallbackForbidden       FallbackReason = "job_token_forbidden"
	FallbackMaskedNotFound  FallbackReason = "job_token_masked_not_found"
	FallbackCachedJobDenial FallbackReason = "cached_job_token_denial"
	FallbackEndpointAPIOnly FallbackReason = "endpoint_api_token_only"
)

type ReadDiagnostic struct {
	Endpoint CredentialEndpoint `json:"endpoint"`
	Source   CredentialSource   `json:"source"`
	Fallback FallbackReason     `json:"fallback,omitempty"`
}

type CredentialEndpoint string

const (
	EndpointMergeRequest CredentialEndpoint = "merge_request"
	EndpointNotes        CredentialEndpoint = "merge_request_notes"
	EndpointNote         CredentialEndpoint = "merge_request_note"
	EndpointDiscussions  CredentialEndpoint = "merge_request_discussions"
	EndpointLabels       CredentialEndpoint = "project_labels"
	EndpointCurrentUser  CredentialEndpoint = "current_user"
	EndpointUser         CredentialEndpoint = "user"
)

type AuthorKind string

const (
	AuthorUnknown AuthorKind = "unknown"
	AuthorHuman   AuthorKind = "human"
	AuthorBot     AuthorKind = "bot"
)

type ReadError struct {
	Endpoint CredentialEndpoint
	Source   CredentialSource
	Fallback FallbackReason
	JobErr   error
	APIErr   error
}

func (e *ReadError) Error() string {
	if e == nil {
		return "GitLab read failed"
	}
	if e.JobErr != nil && e.APIErr != nil {
		return fmt.Sprintf("GitLab %s read failed with job token (%v) and API-token fallback (%v)", e.Endpoint, e.JobErr, e.APIErr)
	}
	if e.APIErr != nil {
		return fmt.Sprintf("GitLab %s read failed with API token: %v", e.Endpoint, e.APIErr)
	}
	return fmt.Sprintf("GitLab %s read failed with job token: %v", e.Endpoint, e.JobErr)
}

func (e *ReadError) Unwrap() error {
	if e == nil {
		return nil
	}
	if e.APIErr != nil {
		return e.APIErr
	}
	return e.JobErr
}

func (e *ReadError) Is(target error) bool {
	if e == nil {
		return false
	}
	return errors.Is(e.JobErr, target) || errors.Is(e.APIErr, target)
}

type Discussion struct {
	ID             string  `json:"id"`
	IndividualNote bool    `json:"individual_note"`
	NoteIDs        []int64 `json:"note_ids"`
	Restricted     bool    `json:"restricted"`
}

type Context struct {
	MergeRequest    *gitlabapi.MergeRequest `json:"merge_request"`
	Notes           []*gitlabapi.Note       `json:"notes"`
	RestrictedNotes []*gitlabapi.Note       `json:"restricted_notes"`
	Discussions     []Discussion            `json:"discussions"`
	AuthorKinds     map[int64]AuthorKind    `json:"author_kinds"`
	Labels          []*gitlabapi.Label      `json:"labels"`
	Diagnostics     []ReadDiagnostic        `json:"diagnostics"`
	Complete        bool                    `json:"complete"`
}
