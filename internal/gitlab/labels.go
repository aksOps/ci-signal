package gitlab

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"ci-signal/internal/config"
	"ci-signal/internal/review"
)

const defaultCreatedLabelColor = "#428BCA"

type LabelInput struct {
	Verdict   review.Verdict
	Coverage  review.CoverageOutcome
	Telemetry review.Telemetry
}

type LabelPlan struct {
	Desired         []string
	ManagedNames    []string
	ManagedScopes   []string
	ManagedPrefixes []string
	TokenUsage      *uint64
	UsageComplete   bool
}

func DeriveLabels(settings config.Labels, metadata config.Metadata, input LabelInput) (LabelPlan, error) {
	desired := stringSet(settings.Static)
	managedNames := stringSet(settings.ReservedNames)
	managedScopes := stringSet(settings.ReservedScopes)
	managedPrefixes := make(map[string]struct{})
	for _, label := range settings.Static {
		managedNames[label] = struct{}{}
		if scope := scopedLabelKey(label); scope != "" {
			managedScopes[scope] = struct{}{}
		}
	}

	usage, usageComplete, err := reportedTokenUsage(input.Telemetry)
	if err != nil {
		return LabelPlan{}, err
	}
	for _, mapping := range settings.Dynamic {
		for _, label := range mapping.Values {
			managedNames[label] = struct{}{}
		}
		for _, bucket := range mapping.TokenBuckets {
			managedNames[bucket.Label] = struct{}{}
		}
		if mapping.Mode == config.LabelSet && mapping.Prefix != "" {
			managedPrefixes[mapping.Prefix] = struct{}{}
		}
		if mapping.Mode == config.LabelSingle {
			for _, label := range append(mappingValues(mapping), mapping.Prefix+"value") {
				if scope := scopedLabelKey(label); scope != "" {
					managedScopes[scope] = struct{}{}
				}
			}
		}

		labels, err := labelsForMapping(mapping, metadata, input, usage, usageComplete)
		if err != nil {
			return LabelPlan{}, fmt.Errorf("derive dynamic label %q: %w", mapping.Name, err)
		}
		for _, label := range labels {
			if err := validateDerivedLabel(label); err != nil {
				return LabelPlan{}, fmt.Errorf("derive dynamic label %q: %w", mapping.Name, err)
			}
			desired[label] = struct{}{}
		}
	}
	if err := validateDesiredScopes(desired); err != nil {
		return LabelPlan{}, err
	}
	plan := LabelPlan{
		Desired:         sortedSet(desired),
		ManagedNames:    sortedSet(managedNames),
		ManagedScopes:   sortedSet(managedScopes),
		ManagedPrefixes: sortedSet(managedPrefixes),
		UsageComplete:   usageComplete,
	}
	if usageComplete {
		value := usage
		plan.TokenUsage = &value
	}
	return plan, nil
}

func labelsForMapping(mapping config.LabelMapping, metadata config.Metadata, input LabelInput, usage uint64, usageComplete bool) ([]string, error) {
	switch mapping.Source {
	case config.LabelFromVerdict:
		return mappedSingle(mapping, string(input.Verdict))
	case config.LabelFromCoverage:
		return mappedSingle(mapping, string(input.Coverage))
	case config.LabelFromTeam:
		return mappedSingle(mapping, metadata.Team)
	case config.LabelFromMetadata:
		return mappedSingle(mapping, metadata.Values[mapping.MetadataKey])
	case config.LabelFromRequestedModels:
		return mappedSet(mapping, input.Telemetry.RequestedModels)
	case config.LabelFromObservedModels:
		values := make([]string, 0, len(input.Telemetry.ObservedModels))
		for _, observation := range input.Telemetry.ObservedModels {
			values = append(values, observation.Model)
		}
		return mappedSet(mapping, values)
	case config.LabelFromExecutedTools:
		values := make([]string, 0, len(input.Telemetry.Tools))
		for _, execution := range input.Telemetry.Tools {
			values = append(values, execution.Tool)
		}
		return mappedSet(mapping, values)
	case config.LabelFromTokenUsage:
		if !usageComplete {
			return missingLabels(mapping)
		}
		if mapping.TokenFormat == config.TokenLabelExact {
			if mapping.Prefix == "" {
				return nil, errors.New("exact token labels require a prefix")
			}
			return []string{mapping.Prefix + strconv.FormatUint(usage, 10)}, nil
		}
		for _, bucket := range mapping.TokenBuckets {
			if bucket.LessThan == 0 || usage < bucket.LessThan {
				return []string{bucket.Label}, nil
			}
		}
		return nil, errors.New("token usage does not match a configured bucket")
	default:
		return nil, fmt.Errorf("unsupported label source %q", mapping.Source)
	}
}

func mappedSingle(mapping config.LabelMapping, value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return missingLabels(mapping)
	}
	label, ok := mapping.Values[value]
	if !ok {
		return missingLabels(mapping)
	}
	return []string{label}, nil
}

func mappedSet(mapping config.LabelMapping, values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			seen[value] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return missingLabels(mapping)
	}
	labels := make([]string, 0, len(seen))
	for value := range seen {
		labels = append(labels, mapping.Prefix+value)
	}
	sort.Strings(labels)
	return labels, nil
}

func missingLabels(mapping config.LabelMapping) ([]string, error) {
	if mapping.Missing == config.MissingOmit {
		return nil, nil
	}
	if label := mapping.Values["unknown"]; label != "" {
		return []string{label}, nil
	}
	if mapping.Prefix != "" {
		return []string{mapping.Prefix + "unknown"}, nil
	}
	return nil, errors.New("missing-value policy requires an unknown label")
}

func reportedTokenUsage(telemetry review.Telemetry) (uint64, bool, error) {
	if !telemetry.UsageComplete || len(telemetry.Usage) == 0 {
		return 0, false, nil
	}
	type sessionUsage struct {
		delta         uint64
		cumulativeMax uint64
		hasCumulative bool
	}
	sessions := make(map[string]sessionUsage)
	events := make(map[string]review.UsageEvent, len(telemetry.Usage))
	for _, event := range telemetry.Usage {
		if event.EventID == "" || event.SessionID == "" {
			return 0, false, errors.New("complete usage contains an event without identity")
		}
		if prior, duplicate := events[event.EventID]; duplicate {
			if prior != event {
				return 0, false, fmt.Errorf("usage event %q has conflicting counters", event.EventID)
			}
			continue
		}
		events[event.EventID] = event
		value, err := usageEventTotal(event)
		if err != nil {
			return 0, false, fmt.Errorf("usage event %q: %w", event.EventID, err)
		}
		session := sessions[event.SessionID]
		if event.Cumulative {
			session.hasCumulative = true
			if value > session.cumulativeMax {
				session.cumulativeMax = value
			}
		} else if session.delta, err = addUint64(session.delta, value); err != nil {
			return 0, false, err
		}
		sessions[event.SessionID] = session
	}
	var total uint64
	for _, session := range sessions {
		value := session.delta
		if session.hasCumulative {
			value = session.cumulativeMax
		}
		var err error
		if total, err = addUint64(total, value); err != nil {
			return 0, false, err
		}
	}
	return total, true, nil
}

func usageEventTotal(event review.UsageEvent) (uint64, error) {
	components, err := addUint64(event.InputTokens, event.OutputTokens)
	if err != nil {
		return 0, err
	}
	if event.TotalTokens != 0 {
		return event.TotalTokens, nil
	}
	return components, nil
}

func addUint64(left, right uint64) (uint64, error) {
	if math.MaxUint64-left < right {
		return 0, errors.New("token usage overflows uint64")
	}
	return left + right, nil
}

func mappingValues(mapping config.LabelMapping) []string {
	result := make([]string, 0, len(mapping.Values)+len(mapping.TokenBuckets))
	for _, label := range mapping.Values {
		result = append(result, label)
	}
	for _, bucket := range mapping.TokenBuckets {
		result = append(result, bucket.Label)
	}
	return result
}

func validateDesiredScopes(labels map[string]struct{}) error {
	scopes := make(map[string]string)
	for label := range labels {
		scope := scopedLabelKey(label)
		if scope == "" {
			continue
		}
		if prior, conflict := scopes[scope]; conflict && prior != label {
			return fmt.Errorf("desired labels %q and %q conflict in exclusive scope %q", prior, label, scope)
		}
		scopes[scope] = label
	}
	return nil
}

func scopedLabelKey(label string) string {
	index := strings.LastIndex(label, "::")
	if index < 1 || index+2 == len(label) {
		return ""
	}
	return label[:index]
}

func validateDerivedLabel(label string) error {
	if strings.TrimSpace(label) == "" || len(label) > 255 || strings.ContainsAny(label, "\r\n") {
		return fmt.Errorf("invalid generated label %q", label)
	}
	return nil
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
