// Package llmtext strips structural markers that leak into model output.
//
// These strippers live in their own leaf package because two packages need
// them and one cannot import the other: internal/agent imports
// internal/channels (loop_history_supplement.go), so internal/channels can
// never import internal/agent, where the full assistant-response sanitizer
// lives.
//
// Scope is deliberately narrow — only markers that expose data the model was
// never meant to republish: downgraded tool-call text, echoed system-message
// blocks, and MEDIA: paths. Answer-shaping steps (thinking tags, duplicate
// collapse) stay in internal/agent; applying them to reasoning text would
// delete the reasoning itself.
package llmtext

import (
	"log/slog"
	"regexp"
	"strings"
)

// MediaPathPattern matches "MEDIA:" followed by a path (absolute or relative).
var MediaPathPattern = regexp.MustCompile(`MEDIA:\S+`)

// StripDowngradedToolCalls removes [Tool Call: ...], [Tool Result ...],
// and [Historical context: ...] blocks that some models emit as text.
// Uses line-by-line scanning (Go regexp doesn't support lookahead).
func StripDowngradedToolCalls(content string) string {
	if !strings.Contains(content, "[Tool Call:") &&
		!strings.Contains(content, "[Tool Result") &&
		!strings.Contains(content, "[Historical context:") {
		return content
	}

	lines := strings.Split(content, "\n")
	var result []string
	skipping := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Start skipping on these markers
		if strings.HasPrefix(trimmed, "[Tool Call:") ||
			strings.HasPrefix(trimmed, "[Tool Result") ||
			strings.HasPrefix(trimmed, "[Historical context:") {
			skipping = true
			continue
		}

		// Stop skipping on non-indented, non-empty line that isn't part of the block
		if skipping {
			// Arguments JSON and tool output are typically indented or empty
			if trimmed == "" || strings.HasPrefix(trimmed, "Arguments:") ||
				strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "}") {
				continue
			}
			// Non-tool-block line → stop skipping
			skipping = false
		}

		result = append(result, line)
	}

	return strings.TrimSpace(strings.Join(result, "\n"))
}

// StripEchoedSystemMessages removes "[System Message] ..." blocks that LLMs
// hallucinate/echo in their output.
// Uses line-based scanning (Go regexp doesn't support lookahead).
func StripEchoedSystemMessages(content string) string {
	if !strings.Contains(content, "[System Message]") {
		return content
	}

	lines := strings.Split(content, "\n")
	var result []string
	skipping := false

	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "[System Message]") {
			skipping = true
			continue
		}
		if skipping {
			// Empty line ends the system message block
			if strings.TrimSpace(line) == "" {
				skipping = false
				continue
			}
			// Still part of the system message block (Stats:, reply instructions, etc.)
			continue
		}
		result = append(result, line)
	}

	cleaned := strings.TrimSpace(strings.Join(result, "\n"))

	if cleaned != strings.TrimSpace(content) {
		slog.Warn("stripped echoed [System Message] from assistant response",
			"original_len", len(content),
			"cleaned_len", len(cleaned),
		)
	}

	return cleaned
}

// StripMediaPaths removes lines containing MEDIA:/path references from LLM
// output. These are tool result artifacts that should not appear in
// user-facing text (media files are delivered separately via
// OutboundMessage.Media).
func StripMediaPaths(content string) string {
	if !strings.Contains(content, "MEDIA:") {
		return content
	}
	lines := strings.Split(content, "\n")
	var result []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[[audio_as_voice]]") {
			continue
		}
		// Strip any line containing a MEDIA: path reference, regardless of wrapping format.
		// LLMs echo these in many forms: bare "MEDIA:/path", markdown "![alt](MEDIA:relative/path)",
		// JSON '{"image":"MEDIA:/path"}', etc. Match MEDIA: followed by any non-space path char.
		if MediaPathPattern.MatchString(trimmed) {
			continue
		}
		result = append(result, line)
	}
	return strings.TrimSpace(strings.Join(result, "\n"))
}

// StripReasoningLeaks removes the marker classes that would republish data the
// model only saw internally: tool-call/tool-result text, echoed system-message
// blocks, and MEDIA: paths. Reasoning prose that contains none of those
// markers is returned unchanged.
//
// Reasoning is not run through the full assistant-response sanitizer: that
// pipeline strips <think>/<thinking> tags, which for reasoning content would
// delete the payload instead of cleaning it.
func StripReasoningLeaks(content string) string {
	if content == "" {
		return content
	}
	content = StripDowngradedToolCalls(content)
	content = StripEchoedSystemMessages(content)
	return StripMediaPaths(content)
}
