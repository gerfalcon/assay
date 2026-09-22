package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Config selects a provider. Three wire protocols cover the clouds asked for;
// a base URL override lets tests and proxies stand in for any of them.
type Config struct {
	Provider string // anthropic | gemini | openai (codex) | claude-code (plan, max)
	Model    string
	BaseURL  string
	APIKey   string // "" reads the provider's usual environment variable
	Effort   string // anthropic only: low | medium | high
}

// New builds the provider. No model is defaulted for gemini or openai: their
// model names change faster than this binary, and a stale default that
// silently 404s is worse than a flag.
func New(c Config) (Provider, error) {
	switch strings.ToLower(c.Provider) {
	case "anthropic", "claude":
		if c.Model == "" {
			c.Model = "claude-opus-5"
		}
		var opts []option.RequestOption
		if c.APIKey != "" {
			opts = append(opts, option.WithAPIKey(c.APIKey))
		}
		if c.BaseURL != "" {
			opts = append(opts, option.WithBaseURL(c.BaseURL))
		}
		return &Anthropic{model: c.Model, effort: c.Effort, client: anthropic.NewClient(opts...)}, nil
	case "gemini", "google":
		if c.Model == "" {
			return nil, fmt.Errorf("gemini: --model is required (e.g. gemini-2.5-pro)")
		}
		key := c.APIKey
		if key == "" {
			key = os.Getenv("GEMINI_API_KEY")
		}
		if key == "" {
			return nil, fmt.Errorf("gemini: set GEMINI_API_KEY")
		}
		base := c.BaseURL
		if base == "" {
			base = "https://generativelanguage.googleapis.com"
		}
		return &Gemini{model: c.Model, key: key, base: strings.TrimRight(base, "/"), client: httpClient()}, nil
	case "openai", "codex":
		if c.Model == "" {
			return nil, fmt.Errorf("openai: --model is required (e.g. a codex model)")
		}
		key := c.APIKey
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY")
		}
		if key == "" {
			return nil, fmt.Errorf("openai: set OPENAI_API_KEY")
		}
		base := c.BaseURL
		if base == "" {
			base = "https://api.openai.com"
		}
		return &OpenAI{model: c.Model, key: key, base: strings.TrimRight(base, "/"), client: httpClient()}, nil
	case "claude-code", "plan", "max":
		return newClaudeCode(c.Model)
	case "":
		return nil, fmt.Errorf("no provider: pass --provider anthropic|gemini|openai|claude-code or set JUDGE_PROVIDER")
	default:
		return nil, fmt.Errorf("unknown provider %q (anthropic, gemini, openai, claude-code)", c.Provider)
	}
}

func httpClient() *http.Client { return &http.Client{Timeout: 120 * time.Second} }

// Anthropic uses the Messages API with structured output and a cache
// breakpoint on the system prompt: the document is identical for every
// declaration in a run, so only the first call pays for it.
type Anthropic struct {
	model  string
	effort string
	client anthropic.Client
}

func (a *Anthropic) Name() string { return "anthropic/" + a.model }

func (a *Anthropic) Ask(ctx context.Context, system, user string) (json.RawMessage, Usage, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 2048,
		System: []anthropic.TextBlockParam{{
			Text:         system,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(user)),
		},
		OutputConfig: anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{Schema: Schema},
		},
	}
	if a.effort != "" {
		params.OutputConfig.Effort = anthropic.OutputConfigEffort(a.effort)
	}
	resp, err := a.client.Messages.New(ctx, params)
	if err != nil {
		return nil, Usage{}, err
	}
	u := Usage{Input: int(resp.Usage.InputTokens), Output: int(resp.Usage.OutputTokens)}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return nil, u, fmt.Errorf("anthropic: refused (%s)", resp.StopDetails.Category)
	}
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			return json.RawMessage(t.Text), u, nil
		}
	}
	return nil, u, fmt.Errorf("anthropic: no text in response")
}

// Gemini uses generateContent with a JSON response schema. The key goes in a
// header rather than the query string so it never lands in a proxy log.
type Gemini struct {
	model, key, base string
	client           *http.Client
}

func (g *Gemini) Name() string { return "gemini/" + g.model }

func (g *Gemini) Ask(ctx context.Context, system, user string) (json.RawMessage, Usage, error) {
	body := map[string]any{
		"systemInstruction": map[string]any{"parts": []map[string]string{{"text": system}}},
		"contents": []map[string]any{{
			"role":  "user",
			"parts": []map[string]string{{"text": user}},
		}},
		"generationConfig": map[string]any{
			"responseMimeType": "application/json",
			"responseSchema":   geminiSchema(Schema),
			"temperature":      0,
		},
	}
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", g.base, g.model)
	raw, err := post(ctx, g.client, url, body, map[string]string{"x-goog-api-key": g.key})
	if err != nil {
		return nil, Usage{}, fmt.Errorf("gemini: %w", err)
	}
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, Usage{}, fmt.Errorf("gemini: bad response: %w", err)
	}
	u := Usage{Input: resp.UsageMetadata.PromptTokenCount, Output: resp.UsageMetadata.CandidatesTokenCount}
	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, u, fmt.Errorf("gemini: no candidates in response")
	}
	return json.RawMessage(resp.Candidates[0].Content.Parts[0].Text), u, nil
}

// geminiSchema strips additionalProperties, which the OpenAPI-subset schema
// Gemini accepts does not know.
func geminiSchema(s map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range s {
		if k == "additionalProperties" {
			continue
		}
		if m, ok := v.(map[string]any); ok {
			v = geminiSchema(m)
		}
		out[k] = v
	}
	return out
}

// OpenAI uses the Responses API with a strict JSON schema. Codex models are
// selected by model name; the wire shape is the same.
type OpenAI struct {
	model, key, base string
	client           *http.Client
}

func (o *OpenAI) Name() string { return "openai/" + o.model }

func (o *OpenAI) Ask(ctx context.Context, system, user string) (json.RawMessage, Usage, error) {
	body := map[string]any{
		"model":        o.model,
		"instructions": system,
		"input": []map[string]any{{
			"role":    "user",
			"content": user,
		}},
		"text": map[string]any{"format": map[string]any{
			"type":   "json_schema",
			"name":   "intent_drift",
			"schema": Schema,
			"strict": true,
		}},
	}
	raw, err := post(ctx, o.client, o.base+"/v1/responses", body, map[string]string{"Authorization": "Bearer " + o.key})
	if err != nil {
		return nil, Usage{}, fmt.Errorf("openai: %w", err)
	}
	var resp struct {
		Status string `json:"status"`
		Output []struct {
			Content []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, Usage{}, fmt.Errorf("openai: bad response: %w", err)
	}
	u := Usage{Input: resp.Usage.InputTokens, Output: resp.Usage.OutputTokens}
	for _, item := range resp.Output {
		for _, c := range item.Content {
			switch c.Type {
			case "output_text":
				return json.RawMessage(c.Text), u, nil
			case "refusal":
				return nil, u, fmt.Errorf("openai: refused: %s", c.Refusal)
			}
		}
	}
	return nil, u, fmt.Errorf("openai: no output_text in response (status %q)", resp.Status)
}

func post(ctx context.Context, client *http.Client, url string, body any, headers map[string]string) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, msg)
	}
	return raw, nil
}
