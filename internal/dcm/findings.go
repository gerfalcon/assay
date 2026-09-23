// Findings: one DCM issue to one finding.
package dcm

import (
	"strconv"
	"strings"

	"github.com/sherzing/assay/internal/model"
)

func (im *importer) findings(section string, files []fileIssues) {
	for _, f := range files {
		im.files[f.path] = true
		for _, is := range f.issues {
			im.finding(section, f.path, is)
		}
	}
}

// finding maps one issue: severity from the section or the issue, function from the metrics index.
func (im *importer) finding(section, path string, is issue) {
	sev := defaultSeverity(section)
	if section == "analyze" {
		sev = severity(is.Severity)
	}
	fn := is.DeclarationName
	if fn == "" {
		fn = im.enclosing(path, is.Location.StartLine)
	}
	im.add(model.Finding{
		Rule: ruleID(section, is), Severity: sev, File: path,
		Line: is.Location.StartLine, Col: is.Location.StartColumn,
		Func: fn, Message: message(section, is),
	}, im.basis(section, path, is))
}

// add fingerprints, judges and records one finding. basis identifies the instance beyond
// file, rule and function, never by line number.
func (im *importer) add(f model.Finding, basis string) {
	f.Fingerprint = model.Fingerprint(f.File, f.Rule, f.Func, basis)
	im.judge(&f)
	im.res.Findings = append(im.res.Findings, f)
}

// ruleID: dcm:<rule> for lint, dcm:<command>[:<kind>] otherwise. A generic -issue id adds nothing.
func ruleID(section string, is issue) string {
	if section == "analyze" {
		if is.ID == "" {
			return Tool + ":unknown"
		}
		return Tool + ":" + is.ID
	}
	if section == "unused-code" && is.DeclarationType != "" {
		return Tool + ":unused-code:" + kebab(is.DeclarationType)
	}
	if is.ID != "" && !strings.HasSuffix(is.ID, "-issue") {
		return Tool + ":" + section + ":" + is.ID
	}
	return Tool + ":" + section
}

// defaultSeverity applies to sections whose issues carry none.
func defaultSeverity(section string) model.Severity {
	switch section {
	case "unused-code", "unused-files", "unused-l10n", "code-duplication", "dependencies", "exports-completeness":
		return model.Warn
	}
	return model.Info
}

func severity(s string) model.Severity {
	switch strings.ToLower(s) {
	case "error":
		return model.Error
	case "warning":
		return model.Warn
	}
	return model.Info
}

func message(section string, is issue) string {
	msg := is.Message
	if msg == "" {
		msg = strings.TrimSpace(section + " " + is.DeclarationType + " " + is.DeclarationName)
	}
	if len(is.Duplications) > 0 {
		msg += " — also " + duplicatesText(is.Duplications)
	}
	return msg
}

// duplicatesText names the other copies, so a ticket says what to merge.
func duplicatesText(ds []duplicate) string {
	parts := make([]string, 0, len(ds))
	for _, d := range ds {
		where := d.RelativePath
		if d.Location.StartLine > 0 {
			where += ":" + strconv.Itoa(d.Location.StartLine)
		}
		if d.DeclarationName != "" {
			where += " (" + d.DeclarationName + ")"
		}
		parts = append(parts, where)
	}
	return strings.Join(parts, ", ")
}

func breached(l string) bool {
	l = level(l)
	return l == "high" || l == "very-high"
}

// breach is a threshold violation as a finding.
func (im *importer) breach(path string, is issue) {
	im.add(model.Finding{
		Rule: Tool + ":metrics:" + is.ID, Severity: levelSeverity(is.Level), File: path,
		Line: is.Location.StartLine, Col: is.Location.StartColumn,
		Func: is.DeclarationName, Message: is.Message,
		Suggest: "split it, or raise the threshold in analysis_options.yaml on purpose",
	}, "metric "+is.DeclarationName)
}

func levelSeverity(l string) model.Severity {
	if level(l) == "very-high" {
		return model.Warn
	}
	return model.Info
}

// level normalises the metric level: the docs say very-high, the tool says very high.
func level(s string) string { return kebab(s) }
