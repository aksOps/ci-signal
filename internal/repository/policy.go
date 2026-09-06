package repository

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"strings"
)

// isTestPath recognizes conventional test locations, not arbitrary occurrences
// of "test" inside production names. Unusual layouts use trusted excluded paths.
func isTestPath(filePath string) bool {
	for _, segment := range strings.Split(filePath, "/") {
		switch strings.ToLower(segment) {
		case "test", "tests", "__tests__", "__mocks__", "testdata":
			return true
		}
		if strings.HasSuffix(segment, ".Tests") || strings.HasSuffix(segment, "Tests") && segment != path.Base(filePath) {
			return true
		}
	}
	base := path.Base(filePath)
	lower := strings.ToLower(base)
	ext := path.Ext(lower)
	switch ext {
	case ".go":
		return strings.HasSuffix(lower, "_test.go")
	case ".py":
		return strings.HasPrefix(lower, "test_") || strings.HasSuffix(lower, "_test.py") || lower == "conftest.py"
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".mts", ".cts":
		stem := strings.TrimSuffix(lower, ext)
		return strings.HasSuffix(stem, ".test") || strings.HasSuffix(stem, ".spec")
	case ".java":
		return strings.HasPrefix(base, "Test") && len(base) > 4 && base[4] >= 'A' && base[4] <= 'Z' || strings.HasSuffix(base, "Test.java") || strings.HasSuffix(base, "Tests.java") || strings.HasSuffix(base, "TestCase.java")
	case ".cs":
		return strings.HasSuffix(base, "Test.cs") || strings.HasSuffix(base, "Tests.cs")
	}
	return false
}

func (a *Analyzer) reviewPathExclusion(filePath string) ExclusionReason {
	if isTestPath(filePath) {
		return ExclusionTests
	}
	for _, excluded := range a.excludedPaths {
		if filePath == excluded || strings.HasPrefix(filePath, excluded+"/") {
			return ExclusionConfigured
		}
	}
	if a.isExcludedDirectory(filePath) {
		return ExclusionVendor
	}
	return ""
}

func (a *Analyzer) validateReviewPath(filePath string) error {
	if _, err := literalPath(filePath); err != nil {
		return err
	}
	if a.reviewPathExclusion(filePath) != "" {
		return fmt.Errorf("path %q is excluded from review by policy", filePath)
	}
	return nil
}

// Git filters the complete output before chunk offsets are applied, so excluded
// source cannot leak through a continuation page or a caller's directory search.
func (a *Analyzer) excludedSearchPaths(ctx context.Context, commit string) ([]string, error) {
	raw, err := a.runGit(ctx, a.maxMetadataBytes, "ls-tree", "-r", "-z", "--name-only", commit, "--")
	if err != nil {
		return nil, err
	}
	var excluded []string
	for _, entry := range bytes.Split(raw, []byte{0}) {
		filePath := string(entry)
		if filePath != "" && (a.reviewPathExclusion(filePath) != "") {
			excluded = append(excluded, ":(exclude,literal)"+filePath)
		}
	}
	return excluded, nil
}
