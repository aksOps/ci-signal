package review

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed submit_review.schema.json
var schemaFiles embed.FS

const submissionSchemaURL = "https://ci-signal.local/schema/submit-review-v1.json"

type Validator struct {
	schema *jsonschema.Schema
}

func NewValidator() (*Validator, error) {
	document, err := SubmissionSchema()
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(submissionSchemaURL, document); err != nil {
		return nil, fmt.Errorf("register submit_review schema: %w", err)
	}
	compiled, err := compiler.Compile(submissionSchemaURL)
	if err != nil {
		return nil, fmt.Errorf("compile submit_review schema: %w", err)
	}
	return &Validator{schema: compiled}, nil
}

func SubmissionSchema() (map[string]any, error) {
	raw, err := schemaFiles.ReadFile("submit_review.schema.json")
	if err != nil {
		return nil, fmt.Errorf("read submit_review schema: %w", err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("decode submit_review schema: %w", err)
	}
	return document, nil
}

func (v *Validator) Validate(raw []byte, assignment Assignment) (Submission, error) {
	if v == nil || v.schema == nil {
		return Submission{}, errors.New("submit_review validator is not initialized")
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return Submission{}, fmt.Errorf("decode submit_review arguments: %w", err)
	}
	if err := v.schema.Validate(document); err != nil {
		return Submission{}, fmt.Errorf("submit_review schema validation: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var submission Submission
	if err := decoder.Decode(&submission); err != nil {
		return Submission{}, fmt.Errorf("decode validated submit_review arguments: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Submission{}, err
	}
	if err := validateSubmission(submission, assignment); err != nil {
		return Submission{}, err
	}
	return submission, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("submit_review arguments contain more than one JSON value")
	}
	return fmt.Errorf("decode trailing submit_review data: %w", err)
}

func validateSubmission(submission Submission, assignment Assignment) error {
	for _, finding := range submission.Findings {
		if err := ValidateProse(finding.Title); err != nil {
			return fmt.Errorf("finding title: %w", err)
		}
		if err := ValidateProse(finding.Explanation); err != nil {
			return fmt.Errorf("finding explanation: %w", err)
		}
	}
	for _, reassessment := range submission.Reassessments {
		if err := ValidateProse(reassessment.Explanation); err != nil {
			return fmt.Errorf("reassessment explanation: %w", err)
		}
	}
	for _, transition := range submission.AcknowledgementChanges {
		if err := ValidateProse(transition.Explanation); err != nil {
			return fmt.Errorf("acknowledgement explanation: %w", err)
		}
	}
	for _, coverage := range submission.Coverage {
		if err := ValidateProse(coverage.Explanation); err != nil {
			return fmt.Errorf("coverage explanation: %w", err)
		}
	}
	for _, limitation := range submission.Limitations {
		if err := ValidateProse(limitation); err != nil {
			return fmt.Errorf("limitation: %w", err)
		}
	}
	if err := validateCoverage(submission, assignment); err != nil {
		return err
	}
	for i, finding := range submission.Findings {
		if err := validateUnits(finding.AssignedUnits, assignment.UnitIDs); err != nil {
			return fmt.Errorf("finding %d: %w", i, err)
		}
		if err := validateEvidence(finding.Evidence, assignment.SourceRefs); err != nil {
			return fmt.Errorf("finding %d: %w", i, err)
		}
	}

	reassessments := make(map[FindingID]Reassessment, len(submission.Reassessments))
	for i, reassessment := range submission.Reassessments {
		if _, ok := assignment.Findings[reassessment.FindingID]; !ok {
			return fmt.Errorf("reassessment %d references foreign finding %q", i, reassessment.FindingID)
		}
		if _, duplicate := reassessments[reassessment.FindingID]; duplicate {
			return fmt.Errorf("reassessment %d duplicates finding %q", i, reassessment.FindingID)
		}
		reassessments[reassessment.FindingID] = reassessment
		if err := validateUnits(reassessment.AssignedUnits, assignment.UnitIDs); err != nil {
			return fmt.Errorf("reassessment %d: %w", i, err)
		}
		if err := validateReassessmentEvidence(reassessment, assignment.SourceRefs); err != nil {
			return fmt.Errorf("reassessment %d: %w", i, err)
		}
	}
	if submission.Completion == SubmissionComplete {
		for findingID := range assignment.Findings {
			if _, ok := reassessments[findingID]; !ok {
				return fmt.Errorf("complete submission is missing reassessment for known finding %q", findingID)
			}
		}
	}

	transitions := make(map[FindingID]AcknowledgementTransition, len(submission.AcknowledgementChanges))
	for i, transition := range submission.AcknowledgementChanges {
		known, ok := assignment.Findings[transition.FindingID]
		if !ok {
			return fmt.Errorf("acknowledgement change %d references foreign finding %q", i, transition.FindingID)
		}
		if _, duplicate := transitions[transition.FindingID]; duplicate {
			return fmt.Errorf("acknowledgement change %d duplicates finding %q", i, transition.FindingID)
		}
		transitions[transition.FindingID] = transition
		if transition.Method == AcknowledgementCheckbox {
			return fmt.Errorf("acknowledgement change %d claims host-only checkbox method", i)
		}
		reassessment, reassessed := reassessments[transition.FindingID]
		if err := validateTransitionEvidence(transition, known, reassessment, reassessed, assignment.SourceRefs); err != nil {
			return fmt.Errorf("acknowledgement change %d: %w", i, err)
		}
		if reassessment, ok := reassessments[transition.FindingID]; ok && reassessment.Assessment == AssessmentAddressed && transition.Method != AcknowledgementAICodeChange {
			return fmt.Errorf("acknowledgement change %d cannot mark a finding addressed from discussion evidence", i)
		}
		switch transition.Action {
		case AcknowledgementActionAcknowledge:
			if known.State != FindingOpen {
				return fmt.Errorf("acknowledgement change %d cannot acknowledge finding in state %q", i, known.State)
			}
		case AcknowledgementActionReopen:
			if known.State != FindingAcknowledged || known.Acknowledgement == nil || known.Acknowledgement.Method == AcknowledgementCheckbox {
				return fmt.Errorf("acknowledgement change %d can reopen only an AI-acknowledged finding", i)
			}
			if known.Acknowledgement.Method != transition.Method {
				return fmt.Errorf("acknowledgement change %d method does not match stored acknowledgement", i)
			}
			reassessment, ok := reassessments[transition.FindingID]
			if !ok || (reassessment.Assessment != AssessmentPresent && reassessment.Assessment != AssessmentUnknown) {
				return fmt.Errorf("acknowledgement change %d reopen requires an explicit present or unknown reassessment", i)
			}
		}
	}
	for findingID, reassessment := range reassessments {
		known := assignment.Findings[findingID]
		if known.State != FindingOpen || reassessment.Assessment != AssessmentAddressed {
			continue
		}
		transition, ok := transitions[findingID]
		if !ok || transition.Action != AcknowledgementActionAcknowledge || transition.Method != AcknowledgementAICodeChange {
			return fmt.Errorf("addressed open finding %q requires an ai_code_change acknowledgement", findingID)
		}
	}
	return nil
}

func validateCoverage(submission Submission, assignment Assignment) error {
	seen := make(map[ReviewUnitID]struct{}, len(submission.Coverage))
	outcomes := make(map[ReviewUnitID]CoverageOutcome, len(submission.Coverage))
	for i, coverage := range submission.Coverage {
		if _, ok := assignment.UnitIDs[coverage.UnitID]; !ok {
			return fmt.Errorf("coverage %d references foreign unit %q", i, coverage.UnitID)
		}
		if _, duplicate := seen[coverage.UnitID]; duplicate {
			return fmt.Errorf("coverage %d duplicates unit %q", i, coverage.UnitID)
		}
		seen[coverage.UnitID] = struct{}{}
		outcomes[coverage.UnitID] = coverage.Outcome
		if submission.Completion == SubmissionComplete && coverage.Outcome != CoverageComplete && coverage.Outcome != CoverageExcludedByPolicy {
			return fmt.Errorf("complete submission reports unit %q as %q", coverage.UnitID, coverage.Outcome)
		}
	}
	for unit := range assignment.UnitIDs {
		if _, ok := seen[unit]; !ok {
			return fmt.Errorf("coverage is missing assigned unit %q", unit)
		}
	}
	for unitID, requirement := range assignment.UnitRequirements {
		if _, ok := assignment.UnitIDs[unitID]; !ok {
			return fmt.Errorf("host coverage requirement references foreign unit %q", unitID)
		}
		if requirement.EnclosingUnitID != "" {
			if requirement.EnclosingUnitID == unitID {
				return fmt.Errorf("host coverage requirement for %q references itself as enclosing unit", unitID)
			}
			if _, ok := assignment.UnitIDs[requirement.EnclosingUnitID]; !ok {
				return fmt.Errorf("host coverage requirement for %q references foreign enclosing unit %q", unitID, requirement.EnclosingUnitID)
			}
		}
		if outcomes[unitID] != CoverageComplete {
			continue
		}
		if !requirement.RetrievalComplete {
			return fmt.Errorf("coverage marks unit %q complete while host retrieval is incomplete", unitID)
		}
		if requirement.EnclosingUnitID != "" && outcomes[requirement.EnclosingUnitID] != CoverageComplete {
			return fmt.Errorf("coverage marks unit %q complete without complete enclosing unit %q", unitID, requirement.EnclosingUnitID)
		}
	}
	return nil
}

func validateUnits(references []ReviewUnitID, units map[ReviewUnitID]struct{}) error {
	for _, unit := range references {
		if _, ok := units[unit]; !ok {
			return fmt.Errorf("references foreign assigned unit %q", unit)
		}
	}
	return nil
}

func validateEvidence(evidence []Evidence, sources map[SourceReferenceID]SourceReference) error {
	for i, item := range evidence {
		if err := ValidateProse(item.Explanation); err != nil {
			return fmt.Errorf("evidence %d: %w", i, err)
		}
		for _, source := range item.SourceRefs {
			if _, ok := sources[source]; !ok {
				return fmt.Errorf("evidence %d references foreign source %q", i, source)
			}
		}
		for j, location := range item.Locations {
			if err := validateLocation(location); err != nil {
				return fmt.Errorf("evidence %d location %d: %w", i, j, err)
			}
		}
	}
	return nil
}

func validateReassessmentEvidence(reassessment Reassessment, sources map[SourceReferenceID]SourceReference) error {
	if err := validateEvidence(reassessment.Evidence, sources); err != nil {
		return err
	}
	for _, item := range reassessment.Evidence {
		for _, sourceID := range item.SourceRefs {
			source := sources[sourceID]
			if source.FindingID != "" && source.FindingID != reassessment.FindingID {
				return fmt.Errorf("source %q is associated with finding %q, not %q", sourceID, source.FindingID, reassessment.FindingID)
			}
		}
	}
	return nil
}

func validateLocation(location Location) error {
	cleaned := path.Clean(strings.ReplaceAll(location.Path, "\\", "/"))
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return fmt.Errorf("path %q is not a repository-relative literal path", location.Path)
	}
	if location.EndLine > 0 && location.StartLine == 0 {
		return errors.New("end_line requires start_line")
	}
	if location.EndLine > 0 && location.EndLine < location.StartLine {
		return errors.New("end_line precedes start_line")
	}
	return nil
}

func validateTransitionEvidence(transition AcknowledgementTransition, known KnownFinding, reassessment Reassessment, reassessed bool, sources map[SourceReferenceID]SourceReference) error {
	reassessmentSources := make(map[SourceReferenceID]struct{})
	if reassessed {
		for _, evidence := range reassessment.Evidence {
			for _, sourceID := range evidence.SourceRefs {
				reassessmentSources[sourceID] = struct{}{}
			}
		}
	}
	for _, id := range transition.SourceRefs {
		source, ok := sources[id]
		if !ok {
			return fmt.Errorf("references foreign source %q", id)
		}
		if source.Consumed {
			return fmt.Errorf("source %q was already consumed", id)
		}
		if _, consumed := known.ConsumedSourceRefs[id]; consumed {
			return fmt.Errorf("source %q was already consumed for finding %q", id, transition.FindingID)
		}
		switch transition.Method {
		case AcknowledgementAIDiscussion:
			if source.FindingID != transition.FindingID {
				return fmt.Errorf("source %q is not associated with finding %q", id, transition.FindingID)
			}
			if (source.Kind != SourceGitLabDiscussion && source.Kind != SourceGitLabNote) || !source.Human {
				return fmt.Errorf("source %q is not a relevant human discussion response", id)
			}
		case AcknowledgementAICodeChange:
			if source.Kind != SourceRepositoryDiff && source.Kind != SourceRepositorySource && source.Kind != SourceRepositoryAST {
				return fmt.Errorf("source %q is not repository change evidence", id)
			}
			if source.FindingID != "" && source.FindingID != transition.FindingID {
				return fmt.Errorf("source %q is associated with finding %q, not %q", id, source.FindingID, transition.FindingID)
			}
			if transition.Action == AcknowledgementActionAcknowledge {
				if !reassessed || reassessment.Assessment != AssessmentAddressed {
					return errors.New("code-change acknowledgement requires an addressed reassessment")
				}
				if _, ok := reassessmentSources[id]; !ok {
					return fmt.Errorf("source %q is not cited by the addressed reassessment evidence", id)
				}
			} else if source.FindingID != transition.FindingID {
				return fmt.Errorf("source %q is not associated with finding %q", id, transition.FindingID)
			}
		default:
			return fmt.Errorf("unsupported AI acknowledgement method %q", transition.Method)
		}
	}
	return nil
}
