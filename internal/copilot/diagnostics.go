package copilot

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"ci-signal/internal/config"

	sdk "github.com/github/copilot-sdk/go"
)

type diagnostics struct {
	logger   *slog.Logger
	closer   io.Closer
	redactor *redactor
}

func newDiagnostics(settings config.Diagnostics, secrets config.Secrets, additionalSecrets ...string) (*diagnostics, error) {
	var writer io.Writer = io.Discard
	var closer io.Closer
	if settings.LogFile != "" {
		file, err := os.OpenFile(settings.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open Copilot diagnostics log: %w", err)
		}
		if err := file.Chmod(0o600); err != nil {
			file.Close()
			return nil, fmt.Errorf("protect Copilot diagnostics log: %w", err)
		}
		writer = file
		closer = file
	}
	secretValues := []string{secrets.ProviderToken.Value(), secrets.JobToken.Value(), secrets.APIToken.Value()}
	secretValues = append(secretValues, additionalSecrets...)
	return &diagnostics{
		logger:   slog.New(slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: slogLevel(settings.LogLevel)})),
		closer:   closer,
		redactor: newRedactor(secretValues...),
	}, nil
}

func (d *diagnostics) Close() error {
	if d == nil || d.closer == nil {
		return nil
	}
	if err := d.closer.Close(); err != nil {
		return fmt.Errorf("close Copilot diagnostics log: %w", err)
	}
	return nil
}

func (d *diagnostics) EventToolCall(name, callID string, arguments any) {
	d.logger.Debug("Copilot tool call", "tool", name, "tool_call_id", callID, "arguments", d.redactor.JSON(arguments))
}

func (d *diagnostics) EventToolResult(name, callID string, result any) {
	d.logger.Debug("Copilot tool result", "tool", name, "tool_call_id", callID, "result", d.redactor.JSON(result))
}

func (d *diagnostics) ToolResult(name, callID string, result sdk.ToolResult) {
	d.logger.Debug("native tool result", "tool", name, "tool_call_id", callID, "result", d.redactor.JSON(result))
}

func (d *diagnostics) SkillInvoked(name, source, trigger string) {
	d.logger.Debug("Copilot skill invoked", "skill", name, "source", source, "trigger", trigger)
}

func (d *diagnostics) Reasoning(reasoningID, content string) {
	d.logger.Debug("Copilot reasoning", "reasoning_id", reasoningID, "content", d.redactor.Text(content))
}

func (d *diagnostics) ReasoningDelta(reasoningID, content string) {
	d.logger.Debug("Copilot reasoning delta", "reasoning_id", reasoningID, "content", d.redactor.Text(content))
}

func slogLevel(level config.LogLevel) slog.Level {
	switch level {
	case config.LogDebug:
		return slog.LevelDebug
	case config.LogWarn:
		return slog.LevelWarn
	case config.LogError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type redactor struct {
	values []string
}

func newRedactor(values ...string) *redactor {
	result := &redactor{}
	for _, value := range values {
		if value != "" {
			result.values = append(result.values, value)
		}
	}
	return result
}

func (r *redactor) Text(value string) string {
	for _, secret := range r.values {
		value = strings.ReplaceAll(value, secret, "[redacted]")
	}
	return value
}

func (r *redactor) JSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "[unavailable]"
	}
	return r.Text(string(raw))
}
