#!/usr/bin/env bash
set -euo pipefail

readonly GOTOOLCHAIN=go1.26.5
export GOTOOLCHAIN

if [[ "$(go env GOVERSION)" != "go1.26.5" ]]; then
  echo "required Go toolchain go1.26.5 is unavailable" >&2
  exit 1
fi

go test ./cmd/ci-signal -run '^(TestHelpExitsZeroWithoutConfiguration|TestLoadIsIndependentOfLaunchDirectory|TestPackagedExampleConfigLoadsFromUnrelatedDirectory)$'
go test ./internal/config -run '^TestLoadTokenBudgetsDefaultOnlyWhenOmitted$'
go test ./internal/review -run '^(TestValidatorRejectsSchemaAndAssignmentViolations|TestValidatorRequiresSupportedFreshAcknowledgementEvidence|TestValidatorDiscussionAcknowledgementCannotMarkFindingAddressed|TestDeriveVerdictIsConservative|TestAcceptorPersistsBeforeReceiptAndRejectsConflictingRetry|TestReconcileHistoryRetainsEvidenceProvenance)$'
go test ./internal/copilot -run '^(TestEngineRunUsesPinnedBYOKAndAcceptsStructuredSubmission|TestEngineRunRejectsMissingInvalidAndUninvokedSubmission|TestEngineRejectsMissingBYOKAndSubscriptionAuthentication|TestEngineProtectsExistingCopilotHome|TestDiagnosticsProtectExistingLogFile|TestTelemetryRejectsModelOrAuthenticationFallbackAndDeduplicatesUsage|TestDisabledTokenBudgetsOmitProviderCapsAndRetainTelemetry|TestTokenBudgetsEnforceOnlyPositiveCumulativeLimits)$'
go test ./internal/repository -run '^(TestSnapshotInventoryAndPinnedRetrieval|TestGuidanceMaterializationUsesHeadObjectsAndRejectsSymlinks|TestExtractionLimitFallsBackToEnclosingUnit|TestConfiguredStructuralScanCoversFullProjectWithoutExpandingMRImpactCoverage)$'
go test ./internal/markdown -run '^(TestCodecRoundTripCanonicalState|TestReconcileControlsCheckUncheckAndIdempotence|TestLateHumanUncheckOverridesOlderSuccessorAIAcknowledgement|TestAIControlTamperingIsInert|TestSampleMarkdown)$'
go test ./internal/gitlab -run '^(TestReadFallbackStatusesAndCacheScope|TestMutationMethodsUseOnlyAPITokenAndTargetedLabels|TestDeriveLabelsTreatsIncompleteUsageAsUnknown|TestSyncLabelsIsTargetedAndRetrySafe|TestPublishRecoversUncertainCreateAndReplacesPredecessor|TestPublishReconcilesLateHumanCheckAndUncheckAcrossReplacement|TestPublishRetainsReferencedHumanSourceTextAndAttributionBeforeDelete)$'
go test ./internal/coordinator -run '^(TestUnchangedAndCheckboxOnlyRerunsAvoidAI|TestContextChangeBatchesAllUnitsAndCrossFileFinding|TestPartialAndStaleRunsCannotApproveOrOverwrite|TestAcceptedCheckpointReusedOnlyForIdenticalFingerprint|TestSessionBudgetRecordsEveryOmittedUnitFailed|TestConfiguredConcurrencyBoundsParallelBatches|TestFingerprintIncludesReviewBudgetsButExcludesDiagnosticsAndLabels|TestRepositoryToolsReturnLosslessReadableChunks)$'
build_dir="$(mktemp -d)"
readonly build_dir
trap 'rm -rf "$build_dir"' EXIT
go build -buildvcs=false -o "$build_dir/ci-signal" ./cmd/ci-signal

if [[ -n "${COPILOT_CLI_PATH:-}" ]]; then
  go test ./internal/copilot -run '^TestPinnedCLIRuntimeContract$'
else
  echo "COPILOT_CLI_PATH is unset; skipped the real CLI/local Responses protocol fixture" >&2
fi
