package chat

import (
	"encoding/json"
	"strings"
)

// Question is what the agent asks when it needs a decision: a fenced
// block in its text that the panel turns into buttons, so the operator
// answers with a click instead of typing the option back. The agent
// writes:
//
//	```question
//	{"question": "Which database?", "options": ["SQLite", "Postgres"], "recommended": "SQLite"}
//	```
type Question struct {
	Question    string   `json:"question"`
	Options     []string `json:"options"`
	Recommended string   `json:"recommended,omitempty"`
}

// questionFence opens the block. The fence is matched on its own line so
// a code sample that mentions it inside another block is not read as one.
const questionFence = "```question"

// ParseQuestions finds every question block in text. A block that is not
// valid JSON, or has no question, is skipped rather than failing the
// turn: the text still shows what the agent wrote, and a question the
// panel cannot render is still one the operator can read.
func ParseQuestions(text string) []Question {
	var out []Question
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != questionFence {
			continue
		}
		var body []string
		closed := false
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "```" {
				i = j
				closed = true
				break
			}
			body = append(body, lines[j])
		}
		if !closed {
			break
		}
		var q Question
		if err := json.Unmarshal([]byte(strings.Join(body, "\n")), &q); err != nil {
			continue
		}
		q.Question = strings.TrimSpace(q.Question)
		if q.Question == "" {
			continue
		}
		if q.Options == nil {
			q.Options = []string{}
		}
		out = append(out, q)
	}
	return out
}
