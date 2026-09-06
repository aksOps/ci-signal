package review

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"
)

type ReconcileMetadata struct {
	RunID            RunID
	At               time.Time
	ConfidentMatches map[int]FindingID
	NewFindingIDs    map[int]FindingID
}

type Reconciler struct {
	newFindingID func() (FindingID, error)
}

func NewReconciler() *Reconciler {
	return &Reconciler{newFindingID: randomFindingID}
}

func (r *Reconciler) Reconcile(previous []Finding, submission Submission, metadata ReconcileMetadata) ([]Finding, error) {
	if r == nil || r.newFindingID == nil {
		return nil, errors.New("reconciler is not initialized")
	}
	if metadata.At.IsZero() || metadata.RunID == "" {
		return nil, errors.New("host reconciliation run and timestamp are required")
	}
	result := append([]Finding(nil), previous...)
	priorByID := make(map[FindingID]int, len(result))
	for i := range result {
		priorByID[result[i].ID] = i
	}
	matchedPrior := make(map[FindingID]struct{}, len(metadata.ConfidentMatches))
	for submissionIndex, findingID := range metadata.ConfidentMatches {
		if submissionIndex < 0 || submissionIndex >= len(submission.Findings) {
			return nil, errors.New("confident finding match has an invalid submission index")
		}
		if _, ok := priorByID[findingID]; !ok {
			return nil, errors.New("confident finding match references absent prior state")
		}
		if _, duplicate := matchedPrior[findingID]; duplicate {
			return nil, errors.New("confident finding match reuses a prior finding")
		}
		matchedPrior[findingID] = struct{}{}
	}

	for submissionIndex, submitted := range submission.Findings {
		key := FindingIdentityKey(submitted)
		if findingID, matched := metadata.ConfidentMatches[submissionIndex]; matched {
			finding := &result[priorByID[findingID]]
			finding.IdentityKey = key
			finding.Category = submitted.Category
			finding.Subcategory = submitted.Subcategory
			finding.Relationship = submitted.Relationship
			finding.Title = submitted.Title
			finding.Explanation = submitted.Explanation
			finding.Evidence = submitted.Evidence
			finding.AssignedUnits = submitted.AssignedUnits
			finding.Assessment = AssessmentPresent
			finding.LastSeenAt = metadata.At.UTC()
			finding.History = append(finding.History, FindingEvent{Kind: FindingEventReassessed, At: metadata.At.UTC(), RunID: metadata.RunID, Assessment: AssessmentPresent, SourceRefs: evidenceSourceRefs(submitted.Evidence)})
			continue
		}
		id := metadata.NewFindingIDs[submissionIndex]
		if id == "" {
			var err error
			id, err = r.newFindingID()
			if err != nil {
				return nil, err
			}
		}
		if _, exists := priorByID[id]; exists {
			return nil, errors.New("host new finding ID collides with prior state")
		}
		created := Finding{
			ID:            id,
			IdentityKey:   key,
			Category:      submitted.Category,
			Subcategory:   submitted.Subcategory,
			Relationship:  submitted.Relationship,
			Title:         submitted.Title,
			Explanation:   submitted.Explanation,
			Evidence:      submitted.Evidence,
			AssignedUnits: submitted.AssignedUnits,
			Assessment:    AssessmentPresent,
			State:         FindingOpen,
			History:       []FindingEvent{{Kind: FindingEventCreated, At: metadata.At.UTC(), RunID: metadata.RunID, Assessment: AssessmentPresent, SourceRefs: evidenceSourceRefs(submitted.Evidence)}},
			FirstSeenAt:   metadata.At.UTC(),
			LastSeenAt:    metadata.At.UTC(),
		}
		priorByID[id] = len(result)
		result = append(result, created)
	}

	for _, reassessment := range submission.Reassessments {
		index, ok := priorByID[reassessment.FindingID]
		if !ok {
			return nil, errors.New("validated reassessment finding is absent from prior state")
		}
		finding := &result[index]
		finding.Assessment = reassessment.Assessment
		finding.Explanation = reassessment.Explanation
		finding.Evidence = reassessment.Evidence
		finding.AssignedUnits = reassessment.AssignedUnits
		finding.LastSeenAt = metadata.At.UTC()
		finding.History = append(finding.History, FindingEvent{Kind: FindingEventReassessed, At: metadata.At.UTC(), RunID: metadata.RunID, Assessment: reassessment.Assessment, SourceRefs: evidenceSourceRefs(reassessment.Evidence)})
	}

	for _, transition := range submission.AcknowledgementChanges {
		index, ok := priorByID[transition.FindingID]
		if !ok {
			return nil, errors.New("validated acknowledgement finding is absent from prior state")
		}
		finding := &result[index]
		switch transition.Action {
		case AcknowledgementActionAcknowledge:
			finding.State = FindingAcknowledged
			finding.Acknowledgement = &Acknowledgement{Method: transition.Method, SourceRefs: append([]SourceReferenceID(nil), transition.SourceRefs...), At: metadata.At.UTC()}
			finding.History = append(finding.History, FindingEvent{Kind: FindingEventAcknowledged, At: metadata.At.UTC(), RunID: metadata.RunID, Method: transition.Method, SourceRefs: append([]SourceReferenceID(nil), transition.SourceRefs...)})
		case AcknowledgementActionReopen:
			finding.State = FindingOpen
			finding.Acknowledgement = nil
			finding.History = append(finding.History, FindingEvent{Kind: FindingEventReopened, At: metadata.At.UTC(), RunID: metadata.RunID, Method: transition.Method, SourceRefs: append([]SourceReferenceID(nil), transition.SourceRefs...)})
		}
	}
	return result, nil
}

func evidenceSourceRefs(evidence []Evidence) []SourceReferenceID {
	seen := make(map[SourceReferenceID]struct{})
	var result []SourceReferenceID
	for _, item := range evidence {
		for _, id := range item.SourceRefs {
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			result = append(result, id)
		}
	}
	return result
}

func FindingIdentityKey(finding SubmittedFinding) string {
	units := make([]string, 0, len(finding.AssignedUnits))
	for _, unit := range finding.AssignedUnits {
		units = append(units, string(unit))
	}
	sort.Strings(units)
	payload := strings.Join([]string{string(finding.Category), string(finding.Subcategory), strings.Join(units, "\x00")}, "\x00")
	digest := sha256.Sum256([]byte(payload))
	return "finding-v1:" + hex.EncodeToString(digest[:])
}

func randomFindingID() (FindingID, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return FindingID("f_" + hex.EncodeToString(value)), nil
}
