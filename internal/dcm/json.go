// The DCM JSON shapes this importer reads, and the readers for them.
package dcm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// Sniff reports whether data is a DCM report: its root carries formatVersion, SARIF's does not.
func Sniff(data []byte) bool {
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	return bytes.Contains(head, []byte(`"formatVersion"`))
}

type fileResult struct {
	Path   string          `json:"path"`
	Issues json.RawMessage `json:"issues"`
}

type issue struct {
	ID              string          `json:"id"`
	Message         string          `json:"message"`
	Severity        string          `json:"severity"`
	Location        location        `json:"location"`
	DeclarationName string          `json:"declarationName"`
	DeclarationType string          `json:"declarationType"`
	Level           string          `json:"level"`
	Value           json.RawMessage `json:"value"`
	Duplications    []duplicate     `json:"duplications"`
}

// duplicate is one other copy of a duplicated declaration.
type duplicate struct {
	DeclarationName string   `json:"declarationName"`
	DeclarationType string   `json:"declarationType"`
	Location        location `json:"location"`
	RelativePath    string   `json:"relativePath"`
}

type location struct {
	StartLine   int `json:"startLine"`
	StartColumn int `json:"startColumn"`
	EndLine     int `json:"endLine"`
	EndColumn   int `json:"endColumn"`
}

type fileIssues struct {
	path   string
	issues []issue
}

func str(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// num reads a metric value; an unreadable one is skipped, not fatal.
func num(raw json.RawMessage) (float64, bool) {
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f, true
	}
	if s := str(raw); s != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

func readFiles(raw json.RawMessage) ([]fileIssues, error) {
	var frs []fileResult
	if err := json.Unmarshal(raw, &frs); err != nil {
		return nil, err
	}
	out := make([]fileIssues, 0, len(frs))
	for _, fr := range frs {
		issues, err := readIssues(fr.Issues)
		if err != nil {
			return nil, fmt.Errorf("%s: issues: %w", fr.Path, err)
		}
		out = append(out, fileIssues{path: filepath.ToSlash(fr.Path), issues: issues})
	}
	return out, nil
}

// readIssues accepts an array or a single object; the docs show both.
func readIssues(raw json.RawMessage) ([]issue, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var many []issue
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	var one issue
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, err
	}
	return []issue{one}, nil
}

func sectionName(key string) string {
	if s, ok := sections[key]; ok {
		return s
	}
	var b strings.Builder
	for i, r := range strings.TrimSuffix(key, "Results") {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// kebab makes a DCM label safe inside a rule ID.
func kebab(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), " ", "-"), "_", "-")
}
