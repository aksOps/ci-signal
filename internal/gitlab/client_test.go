package gitlab

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ci-signal/internal/config"
	gitlabapi "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	testJobToken = "job-secret"
	testAPIToken = "api-secret"
)

func TestGetMergeRequestUsesOnlyJobTokenAndConfiguredPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertCredential(t, request, CredentialJobToken)
		if got, want := request.URL.EscapedPath(), "/gitlab/api/v4/projects/group%2Fproject/merge_requests/7"; got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
		writeJSON(t, writer, http.StatusOK, map[string]any{"id": 70, "iid": 7, "title": "MR", "description": "raw description", "references": map[string]string{"full": "group/project!7"}})
	}))
	defer server.Close()

	client := newTestClient(t, server, testJobToken)
	mergeRequest, diagnostic, err := client.GetMergeRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mergeRequest.IID != 7 || mergeRequest.Description != "raw description" {
		t.Fatalf("merge request = %#v", mergeRequest)
	}
	if diagnostic.Source != CredentialJobToken || diagnostic.Fallback != FallbackNone {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
}

func TestReadFallbackStatusesAndCacheScope(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var jobCalls atomic.Int32
			var apiCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch credentialOf(request) {
				case CredentialJobToken:
					jobCalls.Add(1)
					writer.WriteHeader(status)
				case CredentialAPIToken:
					apiCalls.Add(1)
					writeJSON(t, writer, http.StatusOK, map[string]any{"id": 70, "iid": 7})
				default:
					t.Fatal("request had no accepted credential")
				}
			}))
			defer server.Close()

			client := newTestClient(t, server, testJobToken, WithRetryPolicy(0, 0, 0))
			for range 2 {
				_, diagnostic, err := client.GetMergeRequest(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if diagnostic.Source != CredentialAPIToken {
					t.Fatalf("diagnostic = %#v", diagnostic)
				}
			}
			wantJobCalls := int32(1)
			if status == http.StatusNotFound {
				wantJobCalls = 2
			}
			if jobCalls.Load() != wantJobCalls || apiCalls.Load() != 2 {
				t.Fatalf("job/API calls = %d/%d, want %d/2", jobCalls.Load(), apiCalls.Load(), wantJobCalls)
			}
		})
	}
}

func TestJobDenialCacheIsEndpointScoped(t *testing.T) {
	var noteJobCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/notes/9"):
			assertCredential(t, request, CredentialJobToken)
			noteJobCalls.Add(1)
			writeJSON(t, writer, http.StatusOK, noteJSON(9, "note", false))
		case credentialOf(request) == CredentialJobToken:
			writer.WriteHeader(http.StatusForbidden)
		case credentialOf(request) == CredentialAPIToken:
			writeJSON(t, writer, http.StatusOK, map[string]any{"id": 70, "iid": 7})
		default:
			t.Fatal("unexpected credential")
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, testJobToken, WithRetryPolicy(0, 0, 0))
	if _, _, err := client.GetMergeRequest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, diagnostic, err := client.GetNote(context.Background(), 9); err != nil || diagnostic.Source != CredentialJobToken || noteJobCalls.Load() != 1 {
		t.Fatalf("note route = %#v, %v, calls=%d", diagnostic, err, noteJobCalls.Load())
	}
}

func TestFallbackFailurePreservesBothErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if credentialOf(request) == CredentialJobToken {
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client := newTestClient(t, server, testJobToken, WithRetryPolicy(0, 0, 0))
	_, _, err := client.GetMergeRequest(context.Background())
	var readErr *ReadError
	if !errors.As(err, &readErr) || readErr.JobErr == nil || readErr.APIErr == nil || readErr.Fallback != FallbackForbidden {
		t.Fatalf("error = %#v", err)
	}
}

func TestReadDoesNotSwitchCredentialForOperationalFailures(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var apiCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if credentialOf(request) == CredentialAPIToken {
					apiCalls.Add(1)
				}
				writer.WriteHeader(status)
			}))
			defer server.Close()

			client := newTestClient(t, server, testJobToken, WithRetryPolicy(0, 0, 0))
			_, diagnostic, err := client.GetMergeRequest(context.Background())
			if err == nil || diagnostic.Source != CredentialJobToken || apiCalls.Load() != 0 {
				t.Fatalf("diagnostic/error/API calls = %#v/%v/%d", diagnostic, err, apiCalls.Load())
			}
		})
	}
}

func TestCanceledReadDoesNotSwitchCredential(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, testJobToken)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, diagnostic, err := client.GetMergeRequest(ctx)
	if err == nil || diagnostic.Source != CredentialJobToken || requests.Load() != 0 {
		t.Fatalf("diagnostic/error/requests = %#v/%v/%d", diagnostic, err, requests.Load())
	}
}

func TestTLSErrorDoesNotSwitchCredential(t *testing.T) {
	var jobCalls atomic.Int32
	var apiCalls atomic.Int32
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		switch credentialOf(request) {
		case CredentialJobToken:
			jobCalls.Add(1)
		case CredentialAPIToken:
			apiCalls.Add(1)
		}
		return nil, x509.UnknownAuthorityError{}
	})
	client := newConfiguredClient(t, "https://gitlab.example.com/root", testJobToken, config.Duration(time.Second), WithHTTPClient(&http.Client{Transport: transport}), WithRetryPolicy(0, 0, 0))
	if _, diagnostic, err := client.GetMergeRequest(context.Background()); err == nil || diagnostic.Source != CredentialJobToken || jobCalls.Load() != 1 || apiCalls.Load() != 0 {
		t.Fatalf("TLS route = %#v, %v, job/API=%d/%d", diagnostic, err, jobCalls.Load(), apiCalls.Load())
	}
}

func TestTimeoutDoesNotSwitchCredential(t *testing.T) {
	var jobCalls atomic.Int32
	var apiCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if credentialOf(request) == CredentialJobToken {
			jobCalls.Add(1)
		} else if credentialOf(request) == CredentialAPIToken {
			apiCalls.Add(1)
		}
		<-request.Context().Done()
	}))
	defer server.Close()
	client := newConfiguredClient(t, server.URL+"/gitlab", testJobToken, config.Duration(20*time.Millisecond), WithHTTPClient(server.Client()), WithRetryPolicy(0, 0, 0))
	if _, diagnostic, err := client.GetMergeRequest(context.Background()); err == nil || diagnostic.Source != CredentialJobToken || jobCalls.Load() != 1 || apiCalls.Load() != 0 {
		t.Fatalf("timeout route = %#v, %v, job/API=%d/%d", diagnostic, err, jobCalls.Load(), apiCalls.Load())
	}
}

func TestMissingJobTokenUsesAPITokenDirectly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertCredential(t, request, CredentialAPIToken)
		writeJSON(t, writer, http.StatusOK, map[string]any{"id": 70, "iid": 7})
	}))
	defer server.Close()
	client := newTestClient(t, server, "")
	_, diagnostic, err := client.GetMergeRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if diagnostic.Fallback != FallbackJobTokenAbsent {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
}

func TestAuthorClassificationUsesAPITokenCachesAndFailsClosed(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertCredential(t, request, CredentialAPIToken)
		calls.Add(1)
		switch request.URL.Path {
		case "/gitlab/api/v4/users/101":
			writeJSON(t, writer, http.StatusOK, map[string]any{"id": 101, "bot": false})
		case "/gitlab/api/v4/users/102":
			writeJSON(t, writer, http.StatusOK, map[string]any{"id": 102, "bot": true})
		case "/gitlab/api/v4/users/103":
			writer.WriteHeader(http.StatusForbidden)
		case "/gitlab/api/v4/users/104":
			writeJSON(t, writer, http.StatusOK, map[string]any{"id": 104})
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, testJobToken, WithRetryPolicy(0, 0, 0))
	tests := []struct {
		id      int64
		want    AuthorKind
		wantErr bool
	}{
		{id: 101, want: AuthorHuman},
		{id: 102, want: AuthorBot},
		{id: 103, want: AuthorUnknown, wantErr: true},
		{id: 104, want: AuthorUnknown, wantErr: true},
	}
	for _, test := range tests {
		for range 2 {
			kind, diagnostic, err := client.ClassifyAuthor(context.Background(), test.id)
			if kind != test.want || (err != nil) != test.wantErr {
				t.Fatalf("author %d classification = %q, %v", test.id, kind, err)
			}
			if diagnostic.Source != CredentialAPIToken || diagnostic.Fallback != FallbackEndpointAPIOnly {
				t.Fatalf("author %d diagnostic = %#v", test.id, diagnostic)
			}
		}
	}
	if calls.Load() != int32(len(tests)) {
		t.Fatalf("classification calls = %d, want %d", calls.Load(), len(tests))
	}
}

func TestReadRetriesIdempotentFailureWithoutCredentialSwitch(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertCredential(t, request, CredentialJobToken)
		if calls.Add(1) < 3 {
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(t, writer, http.StatusOK, map[string]any{"id": 70, "iid": 7})
	}))
	defer server.Close()
	client := newTestClient(t, server, testJobToken, WithRetryPolicy(2, 0, 0))
	if _, _, err := client.GetMergeRequest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
}

func TestRedirectsCannotLeaveConfiguredOriginOrAPIPath(t *testing.T) {
	var externalCalls atomic.Int32
	external := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		externalCalls.Add(1)
	}))
	defer external.Close()

	for _, location := range []string{external.URL + "/gitlab/api/v4/projects/x", "/users/sign_in"} {
		t.Run(url.PathEscape(location), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				http.Redirect(writer, request, location, http.StatusFound)
			}))
			defer server.Close()
			client := newTestClient(t, server, testJobToken, WithRetryPolicy(0, 0, 0))
			if _, _, err := client.GetMergeRequest(context.Background()); err == nil || !strings.Contains(err.Error(), "configured") {
				t.Fatalf("redirect error = %v", err)
			}
		})
	}
	if externalCalls.Load() != 0 {
		t.Fatalf("external redirect received %d requests", externalCalls.Load())
	}
}

func TestCreateNoteDoesNotRetryUncertainServerFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertCredential(t, request, CredentialAPIToken)
		calls.Add(1)
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client := newTestClient(t, server, testJobToken, WithRetryPolicy(2, 0, 0))
	if _, err := client.CreateNote(context.Background(), "report"); err == nil {
		t.Fatal("create succeeded")
	}
	if calls.Load() != 1 {
		t.Fatalf("POST calls = %d, want 1", calls.Load())
	}
}

func TestNewRequiresAPITokenAndBoundedTimeout(t *testing.T) {
	loaded := config.Loaded{Config: config.Config{GitLab: config.GitLab{BaseURL: "https://gitlab.example.com", Project: "group/project", MRIID: 7}}}
	if _, err := New(loaded, nil); err == nil || !strings.Contains(err.Error(), "API token") {
		t.Fatalf("missing API token error = %v", err)
	}
	loaded.Secrets.APIToken = config.Secret(testAPIToken)
	if _, err := New(loaded, nil); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("missing timeout error = %v", err)
	}
}

func newTestClient(t *testing.T, server *httptest.Server, jobToken string, options ...Option) *Client {
	t.Helper()
	options = append([]Option{WithHTTPClient(server.Client())}, options...)
	return newConfiguredClient(t, server.URL+"/gitlab", jobToken, config.Duration(2*time.Second), options...)
}

func newConfiguredClient(t *testing.T, baseURL, jobToken string, timeout config.Duration, options ...Option) *Client {
	t.Helper()
	loaded := config.Loaded{
		Config: config.Config{
			GitLab: config.GitLab{BaseURL: baseURL, Project: "group/project", MRIID: 7},
			Limits: config.Limits{APITimeout: timeout},
		},
		Secrets: config.Secrets{JobToken: config.Secret(jobToken), APIToken: config.Secret(testAPIToken)},
	}
	client, err := New(loaded, nil, options...)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func credentialOf(request *http.Request) CredentialSource {
	job := request.Header.Get(gitlabapi.JobTokenHeaderName)
	api := request.Header.Get(gitlabapi.AccessTokenHeaderName)
	if job != "" && api != "" {
		return "both"
	}
	if job == testJobToken {
		return CredentialJobToken
	}
	if api == testAPIToken {
		return CredentialAPIToken
	}
	return ""
}

func assertCredential(t *testing.T, request *http.Request, want CredentialSource) {
	t.Helper()
	if got := credentialOf(request); got != want {
		t.Fatalf("credential = %q, want %q; headers = %#v", got, want, request.Header)
	}
}

func writeJSON(t *testing.T, writer http.ResponseWriter, status int, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if value != nil {
		if err := json.NewEncoder(writer).Encode(value); err != nil {
			t.Fatal(err)
		}
	}
}

func noteJSON(id int64, body string, internal bool) map[string]any {
	return map[string]any{"id": id, "body": body, "internal": internal, "author": map[string]any{"id": id + 100, "username": fmt.Sprintf("user-%d", id)}}
}
