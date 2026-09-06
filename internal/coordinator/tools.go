package coordinator

import (
	"bytes"
	"encoding/json"
	"errors"

	"ci-signal/internal/config"
	"ci-signal/internal/copilot"
	"ci-signal/internal/repository"
	"ci-signal/internal/review"

	sdk "github.com/github/copilot-sdk/go"
)

type sourceArgs struct {
	Side   repository.Side     `json:"side"`
	Path   string              `json:"path"`
	Offset int64               `json:"offset"`
	UnitID review.ReviewUnitID `json:"unit_id,omitempty"`
}
type diffArgs struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
}
type searchArgs struct {
	Side   repository.Side `json:"side"`
	Query  string          `json:"query"`
	Paths  []string        `json:"paths"`
	Offset int64           `json:"offset"`
}
type astArgs struct {
	Side repository.Side `json:"side"`
	Path string          `json:"path"`
}
type structuralScanArgs struct {
	Name      string `json:"name"`
	AfterPath string `json:"after_path,omitempty"`
}

func repositoryTools(analyzer *repository.Analyzer, snapshot repository.Snapshot, registry *copilot.AssignmentRegistry, limits config.Limits, allowed []string, scans []config.StructuralScan) []sdk.Tool {
	tools := []sdk.Tool{
		tool("repository_read", "Read a bounded source-head or base blob by literal path", properties("side", "path", "offset", "unit_id"), []string{"side", "path", "offset"}, func(inv sdk.ToolInvocation) (sdk.ToolResult, error) {
			var args sourceArgs
			if err := decodeToolArgs(inv.Arguments, &args); err != nil {
				return sdk.ToolResult{}, err
			}
			value, err := analyzer.ReadSource(inv.TraceContext, snapshot, args.Side, args.Path, args.Offset, limits.MaxSourceBytes)
			if err != nil {
				return sdk.ToolResult{}, err
			}
			if err := registry.RegisterSource(value.Reference); err != nil {
				return sdk.ToolResult{}, err
			}
			if args.UnitID != "" {
				if err := registry.RecordUnitRetrieval(args.UnitID, value.Chunk.Offset, value.Chunk.NextOffset, value.Chunk.Complete); err != nil {
					return sdk.ToolResult{}, err
				}
			}
			return marshalToolResult(value)
		}),
		tool("git_read", "Read a bounded diff for one literal path", properties("path", "offset"), []string{"path", "offset"}, func(inv sdk.ToolInvocation) (sdk.ToolResult, error) {
			var args diffArgs
			if err := decodeToolArgs(inv.Arguments, &args); err != nil {
				return sdk.ToolResult{}, err
			}
			value, err := analyzer.ReadDiff(inv.TraceContext, snapshot, args.Path, args.Offset, limits.MaxDiffBytes)
			if err != nil {
				return sdk.ToolResult{}, err
			}
			if err := registry.RegisterSource(value.Reference); err != nil {
				return sdk.ToolResult{}, err
			}
			return marshalToolResult(value)
		}),
		tool("repository_search", "Search pinned repository source with bounded output", properties("side", "query", "paths", "offset"), []string{"side", "query", "paths", "offset"}, func(inv sdk.ToolInvocation) (sdk.ToolResult, error) {
			var args searchArgs
			if err := decodeToolArgs(inv.Arguments, &args); err != nil {
				return sdk.ToolResult{}, err
			}
			value, err := analyzer.Search(inv.TraceContext, snapshot, args.Side, args.Query, args.Paths, args.Offset, limits.MaxToolOutputBytes)
			if err != nil {
				return sdk.ToolResult{}, err
			}
			if err := registry.RegisterSource(value.Reference); err != nil {
				return sdk.ToolResult{}, err
			}
			return marshalToolResult(value)
		}),
		tool("ast_grep", "Extract Go declarations from one pinned source blob", properties("side", "path"), []string{"side", "path"}, func(inv sdk.ToolInvocation) (sdk.ToolResult, error) {
			var args astArgs
			if err := decodeToolArgs(inv.Arguments, &args); err != nil {
				return sdk.ToolResult{}, err
			}
			value, err := analyzer.ExtractDeclarations(inv.TraceContext, snapshot, args.Side, args.Path)
			if err != nil {
				return sdk.ToolResult{}, err
			}
			if err := registry.RegisterSource(value.Reference); err != nil {
				return sdk.ToolResult{}, err
			}
			return marshalToolResult(value)
		}),
		tool("structural_scan", "Run a configured structural rule across every eligible file in the pinned source-head tree", properties("name", "after_path"), []string{"name"}, func(inv sdk.ToolInvocation) (sdk.ToolResult, error) {
			var args structuralScanArgs
			if err := decodeToolArgs(inv.Arguments, &args); err != nil {
				return sdk.ToolResult{}, err
			}
			var selected *config.StructuralScan
			for index := range scans {
				if scans[index].Name == args.Name {
					selected = &scans[index]
					break
				}
			}
			if selected == nil {
				return sdk.ToolResult{}, errors.New("unknown configured structural scan")
			}
			value, err := analyzer.RunStructuralScan(inv.TraceContext, snapshot, selected.Name, selected.Language, selected.RulePath, args.AfterPath)
			if err != nil {
				return sdk.ToolResult{}, err
			}
			if err := registry.RegisterSource(value.Reference); err != nil {
				return sdk.ToolResult{}, err
			}
			return marshalToolResult(value)
		}),
	}
	allow := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allow[name] = struct{}{}
	}
	result := tools[:0]
	for _, candidate := range tools {
		if _, ok := allow[candidate.Name]; ok {
			result = append(result, candidate)
		}
	}
	return result
}

func tool(name, description string, fields map[string]any, required []string, handler sdk.ToolHandler) sdk.Tool {
	return sdk.Tool{Name: name, Description: description, Parameters: map[string]any{"type": "object", "additionalProperties": false, "properties": fields, "required": required}, Defer: sdk.ToolDeferNever, Handler: handler}
}

func properties(names ...string) map[string]any {
	result := make(map[string]any, len(names))
	for _, name := range names {
		schema := map[string]any{"type": "string"}
		switch name {
		case "offset":
			schema = map[string]any{"type": "integer", "minimum": 0}
		case "paths":
			schema = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
		case "side":
			schema = map[string]any{"type": "string", "enum": []string{"base", "head"}}
		}
		result[name] = schema
	}
	return result
}

func decodeToolArgs(input any, output any) error {
	if input == nil {
		return errors.New("tool arguments are required")
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(output)
}
