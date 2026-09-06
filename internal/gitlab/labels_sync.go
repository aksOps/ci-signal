package gitlab

import (
	"context"
	"fmt"
	"sort"
	"strings"

	gitlabapi "gitlab.com/gitlab-org/api/client-go/v2"
)

type LabelSyncResult struct {
	Added   []string
	Removed []string
	Final   []string
}

func (c *Client) SyncLabels(ctx context.Context, current []string, plan LabelPlan, createMissing bool) (LabelSyncResult, error) {
	if err := validateDesiredScopes(stringSet(plan.Desired)); err != nil {
		return LabelSyncResult{}, err
	}
	if err := c.ensureLabelDefinitions(ctx, plan.Desired, createMissing); err != nil {
		return LabelSyncResult{}, err
	}
	desired := stringSet(plan.Desired)
	currentSet := stringSet(current)
	managedNames := stringSet(plan.ManagedNames)
	managedScopes := stringSet(plan.ManagedScopes)
	add := make([]string, 0)
	remove := make([]string, 0)
	for label := range desired {
		if _, exists := currentSet[label]; !exists {
			add = append(add, label)
		}
	}
	for label := range currentSet {
		if _, keep := desired[label]; keep || !labelManaged(label, managedNames, managedScopes, plan.ManagedPrefixes) {
			continue
		}
		remove = append(remove, label)
	}
	sort.Strings(add)
	sort.Strings(remove)
	if len(add) > 0 || len(remove) > 0 {
		if _, err := c.UpdateLabels(ctx, add, remove); err != nil {
			return LabelSyncResult{}, err
		}
	}
	mergeRequest, _, err := c.GetMergeRequest(ctx)
	if err != nil {
		return LabelSyncResult{}, fmt.Errorf("verify GitLab merge-request labels: %w", err)
	}
	final := append([]string(nil), mergeRequest.Labels...)
	sort.Strings(final)
	if err := verifyManagedLabels(final, plan); err != nil {
		return LabelSyncResult{}, err
	}
	return LabelSyncResult{Added: add, Removed: remove, Final: final}, nil
}

func (c *Client) ensureLabelDefinitions(ctx context.Context, desired []string, createMissing bool) error {
	labels, _, err := c.ListLabels(ctx)
	if err != nil {
		return err
	}
	existing := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		if label != nil {
			existing[label.Name] = struct{}{}
		}
	}
	for _, name := range desired {
		if _, exists := existing[name]; exists {
			continue
		}
		if !createMissing {
			return fmt.Errorf("required GitLab label %q does not exist", name)
		}
		if _, err := c.CreateLabel(ctx, name, defaultCreatedLabelColor, "Managed by ci-signal"); err != nil {
			labels, _, readErr := c.ListLabels(ctx)
			if readErr != nil || !containsLabel(labels, name) {
				return fmt.Errorf("create GitLab label %q: %w", name, err)
			}
		}
		existing[name] = struct{}{}
	}
	return nil
}

func containsLabel(labels []*gitlabapi.Label, name string) bool {
	for _, label := range labels {
		if label != nil && label.Name == name {
			return true
		}
	}
	return false
}

func labelManaged(label string, names, scopes map[string]struct{}, prefixes []string) bool {
	if _, ok := names[label]; ok {
		return true
	}
	if _, ok := scopes[scopedLabelKey(label)]; ok {
		return true
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(label, prefix) {
			return true
		}
	}
	return false
}

func verifyManagedLabels(current []string, plan LabelPlan) error {
	desired := stringSet(plan.Desired)
	currentSet := stringSet(current)
	for label := range desired {
		if _, ok := currentSet[label]; !ok {
			return fmt.Errorf("GitLab label verification is missing %q", label)
		}
	}
	names := stringSet(plan.ManagedNames)
	scopes := stringSet(plan.ManagedScopes)
	for label := range currentSet {
		if labelManaged(label, names, scopes, plan.ManagedPrefixes) {
			if _, ok := desired[label]; !ok {
				return fmt.Errorf("GitLab label verification found stale managed label %q", label)
			}
		}
	}
	return nil
}
