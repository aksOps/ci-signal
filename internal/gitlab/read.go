package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	gitlabapi "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	pageSize     = int64(100)
	maximumPages = int64(10_000)
)

type readCall[T any] func(*gitlabapi.Client) (T, error)

func routedRead[T any](client *Client, endpoint CredentialEndpoint, jobCall, apiCall readCall[T]) (T, ReadDiagnostic, error) {
	var zero T
	if client.job == nil {
		diagnostic := ReadDiagnostic{Endpoint: endpoint, Source: CredentialAPIToken, Fallback: FallbackJobTokenAbsent}
		value, err := apiCall(client.api)
		client.logRead(diagnostic)
		if err != nil {
			return zero, diagnostic, &ReadError{Endpoint: endpoint, Source: CredentialAPIToken, Fallback: diagnostic.Fallback, APIErr: err}
		}
		return value, diagnostic, nil
	}
	if client.jobCapabilityDenied(endpoint) {
		diagnostic := ReadDiagnostic{Endpoint: endpoint, Source: CredentialAPIToken, Fallback: FallbackCachedJobDenial}
		value, err := apiCall(client.api)
		client.logRead(diagnostic)
		if err != nil {
			return zero, diagnostic, &ReadError{Endpoint: endpoint, Source: CredentialAPIToken, Fallback: diagnostic.Fallback, APIErr: err}
		}
		return value, diagnostic, nil
	}
	value, jobErr := jobCall(client.job)
	if jobErr == nil {
		diagnostic := ReadDiagnostic{Endpoint: endpoint, Source: CredentialJobToken}
		client.logRead(diagnostic)
		return value, diagnostic, nil
	}
	reason, fallback, cache := fallbackFor(jobErr)
	if !fallback {
		diagnostic := ReadDiagnostic{Endpoint: endpoint, Source: CredentialJobToken}
		client.logRead(diagnostic)
		return zero, diagnostic, &ReadError{Endpoint: endpoint, Source: CredentialJobToken, JobErr: jobErr}
	}
	if cache {
		client.rememberJobDenial(endpoint)
	}
	diagnostic := ReadDiagnostic{Endpoint: endpoint, Source: CredentialAPIToken, Fallback: reason}
	value, apiErr := apiCall(client.api)
	client.logRead(diagnostic)
	if apiErr != nil {
		return zero, diagnostic, &ReadError{Endpoint: endpoint, Source: CredentialAPIToken, Fallback: reason, JobErr: jobErr, APIErr: apiErr}
	}
	return value, diagnostic, nil
}

func apiRead[T any](client *Client, endpoint CredentialEndpoint, call readCall[T]) (T, ReadDiagnostic, error) {
	var zero T
	diagnostic := ReadDiagnostic{Endpoint: endpoint, Source: CredentialAPIToken, Fallback: FallbackEndpointAPIOnly}
	value, err := call(client.api)
	client.logRead(diagnostic)
	if err != nil {
		return zero, diagnostic, &ReadError{Endpoint: endpoint, Source: CredentialAPIToken, Fallback: diagnostic.Fallback, APIErr: err}
	}
	return value, diagnostic, nil
}

func fallbackFor(err error) (FallbackReason, bool, bool) {
	switch {
	case gitlabapi.HasStatusCode(err, http.StatusUnauthorized):
		return FallbackUnauthorized, true, true
	case gitlabapi.HasStatusCode(err, http.StatusForbidden):
		return FallbackForbidden, true, true
	case gitlabapi.HasStatusCode(err, http.StatusNotFound):
		return FallbackMaskedNotFound, true, false
	default:
		return FallbackNone, false, false
	}
}

func (c *Client) GetMergeRequest(ctx context.Context) (*gitlabapi.MergeRequest, ReadDiagnostic, error) {
	call := func(client *gitlabapi.Client) (*gitlabapi.MergeRequest, error) {
		value, _, err := client.MergeRequests.GetMergeRequest(c.project, c.mrIID, nil, gitlabapi.WithContext(ctx))
		return value, err
	}
	return routedRead(c, EndpointMergeRequest, call, call)
}

func (c *Client) GetNote(ctx context.Context, noteID int64) (*gitlabapi.Note, ReadDiagnostic, error) {
	if noteID < 1 {
		return nil, ReadDiagnostic{}, errors.New("GitLab note ID must be positive")
	}
	call := func(client *gitlabapi.Client) (*gitlabapi.Note, error) {
		value, _, err := client.Notes.GetMergeRequestNote(c.project, c.mrIID, noteID, gitlabapi.WithContext(ctx))
		return value, err
	}
	return routedRead(c, EndpointNote, call, call)
}

func (c *Client) ListNotes(ctx context.Context) ([]*gitlabapi.Note, ReadDiagnostic, error) {
	call := func(client *gitlabapi.Client) ([]*gitlabapi.Note, error) {
		return paginate(ctx, func(page int64) ([]*gitlabapi.Note, *gitlabapi.Response, error) {
			options := &gitlabapi.ListMergeRequestNotesOptions{ListOptions: gitlabapi.ListOptions{Page: page, PerPage: pageSize}}
			return client.Notes.ListMergeRequestNotes(c.project, c.mrIID, options, gitlabapi.WithContext(ctx))
		})
	}
	return routedRead(c, EndpointNotes, call, call)
}

func (c *Client) ListDiscussions(ctx context.Context) ([]*gitlabapi.Discussion, ReadDiagnostic, error) {
	call := func(client *gitlabapi.Client) ([]*gitlabapi.Discussion, error) {
		return paginate(ctx, func(page int64) ([]*gitlabapi.Discussion, *gitlabapi.Response, error) {
			options := &gitlabapi.ListMergeRequestDiscussionsOptions{ListOptions: gitlabapi.ListOptions{Page: page, PerPage: pageSize}}
			return client.Discussions.ListMergeRequestDiscussions(c.project, c.mrIID, options, gitlabapi.WithContext(ctx))
		})
	}
	return apiRead(c, EndpointDiscussions, call)
}

func (c *Client) ListLabels(ctx context.Context) ([]*gitlabapi.Label, ReadDiagnostic, error) {
	call := func(client *gitlabapi.Client) ([]*gitlabapi.Label, error) {
		return paginate(ctx, func(page int64) ([]*gitlabapi.Label, *gitlabapi.Response, error) {
			includeAncestors := true
			options := &gitlabapi.ListLabelsOptions{ListOptions: gitlabapi.ListOptions{Page: page, PerPage: pageSize}, IncludeAncestorGroups: &includeAncestors}
			return client.Labels.ListLabels(c.project, options, gitlabapi.WithContext(ctx))
		})
	}
	return apiRead(c, EndpointLabels, call)
}

func (c *Client) CurrentUser(ctx context.Context) (*gitlabapi.User, ReadDiagnostic, error) {
	call := func(client *gitlabapi.Client) (*gitlabapi.User, error) {
		value, _, err := client.Users.CurrentUser(gitlabapi.WithContext(ctx))
		return value, err
	}
	return apiRead(c, EndpointCurrentUser, call)
}

type userClassificationPayload struct {
	ID  int64 `json:"id"`
	Bot *bool `json:"bot"`
}

// ClassifyAuthor resolves GitLab's authoritative bot field without exposing
// the rest of the user profile to review context. The pointer preserves the
// distinction between a human (false) and a response that omitted the field.
func (c *Client) ClassifyAuthor(ctx context.Context, userID int64) (AuthorKind, ReadDiagnostic, error) {
	if userID < 1 {
		return AuthorUnknown, ReadDiagnostic{}, errors.New("GitLab author ID must be positive")
	}
	c.authorMu.RLock()
	cached, ok := c.authors[userID]
	c.authorMu.RUnlock()
	if ok {
		return cached.kind, cached.diagnostic, cached.err
	}
	call := func(client *gitlabapi.Client) (userClassificationPayload, error) {
		request, err := client.NewRequest(http.MethodGet, fmt.Sprintf("users/%d", userID), nil, []gitlabapi.RequestOptionFunc{gitlabapi.WithContext(ctx)})
		if err != nil {
			return userClassificationPayload{}, err
		}
		var payload userClassificationPayload
		_, err = client.Do(request, &payload)
		return payload, err
	}
	payload, diagnostic, err := apiRead(c, EndpointUser, call)
	kind := AuthorUnknown
	if err == nil {
		switch {
		case payload.ID != userID:
			err = errors.New("GitLab author classification returned a mismatched user")
		case payload.Bot == nil:
			err = errors.New("GitLab author classification omitted bot status")
		case *payload.Bot:
			kind = AuthorBot
		default:
			kind = AuthorHuman
		}
	}
	result := authorLookup{kind: kind, diagnostic: diagnostic, err: err}
	c.authorMu.Lock()
	if prior, exists := c.authors[userID]; exists {
		result = prior
	} else if ctx.Err() == nil {
		c.authors[userID] = result
	}
	c.authorMu.Unlock()
	return result.kind, result.diagnostic, result.err
}

func (c *Client) FetchContext(ctx context.Context) (Context, error) {
	result := Context{}
	mergeRequest, diagnostic, err := c.GetMergeRequest(ctx)
	result.Diagnostics = append(result.Diagnostics, diagnostic)
	if err != nil {
		return result, fmt.Errorf("fetch merge-request context: %w", err)
	}
	result.MergeRequest = mergeRequest
	notes, diagnostic, err := c.ListNotes(ctx)
	result.Diagnostics = append(result.Diagnostics, diagnostic)
	if err != nil {
		return result, fmt.Errorf("fetch merge-request notes: %w", err)
	}
	discussions, diagnostic, err := c.ListDiscussions(ctx)
	result.Diagnostics = append(result.Diagnostics, diagnostic)
	if err != nil {
		return result, fmt.Errorf("fetch merge-request discussions: %w", err)
	}
	labels, diagnostic, err := c.ListLabels(ctx)
	result.Diagnostics = append(result.Diagnostics, diagnostic)
	if err != nil {
		return result, fmt.Errorf("fetch project labels: %w", err)
	}
	result.Notes, result.RestrictedNotes, result.Discussions = normalizeDiscussionContext(notes, discussions)
	result.AuthorKinds, err = c.classifyVisibleAuthors(ctx, result.Notes)
	if err != nil {
		return result, err
	}
	result.Labels = uniqueLabels(labels)
	result.Complete = true
	return result, nil
}

func (c *Client) classifyVisibleAuthors(ctx context.Context, notes []*gitlabapi.Note) (map[int64]AuthorKind, error) {
	ids := make(map[int64]struct{})
	for _, note := range notes {
		if note != nil && !note.System && note.Author.ID > 0 {
			ids[note.Author.ID] = struct{}{}
		}
	}
	ordered := make([]int64, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	result := make(map[int64]AuthorKind, len(ordered))
	for _, id := range ordered {
		kind, _, err := c.ClassifyAuthor(ctx, id)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			c.logAuthorClassificationUnavailable(id)
			kind = AuthorUnknown
		}
		result[id] = kind
	}
	return result, nil
}

type pageCall[T any] func(page int64) ([]T, *gitlabapi.Response, error)

func paginate[T any](ctx context.Context, call pageCall[T]) ([]T, error) {
	var result []T
	for page := int64(1); page <= maximumPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		items, response, err := call(page)
		if err != nil {
			return nil, err
		}
		result = append(result, items...)
		next, err := validatedNextPage(response, page, len(items))
		if err != nil {
			return nil, err
		}
		if next == 0 {
			return result, nil
		}
		if next != page+1 {
			return nil, fmt.Errorf("incomplete GitLab pagination: page %d points to %d", page, next)
		}
	}
	return nil, fmt.Errorf("incomplete GitLab pagination: exceeded %d pages", maximumPages)
}

func validatedNextPage(response *gitlabapi.Response, requested int64, itemCount int) (int64, error) {
	if response == nil || response.Response == nil {
		return 0, errors.New("incomplete GitLab pagination: missing response metadata")
	}
	if response.CurrentPage != 0 && response.CurrentPage != requested {
		return 0, fmt.Errorf("incomplete GitLab pagination: requested page %d received page %d", requested, response.CurrentPage)
	}
	rawNext, hasNextHeader := response.Header[http.CanonicalHeaderKey("X-Next-Page")]
	if len(rawNext) > 0 && strings.TrimSpace(rawNext[0]) != "" {
		next, err := strconv.ParseInt(strings.TrimSpace(rawNext[0]), 10, 64)
		if err != nil || next < 1 {
			return 0, errors.New("incomplete GitLab pagination: invalid X-Next-Page")
		}
		return next, nil
	}
	if response.NextLink != "" {
		next, err := pageFromLink(response.NextLink, response.Request.URL)
		if err != nil {
			return 0, err
		}
		return next, nil
	}
	if response.TotalPages > requested {
		return 0, errors.New("incomplete GitLab pagination: total pages indicate a missing next page")
	}
	if !hasNextHeader && response.TotalPages == 0 && itemCount > 0 {
		return 0, errors.New("incomplete GitLab pagination: nonempty page lacks continuation metadata")
	}
	return 0, nil
}

func pageFromLink(raw string, requestURL *url.URL) (int64, error) {
	next, err := url.Parse(raw)
	if err != nil || requestURL == nil || !strings.EqualFold(next.Scheme, requestURL.Scheme) || !strings.EqualFold(next.Host, requestURL.Host) || next.Path != requestURL.Path {
		return 0, errors.New("incomplete GitLab pagination: unsafe next-page link")
	}
	page, err := strconv.ParseInt(next.Query().Get("page"), 10, 64)
	if err != nil || page < 1 {
		return 0, errors.New("incomplete GitLab pagination: invalid next-page link")
	}
	return page, nil
}

func normalizeDiscussionContext(notes []*gitlabapi.Note, discussions []*gitlabapi.Discussion) ([]*gitlabapi.Note, []*gitlabapi.Note, []Discussion) {
	noteByID := make(map[int64]*gitlabapi.Note)
	restrictedNoteIDs := make(map[int64]struct{})
	for _, note := range notes {
		if note != nil && note.ID > 0 {
			noteByID[note.ID] = note
			if note.Internal || note.Confidential {
				restrictedNoteIDs[note.ID] = struct{}{}
			}
		}
	}
	discussionByID := make(map[string]Discussion)
	for _, discussion := range discussions {
		if discussion == nil || discussion.ID == "" {
			continue
		}
		normalized := discussionByID[discussion.ID]
		normalized.ID = discussion.ID
		normalized.IndividualNote = discussion.IndividualNote
		seen := make(map[int64]struct{}, len(normalized.NoteIDs)+len(discussion.Notes))
		for _, id := range normalized.NoteIDs {
			seen[id] = struct{}{}
		}
		for _, note := range discussion.Notes {
			if note == nil || note.ID < 1 {
				continue
			}
			noteByID[note.ID] = note
			if note.Internal || note.Confidential {
				restrictedNoteIDs[note.ID] = struct{}{}
			}
			if _, exists := seen[note.ID]; !exists {
				normalized.NoteIDs = append(normalized.NoteIDs, note.ID)
				seen[note.ID] = struct{}{}
			}
			if note.Internal || note.Confidential {
				normalized.Restricted = true
			}
		}
		sort.Slice(normalized.NoteIDs, func(i, j int) bool { return normalized.NoteIDs[i] < normalized.NoteIDs[j] })
		discussionByID[discussion.ID] = normalized
	}
	ids := make([]int64, 0, len(noteByID))
	for id := range noteByID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	visible := make([]*gitlabapi.Note, 0, len(ids))
	restricted := make([]*gitlabapi.Note, 0)
	for _, id := range ids {
		note := noteByID[id]
		if _, isRestricted := restrictedNoteIDs[id]; isRestricted {
			restricted = append(restricted, note)
		} else {
			visible = append(visible, note)
		}
	}
	discussionIDs := make([]string, 0, len(discussionByID))
	for id := range discussionByID {
		discussionIDs = append(discussionIDs, id)
	}
	sort.Strings(discussionIDs)
	normalized := make([]Discussion, 0, len(discussionIDs))
	for _, id := range discussionIDs {
		normalized = append(normalized, discussionByID[id])
	}
	return visible, restricted, normalized
}

func uniqueLabels(labels []*gitlabapi.Label) []*gitlabapi.Label {
	byID := make(map[int64]*gitlabapi.Label)
	withoutID := make(map[string]*gitlabapi.Label)
	for _, label := range labels {
		if label == nil {
			continue
		}
		if label.ID > 0 {
			byID[label.ID] = label
		} else if label.Name != "" {
			withoutID[label.Name] = label
		}
	}
	ids := make([]int64, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	result := make([]*gitlabapi.Label, 0, len(byID)+len(withoutID))
	for _, id := range ids {
		result = append(result, byID[id])
	}
	names := make([]string, 0, len(withoutID))
	for name := range withoutID {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		result = append(result, withoutID[name])
	}
	return result
}
