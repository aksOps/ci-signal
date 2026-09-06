package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"ci-signal/internal/config"
	"ci-signal/internal/review"
)

func TestDeriveLabelsUsesTrustedStateAndDeduplicatedTelemetry(t *testing.T) {
	settings := config.Labels{
		Static: []string{"ai-review::engine::copilot-cli"},
		Dynamic: []config.LabelMapping{
			{Name: "verdict", Source: config.LabelFromVerdict, Mode: config.LabelSingle, Missing: config.MissingOmit, Values: map[string]string{"approved": "ai-review::verdict::approved"}},
			{Name: "coverage", Source: config.LabelFromCoverage, Mode: config.LabelSingle, Missing: config.MissingOmit, Values: map[string]string{"complete": "ai-review::coverage::complete"}},
			{Name: "requested", Source: config.LabelFromRequestedModels, Mode: config.LabelSet, Missing: config.MissingOmit, Prefix: "ai-review/requested-model/"},
			{Name: "observed", Source: config.LabelFromObservedModels, Mode: config.LabelSet, Missing: config.MissingOmit, Prefix: "ai-review/model/"},
			{Name: "tools", Source: config.LabelFromExecutedTools, Mode: config.LabelSet, Missing: config.MissingOmit, Prefix: "ai-review/tool/"},
			{Name: "tokens", Source: config.LabelFromTokenUsage, Mode: config.LabelSingle, Missing: config.MissingUnknown, Values: map[string]string{"unknown": "ai-review::tokens::unknown"}, TokenFormat: config.TokenLabelBuckets, TokenBuckets: []config.TokenBucket{{LessThan: 50, Label: "ai-review::tokens::0-50"}, {LessThan: 100, Label: "ai-review::tokens::50-100"}, {Label: "ai-review::tokens::100+"}}},
			{Name: "team", Source: config.LabelFromTeam, Mode: config.LabelSingle, Missing: config.MissingOmit, Values: map[string]string{"payments": "ai-review::team::payments"}},
			{Name: "tier", Source: config.LabelFromMetadata, Mode: config.LabelSingle, Missing: config.MissingOmit, MetadataKey: "tier", Values: map[string]string{"prod": "ai-review::tier::prod"}},
		},
	}
	telemetry := review.Telemetry{
		RequestedModels: []string{"deepseek-v4-flash:cloud", "deepseek-v4-flash:cloud"},
		ObservedModels:  []review.ModelObservation{{SessionID: "s1", Model: "deepseek-v4-flash:cloud"}, {SessionID: "s1", Model: "deepseek-v4-flash:cloud"}},
		Tools:           []review.ToolExecution{{SessionID: "s1", Tool: "ast-grep", Succeeded: true}, {SessionID: "s1", Tool: "ast-grep", Succeeded: false}},
		UsageComplete:   true,
		Usage: []review.UsageEvent{
			{EventID: "call-1", SessionID: "s1", TotalTokens: 30},
			{EventID: "call-1", SessionID: "s1", TotalTokens: 30},
			{EventID: "checkpoint", SessionID: "s1", TotalTokens: 50, Cumulative: true},
			{EventID: "call-2", SessionID: "s2", InputTokens: 10, OutputTokens: 10},
		},
	}
	plan, err := DeriveLabels(settings, config.Metadata{Team: "payments", Values: map[string]string{"tier": "prod"}}, LabelInput{Verdict: review.VerdictApproved, Coverage: review.CoverageComplete, Telemetry: telemetry})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ai-review/model/deepseek-v4-flash:cloud",
		"ai-review/requested-model/deepseek-v4-flash:cloud",
		"ai-review/tool/ast-grep",
		"ai-review::coverage::complete",
		"ai-review::engine::copilot-cli",
		"ai-review::team::payments",
		"ai-review::tier::prod",
		"ai-review::tokens::50-100",
		"ai-review::verdict::approved",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(plan.Desired, want) || plan.TokenUsage == nil || *plan.TokenUsage != 70 || !plan.UsageComplete {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestDeriveLabelsTreatsIncompleteUsageAsUnknown(t *testing.T) {
	settings := config.Labels{Dynamic: []config.LabelMapping{{Name: "tokens", Source: config.LabelFromTokenUsage, Mode: config.LabelSingle, Missing: config.MissingUnknown, Prefix: "ai-review::tokens::", TokenFormat: config.TokenLabelExact}}}
	plan, err := DeriveLabels(settings, config.Metadata{}, LabelInput{Telemetry: review.Telemetry{Usage: []review.UsageEvent{{EventID: "partial", SessionID: "s1", TotalTokens: 42}}, UsageComplete: false}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Desired, []string{"ai-review::tokens::unknown"}) || plan.TokenUsage != nil || plan.UsageComplete {
		t.Fatalf("plan = %#v", plan)
	}
	plan, err = DeriveLabels(settings, config.Metadata{}, LabelInput{Telemetry: review.Telemetry{Usage: []review.UsageEvent{{EventID: "complete", SessionID: "s1", TotalTokens: 42}}, UsageComplete: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Desired, []string{"ai-review::tokens::42"}) || plan.TokenUsage == nil || *plan.TokenUsage != 42 || !plan.UsageComplete {
		t.Fatalf("exact plan = %#v", plan)
	}
}

func TestDeriveLabelsRejectsConflictingUsageAndScopes(t *testing.T) {
	telemetry := review.Telemetry{UsageComplete: true, Usage: []review.UsageEvent{{EventID: "same", SessionID: "s1", TotalTokens: 1}, {EventID: "same", SessionID: "s1", TotalTokens: 2}}}
	if _, err := DeriveLabels(config.Labels{}, config.Metadata{}, LabelInput{Telemetry: telemetry}); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("usage error = %v", err)
	}
	settings := config.Labels{Static: []string{"ai-review::verdict::approved", "ai-review::verdict::needs-review"}}
	if _, err := DeriveLabels(settings, config.Metadata{}, LabelInput{}); err == nil || !strings.Contains(err.Error(), "exclusive scope") {
		t.Fatalf("scope error = %v", err)
	}
}

func TestSyncLabelsIsTargetedAndRetrySafe(t *testing.T) {
	assigned := []string{"unrelated", "ai-review::verdict::needs-review", "ai-review/model/old", "keep"}
	definitions := []string{"ai-review::verdict::approved", "ai-review/model/new", "keep", "unrelated"}
	var updates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/labels"):
			assertCredential(t, request, CredentialAPIToken)
			writer.Header().Set("X-Next-Page", "")
			items := make([]map[string]any, 0, len(definitions))
			for index, name := range definitions {
				items = append(items, map[string]any{"id": index + 1, "name": name})
			}
			writeJSON(t, writer, http.StatusOK, items)
		case request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/merge_requests/7"):
			assertCredential(t, request, CredentialAPIToken)
			updates.Add(1)
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			assigned = applyLabels(assigned, strings.Split(body["add_labels"], ","), strings.Split(body["remove_labels"], ","))
			writeJSON(t, writer, http.StatusOK, map[string]any{"id": 70, "iid": 7, "labels": assigned})
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/merge_requests/7"):
			assertCredential(t, request, CredentialJobToken)
			writeJSON(t, writer, http.StatusOK, map[string]any{"id": 70, "iid": 7, "labels": assigned})
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, testJobToken)
	plan := LabelPlan{
		Desired:         []string{"ai-review::verdict::approved", "ai-review/model/new", "keep"},
		ManagedScopes:   []string{"ai-review::verdict"},
		ManagedPrefixes: []string{"ai-review/model/"},
		ManagedNames:    []string{"keep"},
	}
	first, err := client.SyncLabels(context.Background(), append([]string(nil), assigned...), plan, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Added, []string{"ai-review/model/new", "ai-review::verdict::approved"}) || !reflect.DeepEqual(first.Removed, []string{"ai-review/model/old", "ai-review::verdict::needs-review"}) {
		t.Fatalf("first sync = %#v", first)
	}
	if !containsString(first.Final, "unrelated") {
		t.Fatalf("unrelated label removed: %v", first.Final)
	}
	second, err := client.SyncLabels(context.Background(), append([]string(nil), assigned...), plan, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Added) != 0 || len(second.Removed) != 0 || updates.Load() != 1 {
		t.Fatalf("retry sync = %#v, updates=%d", second, updates.Load())
	}
}

func TestEnsureLabelDefinitionsRecoversUncertainCreate(t *testing.T) {
	created := false
	var creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertCredential(t, request, CredentialAPIToken)
		switch request.Method {
		case http.MethodGet:
			writer.Header().Set("X-Next-Page", "")
			if created {
				writeJSON(t, writer, http.StatusOK, []any{map[string]any{"id": 1, "name": "missing"}})
			} else {
				writeJSON(t, writer, http.StatusOK, []any{})
			}
		case http.MethodPost:
			creates.Add(1)
			created = true
			writer.WriteHeader(http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected method %s", request.Method)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, testJobToken, WithRetryPolicy(2, 0, 0))
	if err := client.ensureLabelDefinitions(context.Background(), []string{"missing"}, true); err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 {
		t.Fatalf("create calls = %d", creates.Load())
	}
}

func applyLabels(current, add, remove []string) []string {
	values := stringSet(current)
	for _, label := range add {
		if label != "" {
			values[label] = struct{}{}
		}
	}
	for _, label := range remove {
		if label != "" {
			delete(values, label)
		}
	}
	return sortedSet(values)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
