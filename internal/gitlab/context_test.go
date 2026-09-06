package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	gitlabapi "gitlab.com/gitlab-org/api/client-go/v2"
)

func TestFetchContextPaginatesDeduplicatesAndSeparatesRestrictedNotes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if credentialOf(request) == "both" || credentialOf(request) == "" {
			t.Fatalf("bad credential headers: %#v", request.Header)
		}
		page, _ := strconv.Atoi(request.URL.Query().Get("page"))
		if page == 0 {
			page = 1
		}
		switch {
		case request.URL.Path == "/gitlab/api/v4/projects/group/project/merge_requests/7":
			assertCredential(t, request, CredentialJobToken)
			writeJSON(t, writer, http.StatusOK, map[string]any{"id": 70, "iid": 7, "title": "MR", "description": "raw", "labels": []string{"assigned"}, "blocking_discussions_resolved": false})
		case request.URL.Path == "/gitlab/api/v4/projects/group/project/merge_requests/7/notes":
			assertCredential(t, request, CredentialJobToken)
			writer.Header().Set("X-Page", strconv.Itoa(page))
			writer.Header().Set("X-Total-Pages", "2")
			if page == 1 {
				writer.Header().Set("X-Next-Page", "2")
				writeJSON(t, writer, http.StatusOK, []any{noteJSON(1, "first", false), noteJSON(2, "from notes", false)})
			} else {
				writer.Header().Set("X-Next-Page", "")
				writeJSON(t, writer, http.StatusOK, []any{noteJSON(2, "duplicate", false), noteJSON(3, "internal", true)})
			}
		case request.URL.Path == "/gitlab/api/v4/projects/group/project/merge_requests/7/discussions":
			assertCredential(t, request, CredentialAPIToken)
			writer.Header().Set("X-Next-Page", "")
			writeJSON(t, writer, http.StatusOK, []any{
				map[string]any{"id": "discussion-a", "individual_note": false, "notes": []any{noteJSON(2, "thread version", false), noteJSON(4, "reply", false)}},
				map[string]any{"id": "discussion-b", "individual_note": true, "notes": []any{noteJSON(3, "internal thread", true)}},
			})
		case request.URL.Path == "/gitlab/api/v4/projects/group/project/labels":
			assertCredential(t, request, CredentialAPIToken)
			writer.Header().Set("X-Next-Page", "")
			if request.URL.Query().Get("include_ancestor_groups") != "true" {
				t.Fatal("ancestor group labels not requested")
			}
			writeJSON(t, writer, http.StatusOK, []any{map[string]any{"id": 1, "name": "one"}, map[string]any{"id": 1, "name": "one duplicate"}, map[string]any{"id": 2, "name": "two"}})
		case strings.HasPrefix(request.URL.Path, "/gitlab/api/v4/users/"):
			assertCredential(t, request, CredentialAPIToken)
			id, err := strconv.ParseInt(strings.TrimPrefix(request.URL.Path, "/gitlab/api/v4/users/"), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			switch id {
			case 101:
				writeJSON(t, writer, http.StatusOK, map[string]any{"id": id, "bot": false})
			case 102:
				writeJSON(t, writer, http.StatusOK, map[string]any{"id": id, "bot": true})
			case 104:
				writer.WriteHeader(http.StatusForbidden)
			default:
				t.Fatalf("unexpected author %d", id)
			}
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.String())
		}
	}))
	defer server.Close()

	client := newTestClient(t, server, testJobToken)
	result, err := client.FetchContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || result.MergeRequest.Description != "raw" || len(result.Diagnostics) != 4 {
		t.Fatalf("context summary = %#v", result)
	}
	if got := noteIDs(result.Notes); !reflect.DeepEqual(got, []int64{1, 2, 4}) {
		t.Fatalf("visible note IDs = %v", got)
	}
	if result.Notes[1].Body != "thread version" {
		t.Fatalf("deduplicated note body = %q", result.Notes[1].Body)
	}
	if got := noteIDs(result.RestrictedNotes); !reflect.DeepEqual(got, []int64{3}) {
		t.Fatalf("restricted note IDs = %v", got)
	}
	if len(result.Discussions) != 2 || result.Discussions[0].Restricted || !result.Discussions[1].Restricted {
		t.Fatalf("discussions = %#v", result.Discussions)
	}
	if len(result.Labels) != 2 || result.Labels[0].Name != "one duplicate" {
		t.Fatalf("labels = %#v", result.Labels)
	}
	wantKinds := map[int64]AuthorKind{101: AuthorHuman, 102: AuthorBot, 104: AuthorUnknown}
	for id, want := range wantKinds {
		if result.AuthorKinds[id] != want {
			t.Fatalf("author %d kind = %q, want %q", id, result.AuthorKinds[id], want)
		}
	}
}

func TestIncompletePaginationIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertCredential(t, request, CredentialJobToken)
		writer.Header().Set("X-Page", "1")
		writer.Header().Set("X-Total-Pages", "2")
		writeJSON(t, writer, http.StatusOK, []any{noteJSON(1, "only page", false)})
	}))
	defer server.Close()
	client := newTestClient(t, server, testJobToken)
	if _, _, err := client.ListNotes(context.Background()); err == nil {
		t.Fatal("incomplete pagination succeeded")
	}
}

func TestNonemptyCollectionWithoutPaginationMetadataIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertCredential(t, request, CredentialJobToken)
		writeJSON(t, writer, http.StatusOK, []any{noteJSON(1, "ambiguous page", false)})
	}))
	defer server.Close()
	client := newTestClient(t, server, testJobToken)
	if _, _, err := client.ListNotes(context.Background()); err == nil {
		t.Fatal("ambiguous pagination succeeded")
	}
}

func noteIDs(notes []*gitlabapi.Note) []int64 {
	result := make([]int64, 0, len(notes))
	for _, note := range notes {
		result = append(result, note.ID)
	}
	return result
}
