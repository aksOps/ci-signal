package review

import (
	"errors"
	"strings"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/parser"
)

// ValidateProse rejects code-block and patch formats. Meaning remains a review
// instruction: Markdown syntax cannot classify code disguised as ordinary text.
func ValidateProse(value string) error {
	document := parser.New().Parse([]byte(value))
	if err := ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			switch node.Kind() {
			case ast.KindCodeBlock, ast.KindHTMLBlock:
				return ast.WalkStop, errors.New("review text must be a prose summary without code blocks")
			}
		}
		return ast.WalkContinue, nil
	}); err != nil {
		return err
	}
	for _, line := range strings.Split(value, "\n") {
		if strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "@@ ") {
			return errors.New("review text must describe the change without a patch")
		}
	}
	return nil
}
