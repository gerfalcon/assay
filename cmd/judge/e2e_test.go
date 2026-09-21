package main_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sherzing/assay/cmd/internal/cmdtest"
)

// Drives the real binary against a fake OpenAI-shaped server. Proves the
// wiring: declarations in, one request per declaration, findings out, cache.
func TestScanEmitsVerifiedFindings(t *testing.T) {
	bin := cmdtest.Build(t, "judge")
	t.Setenv("OPENAI_API_KEY", "test-key")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Input []struct{ Content string } `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		ans := `{"belongs":true,"layer":"cart","reason":"","citation":""}`
		if strings.Contains(body.Input[0].Content, "name:  AverageStars") {
			ans = `{"belongs":false,"layer":"rating","reason":"averages ratings","citation":"Ratings belong to the rating layer."}`
		}
		q, _ := json.Marshal(ans)
		w.Write([]byte(`{"status":"completed","output":[{"content":[{"type":"output_text","text":` + string(q) + `}]}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	dir := cmdtest.Tree(t, map[string]string{
		"ARCHITECTURE.md":           "# Architecture\n\nThe cart holds intent to buy.\nRatings belong to the rating layer.\n\n```arch\nlayer cart internal/cart\nlayer rating internal/rating\nowns rating rating\n```\n",
		"internal/cart/cart.go":     "package cart\n\nfunc AddLine() {}\n\nfunc AverageStars() int { return 0 }\n",
		"internal/rating/rating.go": "package rating\n\ntype Rating struct{}\n",
	})
	args := []string{"scan", ".", "--provider", "codex", "--model", "m", "--base-url", srv.URL, "--emit", "findings"}
	r := bin.Run(t, dir, args...)
	r.MustPass(t).MustSay(t, `"rule":"intent-drift@1"`, "AverageStars", "belongs in rating", "3 judged")
	if strings.Contains(r.Stdout, "AddLine") {
		t.Error("a declaration that belongs must not become a finding")
	}
	if calls != 3 {
		t.Errorf("expected one request per declaration, got %d", calls)
	}
	// Second run answers from the cache.
	bin.Run(t, dir, args...).MustPass(t).MustSay(t, "3 from cache")
	if calls != 3 {
		t.Errorf("cache miss: %d calls", calls)
	}
	bin.Run(t, dir, "scan", ".", "--provider", "codex", "--model", "m", "--base-url", srv.URL, "--layer", "rating").MustPass(t).MustSay(t, "1 judged")
}

func TestScanNeedsAProvider(t *testing.T) {
	bin := cmdtest.Build(t, "judge")
	dir := cmdtest.Tree(t, map[string]string{"ARCHITECTURE.md": "```arch\nlayer a a\nowns a x\n```\n"})
	bin.Run(t, dir, "scan", ".").MustFail(t).MustSay(t, "no provider")
}
