package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ClaudeCode drives the Claude Code CLI in headless mode. It exists for one
// reason: a Claude subscription covers Claude Code and not the API, so a
// person with a plan and no credits can still run the judge, on the terms
// Claude Code itself runs under.
//
// It is the only provider that is a process rather than an HTTP call, and the
// only one that batches. Claude Code loads its own context on every invocation;
// one call per declaration would spend most of a plan's window on overhead.
type ClaudeCode struct {
	bin   string
	model string
	batch int
}

const claudeCodeBatch = 8

func (c *ClaudeCode) Name() string {
	m := c.model
	if m == "" {
		m = "default"
	}
	return "claude-code/" + m
}

func (c *ClaudeCode) BatchSize() int { return c.batch }

func (c *ClaudeCode) Ask(ctx context.Context, system, user string) (json.RawMessage, Usage, error) {
	raws, u, err := c.AskBatch(ctx, system, []string{user})
	if err != nil {
		return nil, u, err
	}
	return raws[0], u, nil
}

// batchSchema wraps the per-case schema so one call answers several cases and
// each answer says which case it is for.
func batchSchema() map[string]any {
	props, required := answerProperties()
	props["index"] = map[string]any{"type": "integer", "description": "the case number, starting at 1"}
	item := map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             append([]string{"index"}, required...),
		"additionalProperties": false,
	}
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"answers": map[string]any{"type": "array", "items": item}},
		"required":             []string{"answers"},
		"additionalProperties": false,
	}
}

func (c *ClaudeCode) AskBatch(ctx context.Context, system string, users []string) ([]json.RawMessage, Usage, error) {
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Judge each of the %d cases below independently. Answer with one entry per case, carrying its number.\n", len(users))
	for i, u := range users {
		fmt.Fprintf(&prompt, "\n===== CASE %d =====\n%s", i+1, u)
	}
	schema, _ := json.Marshal(batchSchema())
	args := []string{
		"-p", "--output-format", "json", "--no-session-persistence",
		"--tools", "", // judgement only: the skill is the path that reads further
		"--system-prompt", system,
		"--json-schema", string(schema),
	}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Stdin = strings.NewReader(prompt.String())
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return nil, Usage{}, fmt.Errorf("claude-code: %v: %s", err, msg)
	}
	var resp struct {
		IsError          bool            `json:"is_error"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		Usage            struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		return nil, Usage{}, fmt.Errorf("claude-code: bad output: %w", err)
	}
	u := Usage{Input: resp.Usage.InputTokens, Output: resp.Usage.OutputTokens}
	if resp.IsError {
		return nil, u, fmt.Errorf("claude-code: %s", strings.TrimSpace(resp.Result))
	}
	if len(resp.StructuredOutput) == 0 {
		return nil, u, fmt.Errorf("claude-code: no structured output (is --json-schema supported by this version?)")
	}
	var parsed struct {
		Answers []struct {
			Index int `json:"index"`
			Answer
		} `json:"answers"`
	}
	if err := json.Unmarshal(resp.StructuredOutput, &parsed); err != nil {
		return nil, u, fmt.Errorf("claude-code: structured output does not match: %w", err)
	}
	raws := make([]json.RawMessage, len(users))
	for _, a := range parsed.Answers {
		if a.Index < 1 || a.Index > len(users) || raws[a.Index-1] != nil {
			continue
		}
		b, err := json.Marshal(a.Answer)
		if err != nil {
			return nil, u, err
		}
		raws[a.Index-1] = b
	}
	for i, r := range raws {
		if r == nil {
			return nil, u, fmt.Errorf("claude-code: no answer for case %d", i+1)
		}
	}
	return raws, u, nil
}

func newClaudeCode(model string) (*ClaudeCode, error) {
	bin := os.Getenv("JUDGE_CLAUDE_BIN")
	if bin == "" {
		bin = "claude"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("claude-code: %q not found on PATH — install Claude Code, or set JUDGE_CLAUDE_BIN", bin)
	}
	return &ClaudeCode{bin: path, model: model, batch: claudeCodeBatch}, nil
}
