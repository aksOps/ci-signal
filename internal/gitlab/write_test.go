package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestMutationMethodsUseOnlyAPITokenAndTargetedLabels(t *testing.T) {
	seen := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertCredential(t, request, CredentialAPIToken)
		key := request.Method + " " + request.URL.Path
		seen[key]++
		var body map[string]any
		if request.Body != nil && request.ContentLength != 0 {
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("decode %s body: %v", key, err)
			}
		}
		switch key {
		case "POST /gitlab/api/v4/projects/group/project/merge_requests/7/notes":
			if body["body"] != "report" {
				t.Fatalf("create note body = %#v", body)
			}
			writeJSON(t, writer, http.StatusCreated, noteJSON(91, "report", false))
		case "PUT /gitlab/api/v4/projects/group/project/merge_requests/7/notes/91":
			if body["body"] != "updated" {
				t.Fatalf("update note body = %#v", body)
			}
			writeJSON(t, writer, http.StatusOK, noteJSON(91, "updated", false))
		case "DELETE /gitlab/api/v4/projects/group/project/merge_requests/7/notes/91":
			writer.WriteHeader(http.StatusNoContent)
		case "POST /gitlab/api/v4/projects/group/project/labels":
			if body["name"] != "ai-review::approved" || body["color"] != "#00ff00" {
				t.Fatalf("create label body = %#v", body)
			}
			writeJSON(t, writer, http.StatusCreated, map[string]any{"id": 11, "name": "ai-review::approved", "color": "#00ff00"})
		case "PUT /gitlab/api/v4/projects/group/project/merge_requests/7":
			if _, exists := body["labels"]; exists {
				t.Fatalf("label replacement field present: %#v", body)
			}
			if !reflect.DeepEqual(body["add_labels"], "ai-review::approved,keep") || !reflect.DeepEqual(body["remove_labels"], "ai-review::needs-review") {
				t.Fatalf("targeted labels body = %#v", body)
			}
			writeJSON(t, writer, http.StatusOK, map[string]any{"id": 70, "iid": 7, "labels": []string{"keep", "ai-review::approved"}})
		case "GET /gitlab/api/v4/user":
			writeJSON(t, writer, http.StatusOK, map[string]any{"id": 5, "username": "review-bot", "bot": true})
		default:
			t.Fatalf("unexpected mutation request: %s", key)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server, testJobToken)
	ctx := context.Background()
	created, err := client.CreateNote(ctx, "report")
	if err != nil || created.ID != 91 {
		t.Fatalf("create note = %#v, %v", created, err)
	}
	updated, err := client.UpdateNote(ctx, 91, "updated")
	if err != nil || updated.Body != "updated" {
		t.Fatalf("update note = %#v, %v", updated, err)
	}
	if err := client.DeleteNote(ctx, 91); err != nil {
		t.Fatal(err)
	}
	label, err := client.CreateLabel(ctx, "ai-review::approved", "#00ff00", "AI verdict")
	if err != nil || label.ID != 11 {
		t.Fatalf("create label = %#v, %v", label, err)
	}
	mergeRequest, err := client.UpdateLabels(ctx, []string{"keep", "ai-review::approved", "keep"}, []string{"ai-review::needs-review"})
	if err != nil || mergeRequest.IID != 7 {
		t.Fatalf("update labels = %#v, %v", mergeRequest, err)
	}
	user, diagnostic, err := client.CurrentUser(ctx)
	if err != nil || user.ID != 5 || diagnostic.Source != CredentialAPIToken {
		t.Fatalf("current user = %#v, %#v, %v", user, diagnostic, err)
	}
	for operation, count := range seen {
		if count != 1 {
			t.Fatalf("%s calls = %d", operation, count)
		}
	}
}

func TestUpdateLabelsRejectsConflictingAssignment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid label update reached server")
	}))
	defer server.Close()
	client := newTestClient(t, server, testJobToken)
	if _, err := client.UpdateLabels(context.Background(), []string{"same"}, []string{"same"}); err == nil {
		t.Fatal("conflicting label update succeeded")
	}
}
