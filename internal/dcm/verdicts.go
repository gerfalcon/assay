// Verdicts for imported findings: the marker in the source, then the config.
package dcm

import (
	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/internal/verdict"
)

// judge resolves a verdict like the Go scan: marker first, then config. It never bypasses the gate.
func (im *importer) judge(f *model.Finding) {
	v, ok := im.marker(f.File, f.Line)
	if !ok {
		v, ok = im.opt.Config.Match(f.Rule, f.File)
	}
	if !ok {
		return
	}
	f.Verdict, f.VerdictWhy, f.VerdictFrom = string(v.Verdict), v.Reason, v.Source
	if !v.Until.IsZero() {
		f.VerdictUntil = v.Until.Format("2006-01-02")
	}
}

// marker looks for a quality: comment on the line or the line above. Text-based: no Dart parser.
func (im *importer) marker(path string, line int) (verdict.V, bool) {
	lines, ok := im.lines(path)
	if !ok || line < 1 || line > len(lines) {
		return verdict.V{}, false
	}
	for _, n := range []int{line, line - 1} {
		if n < 1 {
			continue
		}
		if v, ok := verdict.Parse(lines[n-1]); ok {
			v.Line = n
			return v, true
		}
	}
	return verdict.V{}, false
}
