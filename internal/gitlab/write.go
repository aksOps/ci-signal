package gitlab

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	gitlabapi "gitlab.com/gitlab-org/api/client-go/v2"
)

func (c *Client) CreateNote(ctx context.Context, body string) (*gitlabapi.Note, error) {
	if strings.TrimSpace(body) == "" {
		return nil, errors.New("GitLab note body cannot be empty")
	}
	c.logWrite("create_note")
	value, _, err := c.api.Notes.CreateMergeRequestNote(c.project, c.mrIID, &gitlabapi.CreateMergeRequestNoteOptions{Body: &body}, gitlabapi.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("create GitLab merge-request note: %w", err)
	}
	return value, nil
}

func (c *Client) UpdateNote(ctx context.Context, noteID int64, body string) (*gitlabapi.Note, error) {
	if noteID < 1 || strings.TrimSpace(body) == "" {
		return nil, errors.New("positive GitLab note ID and nonempty body are required")
	}
	c.logWrite("update_note")
	value, _, err := c.api.Notes.UpdateMergeRequestNote(c.project, c.mrIID, noteID, &gitlabapi.UpdateMergeRequestNoteOptions{Body: &body}, gitlabapi.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("update GitLab merge-request note: %w", err)
	}
	return value, nil
}

func (c *Client) DeleteNote(ctx context.Context, noteID int64) error {
	if noteID < 1 {
		return errors.New("GitLab note ID must be positive")
	}
	c.logWrite("delete_note")
	if _, err := c.api.Notes.DeleteMergeRequestNote(c.project, c.mrIID, noteID, gitlabapi.WithContext(ctx)); err != nil {
		return fmt.Errorf("delete GitLab merge-request note: %w", err)
	}
	return nil
}

func (c *Client) CreateLabel(ctx context.Context, name, color, description string) (*gitlabapi.Label, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(color) == "" {
		return nil, errors.New("GitLab label name and color are required")
	}
	c.logWrite("create_label")
	value, _, err := c.api.Labels.CreateLabel(c.project, &gitlabapi.CreateLabelOptions{Name: &name, Color: &color, Description: &description}, gitlabapi.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("create GitLab project label: %w", err)
	}
	return value, nil
}

func (c *Client) UpdateLabels(ctx context.Context, add, remove []string) (*gitlabapi.MergeRequest, error) {
	add = uniqueNonempty(add)
	remove = uniqueNonempty(remove)
	if overlap := commonLabel(add, remove); overlap != "" {
		return nil, fmt.Errorf("GitLab label %q cannot be both added and removed", overlap)
	}
	if len(add) == 0 && len(remove) == 0 {
		return nil, errors.New("GitLab label update has no assignment differences")
	}
	options := &gitlabapi.UpdateMergeRequestOptions{}
	if len(add) > 0 {
		labels := gitlabapi.LabelOptions(add)
		options.AddLabels = &labels
	}
	if len(remove) > 0 {
		labels := gitlabapi.LabelOptions(remove)
		options.RemoveLabels = &labels
	}
	c.logWrite("update_labels")
	value, _, err := c.api.MergeRequests.UpdateMergeRequest(c.project, c.mrIID, options, gitlabapi.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("update GitLab merge-request labels: %w", err)
	}
	return value, nil
}

func uniqueNonempty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			seen[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func commonLabel(left, right []string) string {
	seen := make(map[string]struct{}, len(left))
	for _, value := range left {
		seen[value] = struct{}{}
	}
	for _, value := range right {
		if _, ok := seen[value]; ok {
			return value
		}
	}
	return ""
}
