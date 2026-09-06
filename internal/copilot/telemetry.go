package copilot

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"ci-signal/internal/config"
	"ci-signal/internal/review"

	sdk "github.com/github/copilot-sdk/go"
)

type eventCollector struct {
	mu             sync.Mutex
	sessionID      string
	requestedModel string
	requiredSkills map[string]string
	invoked        map[string]string
	observed       map[string]struct{}
	toolStarts     map[string]string
	tools          []review.ToolExecution
	usage          []review.UsageEvent
	usageIDs       map[string]struct{}
	usageComplete  bool
	sawUsage       bool
	inputTokens    uint64
	outputTokens   uint64
	maxInput       uint64
	maxOutput      uint64
	integrity      error
	diagnostics    config.Diagnostics
	logger         *diagnostics
	cancel         func()
}

func newEventCollector(sessionID, requestedModel string, requiredSkills []string, limits config.Limits, settings config.Diagnostics, logger *diagnostics, cancel func()) *eventCollector {
	required := make(map[string]string, len(requiredSkills))
	for _, skill := range requiredSkills {
		required[stringsLower(skill)] = skill
	}
	return &eventCollector{
		sessionID:      sessionID,
		requestedModel: requestedModel,
		requiredSkills: required,
		invoked:        make(map[string]string, len(required)),
		observed:       make(map[string]struct{}),
		toolStarts:     make(map[string]string),
		usageIDs:       make(map[string]struct{}),
		usageComplete:  true,
		maxInput:       limits.MaxInputTokens,
		maxOutput:      limits.MaxOutputTokens,
		diagnostics:    settings,
		logger:         logger,
		cancel:         cancel,
	}
}

func (c *eventCollector) handle(event sdk.SessionEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch data := event.Data.(type) {
	case *sdk.ModelCallStartData:
		if data.Model != nil {
			c.observeModel(*data.Model)
		}
	case *sdk.AssistantUsageData:
		c.observeModel(data.Model)
		if data.IsByok != nil && !*data.IsByok {
			c.fail(errors.New("runtime reported a non-BYOK model call"))
		}
		if data.IsByok == nil {
			c.usageComplete = false
		}
		id := event.ID
		if data.APICallID != nil && *data.APICallID != "" {
			id = *data.APICallID
		}
		if _, duplicate := c.usageIDs[id]; duplicate {
			return
		}
		c.usageIDs[id] = struct{}{}
		c.sawUsage = true
		input, inputKnown := nonnegative(data.InputTokens)
		output, outputKnown := nonnegative(data.OutputTokens)
		if !inputKnown || !outputKnown {
			c.usageComplete = false
		}
		c.inputTokens += input
		c.outputTokens += output
		c.usage = append(c.usage, review.UsageEvent{
			EventID:      id,
			SessionID:    c.sessionID,
			InputTokens:  input,
			OutputTokens: output,
			TotalTokens:  input + output,
			Cumulative:   false,
		})
		if c.maxInput > 0 && c.inputTokens > c.maxInput || c.maxOutput > 0 && c.outputTokens > c.maxOutput {
			c.fail(fmt.Errorf("reported token usage exceeds host budget: input=%d/%d output=%d/%d", c.inputTokens, c.maxInput, c.outputTokens, c.maxOutput))
		}
	case *sdk.SessionUsageCheckpointData:
		// Checkpoints are cumulative cost records. They are deliberately not
		// mixed with per-call token usage.
	case *sdk.ToolExecutionStartData:
		c.toolStarts[data.ToolCallID] = data.ToolName
		if data.Model != nil {
			c.observeModel(*data.Model)
		}
		if c.diagnostics.LogToolCalls {
			c.logger.EventToolCall(data.ToolName, data.ToolCallID, data.Arguments)
		}
	case *sdk.ToolExecutionCompleteData:
		name := c.toolStarts[data.ToolCallID]
		if name == "" {
			name = "unknown"
		}
		c.tools = append(c.tools, review.ToolExecution{SessionID: c.sessionID, Tool: name, Succeeded: data.Success})
		if data.Model != nil {
			c.observeModel(*data.Model)
		}
		if c.diagnostics.LogToolResults {
			c.logger.EventToolResult(name, data.ToolCallID, data)
		}
		delete(c.toolStarts, data.ToolCallID)
	case *sdk.SkillInvokedData:
		c.invoked[stringsLower(data.Name)] = data.Name
		if data.Model != nil {
			c.observeModel(*data.Model)
		}
		c.logger.SkillInvoked(data.Name, stringValue(data.Source), stringValueSkillTrigger(data.Trigger))
	case *sdk.AssistantReasoningData:
		if c.diagnostics.LogReasoning {
			c.logger.Reasoning(data.ReasoningID, data.Content)
		}
	case *sdk.AssistantReasoningDeltaData:
		if c.diagnostics.LogReasoning {
			c.logger.ReasoningDelta(data.ReasoningID, data.DeltaContent)
		}
	}
}

func (c *eventCollector) observeModel(model string) {
	if model == "" {
		return
	}
	c.observed[model] = struct{}{}
	if model != c.requestedModel {
		c.fail(fmt.Errorf("runtime used model %q instead of requested %q", model, c.requestedModel))
	}
}

func (c *eventCollector) fail(err error) {
	if c.integrity == nil {
		c.integrity = err
		if c.cancel != nil {
			c.cancel()
		}
	}
}

func (c *eventCollector) integrityError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.integrity
}

func (c *eventCollector) recordInvokedSkills(skills []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, skill := range skills {
		c.invoked[stringsLower(skill)] = skill
	}
}

func (c *eventCollector) missingSkills(required []string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.missingSkillsFor(required)
}

func (c *eventCollector) missingSkillsLocked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	missing := make([]string, 0)
	for key, display := range c.requiredSkills {
		if _, ok := c.invoked[key]; !ok {
			missing = append(missing, display)
		}
	}
	sort.Strings(missing)
	return missing
}

func (c *eventCollector) missingSkillsFor(required []string) []string {
	missing := make([]string, 0)
	for _, skill := range required {
		if _, ok := c.invoked[stringsLower(skill)]; !ok {
			missing = append(missing, skill)
		}
	}
	sort.Strings(missing)
	return missing
}

func (c *eventCollector) invokedSkills() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]string, 0, len(c.invoked))
	for _, name := range c.invoked {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func (c *eventCollector) telemetry() review.Telemetry {
	c.mu.Lock()
	defer c.mu.Unlock()
	models := make([]review.ModelObservation, 0, len(c.observed))
	for model := range c.observed {
		models = append(models, review.ModelObservation{SessionID: c.sessionID, Model: model})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Model < models[j].Model })
	return review.Telemetry{
		RequestedModels: []string{c.requestedModel},
		ObservedModels:  models,
		Tools:           append([]review.ToolExecution(nil), c.tools...),
		Usage:           append([]review.UsageEvent(nil), c.usage...),
		UsageComplete:   c.sawUsage && c.usageComplete,
	}
}

func nonnegative(value *int64) (uint64, bool) {
	if value == nil || *value < 0 {
		return 0, false
	}
	return uint64(*value), true
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func stringValueSkillTrigger(value *sdk.SkillInvokedTrigger) string {
	if value == nil {
		return ""
	}
	return string(*value)
}

func stringsLower(value string) string {
	if value == "" {
		return ""
	}
	bytes := []byte(value)
	for i, b := range bytes {
		if b >= 'A' && b <= 'Z' {
			bytes[i] = b + ('a' - 'A')
		}
	}
	return string(bytes)
}
