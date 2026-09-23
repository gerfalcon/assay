package sarif

import (
	"os"
	"strings"
	"testing"
)

// The C# instructions have been wrong three times, and every time the code was
// fine — the defect was in the command we tell people to run. A wrong example
// in a shipped document is a defect like any other, and this is the only test
// that can catch it.
//
// Each rule below corresponds to a failure observed against a real .NET 8
// build, and each of those failures was SILENT: a clean-looking result from a
// broken setup.
func TestCSharpInstructionsCannotRegress(t *testing.T) {
	for _, path := range []string{"../../docs/USAGE.md", "../../README.md"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		doc := string(b)

		// 1. A literal comma is consumed by MSBuild as a property separator, so
		//    Roslyn silently emits SARIF 1.0 — a different document shape.
		for _, line := range strings.Split(doc, "\n") {
			// Only lines that SET it — `ErrorLog=` or `<ErrorLog>` — not prose
			// that happens to name it.
			if !strings.Contains(line, "ErrorLog=") && !strings.Contains(line, "<ErrorLog>") {
				continue
			}
			if strings.Contains(line, "version=2.1") && !strings.Contains(line, "%2c") {
				t.Errorf("%s: ErrorLog uses a literal comma; MSBuild eats it and you get SARIF 1.0\n  %s",
					path, strings.TrimSpace(line))
			}
			// 2. A bare ErrorLog with no version at all is the original bug.
			if !strings.Contains(line, "version=2.1") && !strings.Contains(line, "*.sarif`") {
				t.Errorf("%s: ErrorLog without version=2.1 produces SARIF 1.0\n  %s",
					path, strings.TrimSpace(line))
			}
		}
	}

	usage, err := os.ReadFile("../../docs/USAGE.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(usage)

	// 3. Analysers do not run on an up-to-date build, so no SARIF is written
	//    and whatever is left on disk gets imported again.
	if !strings.Contains(doc, "--no-incremental") {
		t.Error("docs/USAGE.md never mentions --no-incremental; an incremental build runs no analysers and writes no SARIF")
	}

	// 4. One shared ErrorLog path has each project overwrite the last, so the
	//    name has to vary per project — and that only expands inside a props
	//    file, never in a command-line property.
	if !strings.Contains(doc, "$(MSBuildProjectName)") {
		t.Error("docs/USAGE.md does not use a per-project ErrorLog name; one shared path means each project overwrites the last")
	}

	// 5. Importing a single file after a multi-project build gives a baseline
	//    covering one project and a green check over the rest.
	start := strings.Index(doc, "# C# — Roslyn")
	end := strings.Index(doc, "# Dart")
	if start < 0 || end < start {
		t.Fatal("could not isolate the C# example in docs/USAGE.md")
	}
	if csharp := doc[start:end]; !strings.Contains(csharp, "*.sarif") {
		t.Errorf("the C# import example does not glob; a solution writes one SARIF per project:\n%s", csharp)
	}
}
