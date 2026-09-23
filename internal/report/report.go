// Package report renders a scan in the formats each audience needs: JSON for
// the trend series, SARIF so findings land as GitHub PR annotations in front of
// an engineer, CSV for a spreadsheet, and a table for a terminal.
package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sherzing/assay/internal/model"
)

// JSON writes the full report. This is the stable contract other tools read.
func JSON(w io.Writer, rep *model.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// CSV writes one row per function, for the trend spreadsheet.
//
// Uses encoding/csv rather than Fprintf: a path containing a comma —
// `pkg/a,b/handler.go` — would otherwise shift every subsequent column and
// corrupt the row silently, which is the worst way for a spreadsheet to be
// wrong. Values without commas or quotes are emitted identically to before.
func CSV(w io.Writer, rep *model.Report) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"file", "line", "func", "recv", "exported",
		"cyclomatic", "cognitive", "maxNesting", "statements", "params", "results"}); err != nil {
		return err
	}
	for _, f := range rep.Funcs {
		if err := cw.Write([]string{
			f.File, strconv.Itoa(f.Line), f.Name, f.Recv, strconv.FormatBool(f.Exported),
			strconv.Itoa(f.Cyclomatic), strconv.Itoa(f.Cognitive), strconv.Itoa(f.MaxNesting),
			strconv.Itoa(f.Statements), strconv.Itoa(f.Params), strconv.Itoa(f.Results),
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// SARIF renders findings in the format GitHub code scanning ingests. This is
// what turns "a number in a log" into a comment on the line that caused it.
func SARIF(w io.Writer, rep *model.Report, rules map[string]string) error {
	type sarifRule struct {
		ID               string            `json:"id"`
		ShortDescription map[string]string `json:"shortDescription"`
	}
	type sarifResult struct {
		RuleID    string            `json:"ruleId"`
		Level     string            `json:"level"`
		Message   map[string]string `json:"message"`
		Locations []any             `json:"locations"`
	}

	seen := map[string]bool{}
	var ruleList []sarifRule
	var results []sarifResult

	for _, f := range rep.Findings {
		if !seen[f.Rule] {
			seen[f.Rule] = true
			ruleList = append(ruleList, sarifRule{
				ID:               f.Rule,
				ShortDescription: map[string]string{"text": rules[f.Rule]},
			})
		}
		msg := f.Message
		if f.Suggest != "" {
			msg += " — " + f.Suggest
		}
		results = append(results, sarifResult{
			RuleID:  f.Rule,
			Level:   sarifLevel(f.Severity),
			Message: map[string]string{"text": msg},
			Locations: []any{map[string]any{
				"physicalLocation": map[string]any{
					"artifactLocation": map[string]any{"uri": f.File},
					"region":           map[string]any{"startLine": f.Line, "startColumn": f.Col},
				},
			}},
		})
	}
	sort.Slice(ruleList, func(i, j int) bool { return ruleList[i].ID < ruleList[j].ID })

	doc := map[string]any{
		"$schema": "https://json.schemastore.org/sarif-2.1.0.json",
		"version": "2.1.0",
		"runs": []any{map[string]any{
			"tool": map[string]any{"driver": map[string]any{
				"name":           "ratchet",
				"informationUri": "https://github.com/sherzing/assay",
				"rules":          ruleList,
			}},
			"results": results,
		}},
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

func sarifLevel(s model.Severity) string {
	switch s {
	case model.Error:
		return "error"
	case model.Warn:
		return "warning"
	default:
		return "note"
	}
}

// Text renders a human summary. `top` caps the hotspot list.
func Text(w io.Writer, rep *model.Report, top int) error {
	s := rep.Summary
	fmt.Fprintf(w, "scanned %d files, %d functions, %d statements\n\n", s.Files, s.Funcs, s.Statements)
	fmt.Fprintf(w, "%-12s %6s %6s %6s %8s\n", "metric", "p50", "p90", "max", "mean")
	row := func(n string, d model.Dist) {
		fmt.Fprintf(w, "%-12s %6d %6d %6d %8.2f\n", n, d.P50, d.P90, d.Max, d.Mean)
	}
	row("cyclomatic", s.Cyclomatic)
	row("cognitive", s.Cognitive)
	row("nesting", s.MaxNesting)

	if s.FindingsTotal > 0 {
		fmt.Fprintf(w, "\n%d findings:\n", s.FindingsTotal)
		keys := make([]string, 0, len(s.FindingsByRule))
		for k := range s.FindingsByRule {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "  %-28s %d\n", k, s.FindingsByRule[k])
		}
	} else {
		fmt.Fprintln(w, "\nno findings")
	}

	if top > 0 && len(rep.Funcs) > 0 {
		// Rank by cognitive complexity: it tracks "hard for a person to read",
		// which is what you actually want to send someone to fix.
		fns := append([]model.FuncMetrics(nil), rep.Funcs...)
		sort.Slice(fns, func(i, j int) bool { return fns[i].Cognitive > fns[j].Cognitive })
		if len(fns) > top {
			fns = fns[:top]
		}
		fmt.Fprintf(w, "\ntop %d by cognitive complexity:\n", len(fns))
		for _, f := range fns {
			name := f.Name
			if f.Recv != "" {
				name = f.Recv + "." + f.Name
			}
			fmt.Fprintf(w, "  %-34s cog=%-4d cyc=%-4d nest=%-3d %s:%d\n",
				truncate(name, 34), f.Cognitive, f.Cyclomatic, f.MaxNesting, f.File, f.Line)
		}
	}
	return nil
}

// truncate cuts to n display positions, counting runes rather than bytes.
// Slicing bytes splits multi-byte sequences — a Cyrillic or CJK identifier came
// out as mojibake at 24 of 37 widths — and the column alignment was wrong too,
// since a 3-byte rune occupies one column.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// Findings prints findings grouped by file, for local use.
func Findings(w io.Writer, fs []model.Finding) {
	var cur string
	for _, f := range fs {
		if f.File != cur {
			cur = f.File
			fmt.Fprintf(w, "\n%s\n", cur)
		}
		if f.Line == 0 {
			// An imported finding about a whole file, such as an unused one,
			// has no position; "0:0" would send someone to look for a line.
			fmt.Fprintf(w, "  %-26s %s\n", f.Rule, f.Message)
			continue
		}
		fmt.Fprintf(w, "  %d:%d  %-26s %s\n", f.Line, f.Col, f.Rule, f.Message)
		if f.Suggest != "" {
			fmt.Fprintf(w, "  %s   → %s\n", strings.Repeat(" ", len(fmt.Sprint(f.Line))+len(fmt.Sprint(f.Col))), f.Suggest)
		}
	}
}
