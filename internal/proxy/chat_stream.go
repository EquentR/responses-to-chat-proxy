package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

type chatCompletionStreamNormalizer struct {
	buffer          string
	sawChunk        bool
	sawDone         bool
	sawFinishReason bool
	lastID          string
	lastObject      string
	lastModel       string
	lastCreated     int
	choiceIndexes   map[int]struct{}
}

func newChatCompletionStreamNormalizer() *chatCompletionStreamNormalizer {
	return &chatCompletionStreamNormalizer{
		choiceIndexes: map[int]struct{}{},
	}
}

func (n *chatCompletionStreamNormalizer) Feed(chunk []byte) [][]byte {
	n.buffer += strings.ReplaceAll(strings.ReplaceAll(string(chunk), "\r\n", "\n"), "\r", "\n")
	var events [][]byte

	for {
		index := strings.Index(n.buffer, "\n\n")
		if index < 0 {
			break
		}
		rawEvent := n.buffer[:index]
		n.buffer = n.buffer[index+2:]
		events = append(events, n.processEvent(rawEvent)...)
	}

	return events
}

func (n *chatCompletionStreamNormalizer) Finish() [][]byte {
	var events [][]byte
	if strings.TrimSpace(n.buffer) != "" {
		events = append(events, n.processEvent(n.buffer)...)
	}
	n.buffer = ""
	events = append(events, n.finishEvents()...)
	return events
}

func (n *chatCompletionStreamNormalizer) processEvent(rawEvent string) [][]byte {
	trimmed := strings.TrimSpace(rawEvent)
	if trimmed == "" {
		return nil
	}

	var dataLines []string
	for _, line := range strings.Split(rawEvent, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(dataLines) == 0 {
		return [][]byte{[]byte(trimmed + "\n\n")}
	}

	payload := strings.TrimSpace(strings.Join(dataLines, "\n"))
	if payload == "[DONE]" {
		n.sawDone = true
		events := n.maybeSynthesizeFinishReason()
		events = append(events, []byte("data: [DONE]\n\n"))
		return events
	}

	var data map[string]any
	if err := json.Unmarshal([]byte(payload), &data); err == nil {
		n.recordChunk(data)
	}

	return [][]byte{[]byte(trimmed + "\n\n")}
}

func (n *chatCompletionStreamNormalizer) recordChunk(data map[string]any) {
	n.sawChunk = true

	if id := stringValue(data["id"]); id != "" {
		n.lastID = id
	}
	if object := stringValue(data["object"]); object != "" {
		n.lastObject = object
	}
	if model := stringValue(data["model"]); model != "" {
		n.lastModel = model
	}
	if created := intValue(data["created"]); created != 0 {
		n.lastCreated = created
	}

	choices, ok := data["choices"].([]any)
	if !ok || len(choices) == 0 {
		return
	}
	for index, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		if finishReason := stringValue(choice["finish_reason"]); finishReason != "" {
			n.sawFinishReason = true
		}
		choiceIndex := index
		if rawIndex, ok := choice["index"]; ok {
			choiceIndex = intValue(rawIndex)
		}
		n.choiceIndexes[choiceIndex] = struct{}{}
	}
}

func (n *chatCompletionStreamNormalizer) finishEvents() [][]byte {
	if n.sawDone {
		return nil
	}
	events := n.maybeSynthesizeFinishReason()
	if n.sawChunk {
		events = append(events, []byte("data: [DONE]\n\n"))
	}
	n.sawDone = true
	return events
}

func (n *chatCompletionStreamNormalizer) maybeSynthesizeFinishReason() [][]byte {
	if !n.sawChunk || n.sawFinishReason {
		return nil
	}
	n.sawFinishReason = true

	chunk := map[string]any{
		"object":  valueOrDefault(n.lastObject, "chat.completion.chunk"),
		"choices": n.finishChoices(),
	}
	if n.lastID != "" {
		chunk["id"] = n.lastID
	}
	if n.lastModel != "" {
		chunk["model"] = n.lastModel
	}
	if n.lastCreated != 0 {
		chunk["created"] = n.lastCreated
	}

	body, _ := json.Marshal(chunk)
	return [][]byte{[]byte(fmt.Sprintf("data: %s\n\n", body))}
}

func (n *chatCompletionStreamNormalizer) finishChoices() []map[string]any {
	indexes := make([]int, 0, len(n.choiceIndexes))
	for index := range n.choiceIndexes {
		indexes = append(indexes, index)
	}
	if len(indexes) == 0 {
		indexes = append(indexes, 0)
	}
	for i := 0; i < len(indexes); i++ {
		for j := i + 1; j < len(indexes); j++ {
			if indexes[j] < indexes[i] {
				indexes[i], indexes[j] = indexes[j], indexes[i]
			}
		}
	}

	choices := make([]map[string]any, 0, len(indexes))
	for _, index := range indexes {
		choices = append(choices, map[string]any{
			"index":         index,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		})
	}
	return choices
}
