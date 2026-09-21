// Package judge asks a language model whether a declaration belongs where it
// is, according to what ARCHITECTURE.md says each part is for.
//
// Plumb's two rules are deterministic: forbid over the import graph, owns over
// declared names. Both are blind to a function named ApplyDiscount in the cart
// layer that actually averages ratings. A judge that reads the body and the
// document is not. It is also probabilistic, so it is built as one more linter
// under the same discipline as every other rule, not as an oracle:
//
//   - It emits ordinary findings. ratchet, strata and docket compose over them
//     without knowing a model was involved.
//   - Its rule id carries the prompt version. Precision is a property of the
//     prompt, and a reworded prompt is a new rule with its own record.
//   - Every finding must cite a sentence that exists verbatim in the document.
//     A citation that is not there is a hallucination, and the finding is
//     dropped before anyone sees it.
//   - The fingerprint is the symbol, not the explanation. The model's wording
//     varies from run to run; the thing it is talking about does not.
//   - Findings are warnings. Nothing gates on them until a team has measured
//     the precision and chosen to.
package judge

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sherzing/assay/internal/arch"
	"github.com/sherzing/assay/internal/model"
)

//go:embed prompts/intent-drift.v1.md
var promptV1 string

// Rule is the rule id. The suffix is the prompt version: bump it when the
// prompt changes, so precision data never mixes two prompts.
const Rule = "intent-drift@1"

// Schema is the JSON shape every provider must return.
var Schema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"belongs":  map[string]any{"type": "boolean", "description": "true if the declaration belongs in its layer"},
		"layer":    map[string]any{"type": "string", "description": "the layer it belongs in; the current layer when belongs is true"},
		"reason":   map[string]any{"type": "string", "description": "one sentence naming the concept that is out of place"},
		"citation": map[string]any{"type": "string", "description": "one sentence copied verbatim from the architecture document"},
	},
	"required":             []string{"belongs", "layer", "reason", "citation"},
	"additionalProperties": false,
}

// Answer is a provider's parsed reply.
type Answer struct {
	Belongs  bool   `json:"belongs"`
	Layer    string `json:"layer"`
	Reason   string `json:"reason"`
	Citation string `json:"citation"`
}

// Usage is what a call cost, for the summary line.
type Usage struct {
	Input, Output int
}

// Provider is one model behind one wire protocol.
type Provider interface {
	// Name identifies provider and model, e.g. anthropic/claude-opus-5.
	Name() string
	// Ask sends the system prompt and the request and returns the raw JSON
	// object the model produced, which the caller validates.
	Ask(ctx context.Context, system, user string) (json.RawMessage, Usage, error)
}

// Case is one declaration to judge, with the code around it.
type Case struct {
	Decl    arch.Declaration
	Excerpt string
}

// Finding is a judged drift with everything needed to act on it.
type Finding struct {
	Case
	Answer
}

// Fingerprint hashes the symbol, not the explanation.
func (f Finding) Fingerprint() string {
	h := sha256.Sum256([]byte(strings.Join([]string{Rule, f.Decl.Layer, f.Decl.Name}, "\x00")))
	return hex.EncodeToString(h[:8])
}

// Options controls a run.
type Options struct {
	Doc      string // the whole ARCHITECTURE.md
	Decl     *arch.Decl
	CacheDir string // "" disables the cache
	Jobs     int
}

// Result is what a run produced.
type Result struct {
	Findings []Finding
	Judged   int
	Belongs  int
	Dropped  []Dropped // answers that failed verification
	Cached   int
	Usage    Usage
}

// Dropped records why an answer was not turned into a finding, because "the
// model said something and we threw it away" should be visible, not silent.
type Dropped struct {
	Case   Case
	Answer Answer
	Why    string
}

// System builds the prompt prefix: instructions, then the document. Identical
// for every case in a run and across runs until the document changes, which is
// what makes it worth caching at the provider.
func System(doc string) string {
	return promptV1 + doc
}

// Request renders one case.
func Request(d *arch.Decl, c Case) string {
	var b strings.Builder
	b.WriteString("Layers, with the directories each covers:\n")
	for _, name := range d.Order {
		fmt.Fprintf(&b, "  %-12s %s\n", name, strings.Join(d.Layers[name], " "))
	}
	if terms, ok := d.Owns[c.Decl.Layer]; ok {
		fmt.Fprintf(&b, "\nDeclared vocabulary of %s: %s\n", c.Decl.Layer, strings.Join(terms, " "))
	}
	fmt.Fprintf(&b, "\nDeclaration under review:\n  layer: %s\n  name:  %s\n  file:  %s:%d\n\n```\n%s\n```\n",
		c.Decl.Layer, c.Decl.Name, c.Decl.File, c.Decl.Line, c.Excerpt)
	return b.String()
}

// Run judges every case, in parallel, with the cache in front of the provider.
func Run(ctx context.Context, p Provider, o Options, cases []Case) (*Result, error) {
	if o.Jobs <= 0 {
		o.Jobs = 4
	}
	system := System(o.Doc)
	docNorm := normalise(o.Doc)
	layers := map[string]bool{}
	for _, l := range o.Decl.Order {
		layers[l] = true
	}

	type slot struct {
		ans    Answer
		cached bool
		usage  Usage
		err    error
	}
	out := make([]slot, len(cases))
	sem := make(chan struct{}, o.Jobs)
	var wg sync.WaitGroup
	for i, c := range cases {
		wg.Add(1)
		go func(i int, c Case) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			raw, cached, usage, err := ask(ctx, p, o, system, Request(o.Decl, c), c)
			if err != nil {
				out[i] = slot{err: err}
				return
			}
			var ans Answer
			if err := json.Unmarshal(raw, &ans); err != nil {
				out[i] = slot{err: fmt.Errorf("%s: model returned invalid JSON: %w", c.Decl.Name, err)}
				return
			}
			out[i] = slot{ans: ans, cached: cached, usage: usage}
		}(i, c)
	}
	wg.Wait()

	res := &Result{}
	for i, s := range out {
		if s.err != nil {
			return res, s.err
		}
		c := cases[i]
		res.Judged++
		res.Usage.Input += s.usage.Input
		res.Usage.Output += s.usage.Output
		if s.cached {
			res.Cached++
		}
		if s.ans.Belongs {
			res.Belongs++
			continue
		}
		// Verification. Each of these is a way for a plausible answer to be
		// wrong in a way a reader could not tell from the text.
		switch {
		case !layers[s.ans.Layer]:
			res.Dropped = append(res.Dropped, Dropped{c, s.ans, fmt.Sprintf("names a layer that is not declared: %q", s.ans.Layer)})
		case s.ans.Layer == c.Decl.Layer:
			res.Dropped = append(res.Dropped, Dropped{c, s.ans, "says it does not belong but names the same layer"})
		case strings.TrimSpace(s.ans.Citation) == "":
			res.Dropped = append(res.Dropped, Dropped{c, s.ans, "no citation"})
		case !strings.Contains(docNorm, normalise(s.ans.Citation)):
			res.Dropped = append(res.Dropped, Dropped{c, s.ans, "citation is not in the document"})
		default:
			res.Findings = append(res.Findings, Finding{c, s.ans})
		}
	}
	sort.Slice(res.Findings, func(i, j int) bool {
		a, b := res.Findings[i].Decl, res.Findings[j].Decl
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	return res, nil
}

func ask(ctx context.Context, p Provider, o Options, system, user string, c Case) (json.RawMessage, bool, Usage, error) {
	key := ""
	if o.CacheDir != "" {
		h := sha256.Sum256([]byte(strings.Join([]string{Rule, p.Name(), system, user}, "\x00")))
		key = filepath.Join(o.CacheDir, hex.EncodeToString(h[:16])+".json")
		if raw, err := os.ReadFile(key); err == nil {
			return raw, true, Usage{}, nil
		}
	}
	raw, usage, err := p.Ask(ctx, system, user)
	if err != nil {
		return nil, false, usage, fmt.Errorf("%s: %w", c.Decl.Name, err)
	}
	if key != "" {
		if err := os.MkdirAll(o.CacheDir, 0o755); err != nil {
			return nil, false, usage, err
		}
		if err := os.WriteFile(key, raw, 0o644); err != nil {
			return nil, false, usage, err
		}
	}
	return raw, false, usage, nil
}

// normalise collapses whitespace so a citation survives line wrapping in the
// markdown. It does not lower-case: a quote is a quote.
func normalise(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ModelFindings converts to the shared finding type.
func (r *Result) ModelFindings(p Provider, declSHA string) []model.Finding {
	out := make([]model.Finding, 0, len(r.Findings))
	for _, f := range r.Findings {
		out = append(out, model.Finding{
			Rule:        Rule,
			Severity:    model.Warn,
			File:        f.Decl.File,
			Line:        f.Decl.Line,
			Func:        declSHA,
			Message:     fmt.Sprintf("%s declares %s: %s (belongs in %s)", f.Decl.Layer, f.Decl.Name, strings.TrimSuffix(f.Reason, "."), f.Layer),
			Suggest:     fmt.Sprintf("move %s into %s — the document says: %q", f.Decl.Name, f.Layer, normalise(f.Citation)),
			Fingerprint: f.Fingerprint(),
		})
	}
	return out
}

// Excerpt returns the source from the declaration's line for up to maxLines,
// stopping at the next top-level declaration so the model sees one thing.
func Excerpt(root string, d arch.Declaration, maxLines int) (string, error) {
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(d.File)))
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(src), "\n")
	if d.Line < 1 || d.Line > len(lines) {
		return "", fmt.Errorf("%s:%d: line out of range", d.File, d.Line)
	}
	start := d.Line - 1
	end := start + 1
	for end < len(lines) && end-start < maxLines {
		l := lines[end]
		// A new unindented declaration means we have left this one.
		if len(l) > 0 && l[0] != ' ' && l[0] != '\t' && l[0] != '}' && l[0] != ')' && !strings.HasPrefix(l, "//") && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "@") {
			if end > start+1 {
				break
			}
		}
		end++
	}
	return strings.Join(lines[start:end], "\n"), nil
}
