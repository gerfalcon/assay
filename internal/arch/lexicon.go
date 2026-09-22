package arch

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/sherzing/assay/internal/model"
)

// Ownership is the second thing a declaration can say, and it answers a
// different question from layering. `forbid` governs REACHABILITY: which
// packages may depend on which. `owns` governs RESPONSIBILITY: which layer a
// concept belongs to. Rating logic in a cart service can be perfectly layered
// and still be in the wrong place, and no import graph will ever see it. What
// sees it is vocabulary — what the code DECLARES, not what it imports.
//
// So the check is over declarations only. Cart calling the rating package's
// API is legitimate, and layering already governs it. Cart declaring a Rating
// type or a SubmitReview function is the smell.
//
// The vocabulary is written by a human. It is intent, and intent is the one
// thing no scan can recover from code: the code shows what a package does
// today, including the mistakes the rule exists to catch. `plumb learn` drafts
// a starting list from the code so the human edits rather than invents, and
// that draft is a starting point, never the truth.
//
// A layer with no `owns` line is a consumer: it may name any concept and is
// never checked. Presentation layers legitimately declare an InsightsScreen and
// an OrdersPage, and forcing them to opt in is what keeps the rule quiet where
// it would only be noise.

// RuleOwns is the rule id on every ownership finding.
const RuleOwns = "responsibility-drift"

// Declaration is one named thing the source declares, located in a layer.
type Declaration struct {
	Layer string
	Name  string
	File  string
	Line  int
}

// Drift is a declaration in one layer carrying a term another layer owns.
type Drift struct {
	Layer string // where it is declared
	Owner string // who owns the term
	Term  string
	Name  string
	File  string
	Line  int
}

// Fingerprint identifies a drift across edits. Excludes the line and the file:
// moving a declaration within its layer changes nothing architecturally, and
// moving it OUT of the layer is the fix, which should make the finding vanish
// rather than reappear elsewhere.
func (d Drift) Fingerprint() string {
	h := sha256.Sum256([]byte(strings.Join([]string{RuleOwns, d.Layer, d.Owner, d.Name}, "\x00")))
	return hex.EncodeToString(h[:8])
}

func (d Drift) Message() string {
	return fmt.Sprintf("%s declares %s, but %q is %s's vocabulary", d.Layer, d.Name, d.Term, d.Owner)
}

// Suggest lists the three honest resolutions. Every one of them is visible in
// review: a move, a rename, or a change to the declaration that `plumb diff`
// classifies.
func (d Drift) Suggest() string {
	return fmt.Sprintf("move %s into %s; or rename it so it does not claim %q; or transfer %q from %s to %s in the declaration, which is a loosening of %s and gets a second reviewer",
		d.Name, d.Owner, d.Term, d.Term, d.Owner, d.Layer, d.Owner)
}

// declPatterns extract declared names per language. Deliberately regex rather
// than a parser per language: the check needs names, not semantics, and a
// five-year-old service is rarely in the language its checker was written in.
//
// EVERY PATTERN MATCHES THE PUBLIC SURFACE ONLY, spelled the way each language
// spells it — an initial capital in Go, no leading underscore in Python, a
// capital or `public` elsewhere. The PRINCIPLE has to be consistent, not the
// character class: a private helper's blast radius stops at its own package,
// so a misplaced one is a far weaker signal than a misplaced type that other
// contexts consume.
//
// Getting this wrong makes measured precision LANGUAGE-DEPENDENT, which is
// fatal for a corpus whose currency is precision compared across
// organisations. Python's pattern admitted `_private_helper` while Go's
// rejected `privateHelper`, so the same misplacement counted in one language
// and not the other. TestPublicSurfaceOnlyAcrossLanguages pins it.
//
// Known limits, both erring towards silence: misplaced PRIVATE declarations are
// not seen at all, and in TypeScript a lower-case exported function is missed
// because `export` is not reliably on the same line as the name.
var declPatterns = map[string]*regexp.Regexp{
	".go":   regexp.MustCompile(`(?m)^(?:func\s+(?:\([^)]*\)\s*)?|type\s+)([A-Z][A-Za-z0-9_]*)`),
	".cs":   regexp.MustCompile(`\b(?:class|record|interface|struct|enum)\s+([A-Z][A-Za-z0-9_]*)|\bpublic\s+(?:static\s+|async\s+|virtual\s+|override\s+|sealed\s+)*[A-Za-z0-9_<>\[\],.?]+\s+([A-Z][A-Za-z0-9_]*)\s*\(`),
	".dart": regexp.MustCompile(`\b(?:class|mixin|enum|extension)\s+([A-Z][A-Za-z0-9_]*)`),
	".ts":   regexp.MustCompile(`\b(?:class|interface|type|enum|function)\s+([A-Z][A-Za-z0-9_]*)`),
	".tsx":  regexp.MustCompile(`\b(?:class|interface|type|enum|function)\s+([A-Z][A-Za-z0-9_]*)`),
	".java": regexp.MustCompile(`\b(?:class|interface|enum|record)\s+([A-Z][A-Za-z0-9_]*)`),
	".kt":   regexp.MustCompile(`\b(?:class|interface|object|fun)\s+([A-Z][A-Za-z0-9_]*)`),
	// Python names the public surface by the ABSENCE of a leading underscore,
	// and uses snake_case for functions — so requiring a capital would reject
	// nearly every function, and allowing `_` would admit every private one.
	".py": regexp.MustCompile(`(?m)^\s*(?:class|def)\s+([A-Za-z][A-Za-z0-9_]*)`),
}

// privateMarkers are keywords that put a declaration below the public surface
// in languages that spell visibility with a word rather than with the name.
//
// Needed because Go's RE2 has no lookbehind, so a pattern cannot say "class,
// but not preceded by private". Checked against the text BEFORE the matched
// name rather than the whole line, so `public class Cart { private int n; }`
// is not mistaken for a private declaration.
//
// `internal` is deliberately absent for both C# and Kotlin: it means
// assembly- or module-visible, which within one service IS the surface other
// contexts consume. An unmarked C# type is internal by default, and plenty of
// codebases never write `public` on one.
var privateMarkers = map[string][]string{
	".cs":   {"private", "protected"},
	".java": {"private", "protected"},
	".kt":   {"private", "protected"},
	".ts":   {"private"},
	".tsx":  {"private"},
}

// belowSurface reports whether the declaration matched at off is marked private
// in a language that says so with a keyword.
func belowSurface(ext string, src []byte, off int) bool {
	markers, ok := privateMarkers[ext]
	if !ok {
		return false
	}
	start := 0
	if i := strings.LastIndexByte(string(src[:off]), '\n'); i >= 0 {
		start = i + 1
	}
	prefix := string(src[start:off])
	for _, m := range markers {
		for _, f := range strings.Fields(prefix) {
			if f == m {
				return true
			}
		}
	}
	return false
}

var testFileRe = regexp.MustCompile(`(_test\.go|Tests?\.cs|_test\.dart|\.(test|spec)\.tsx?|Test\.(java|kt)|^test_.*\.py)$`)

// generatedFileRe matches files a tool wrote. Their names are chosen by a
// generator from a schema, so they carry whatever vocabulary the upstream
// contract uses and nobody can move or rename a declaration in them — every
// finding there is unactionable by construction.
//
// This is not a marginal filter. On one service 10 of 17 drifts came from
// `*_gen.go` analytics events, and a Flutter monorepo carries 310 `.g.dart`
// and `.freezed.dart` files. `generated` was already skipped as a DIRECTORY,
// which misses every convention that marks generated code by FILENAME instead.
var generatedFileRe = regexp.MustCompile(`(_gen\.go|\.pb\.go|\.pb\.gw\.go|_generated\.go|\.g\.dart|\.freezed\.dart|\.gr\.dart|\.mocks\.dart|\.designer\.cs|\.generated\.cs|_pb2\.py|\.g\.ts)$`)

// skipDirs are never source of record for any layer.
var skipDirs = map[string]bool{
	".git": true, "vendor": true, "node_modules": true, "bin": true, "obj": true,
	"build": true, "dist": true, ".dart_tool": true, "testdata": true,
	"migrations": true, "Migrations": true, "generated": true,
}

// ScanDecls collects declarations under root that fall inside a declared layer.
func ScanDecls(root string, d *Decl, includeTests bool) ([]Declaration, error) {
	return scanDecls(root, d.LayerOf, includeTests)
}

func scanDecls(root string, layerOf func(relDir string) (string, bool), includeTests bool) ([]Declaration, error) {
	var out []Declaration
	// Errors are returned, not skipped. A directory that cannot be read is a
	// layer that cannot be checked, and a silent pass there is the failure
	// this whole tool exists to avoid.
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if p != root && skipDirs[e.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		re, ok := declPatterns[filepath.Ext(p)]
		if !ok {
			return nil
		}
		if !includeTests && testFileRe.MatchString(e.Name()) {
			return nil
		}
		if generatedFileRe.MatchString(e.Name()) {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		layer, ok := layerOf(dirOf(rel))
		if !ok {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		line, last := 1, 0
		for _, m := range re.FindAllSubmatchIndex(src, -1) {
			line += strings.Count(string(src[last:m[0]]), "\n")
			last = m[0]
			name := ""
			for g := 1; g < len(m)/2; g++ {
				if m[2*g] >= 0 {
					name = string(src[m[2*g]:m[2*g+1]])
					break
				}
			}
			if name != "" && !belowSurface(filepath.Ext(p), src, m[0]) {
				out = append(out, Declaration{Layer: layer, Name: name, File: rel, Line: line})
			}
		}
		return nil
	})
	return out, err
}

func dirOf(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[:i]
	}
	return "."
}

// CheckOwns finds declarations whose name carries another layer's vocabulary.
// Only layers that declared ownership are checked; the rest are consumers.
func CheckOwns(d *Decl, decls []Declaration) []Drift {
	ownerOf := map[string]string{}
	for layer, terms := range d.Owns {
		for _, t := range terms {
			ownerOf[t] = layer
		}
	}
	var out []Drift
	for _, dc := range decls {
		if _, optedIn := d.Owns[dc.Layer]; !optedIn {
			continue
		}
		seen := map[string]bool{}
		for _, t := range Stems(dc.Name) {
			o := ownerOf[t]
			if o == "" || o == dc.Layer || seen[o] {
				continue
			}
			seen[o] = true
			out = append(out, Drift{Layer: dc.Layer, Owner: o, Term: t, Name: dc.Name, File: dc.File, Line: dc.Line})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Layer != b.Layer {
			return a.Layer < b.Layer
		}
		if a.Owner != b.Owner {
			return a.Owner < b.Owner
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	return out
}

func driftFindings(d *Decl, drifts []Drift) []model.Finding {
	out := make([]model.Finding, 0, len(drifts))
	for _, dr := range drifts {
		out = append(out, model.Finding{
			Rule:        RuleOwns,
			Severity:    model.Error,
			File:        dr.File,
			Line:        dr.Line,
			Func:        d.SHA,
			Message:     dr.Message(),
			Suggest:     dr.Suggest(),
			Fingerprint: dr.Fingerprint(),
		})
	}
	return out
}

// Stems splits an identifier into lower-case word stems, dropping the words
// that carry no concept: OrderRepository yields [order]; GetCartItemsAsync
// yields [cart]. Crude on purpose — a stemmer that knew English would still not
// know that "sheet" means a spreadsheet in one package and a bottom sheet in
// another, and that is the human's line to write.
func Stems(name string) []string {
	var out []string
	for _, w := range splitWords(name) {
		w = strings.ToLower(w)
		if len(w) < 3 || stopwords[w] || isDigits(w) {
			continue
		}
		w = singular(w)
		if len(w) >= 3 && !stopwords[w] {
			out = append(out, w)
		}
	}
	return out
}

func splitWords(s string) []string {
	var words []string
	var cur []rune
	rs := []rune(s)
	flush := func() {
		if len(cur) > 0 {
			words = append(words, string(cur))
			cur = cur[:0]
		}
	}
	for i, r := range rs {
		if r == '_' || r == '-' || r == '$' {
			flush()
			continue
		}
		if i > 0 {
			prev := rs[i-1]
			switch {
			case unicode.IsUpper(r) && unicode.IsLower(prev):
				flush()
			case unicode.IsUpper(r) && unicode.IsUpper(prev) && i+1 < len(rs) && unicode.IsLower(rs[i+1]):
				flush() // HTTPServer → HTTP Server
			case unicode.IsDigit(r) != unicode.IsDigit(prev):
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return words
}

func singular(w string) string {
	switch {
	case strings.HasSuffix(w, "ies") && len(w) > 4:
		return w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "ss"), strings.HasSuffix(w, "us"), strings.HasSuffix(w, "is"):
		return w
	case strings.HasSuffix(w, "s") && len(w) > 3:
		return w[:len(w)-1]
	}
	return w
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// stopwords are the words every layer uses. Anything here can never be owned,
// so it can never drift. The list is generic across codebases; a repository's
// own noise words are handled by leaving them out of its owns lines.
var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		get set new create update delete remove add list find fetch load save build make run
		handler handlers service services controller controllers request response dto dtos
		model models entity entities repository repositories repo impl base abstract util utils
		helper helpers common core config configuration options option client clients api
		internal id ids type types data item items value values result results error errors
		exception async task string int bool count total name manager provider factory
		command query event events message messages mapper mappers extension extensions
		validator validators test tests mock mocks with from to by of for and or the is has
		can default main app application page pages screen screens widget widgets view views
		state states cubit bloc notifier interface func function method params args context
		info input output row rows record records field fields key keys map maps
	`) {
		m[w] = true
	}
	return m
}()

// LearnOptions tunes the draft.
type LearnOptions struct {
	// Depth is the directory depth that defines a context when no declaration
	// exists yet: 2 makes internal/cart and src/Rating contexts.
	Depth int
	// Min is the fewest declarations in a context that must carry a term.
	Min int
	// Share is the fraction of a term's uses that must sit in one context.
	Share float64
	// Top caps terms per context.
	Top          int
	IncludeTests bool
}

// DraftLine is a proposed `owns` statement with the evidence behind it.
type DraftLine struct {
	Layer string
	Terms []TermCount
	Decls int
}

// TermCount is one owned term and how many declarations carry it.
type TermCount struct {
	Term  string
	Count int
}

// Learn drafts owns lines from what the code declares. With a declaration, the
// contexts are its layers; without one, directories at Depth stand in, so the
// draft can be produced before anyone has written ARCHITECTURE.md.
func Learn(root string, d *Decl, o LearnOptions) ([]DraftLine, error) {
	if o.Depth <= 0 {
		o.Depth = 2
	}
	if o.Min <= 0 {
		o.Min = 3
	}
	if o.Share <= 0 {
		o.Share = 0.75
	}
	if o.Top <= 0 {
		o.Top = 8
	}
	layerOf := func(relDir string) (string, bool) {
		if relDir == "." {
			return "", false
		}
		parts := strings.Split(relDir, "/")
		if len(parts) > o.Depth {
			parts = parts[:o.Depth]
		}
		return strings.Join(parts, "/"), true
	}
	if d != nil && len(d.Layers) > 0 {
		layerOf = d.LayerOf
	}
	decls, err := scanDecls(root, layerOf, o.IncludeTests)
	if err != nil {
		return nil, err
	}
	if len(decls) == 0 {
		return nil, fmt.Errorf("no declarations found under %s", root)
	}

	perCtx := map[string]map[string]int{}
	total := map[string]int{}
	nDecls := map[string]int{}
	for _, dc := range decls {
		nDecls[dc.Layer]++
		if perCtx[dc.Layer] == nil {
			perCtx[dc.Layer] = map[string]int{}
		}
		seen := map[string]bool{}
		for _, t := range Stems(dc.Name) {
			if seen[t] {
				continue
			}
			seen[t] = true
			perCtx[dc.Layer][t]++
			total[t]++
		}
	}

	var out []DraftLine
	for ctx, counts := range perCtx {
		var terms []TermCount
		for t, n := range counts {
			if n >= o.Min && float64(n)/float64(total[t]) >= o.Share {
				terms = append(terms, TermCount{t, n})
			}
		}
		if len(terms) == 0 {
			continue
		}
		sort.Slice(terms, func(i, j int) bool {
			if terms[i].Count != terms[j].Count {
				return terms[i].Count > terms[j].Count
			}
			return terms[i].Term < terms[j].Term
		})
		if len(terms) > o.Top {
			terms = terms[:o.Top]
		}
		out = append(out, DraftLine{Layer: ctx, Terms: terms, Decls: nDecls[ctx]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Decls != out[j].Decls {
			return out[i].Decls > out[j].Decls
		}
		return out[i].Layer < out[j].Layer
	})
	return out, nil
}

// utilityNames are the last path segments of packages that hold mechanism
// rather than domain.
var utilityNames = map[string]bool{
	"utils": true, "util": true, "helpers": true, "helper": true, "common": true,
	"shared": true, "lib": true, "libs": true, "misc": true, "tools": true,
	"scripts": true, "base": true,
	// compound forms, since the check splits on "/" and not on "_"
	"web_utils": true, "webutils": true, "test_utils": true, "testutils": true,
}

// NotAContext reports whether a drafted layer looks like a utility package
// rather than a domain context.
//
// A utility package has no vocabulary of its own — it has the generic words
// mechanism is written in. Letting one own `content format` would flag every
// ContentType in the codebase, so the fix is to strike the LINE, not to prune
// its terms.
//
// Every path segment is checked, not just the last: depth-based discovery
// produced `tools/playground` and `tools/ci_detect`, where only the first
// segment says "not a domain".
//
// DELIBERATELY EXCLUDES `core`, `infrastructure`, `infra` and `client`. Those
// are real layer names in clean and hexagonal architectures — a C# service
// here has a genuine `src/Infrastructure` layer owning `polly` and `circuit`,
// and telling the reader to strike it would be wrong. The list keeps only
// names that are junk drawers wherever they appear. `internal` came out for
// the same reason once every segment was checked: it is Go's standard
// directory for non-exported packages, so every `internal/billing` in every
// Go service would have been marked. And because judging this
// correctly needs context the tool does not have, the marker now asks the
// reader to check rather than telling them to delete.
func NotAContext(layer string) bool {
	for _, seg := range strings.Split(layer, "/") {
		if utilityNames[strings.ToLower(seg)] {
			return true
		}
	}
	return false
}
