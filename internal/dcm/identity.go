// What identifies a finding across edits: the span, the declaration, the ordinal.
package dcm

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/sherzing/assay/internal/model"
)

// decl is a declaration range from the metrics section, used to attribute lint issues to their function.
type decl struct {
	name       string
	start, end int
}

// basis picks what identifies an instance: a lint issue's span, a declaration's name, or the file.
func (im *importer) basis(section, path string, is issue) string {
	switch section {
	case "unused-files":
		return ""
	case "analyze":
		if wholeFile(is.Location) {
			return "" // the file is the finding; its first line is not
		}
		if line, ok := im.span(path, is.Location); ok {
			return line
		}
	}
	if is.DeclarationName != "" {
		return is.DeclarationType + " " + is.DeclarationName
	}
	return is.Message
}

// wholeFile recognises a region covering the file from line 1, like avoid-long-files.
func wholeFile(loc location) bool {
	return loc.StartLine <= 1 && loc.EndLine > loc.StartLine
}

// span returns the first line of the reported region from its start column.
func (im *importer) span(path string, loc location) (string, bool) {
	lines, ok := im.lines(path)
	if !ok || loc.StartLine < 1 || loc.StartLine > len(lines) {
		return "", false
	}
	line := []rune(lines[loc.StartLine-1])
	start := loc.StartColumn - 1
	if start < 0 || start > len(line) {
		start = 0
	}
	end := len(line)
	if loc.EndLine == loc.StartLine && loc.EndColumn-1 > start && loc.EndColumn-1 <= len(line) {
		end = loc.EndColumn - 1
	}
	s := strings.TrimSpace(string(line[start:end]))
	if s == "" {
		return "", false
	}
	return s, true
}

// enclosing names the innermost declaration covering the line.
func (im *importer) enclosing(path string, line int) string {
	best := ""
	size := -1
	for _, d := range im.decls[path] {
		if line < d.start || line > d.end {
			continue
		}
		if size < 0 || d.end-d.start < size {
			best, size = d.name, d.end-d.start
		}
	}
	return best
}

func (im *importer) lines(path string) ([]string, bool) {
	if l, ok := im.src[path]; ok {
		return l, l != nil
	}
	data, err := os.ReadFile(filepath.Join(im.opt.Root, path))
	if err != nil {
		im.src[path] = nil // unreadable: identity falls back to the message
		return nil, false
	}
	l := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	im.src[path] = l
	return l, true
}

// index records a declaration's range once.
func (im *importer) index(path, name string, loc location) {
	end := loc.EndLine
	if end < loc.StartLine {
		end = loc.StartLine
	}
	for _, d := range im.decls[path] {
		if d.name == name && d.start == loc.StartLine {
			return
		}
	}
	im.decls[path] = append(im.decls[path], decl{name: name, start: loc.StartLine, end: end})
}

// disambiguate numbers identical findings in source order, so the gate tracks their count
// without line numbers. On a real app 13% of findings collided before this.
func (im *importer) disambiguate() {
	byFP := map[string][]int{}
	for i, f := range im.res.Findings {
		byFP[f.Fingerprint] = append(byFP[f.Fingerprint], i)
	}
	for fp, idx := range byFP {
		if len(idx) < 2 {
			continue
		}
		fs := im.res.Findings
		sort.Slice(idx, func(a, b int) bool {
			if fs[idx[a]].Line != fs[idx[b]].Line {
				return fs[idx[a]].Line < fs[idx[b]].Line
			}
			return fs[idx[a]].Col < fs[idx[b]].Col
		})
		for k, i := range idx[1:] {
			f := &fs[i]
			f.Fingerprint = model.Fingerprint(f.File, f.Rule, f.Func, fp+"#"+strconv.Itoa(k+2))
		}
	}
}
