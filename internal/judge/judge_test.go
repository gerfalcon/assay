package judge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sherzing/assay/internal/arch"
)

const doc = "# Architecture\n\nThe cart layer holds what a customer intends to buy.\nRatings and reviews belong to the rating layer, which owns the score.\n"

func decl(t *testing.T) *arch.Decl {
	t.Helper()
	d, err := arch.Parse("layer cart internal/cart\nlayer rating internal/rating\nowns rating rating review\n", 1)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// scripted answers by declaration name.
type scripted map[string]Answer

func (s scripted) Name() string { return "test/model" }
func (s scripted) Ask(_ context.Context, _, user string) (json.RawMessage, Usage, error) {
	for name, a := range s {
		if strings.Contains(user, "name:  "+name+"\n") {
			b, _ := json.Marshal(a)
			return b, Usage{Input: 10, Output: 5}, nil
		}
	}
	return json.RawMessage(`{"belongs":true,"layer":"cart","reason":"","citation":""}`), Usage{}, nil
}

func cases() []Case {
	return []Case{
		{Decl: arch.Declaration{Layer: "cart", Name: "AverageStars", File: "internal/cart/x.go", Line: 3}, Excerpt: "func AverageStars() {}"},
		{Decl: arch.Declaration{Layer: "cart", Name: "AddLine", File: "internal/cart/x.go", Line: 9}, Excerpt: "func AddLine() {}"},
		{Decl: arch.Declaration{Layer: "cart", Name: "Bogus", File: "internal/cart/y.go", Line: 1}, Excerpt: "func Bogus() {}"},
		{Decl: arch.Declaration{Layer: "cart", Name: "NoLayer", File: "internal/cart/y.go", Line: 5}, Excerpt: "func NoLayer() {}"},
	}
}

// THE LOAD-BEARING TEST. A model can say anything; only an answer that names a
// declared layer and quotes the document survives.
func TestVerificationDropsWhatItCannotCheck(t *testing.T) {
	p := scripted{
		"AverageStars": {Belongs: false, Layer: "rating", Reason: "averages ratings", Citation: "Ratings and reviews belong to the rating layer, which owns the score."},
		"Bogus":        {Belongs: false, Layer: "rating", Reason: "made up", Citation: "The rating layer computes averages nightly."},
		"NoLayer":      {Belongs: false, Layer: "reviews", Reason: "x", Citation: "The cart layer holds what a customer intends to buy."},
	}
	res, err := Run(context.Background(), p, Options{Doc: doc, Decl: decl(t)}, cases())
	if err != nil {
		t.Fatal(err)
	}
	if res.Judged != 4 || res.Belongs != 1 {
		t.Errorf("judged=%d belongs=%d", res.Judged, res.Belongs)
	}
	if len(res.Findings) != 1 || res.Findings[0].Decl.Name != "AverageStars" || res.Findings[0].Layer != "rating" {
		t.Fatalf("findings = %+v", res.Findings)
	}
	if len(res.Dropped) != 2 {
		t.Fatalf("dropped = %+v", res.Dropped)
	}
	whys := res.Dropped[0].Why + " | " + res.Dropped[1].Why
	if !strings.Contains(whys, "citation is not in the document") || !strings.Contains(whys, "not declared") {
		t.Errorf("drop reasons = %s", whys)
	}
	// Citation survives markdown line wrapping.
	p["AverageStars"] = Answer{Belongs: false, Layer: "rating", Reason: "r", Citation: "Ratings and reviews belong\nto the rating layer, which owns the score."}
	res, _ = Run(context.Background(), p, Options{Doc: doc, Decl: decl(t)}, cases()[:1])
	if len(res.Findings) != 1 {
		t.Error("whitespace-only difference in a citation must not drop it")
	}
	f := res.Findings[0]
	moved := f
	moved.Decl.File, moved.Decl.Line = "internal/cart/z.go", 40
	if f.Fingerprint() != moved.Fingerprint() {
		t.Error("fingerprint must not depend on file or line")
	}
	mf := res.ModelFindings(p, "abc")[0]
	if mf.Rule != Rule || mf.Severity != "warn" || !strings.Contains(mf.Suggest, "rating layer") {
		t.Errorf("model finding = %+v", mf)
	}
}

func TestCacheAnswersSecondRun(t *testing.T) {
	p := scripted{"AverageStars": {Belongs: false, Layer: "rating", Reason: "r", Citation: "Ratings and reviews belong to the rating layer, which owns the score."}}
	dir := t.TempDir()
	o := Options{Doc: doc, Decl: decl(t), CacheDir: dir}
	first, err := Run(context.Background(), p, o, cases()[:1])
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(context.Background(), p, o, cases()[:1])
	if err != nil {
		t.Fatal(err)
	}
	if first.Cached != 0 || second.Cached != 1 || len(second.Findings) != 1 || second.Usage.Input != 0 {
		t.Errorf("first=%+v second=%+v", first, second)
	}
}

func TestExcerptStopsAtNextDeclaration(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "p"), 0o755)
	os.WriteFile(filepath.Join(root, "p", "a.go"), []byte("package p\n\nfunc A() {\n\treturn\n}\n\n// B does b.\nfunc B() {}\n"), 0o644)
	got, err := Excerpt(root, arch.Declaration{File: "p/a.go", Line: 3}, 60)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "func B") || !strings.Contains(got, "return") {
		t.Errorf("excerpt = %q", got)
	}
}

// Each adapter against a fake server: the request must carry the shape the
// real API documents, and the reply must be read from the documented path.
func TestAdaptersSpeakTheirProtocols(t *testing.T) {
	answer := `{"belongs":false,"layer":"rating","reason":"r","citation":"c"}`
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization") + r.Header.Get("x-goog-api-key") + r.Header.Get("x-api-key")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, ":generateContent"):
			w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":` + strconvQuote(answer) + `}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3}}`))
		case r.URL.Path == "/v1/responses":
			w.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":` + strconvQuote(answer) + `}]}],"usage":{"input_tokens":7,"output_tokens":3}}`))
		case r.URL.Path == "/v1/messages":
			w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"x","content":[{"type":"text","text":` + strconvQuote(answer) + `}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	for _, c := range []Config{
		{Provider: "gemini", Model: "g", APIKey: "k", BaseURL: srv.URL},
		{Provider: "codex", Model: "o", APIKey: "k", BaseURL: srv.URL},
		{Provider: "anthropic", Model: "a", APIKey: "k", BaseURL: srv.URL},
	} {
		p, err := New(c)
		if err != nil {
			t.Fatal(c.Provider, err)
		}
		raw, u, err := p.Ask(context.Background(), "SYS", "USER")
		if err != nil {
			t.Fatalf("%s: %v", c.Provider, err)
		}
		if string(raw) != answer || u.Input != 7 || u.Output != 3 {
			t.Errorf("%s: raw=%s usage=%+v", c.Provider, raw, u)
		}
		if gotAuth == "" || !strings.Contains(gotAuth, "k") {
			t.Errorf("%s: no credential sent", c.Provider)
		}
		body, _ := json.Marshal(gotBody)
		switch c.Provider {
		case "gemini":
			if !strings.Contains(gotPath, "/v1beta/models/g:generateContent") || !strings.Contains(string(body), `"responseMimeType":"application/json"`) || strings.Contains(string(body), "additionalProperties") {
				t.Errorf("gemini request: %s %s", gotPath, body)
			}
		case "codex":
			if !strings.Contains(string(body), `"type":"json_schema"`) || !strings.Contains(string(body), `"strict":true`) || !strings.Contains(string(body), `"instructions":"SYS"`) {
				t.Errorf("openai request: %s", body)
			}
		case "anthropic":
			if !strings.Contains(string(body), `"output_config"`) || !strings.Contains(string(body), `"cache_control"`) || !strings.Contains(string(body), `"model":"a"`) {
				t.Errorf("anthropic request: %s", body)
			}
		}
	}
	if _, err := New(Config{Provider: "gemini", Model: "g"}); err == nil && os.Getenv("GEMINI_API_KEY") == "" {
		t.Error("gemini without a key must fail")
	}
	if _, err := New(Config{Provider: "nope"}); err == nil {
		t.Error("unknown provider must fail")
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The Claude Code provider is a process. A fake binary stands in for it and
// answers a batch, so the mapping from case numbers back to cases is tested
// without a subscription.
func TestClaudeCodeBatchesAndMapsAnswers(t *testing.T) {
	dir := t.TempDir()
	py := filepath.Join(dir, "fake.py")
	os.WriteFile(py, []byte(`import sys, json, re
prompt = sys.stdin.read()
parts = re.split(r'===== CASE (\d+) =====', prompt)[1:]
answers = []
for i in range(0, len(parts), 2):
    n, body = int(parts[i]), parts[i+1]
    if 'AverageStars' in body:
        answers.append({"index": n, "belongs": False, "layer": "rating", "reason": "r", "citation": "Ratings and reviews belong to the rating layer, which owns the score."})
    else:
        answers.append({"index": n, "belongs": True, "layer": "", "reason": "", "citation": ""})
print(json.dumps({"type": "result", "is_error": False, "result": "", "structured_output": {"answers": answers}, "usage": {"input_tokens": 100, "output_tokens": 50}}))
`), 0o644)
	fake := filepath.Join(dir, "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec python3 "+py+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JUDGE_CLAUDE_BIN", fake)
	p, err := New(Config{Provider: "max"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(Batcher); !ok {
		t.Fatal("claude-code must batch")
	}
	res, err := Run(context.Background(), p, Options{Doc: doc, Decl: decl(t), Jobs: 1}, cases())
	if err != nil {
		t.Fatal(err)
	}
	if res.Judged != 4 || len(res.Findings) != 1 || res.Findings[0].Decl.Name != "AverageStars" {
		t.Errorf("result = judged %d findings %+v", res.Judged, res.Findings)
	}
	if res.Usage.Input != 100 {
		t.Errorf("one batched call should cost once, got %+v", res.Usage)
	}
}

func TestVerifyExternalAnswers(t *testing.T) {
	cs := cases()
	answered := []Answered{
		{File: cs[0].Decl.File, Line: cs[0].Decl.Line, Name: cs[0].Decl.Name, In: "cart",
			Answer: Answer{Belongs: false, Layer: "rating", Reason: "r", Citation: "Ratings and reviews belong to the rating layer, which owns the score."}},
		{File: cs[1].Decl.File, Line: cs[1].Decl.Line, Name: cs[1].Decl.Name, In: "cart", Answer: Answer{Belongs: true}},
		{File: "nowhere.go", Line: 1, Name: "Ghost", In: "cart", Answer: Answer{Belongs: false, Layer: "rating", Citation: "x"}},
	}
	res, unjudged := Verify(Options{Doc: doc, Decl: decl(t)}, cs, answered)
	if unjudged != 2 || res.Judged != 2 || len(res.Findings) != 1 || res.Findings[0].Decl.Name != "AverageStars" {
		t.Errorf("unjudged=%d judged=%d findings=%+v", unjudged, res.Judged, res.Findings)
	}
}
