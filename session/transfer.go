// This file preserves the source snapshot alongside a translated projection.
package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Transfer keeps the complete original and the target's readable projection together.
// Original is a snapshot of the source, not a serialization of later edits.
type Transfer struct {
	SourceType  string   `json:"source_type"`
	TargetType  string   `json:"target_type"`
	Original    []byte   `json:"original"`
	Target      []byte   `json:"target"`
	Limitations []string `json:"limitations,omitempty"`
}

// RestoreOriginal writes the exact captured input, including line endings.
func (s Session) RestoreOriginal(w io.Writer) error {
	if s.Original == nil {
		return fmt.Errorf("no original session was captured")
	}
	_, err := io.Copy(w, bytes.NewReader(s.Original))
	return err
}

// Convert retains the original bytes and produces a target projection. A target
// projection is never claimed lossless: providers have incompatible metadata and
// Ferry does not yet map all source records. Unsupported visible content is an error.
func Convert(s Session, target string) (Transfer, error) {
	var codec Codec
	switch target {
	case "claude":
		codec = Claude{}
	case "codex":
		codec = Codex{}
	case "gemini":
		codec = Gemini{}
	case "opencode":
		codec = OpenCode{}
	case "trae":
		codec = Trae{}
	case "pi":
		codec = Pi{}
	case "qoder":
		codec = Qoder{}
	case "deepseek":
		codec = DeepSeek{}
	case "qwen":
		codec = Qwen{}
	case "copilot":
		codec = Copilot{}
	default:
		return Transfer{}, fmt.Errorf("unsupported target %q", target)
	}
	if s.Original == nil {
		return Transfer{}, fmt.Errorf("no original session was captured")
	}
	if target == s.SourceType {
		if err := validateOriginalIdentity(s, codec); err != nil {
			return Transfer{}, err
		}
		return Transfer{SourceType: s.SourceType, TargetType: target, Original: bytes.Clone(s.Original), Target: bytes.Clone(s.Original)}, nil
	}
	if len(s.Messages) == 0 {
		return Transfer{}, fmt.Errorf("no supported messages to convert; restore the original session instead")
	}
	// Codex rollouts identify sessions by UUID, which not every provider uses
	// (OpenCode ids are prefixed nanoids). Deriving a stable UUID keeps the
	// projection reproducible instead of inventing a random identity; the
	// original snapshot retains the source ID.
	derivedID := false
	if (target == "codex" || target == "trae" || target == "qoder" || target == "deepseek" || target == "qwen" || target == "copilot") && !isUUID(s.NativeID) {
		s.NativeID = ferryUUID(s.SourceType, s.NativeID)
		derivedID = true
	}
	var out bytes.Buffer
	projection := s
	envelopesDropped := 0
	if target == "pi" {
		projection.Messages = append([]Message(nil), s.Messages...)
		for i := range projection.Messages {
			projection.Messages[i].Parts = append([]ContentBlock(nil), s.Messages[i].Parts...)
			for j := range projection.Messages[i].Parts {
				part := &projection.Messages[i].Parts[j]
				if part.ToolCall == nil {
					// Pi stores structured results natively, so its read-back
					// carries their text rather than the source JSON spelling.
					// Mirror the reader so the projection stays comparable.
					if part.ToolResult != nil && len(part.ToolResult.RawContent) > 0 {
						if _, err := piToolResultBlocks(part.ToolResult.RawContent); err == nil {
							result := *part.ToolResult
							result.Content = piToolResultText(result.RawContent)
							if piToolResultTextOnly(result.RawContent) {
								result.RawContent = nil
							}
							part.ToolResult = &result
						}
					}
					continue
				}
				call := *part.ToolCall
				input, _, err := wrapToolInputForCall(*part)
				if err != nil {
					return Transfer{}, fmt.Errorf("message %d part %d: %w", i, j, err)
				}
				call.Input, call.Status = input, ""
				part.ToolCall = &call
				// Pi has no custom-call concept: once the free text is wrapped
				// for its native toolCall block, the Codex payload marker must
				// not make later canonicalization treat the object as text.
				part.Raw = nil
			}
		}
	}
	if target == "trae" {
		// Trae re-injects its own context envelopes (AGENTS.md instructions,
		// environment context) and its reader drops user records consisting
		// only of them, so writing them would fail read-back validation.
		// Skip them here and report instead of failing the conversion.
		projection.Messages, envelopesDropped = filterTraeEnvelopeMessages(s.Messages)
		if len(projection.Messages) == 0 {
			return Transfer{}, fmt.Errorf("no supported messages to convert; restore the original session instead")
		}
	}
	if err := codec.Write(&out, projection); err != nil {
		return Transfer{}, err
	}
	result, err := codec.Read(bytes.NewReader(out.Bytes()))
	if err != nil {
		return Transfer{}, fmt.Errorf("target read-back: %w", err)
	}
	if err := validateTargetIdentity(s, result, target); err != nil {
		return Transfer{}, fmt.Errorf("target read-back: %w", err)
	}
	// The comparison source follows the projection: filters and mirrors above
	// transform content into the form the target reader produces anyway, and
	// that transformation is reported as a limitation rather than hidden or
	// failed.
	comparisonSource := s
	if target == "trae" || target == "pi" {
		comparisonSource = projection
	}
	precisionNotes, err := compareProjections(comparisonSource, result, target == "opencode" || target == "qoder")
	if err != nil {
		return Transfer{}, fmt.Errorf("target read-back: %w", err)
	}
	limitations := []string{"Target output is a projection: source-specific records and metadata may not be represented.", "The target CLI's ability to resume this output has not been verified."}
	if changed, sourceOrder, targetOrder := reorderedProjectionParts(comparisonSource, result); changed {
		limitations = append(limitations, fmt.Sprintf("Target %s reordered supported parts from [%s] to [%s]; the original bytes retain the source order.", target, sourceOrder, targetOrder))
	}
	if derivedID {
		limitations = append(limitations, fmt.Sprintf("%s requires a UUID native ID; the target ID was derived deterministically from the source ID, and the original snapshot retains the source ID.", providerDisplayName(target)))
	}
	if target == "codex" || target == "copilot" {
		if flagged := errorFlaggedResults(s); flagged > 0 {
			limitations = append(limitations, fmt.Sprintf("Source contains %d tool results flagged as errors; %s has no error flag, so the flag is dropped in the projection.", flagged, providerDisplayName(target)))
		}
	}
	if target != "gemini" {
		structured := structuredResultCount(s)
		if target == "pi" {
			// Pi carries block-shaped results natively; only the shapes it
			// cannot represent are flattened to JSON text.
			structured = piFlattenedResults(s)
		}
		if structured > 0 {
			limitations = append(limitations, fmt.Sprintf("Source contains %d structured tool results serialized as JSON text; the original bytes retain their exact shape.", structured))
		}
	}
	if target != "codex" && target != "trae" {
		if wrapped := freeTextInputCalls(s); wrapped > 0 {
			limitations = append(limitations, fmt.Sprintf("Source contains %d custom tool calls with free-text input, wrapped in an object argument for %s; the original bytes retain the exact text.", wrapped, target))
		}
	}
	if envelopesDropped > 0 {
		limitations = append(limitations, fmt.Sprintf("Trae treats %d injected context envelope text part(s) (AGENTS.md instructions, environment context) as its own context and drops them; they were not written to the target, and the original bytes retain them.", envelopesDropped))
	}
	if target == "qoder" {
		limitations = append(limitations, "Qoder stores the workspace in an optional sidecar file that the JSONL stream cannot carry; target read-back cannot recover the workspace.")
	}
	if len(precisionNotes) > 0 {
		limitations = append(limitations, precisionNotes...)
	}
	if target == "gemini" {
		limitations = append(limitations, "Gemini output stores a project hash derived from the source workspace; the workspace cannot be recovered from target read-back.")
	}
	limitations = append(limitations, skippedRecordLimitations(s, target)...)
	limitations = append(limitations, toolCallStatusLimitations(s, result, target)...)
	limitations = append(limitations, unknownToolCallStatusLimitations(s, target)...)
	limitations = append(limitations, unpairedToolCallLimitations(s)...)
	limitations = append(limitations, duplicateToolIDLimitations(s)...)
	return Transfer{SourceType: s.SourceType, TargetType: target, Original: bytes.Clone(s.Original), Target: bytes.Clone(out.Bytes()), Limitations: limitations}, nil
}

func reorderedProjectionParts(source, target Session) (bool, string, string) {
	sourceParts, sourceErr := projectedParts(source)
	targetParts, targetErr := projectedParts(target)
	if sourceErr != nil || targetErr != nil || len(sourceParts) != len(targetParts) {
		return false, "", ""
	}
	for i := range sourceParts {
		left, right := sourceParts[i], targetParts[i]
		left.Timestamp, right.Timestamp = "", ""
		left.Status, right.Status = "", ""
		if left != right {
			return true, formatProjectedPartOrder(sourceParts), formatProjectedPartOrder(targetParts)
		}
	}
	return false, "", ""
}

func formatProjectedPartOrder(parts []projectedPart) string {
	labels := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "tool_call":
			labels = append(labels, "tool_call:"+part.ID)
		case "tool_result":
			labels = append(labels, "tool_result:"+part.ID)
		default:
			labels = append(labels, part.Type)
		}
	}
	return strings.Join(labels, ", ")
}

func toolResultOrder(s Session) []string {
	order := make([]string, 0)
	for _, message := range s.Messages {
		for _, part := range message.Parts {
			if part.Type == "tool_result" && part.ToolResult != nil {
				order = append(order, part.ToolResult.ID)
			}
		}
	}
	return order
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// projectedPart is the provider-neutral content that a writer promises to
// carry across a conversion. Provider records and metadata deliberately stay
// outside this comparison; the original snapshot remains the source of truth
// for those details.
type projectedPart struct {
	Role      string
	Type      string
	Text      string
	ID        string
	Name      string
	Input     string
	Status    string
	Timestamp string
}

func validateProjection(source, target Session) error {
	_, err := compareProjections(source, target, false)
	return err
}

// compareProjections checks that a read-back target still carries the source's
// supported parts. millisecondPrecision tolerates targets that store
// timestamps at millisecond precision and returns the truncation as a
// limitation instead of failing the conversion.
func compareProjections(source, target Session, millisecondPrecision bool) ([]string, error) {
	want, err := projectedParts(source)
	if err != nil {
		return nil, err
	}
	got, err := projectedParts(target)
	if err != nil {
		return nil, err
	}
	// Providers place tool results differently: Claude and Codex keep them in
	// their own records, OpenCode couples each result with its call. Compare
	// the order every provider can express: each result directly after its
	// call, calls in message order, everything else in place.
	want, got = pairToolResults(want), pairToolResults(got)
	if len(want) != len(got) {
		return nil, fmt.Errorf("projection changed content: source has %d supported parts, target has %d", len(want), len(got))
	}
	truncated := 0
	for i := range want {
		// Compare content first without timestamps: a writer may fill a
		// timestamp the source did not carry (Codex backfills missing
		// message times), but it may not alter one that exists.
		sourceTimestamp, targetTimestamp := want[i].Timestamp, got[i].Timestamp
		want[i].Timestamp, got[i].Timestamp = "", ""
		// Tool-call status is reported separately as a limitation because
		// providers differ in whether they can carry it. Supported content
		// must still validate even when that optional metadata is dropped.
		want[i].Status, got[i].Status = "", ""
		if want[i] != got[i] {
			return nil, fmt.Errorf("projection changed supported part %d", i)
		}
		if sourceTimestamp != "" && targetTimestamp != sourceTimestamp {
			// The same instant may print with different precision ("...Z" vs
			// "...000Z"), so compare parsed times rather than strings.
			sourceTime, sourceErr := time.Parse(time.RFC3339Nano, sourceTimestamp)
			targetTime, targetErr := time.Parse(time.RFC3339Nano, targetTimestamp)
			if sourceErr != nil || targetErr != nil || !sourceTime.Equal(targetTime) {
				if millisecondPrecision && sourceErr == nil && targetErr == nil && sourceTime.UnixMilli() == targetTime.UnixMilli() {
					truncated++
					continue
				}
				return nil, fmt.Errorf("projection changed supported part %d timestamp from %q to %q", i, sourceTimestamp, targetTimestamp)
			}
		}
	}
	if truncated > 0 {
		return []string{fmt.Sprintf("Target stores timestamps at millisecond precision; %d supported part timestamp(s) were truncated. The original bytes retain full precision.", truncated)}, nil
	}
	return nil, nil
}

// toolCallStatusLimitations reports loss of the optional canonical status
// after a target has been written and read back. Status is not part of the
// provider-neutral content comparison because a target may represent the
// call correctly while lacking an equivalent status field.
func toolCallStatusLimitations(source, target Session, targetType string) []string {
	want, sourceErr := projectedParts(source)
	got, targetErr := projectedParts(target)
	if sourceErr != nil || targetErr != nil {
		return nil
	}
	want, got = pairToolResults(want), pairToolResults(got)
	targetStatus := make(map[string]string)
	for _, part := range got {
		if part.Type == "tool_call" {
			targetStatus[part.ID] = part.Status
		}
	}
	// Aggregated by status value: real sessions carry hundreds of calls, and
	// one line per call made the report unreadable without adding information.
	lost := make(map[string]int)
	changed := make(map[[2]string]int)
	for _, part := range want {
		if part.Type != "tool_call" || part.Status == "" {
			continue
		}
		status, ok := targetStatus[part.ID]
		if ok && status == part.Status {
			continue
		}
		if !ok || status == "" {
			lost[part.Status]++
			continue
		}
		changed[[2]string{part.Status, status}]++
	}
	limitations := make([]string, 0, len(lost)+len(changed))
	statuses := make([]string, 0, len(lost))
	for status := range lost {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	for _, status := range statuses {
		limitations = append(limitations, fmt.Sprintf("Tool call status %q on %d call(s) was not preserved by %s; the target has no canonical status, and the original bytes retain it.", status, lost[status], targetType))
	}
	pairs := make([][2]string, 0, len(changed))
	for pair := range changed {
		pairs = append(pairs, pair)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
	for _, pair := range pairs {
		limitations = append(limitations, fmt.Sprintf("Tool call status changed from %q to %q on %d call(s) in the %s projection; the original bytes retain the source status.", pair[0], pair[1], changed[pair], targetType))
	}
	return limitations
}

// unknownToolCallStatusLimitations makes provider statuses that have no ACP
// equivalent visible to callers. They cannot populate ToolCall.Status, but
// their source block and Session.Original still retain the exact value.
func unknownToolCallStatusLimitations(source Session, targetType string) []string {
	limitations := make([]string, 0)
	for _, message := range source.Messages {
		for _, part := range message.Parts {
			if part.Type != "tool_call" || part.ToolCall == nil || part.ToolCall.ID == "" || part.ToolCall.Status != "" {
				continue
			}
			status := rawToolCallStatus(part.Raw)
			if status == "" || acpToolCallStatus(status) != "" {
				continue
			}
			limitations = append(limitations, fmt.Sprintf("Tool call %q source status %q has no ACP equivalent for the %s projection; the original bytes retain it.", part.ToolCall.ID, status, targetType))
		}
	}
	return limitations
}

// unpairedToolCallLimitations reports tool calls and results that cannot be
// matched by ID and source order in the common session.  A source can
// legitimately end while a tool is still running, and a damaged or
// provider-specific history can contain a result without its call.  Keep
// these records in the projection when the target can represent them, but
// make the limitation explicit so a caller does not mistake an open call for
// a completed one.
//
// Counts are calculated per occurrence rather than by set membership.  This
// matters for malformed histories with duplicate IDs and for parallel calls:
// one result pairs with one earlier call, while any remaining occurrences are
// reported.  Only IDs and counts are emitted; tool inputs and outputs are
// deliberately excluded from the diagnostic.
func unpairedToolCallLimitations(source Session) []string {
	openCalls := make(map[string]int)
	orphanResults := make(map[string]int)
	for _, message := range source.Messages {
		for _, part := range message.Parts {
			switch part.Type {
			case "tool_call":
				if part.ToolCall != nil {
					openCalls[part.ToolCall.ID]++
				}
			case "tool_result":
				if part.ToolResult != nil {
					if openCalls[part.ToolResult.ID] > 0 {
						openCalls[part.ToolResult.ID]--
					} else {
						orphanResults[part.ToolResult.ID]++
					}
				}
			}
		}
	}

	callParts, callCount := formatUnpairedToolIDs(openCalls)
	resultParts, resultCount := formatUnpairedToolIDs(orphanResults)
	limitations := make([]string, 0, 2)
	if callCount > 0 {
		limitations = append(limitations, fmt.Sprintf("Source contains %d unpaired tool calls without results (%s); the original bytes retain them.", callCount, strings.Join(callParts, ", ")))
	}
	if resultCount > 0 {
		limitations = append(limitations, fmt.Sprintf("Source contains %d unpaired tool results without calls (%s); the original bytes retain them.", resultCount, strings.Join(resultParts, ", ")))
	}
	return limitations
}

func formatUnpairedToolIDs(counts map[string]int) ([]string, int) {
	ids := make([]string, 0)
	total := 0
	for id, count := range counts {
		if count <= 0 {
			continue
		}
		total += count
		if id == "" {
			id = "<missing id>"
		}
		ids = append(ids, fmt.Sprintf("%s=%d", id, count))
	}
	sort.Strings(ids)
	return ids, total
}

// duplicateToolIDLimitations calls out repeated IDs even when every
// occurrence happens to have a result.  A count-balanced duplicate is still
// ambiguous to providers that require IDs to be unique, so it must not be
// mistaken for a lossless pairing.  As with the unpaired report, this emits
// only IDs and counts and never tool inputs or outputs.
func duplicateToolIDLimitations(source Session) []string {
	calls := make(map[string]int)
	results := make(map[string]int)
	for _, message := range source.Messages {
		for _, part := range message.Parts {
			switch part.Type {
			case "tool_call":
				if part.ToolCall != nil {
					calls[part.ToolCall.ID]++
				}
			case "tool_result":
				if part.ToolResult != nil {
					results[part.ToolResult.ID]++
				}
			}
		}
	}

	callIDs := duplicateToolIDs(calls)
	resultIDs := duplicateToolIDs(results)
	limitations := make([]string, 0, 2)
	if len(callIDs) > 0 {
		limitations = append(limitations, fmt.Sprintf("Source contains duplicate tool call IDs (%s); the original bytes retain them.", strings.Join(callIDs, ", ")))
	}
	if len(resultIDs) > 0 {
		limitations = append(limitations, fmt.Sprintf("Source contains duplicate tool result IDs (%s); the original bytes retain them.", strings.Join(resultIDs, ", ")))
	}
	return limitations
}

func duplicateToolIDs(counts map[string]int) []string {
	ids := make([]string, 0)
	for id, count := range counts {
		if count < 2 {
			continue
		}
		if id == "" {
			id = "<missing id>"
		}
		ids = append(ids, fmt.Sprintf("%s=%d", id, count))
	}
	sort.Strings(ids)
	return ids
}

func rawToolCallStatus(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value struct {
		Status string          `json:"status"`
		State  json.RawMessage `json:"state"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	if value.Status == "" && len(value.State) > 0 {
		var state struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(value.State, &state) == nil {
			return state.Status
		}
	}
	return value.Status
}

func validateOriginalIdentity(s Session, codec Codec) error {
	original, err := codec.Read(bytes.NewReader(s.Original))
	if err != nil {
		return fmt.Errorf("original %s session cannot be validated: %w", s.SourceType, err)
	}
	if original.SourceType != s.SourceType {
		return fmt.Errorf("original session type is %q, not %q", original.SourceType, s.SourceType)
	}
	if original.NativeID != s.NativeID {
		return fmt.Errorf("original session ID %q does not match session ID %q", original.NativeID, s.NativeID)
	}
	if original.Workspace != s.Workspace {
		return fmt.Errorf("original workspace %q does not match session workspace %q", original.Workspace, s.Workspace)
	}
	return nil
}

func validateTargetIdentity(source, target Session, targetType string) error {
	if target.SourceType != targetType {
		return fmt.Errorf("read-back type is %q, not %q", target.SourceType, targetType)
	}
	if target.NativeID != source.NativeID {
		return fmt.Errorf("read-back session ID %q does not match source ID %q", target.NativeID, source.NativeID)
	}
	// Gemini stores a project hash rather than the source workspace, so its
	// reader cannot recover this field for a read-back identity check. Qoder
	// keeps the workspace in an optional sidecar outside the JSONL stream.
	if targetType != "gemini" && targetType != "qoder" && target.Workspace != source.Workspace {
		return fmt.Errorf("read-back workspace %q does not match source workspace %q", target.Workspace, source.Workspace)
	}
	return nil
}

// providerDisplayName gives limitation messages the provider's proper name.
func providerDisplayName(target string) string {
	switch target {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "gemini":
		return "Gemini"
	case "opencode":
		return "OpenCode"
	case "trae":
		return "Trae"
	case "pi":
		return "Pi"
	case "qoder":
		return "Qoder"
	case "deepseek":
		return "DeepSeek"
	case "qwen":
		return "Qwen"
	case "copilot":
		return "Copilot"
	default:
		return target
	}
}

func projectedParts(s Session) ([]projectedPart, error) {
	parts := make([]projectedPart, 0)
	for messageIndex, message := range s.Messages {
		for partIndex, part := range message.Parts {
			switch part.Type {
			case "text":
				parts = append(parts, projectedPart{Role: message.Role, Type: "text", Text: part.Text, Timestamp: message.Timestamp})
			case "tool_call":
				if part.ToolCall == nil {
					return nil, fmt.Errorf("message %d part %d: missing tool call", messageIndex, partIndex)
				}
				input, err := canonicalToolInputForCall(part)
				if err != nil {
					return nil, fmt.Errorf("message %d part %d: %w", messageIndex, partIndex, err)
				}
				parts = append(parts, projectedPart{Role: "assistant", Type: "tool_call", ID: part.ToolCall.ID, Name: qualifiedToolName(part.ToolCall), Input: input, Status: part.ToolCall.Status, Timestamp: message.Timestamp})
			case "tool_result":
				if part.ToolResult == nil {
					return nil, fmt.Errorf("message %d part %d: missing tool result", messageIndex, partIndex)
				}
				content, err := toolResultText(part.ToolResult)
				if err != nil {
					return nil, fmt.Errorf("message %d part %d: %w", messageIndex, partIndex, err)
				}
				parts = append(parts, projectedPart{Role: "tool", Type: "tool_result", ID: part.ToolResult.ID, Text: content, Timestamp: message.Timestamp})
			case "image", "input_image":
				image, err := canonicalImage(part)
				if err != nil {
					return nil, fmt.Errorf("message %d part %d: %w", messageIndex, partIndex, err)
				}
				parts = append(parts, projectedPart{Role: message.Role, Type: "image", Input: image, Timestamp: message.Timestamp})
			default:
				return nil, fmt.Errorf("message %d part %d: unsupported content %q", messageIndex, partIndex, part.Type)
			}
		}
	}
	return parts, nil
}

// pairToolResults moves every tool result to directly follow its matching
// call. Results whose call never appears keep their relative order at the
// end.
// pairToolResults rebuilds the part order as every provider can express it:
// each tool result sits directly after its call, call-result units follow the
// call order, and anything else keeps its place. Providers that couple a call
// and its result in one record (one Gemini toolCalls entry, one OpenCode tool
// part) can only express this order, so both sides of a comparison normalize
// to it. Result timestamps stay attached to the result so an accidental
// timestamp change is still detected by validateProjection.
func pairToolResults(parts []projectedPart) []projectedPart {
	results := make(map[string][]projectedPart)
	calls := make(map[string]bool)
	for _, p := range parts {
		switch p.Type {
		case "tool_call":
			calls[p.ID] = true
		case "tool_result":
			results[p.ID] = append(results[p.ID], p)
		}
	}
	out := make([]projectedPart, 0, len(parts))
	// Results without a corresponding call cannot be paired. Keep them in their
	// original order, but append them after the representable call/result units.
	var unmatched []projectedPart
	for _, p := range parts {
		switch p.Type {
		case "tool_result":
			if !calls[p.ID] {
				unmatched = append(unmatched, p)
			}
		case "tool_call":
			out = append(out, p)
			for _, result := range results[p.ID] {
				out = append(out, result)
			}
			// Consume this queue so a duplicate call ID (even if malformed input
			// gets this far) cannot duplicate its results.
			delete(results, p.ID)
		default:
			out = append(out, p)
		}
	}
	return append(out, unmatched...)
}

func canonicalToolInput(raw json.RawMessage) (string, error) {
	input, _, err := wrapToolInput(raw)
	if err != nil {
		return "", err
	}
	// Normalize through json.Number, never float64: large integers must stay
	// exact, or the comparison would call two different numbers equal.
	exact, err := exactJSON(input)
	if err != nil {
		return "", fmt.Errorf("tool input cannot be normalized: %w", err)
	}
	return string(exact), nil
}

func canonicalToolInputForCall(part ContentBlock) (string, error) {
	input, _, err := wrapToolInputForCall(part)
	if err != nil {
		return "", err
	}
	// Normalize through json.Number, never float64: large integers must stay
	// exact, or the comparison would call two different numbers equal.
	exact, err := exactJSON(input)
	if err != nil {
		return "", fmt.Errorf("tool input cannot be normalized: %w", err)
	}
	return string(exact), nil
}

// canonicalImage reduces the two provider-specific image blocks to the same
// data-or-URL identity. The provider conversion helper is called first so the
// comparison enforces the same representability rules as the writer.
func canonicalImage(p ContentBlock) (string, error) {
	var converted json.RawMessage
	var err error
	switch p.Type {
	case "image":
		converted, err = imageForProvider(p, "codex")
	case "input_image":
		converted, err = imageForProvider(p, "claude")
	default:
		return "", fmt.Errorf("unsupported image type %q", p.Type)
	}
	if err != nil {
		return "", err
	}
	if p.Type == "image" {
		var block struct {
			ImageURL string `json:"image_url"`
		}
		if err := json.Unmarshal(converted, &block); err != nil || block.ImageURL == "" {
			return "", fmt.Errorf("converted image has no image_url")
		}
		return canonicalImageURL(block.ImageURL)
	}
	var block struct {
		Source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
			URL       string `json:"url"`
		} `json:"source"`
	}
	if err := json.Unmarshal(converted, &block); err != nil {
		return "", fmt.Errorf("converted image is invalid: %w", err)
	}
	switch block.Source.Type {
	case "base64":
		return canonicalJSON(map[string]string{"kind": "base64", "media_type": block.Source.MediaType, "data": block.Source.Data})
	case "url":
		return canonicalJSON(map[string]string{"kind": "url", "url": block.Source.URL})
	default:
		return "", fmt.Errorf("converted image source type %q is unsupported", block.Source.Type)
	}
}

func canonicalImageURL(imageURL string) (string, error) {
	if len(imageURL) >= 5 && imageURL[:5] == "data:" {
		mediaType, data, err := decodeDataURL(imageURL)
		if err != nil {
			return "", err
		}
		return canonicalJSON(map[string]string{"kind": "base64", "media_type": mediaType, "data": data})
	}
	return canonicalJSON(map[string]string{"kind": "url", "url": imageURL})
}

func canonicalJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("cannot normalize content: %w", err)
	}
	return string(encoded), nil
}

// skippedRecordLimitations reports top-level records that the target cannot
// represent. The reader may intentionally omit records of known types (for
// example duplicate display events), so those are not reported as source loss
// here unless the target boundary makes the omission certain.
func skippedRecordLimitations(s Session, target string) []string {
	if s.SourceType == "gemini" {
		limitations := []string{"Gemini replay, patch, and provider metadata records are not represented in the target projection; the original bytes retain them."}
		return append(limitations, skippedGeminiMessageLimitations(s)...)
	}
	if s.SourceType == "opencode" {
		return skippedOpenCodeLimitations(s)
	}
	if s.SourceType == "claude" {
		return skippedClaudeLimitations(s, target)
	}
	if s.SourceType == "trae" {
		return skippedTraeLimitations(s, target)
	}
	if s.SourceType == "pi" {
		return skippedPiLimitations(s, target)
	}
	if s.SourceType == "qoder" {
		return skippedQoderLimitations(s)
	}
	if s.SourceType == "deepseek" {
		return skippedDeepSeekLimitations(s)
	}
	if s.SourceType == "qwen" {
		return skippedQwenLimitations(s)
	}
	if s.SourceType == "copilot" {
		return skippedCopilotLimitations(s)
	}
	counts := make(map[string]int)
	for _, line := range bytes.Split(s.Original, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &record) != nil || representedRecord(s.SourceType, target, record.Type) {
			continue
		}
		typeName := record.Type
		if typeName == "" {
			typeName = "<missing type>"
		}
		counts[typeName]++
	}
	if len(counts) == 0 {
		return skippedCodexSubtypeLimitations(s)
	}
	types := make([]string, 0, len(counts))
	for typeName := range counts {
		types = append(types, typeName)
	}
	// JSONL record order is not semantically relevant here; sorting keeps the
	// limitation stable for callers and tests.
	sort.Strings(types)
	parts := make([]string, 0, len(types))
	for _, typeName := range types {
		parts = append(parts, fmt.Sprintf("%s=%d", typeName, counts[typeName]))
	}
	limitations := []string{fmt.Sprintf("Source contains unrepresented top-level records (%s); the original bytes retain them.", strings.Join(parts, ", "))}
	return append(limitations, skippedCodexSubtypeLimitations(s)...)
}

func skippedClaudeLimitations(s Session, target string) []string {
	counts := make(map[string]int)
	for _, line := range bytes.Split(s.Original, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			counts["<invalid JSON>"]++
			continue
		}
		represented := record.Type == "user" || record.Type == "assistant" || record.Type == "summary"
		if !represented {
			name := record.Type
			if name == "" {
				name = "<missing type>"
			}
			counts[name]++
		}
	}
	limitations := formatProviderRecordLimitations("Claude", counts)
	if thought := skippedClaudeThinking(s); thought != "" {
		limitations = append(limitations, thought)
	}
	return limitations
}

// skippedTraeLimitations keeps TRAE's provider-specific JSONL records visible
// when the common message projection omits them.
func skippedTraeLimitations(s Session, target string) []string {
	counts := make(map[string]int)
	for _, line := range bytes.Split(s.Original, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			counts["<invalid JSON>"]++
			continue
		}
		switch record.Type {
		case "session_meta":
		case "history_mutation":
			var value struct {
				Operation          string            `json:"operation"`
				Items              []json.RawMessage `json:"items"`
				DisplayCompletions []struct {
					Item json.RawMessage `json:"item"`
				} `json:"display_completions"`
			}
			if json.Unmarshal(record.Payload, &value) != nil {
				counts["history_mutation=<invalid payload>"]++
				continue
			}
			if value.Operation != "append" {
				name := value.Operation
				if name == "" {
					name = "<missing operation>"
				}
				counts["history_mutation="+name]++
			}
			for _, item := range value.Items {
				if traeSyntheticContextMessage(item) {
					counts["history_mutation.item=synthetic_context"]++
					continue
				}
				var itemType struct {
					Type string `json:"type"`
					Role string `json:"role"`
				}
				if json.Unmarshal(item, &itemType) != nil || !traeSupportedItemType(itemType.Type) {
					name := itemType.Type
					if name == "" {
						name = "<missing type>"
					}
					counts["history_mutation.item="+name]++
				} else if itemType.Type == "message" && itemType.Role != "user" && itemType.Role != "assistant" {
					role := itemType.Role
					if role == "" {
						role = "<missing role>"
					}
					counts["history_mutation.item=message/"+role]++
				}
				traeCountUnsupportedContent(item, counts)
			}
		case "response_item":
			var value struct {
				Type string `json:"type"`
				Role string `json:"role"`
			}
			if traeSyntheticContextMessage(record.Payload) {
				counts["response_item=synthetic_context"]++
			} else if json.Unmarshal(record.Payload, &value) != nil || !traeSupportedItemType(value.Type) {
				name := value.Type
				if name == "" {
					name = "<missing type>"
				}
				counts["response_item="+name]++
			} else if value.Type == "message" && value.Role != "user" && value.Role != "assistant" {
				role := value.Role
				if role == "" {
					role = "<missing role>"
				}
				counts["response_item=message/"+role]++
			}
			traeCountUnsupportedContent(record.Payload, counts)
		case "event_msg":
			var value struct {
				Type string          `json:"type"`
				Item json.RawMessage `json:"item"`
			}
			if err := json.Unmarshal(record.Payload, &value); err != nil {
				counts["event_msg=<invalid payload>"]++
			} else if !traeSupportedEventType(value.Type) {
				name := value.Type
				if name == "" {
					name = "<missing type>"
				}
				counts["event_msg="+name]++
			} else if value.Type == "item_completed" {
				var item struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(value.Item, &item) != nil || !traeSupportedCompletedItemType(item.Type) {
					name := item.Type
					if name == "" {
						name = "<missing type>"
					}
					counts["event_msg.item_completed.item="+name]++
				}
			}
			traeCountUnsupportedContent(value.Item, counts)
		case "compacted":
			if target != "codex" && target != "trae" && target != "pi" {
				counts[record.Type]++
			}
		case "turn_context", "world_state", "token_usage_record", "inter_agent_communication":
			counts[record.Type]++
		default:
			name := record.Type
			if name == "" {
				name = "<missing type>"
			}
			counts[name]++
		}
	}
	return formatProviderRecordLimitations("Trae", counts)
}

func traeSupportedCompletedItemType(kind string) bool {
	switch kind {
	case "UserMessage", "AgentMessage":
		return true
	default:
		return false
	}
}

func traeSupportedItemType(kind string) bool {
	switch kind {
	case "message", "function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output":
		return true
	default:
		return false
	}
}

// filterTraeEnvelopeMessages removes user text parts that Trae's reader treats
// as its own injected context (AGENTS.md instructions, environment context).
// Trae re-injects that context natively and drops such records on read-back,
// so writing them would fail projection validation; the count feeds the
// limitation that reports the removal. The source messages are not modified.
func filterTraeEnvelopeMessages(messages []Message) ([]Message, int) {
	filtered := make([]Message, 0, len(messages))
	dropped := 0
	for _, message := range messages {
		parts := make([]ContentBlock, 0, len(message.Parts))
		for _, part := range message.Parts {
			if message.Role == "user" && part.Type == "text" && traeSyntheticContextText(part.Text) {
				dropped++
				continue
			}
			parts = append(parts, part)
		}
		if len(parts) == 0 && len(message.Parts) > 0 {
			continue
		}
		message.Parts = parts
		filtered = append(filtered, message)
	}
	return filtered, dropped
}

func traeSupportedEventType(kind string) bool {
	switch kind {
	case "user_message", "agent_message", "item_completed":
		return true
	default:
		return false
	}
}

func traeCountUnsupportedContent(raw json.RawMessage, counts map[string]int) {
	if len(raw) == 0 {
		return
	}
	var item struct {
		Content json.RawMessage `json:"content"`
		Output  json.RawMessage `json:"output"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return
	}
	traeCountReasoningBlocks(item.Content, counts)
	// Tool outputs carry their own typed blocks; reasoning stripped from them
	// by traeStripReasoning must be reported like message-content reasoning.
	traeCountReasoningBlocks(item.Output, counts)
}

func traeCountReasoningBlocks(raw json.RawMessage, counts map[string]int) {
	if len(raw) == 0 {
		return
	}
	type contentBlock struct {
		Type   string `json:"type"`
		Source string `json:"source"`
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		var block contentBlock
		if json.Unmarshal(raw, &block) != nil {
			return
		}
		blocks = append(blocks, block)
	}
	for _, block := range blocks {
		if traeReasoningContentKind(block.Type) {
			counts["message.content="+block.Type]++
		} else if strings.EqualFold(block.Source, "reasoning") {
			counts["message.content=text(source=reasoning)"]++
		}
	}
}

// skippedPiLimitations reports Pi records outside the active common-message
// projection, including inactive branches and provider-only message content.
func skippedPiLimitations(s Session, target string) []string {
	counts := make(map[string]int)
	var entries []piEntry
	version := 3
	for _, line := range bytes.Split(s.Original, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var head struct {
			Type    string `json:"type"`
			Version int    `json:"version"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			counts["<invalid JSON>"]++
			continue
		}
		if head.Type == "session" {
			if head.Version > 0 {
				version = head.Version
			}
			continue
		}
		var entry piEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			counts[head.Type]++
			continue
		}
		entries = append(entries, entry)
		switch entry.Type {
		case "message":
			var message piMessage
			if json.Unmarshal(entry.Message, &message) != nil {
				counts["message=<invalid payload>"]++
				continue
			}
			if message.Role != "user" && message.Role != "assistant" && message.Role != "toolResult" {
				name := message.Role
				if name == "" {
					name = "<missing role>"
				}
				counts["message.role="+name]++
			}
			// Empty user and assistant messages vanish from the projection;
			// report them so their omission stays visible.
			if piContentIsEmpty(message.Content) {
				counts["message.content=<empty>"]++
			}
			var blocks []struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(message.Content, &blocks) == nil {
				for _, block := range blocks {
					if block.Type == "thinking" || (block.Type != "text" && block.Type != "image" && block.Type != "toolCall") {
						name := block.Type
						if name == "" {
							name = "<missing type>"
						}
						counts["message.content="+name]++
					}
				}
			}
		case "compaction", "branch_summary":
			if target != "codex" && target != "trae" && target != "pi" {
				counts[entry.Type]++
			}
		default:
			name := entry.Type
			if name == "" {
				name = "<missing type>"
			}
			counts[name]++
		}
	}
	if len(entries) > 0 {
		if path, err := piActivePath(append([]piEntry(nil), entries...), version); err == nil {
			active := make(map[string]bool, len(path))
			for _, entry := range path {
				active[entry.ID] = true
			}
			for _, entry := range entries {
				if entry.ID != "" && !active[entry.ID] {
					counts["inactive_branch"]++
				}
			}
		}
	}
	return formatProviderRecordLimitations("Pi", counts)
}

func formatProviderRecordLimitations(provider string, counts map[string]int) []string {
	if len(counts) == 0 {
		return nil
	}
	types := make([]string, 0, len(counts))
	for name := range counts {
		types = append(types, name)
	}
	sort.Strings(types)
	parts := make([]string, 0, len(types))
	for _, name := range types {
		parts = append(parts, fmt.Sprintf("%s=%d", name, counts[name]))
	}
	return []string{fmt.Sprintf("%s source contains unrepresented records (%s); the original bytes retain them.", provider, strings.Join(parts, ", "))}
}

// skippedClaudeThinking counts Claude thinking blocks, which carry model
// reasoning that no target part represents.
func skippedClaudeThinking(s Session) string {
	count := 0
	for _, line := range bytes.Split(s.Original, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record struct {
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &record) != nil || len(record.Message) == 0 {
			continue
		}
		var envelope struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		}
		if json.Unmarshal(record.Message, &envelope) != nil {
			continue
		}
		for _, block := range envelope.Content {
			if block.Type == "thinking" {
				count++
			}
		}
	}
	if count == 0 {
		return ""
	}
	return fmt.Sprintf("Claude source contains %d thinking blocks with model reasoning that is not represented; the original bytes retain them.", count)
}

func skippedGeminiMessageLimitations(s Session) []string {
	messageCounts := make(map[string]int)
	topLevelCounts := make(map[string]int)
	thoughts := 0
	data := bytes.TrimSpace(s.Original)
	if len(data) == 0 {
		return nil
	}
	var legacy struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(data, &legacy) == nil && legacy.Messages != nil {
		for _, message := range legacy.Messages {
			if hasGeminiThoughts(message) {
				thoughts++
			}
			name := ""
			if raw, ok := message["type"]; ok {
				_ = json.Unmarshal(raw, &name)
			}
			if name != "user" && name != "gemini" {
				if name == "" {
					name = "<missing type>"
				}
				messageCounts[name]++
			}
		}
	} else {
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			var record map[string]json.RawMessage
			if json.Unmarshal(line, &record) != nil {
				continue
			}
			if _, ok := record["sessionId"]; ok {
				continue
			}
			if _, ok := record["$set"]; ok {
				continue
			}
			if _, ok := record["$patch"]; ok {
				continue
			}
			if _, ok := record["$rewindTo"]; ok {
				continue
			}
			id, hasID := record["id"]
			var name string
			rawType, hasType := record["type"]
			if hasType {
				_ = json.Unmarshal(rawType, &name)
			}
			if !hasID || string(id) == "null" {
				if hasType && name != "user" && name != "gemini" {
					if name == "" {
						name = "<invalid type>"
					}
					topLevelCounts[name]++
				}
				continue
			}
			if hasGeminiThoughts(record) {
				thoughts++
			}
			if name != "user" && name != "gemini" {
				if name == "" {
					name = "<missing type>"
				}
				messageCounts[name]++
			}
		}
	}
	limitations := make([]string, 0, 3)
	if text := formatGeminiCounts("unsupported message types", messageCounts); text != "" {
		limitations = append(limitations, text)
	}
	if text := formatGeminiCounts("unrepresented top-level records", topLevelCounts); text != "" {
		limitations = append(limitations, text)
	}
	if thoughts > 0 {
		limitations = append(limitations, fmt.Sprintf("Gemini source contains %d messages with model thoughts that are not represented; the original bytes retain them.", thoughts))
	}
	return limitations
}

// hasGeminiThoughts reports whether a Gemini message record carries the
// model's reasoning text ("thoughts"), which has no cross-provider part.
func hasGeminiThoughts(record map[string]json.RawMessage) bool {
	raw, ok := record["thoughts"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var thoughts []json.RawMessage
	return json.Unmarshal(raw, &thoughts) == nil && len(thoughts) > 0
}

func formatGeminiCounts(label string, counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	types := make([]string, 0, len(counts))
	for name := range counts {
		types = append(types, name)
	}
	sort.Strings(types)
	parts := make([]string, 0, len(types))
	for _, name := range types {
		parts = append(parts, fmt.Sprintf("%s=%d", name, counts[name]))
	}
	return fmt.Sprintf("Gemini source contains %s (%s); the original bytes retain them.", label, strings.Join(parts, ", "))
}

// structuredResultCount counts tool results whose structured payload is
// flattened to JSON text by every provider except Gemini.
func structuredResultCount(s Session) int {
	count := 0
	for _, message := range s.Messages {
		for _, part := range message.Parts {
			if part.Type != "tool_result" || part.ToolResult == nil || len(part.ToolResult.RawContent) == 0 {
				continue
			}
			var plain string
			if isJSONNull(part.ToolResult.RawContent) || json.Unmarshal(part.ToolResult.RawContent, &plain) != nil {
				count++
			}
		}
	}
	return count
}

// piFlattenedResults counts structured tool results that Pi cannot carry
// natively and therefore serializes as JSON text. Block-shaped arrays are
// stored natively by Pi and must not be reported as flattened.
func piFlattenedResults(s Session) int {
	count := 0
	for _, message := range s.Messages {
		for _, part := range message.Parts {
			if part.Type != "tool_result" || part.ToolResult == nil || len(part.ToolResult.RawContent) == 0 {
				continue
			}
			var plain string
			if isJSONNull(part.ToolResult.RawContent) || json.Unmarshal(part.ToolResult.RawContent, &plain) != nil {
				if _, err := piToolResultBlocks(part.ToolResult.RawContent); err != nil {
					count++
				}
			}
		}
	}
	return count
}

// freeTextInputCalls counts tool calls whose arguments are free text rather
// than a JSON object, which object-only targets can only carry wrapped.
func freeTextInputCalls(s Session) int {
	count := 0
	for _, message := range s.Messages {
		for _, part := range message.Parts {
			if part.Type != "tool_call" || part.ToolCall == nil || len(part.ToolCall.Input) == 0 {
				continue
			}
			if isCustomToolCall(part) {
				if _, err := customToolInput(part.ToolCall.Input); err == nil {
					count++
				}
				continue
			}
			if _, ok := decodeObjectInput(part.ToolCall.Input); !ok {
				var text string
				if json.Unmarshal(part.ToolCall.Input, &text) == nil {
					count++
				}
			}
		}
	}
	return count
}

// errorFlaggedResults counts tool results whose failure flag cannot survive
// providers without an error marker.
func errorFlaggedResults(s Session) int {
	count := 0
	for _, message := range s.Messages {
		for _, part := range message.Parts {
			if part.Type == "tool_result" && part.ToolResult != nil && part.ToolResult.IsError {
				count++
			}
		}
	}
	return count
}

func skippedCodexSubtypeLimitations(s Session) []string {
	if s.SourceType != "codex" {
		return nil
	}
	responseSubtypes := make(map[string]int)
	eventSubtypes := make(map[string]int)
	roleCounts := make(map[string]int)
	replacementHistories := 0
	thinkingBlocks := 0
	for _, line := range bytes.Split(s.Original, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(line, &record) != nil {
			continue
		}
		var payload struct {
			Type               string          `json:"type"`
			Role               string          `json:"role"`
			CallID             string          `json:"call_id"`
			Name               string          `json:"name"`
			Content            json.RawMessage `json:"content"`
			ReplacementHistory json.RawMessage `json:"replacement_history"`
		}
		if record.Type != "response_item" && record.Type != "event_msg" && record.Type != "compacted" {
			continue
		}
		if json.Unmarshal(record.Payload, &payload) != nil {
			continue
		}
		switch record.Type {
		case "response_item":
			if !knownResponseSubtype(payload.Type) {
				responseSubtypes[subtypeName(payload.Type)]++
			}
			// Role messages such as developer and system carry real session
			// instructions. The reader omits them because no target role
			// exists, so they must be reported instead of vanishing.
			if payload.Type == "message" && payload.Role != "" && payload.Role != "user" && payload.Role != "assistant" {
				roleCounts[payload.Role]++
			}
			// Model reasoning inside message content has no cross-provider
			// part; count it so its omission stays visible.
			if payload.Type == "message" && len(payload.Content) > 0 {
				var blocks []struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(payload.Content, &blocks) == nil {
					for _, block := range blocks {
						if block.Type == "thinking" {
							thinkingBlocks++
						}
					}
				}
			}
			if payload.Type == "function_call_output" && payload.CallID == "" && payload.Name != "" {
				roleCounts["<delegation output without call_id>"]++
			}
		case "event_msg":
			if !knownEventSubtype(payload.Type) {
				eventSubtypes[subtypeName(payload.Type)]++
			}
		case "compacted":
			if len(payload.ReplacementHistory) > 0 && string(payload.ReplacementHistory) != "null" {
				replacementHistories++
			}
		}
	}
	limitations := make([]string, 0, 4)
	if text := formatSubtypeCounts("response_item", responseSubtypes); text != "" {
		limitations = append(limitations, text)
	}
	if text := formatSubtypeCounts("event_msg", eventSubtypes); text != "" {
		limitations = append(limitations, text)
	}
	if len(roleCounts) > 0 {
		types := make([]string, 0, len(roleCounts))
		for role := range roleCounts {
			types = append(types, role)
		}
		sort.Strings(types)
		parts := make([]string, 0, len(types))
		for _, role := range types {
			parts = append(parts, fmt.Sprintf("%s=%d", role, roleCounts[role]))
		}
		limitations = append(limitations, fmt.Sprintf("Source contains Codex role messages with no target representation (%s); the original bytes retain them.", strings.Join(parts, ", ")))
	}
	if replacementHistories > 0 {
		limitations = append(limitations, fmt.Sprintf("Source contains %d compacted records whose replacement histories are not expanded; the original bytes retain them.", replacementHistories))
	}
	if thinkingBlocks > 0 {
		limitations = append(limitations, fmt.Sprintf("Source contains %d Codex thinking blocks that have no cross-provider part; the original bytes retain them.", thinkingBlocks))
	}
	return limitations
}

func knownResponseSubtype(subtype string) bool {
	switch subtype {
	case "message", "function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output":
		return true
	default:
		return false
	}
}

func knownEventSubtype(subtype string) bool {
	return subtype == "user_message" || subtype == "agent_message"
}

func subtypeName(subtype string) string {
	if subtype == "" {
		return "<missing subtype>"
	}
	return subtype
}

func formatSubtypeCounts(recordType string, counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	types := make([]string, 0, len(counts))
	for subtype := range counts {
		types = append(types, subtype)
	}
	sort.Strings(types)
	parts := make([]string, 0, len(types))
	for _, subtype := range types {
		parts = append(parts, fmt.Sprintf("%s=%d", subtype, counts[subtype]))
	}
	return fmt.Sprintf("Source contains unrepresented Codex %s subtypes (%s); the original bytes retain them.", recordType, strings.Join(parts, ", "))
}

func representedRecord(source, target, recordType string) bool {
	if source == "claude" {
		return recordType == "user" || recordType == "assistant" || recordType == "summary"
	}
	if source != "codex" {
		switch source {
		case "trae":
			// These records either feed the common dialogue model or carry the
			// session identity. Provider-specific state and unknown records are
			// intentionally reported by skippedTraeLimitations.
			return recordType == "session_meta" || recordType == "history_mutation" || recordType == "response_item" || recordType == "event_msg" || recordType == "compacted"
		case "pi":
			return recordType == "session" || recordType == "message" || recordType == "compaction" || recordType == "branch_summary"
		default:
			return false
		}
	}
	if target == "claude" {
		return recordType == "response_item" || recordType == "event_msg" || recordType == "compacted"
	}
	return recordType == "session_meta" || recordType == "response_item" || recordType == "event_msg" || recordType == "compacted"
}
