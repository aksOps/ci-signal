package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"ci-signal/internal/review"
)

func TestConventionalTestPathsExcludeOnlyTestNames(t *testing.T) {
	for _, name := range []string{"pkg/order_test.go", "tests/helper.go", "test/helper.py", "app/__tests__/order.ts", "app/order.test.js", "app/order.spec.tsx", "src/test/java/Order.java", "OrderTest.java", "TestOrder.java", "test_orders.py", "orders_test.py", "conftest.py", "OrderTests.cs", "Order.Tests/Program.cs", "OrderTests/Program.cs"} {
		if !isTestPath(name) {
			t.Errorf("test path admitted: %s", name)
		}
	}
	for _, name := range []string{"pkg/contest.go", "pkg/latest.go", "app/production.testable.ts", "src/Program.cs", "src/Testable.java", "testing_helpers.go"} {
		if isTestPath(name) {
			t.Errorf("production path excluded: %s", name)
		}
	}
}

func TestTestFilesExcludedFromInventoryAndBoundedEvidence(t *testing.T) {
	directory := t.TempDir()
	runTestGit(t, "", "init", "--initial-branch=main", directory)
	testPaths := []string{"app/order_test.go", "app/__tests__/order.ts", "app/order.test.js", "app/order.spec.ts", "src/test/java/Order.java", "test_orders.py", "Order.Tests/Program.cs", "csharp/Program.cs", "custom-checks/harness.go"}
	for _, name := range testPaths {
		writeFixture(t, directory, name, "package app\nconst TestOnlySentinel = 1\n")
	}
	writeFixture(t, directory, "app/latest.go", "package app\nconst ProductionSentinel = 1\n")
	runTestGit(t, directory, "add", ".")
	runTestGit(t, directory, "commit", "-m", "base")
	base := gitOutput(t, directory, "rev-parse", "HEAD")
	for _, name := range testPaths {
		writeFixture(t, directory, name, "package app\nconst TestOnlySentinel = 2\n")
	}
	writeFixture(t, directory, "app/latest.go", "package app\nconst ProductionSentinel = 2\n")
	runTestGit(t, directory, "add", ".")
	runTestGit(t, directory, "commit", "-m", "head")
	head := gitOutput(t, directory, "rev-parse", "HEAD")
	analyzer := newTestAnalyzer(t, directory, 4096)
	analyzer.excludedPaths = []string{"csharp/Program.cs", "custom-checks"}
	ctx := context.Background()
	snapshot, err := analyzer.CaptureSnapshot(ctx, base, head, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []review.Scope{review.ScopeMRImpact, review.ScopeFullProject} {
		inventory, err := analyzer.Inventory(ctx, snapshot, scope)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range testPaths {
			reason := ExclusionTests
			if name == "csharp/Program.cs" || strings.HasPrefix(name, "custom-checks/") {
				reason = ExclusionConfigured
			}
			assertExclusion(t, inventory.Exclusions, name, reason, true)
		}
		for _, unit := range inventory.Units {
			if unit.Base != nil && analyzer.reviewPathExclusion(unit.Base.Path) != "" || unit.Head != nil && analyzer.reviewPathExclusion(unit.Head.Path) != "" {
				t.Fatalf("test unit assigned: %#v", unit)
			}
		}
		if len(inventory.Units) == 0 {
			t.Fatal("production work was lost")
		}
	}
	for _, name := range testPaths {
		for _, side := range []Side{SideBase, SideHead} {
			if _, err := analyzer.ReadSource(ctx, snapshot, side, name, 0, 13); err == nil {
				t.Fatalf("test source read: %s", name)
			}
		}
		if _, err := analyzer.ReadDiff(ctx, snapshot, name, 0, 13); err == nil {
			t.Fatalf("test diff read: %s", name)
		}
	}
	if _, err := analyzer.ExtractDeclarations(ctx, snapshot, SideHead, testPaths[0]); err == nil {
		t.Fatal("test AST read")
	}
	for _, name := range []string{"", "app"} {
		if _, err := analyzer.ReadDiff(ctx, snapshot, name, 0, 13); err == nil {
			t.Fatal("directory/full diff bypassed test filtering")
		}
	}
	for _, side := range []Side{SideBase, SideHead} {
		for _, paths := range [][]string{nil, {"app"}} {
			data := readAllChunks(t, func(offset int64) (Chunk, error) {
				result, err := analyzer.Search(ctx, snapshot, side, "Sentinel", paths, offset, 13)
				return result.Chunk, err
			})
			if strings.Contains(string(data), "TestOnly") || !strings.Contains(string(data), "ProductionSentinel") {
				t.Fatalf("filtered search = %q", data)
			}
		}
	}
	scan, err := analyzer.RunStructuralScan(ctx, snapshot, "declarations", "go", analyzer.goRulePath, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range scan.Matches {
		if analyzer.reviewPathExclusion(match.Path) != "" || strings.Contains(match.Text, "TestOnly") {
			t.Fatalf("test structural match: %#v", match)
		}
	}
}

func TestRenameAcrossTestBoundaryRetainsOnlyProductionSide(t *testing.T) {
	for _, fromTest := range []bool{true, false} {
		directory := t.TempDir()
		runTestGit(t, "", "init", "--initial-branch=main", directory)
		from, to := "tests/service.go", "src/service.go"
		if !fromTest {
			from, to = to, from
		}
		writeFixture(t, directory, from, "package app\nfunc Service() {}\n")
		runTestGit(t, directory, "add", ".")
		runTestGit(t, directory, "commit", "-m", "base")
		base := gitOutput(t, directory, "rev-parse", "HEAD")
		writeFixture(t, directory, to, "package app\nfunc Service() {}\n")
		runTestGit(t, directory, "rm", from)
		runTestGit(t, directory, "add", ".")
		runTestGit(t, directory, "commit", "-m", "rename")
		head := gitOutput(t, directory, "rev-parse", "HEAD")
		analyzer := newTestAnalyzer(t, directory, 4096)
		snapshot, err := analyzer.CaptureSnapshot(context.Background(), base, head, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		inventory, err := analyzer.Inventory(context.Background(), snapshot, review.ScopeMRImpact)
		if err != nil {
			t.Fatal(err)
		}
		assertExclusion(t, inventory.Exclusions, "tests/service.go", ExclusionTests, true)
		if len(inventory.Units) == 0 {
			t.Fatal("production rename side was lost")
		}
		for _, unit := range inventory.Units {
			if fromTest && unit.Base != nil || !fromTest && unit.Head != nil {
				t.Fatalf("excluded rename side assigned: %#v", unit)
			}
		}
	}
}
