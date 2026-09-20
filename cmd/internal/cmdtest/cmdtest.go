// Package cmdtest builds the assay binaries and runs them as real processes.
//
// The tests that use it are end-to-end on purpose. Argument parsing, exit codes
// and the JSONL that crosses a pipe are the contract users depend on, and none
// of it is reachable by calling the cmd* functions directly: they call os.Exit,
// which would take the test runner down with them, and a direct call skips the
// flag wiring that has already been wrong once.
//
// The cost is a `go build`, so each binary is built once per test binary and
// reused. Set GOCOVERDIR to get coverage out of the child processes — the build
// is then instrumented and the counters land in that directory.
package cmdtest

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Bin is a built command, ready to run.
type Bin struct {
	Name string
	Path string
}

var (
	mu     sync.Mutex
	binDir string
	built  = map[string]string{}
)

// Main wraps a package's TestMain so the shared build directory is removed once
// every test in the package has finished. Building per test would dominate the
// runtime; cleaning up per test would delete a binary another test is using.
func Main(m *testing.M) int {
	code := m.Run()
	mu.Lock()
	defer mu.Unlock()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	return code
}

// Build compiles cmd/<name> once per test binary.
//
// A missing toolchain is a skip, not a failure: this suite has to stay runnable
// in a container that ships no compiler. A toolchain that is present and cannot
// build is a real failure and is reported as one.
func Build(t *testing.T, name string) Bin {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()

	if p, ok := built[name]; ok {
		return Bin{Name: name, Path: p}
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go toolchain on PATH (%v) — skipping the end-to-end suite", err)
	}
	if binDir == "" {
		d, err := os.MkdirTemp("", "assay-e2e-bin-")
		if err != nil {
			t.Fatal(err)
		}
		binDir = d
	}

	out := filepath.Join(binDir, name)
	args := []string{"build", "-o", out}
	if os.Getenv("GOCOVERDIR") != "" {
		// The work happens in a child process, so the binary has to carry the
		// counters. Plain `go test -cover` would report nothing here.
		args = append(args, "-cover", "-coverpkg=github.com/sherzing/assay/...")
	}
	args = append(args, "github.com/sherzing/assay/cmd/"+name)

	cmd := exec.Command(goBin, args...)
	cmd.Dir = repoRoot()
	if b, err := cmd.CombinedOutput(); err != nil {
		var ee *exec.Error
		if errors.As(err, &ee) {
			t.Skipf("go build is unusable (%v) — skipping the end-to-end suite", err)
		}
		t.Fatalf("go build %s failed: %v\n%s", name, err, b)
	}
	built[name] = out
	return Bin{Name: name, Path: out}
}

// repoRoot locates the module root from this file's own path, so the tests do
// not care what directory the runner started in.
func repoRoot() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(self))))
}

// Result is everything an end-to-end caller can observe.
type Result struct {
	Cmd    string
	Stdout string
	Stderr string
	Code   int
}

// Out is stdout and stderr together, for assertions that should not care which
// stream a tool chose.
func (r Result) Out() string { return r.Stdout + r.Stderr }

func (r Result) String() string {
	return r.Cmd + "\n  exit " + strconv.Itoa(r.Code) + "\n  stdout: " + r.Stdout + "\n  stderr: " + r.Stderr
}

// MustPass fails the test unless the command exited 0.
func (r Result) MustPass(t *testing.T) Result {
	t.Helper()
	if r.Code != 0 {
		t.Fatalf("expected success, got exit %d\n%s", r.Code, r)
	}
	return r
}

// MustFail fails the test unless the command exited non-zero. Returns the
// result so the caller can go on to assert what it said — an exit code with no
// explanation is half a contract.
func (r Result) MustFail(t *testing.T) Result {
	t.Helper()
	if r.Code == 0 {
		t.Fatalf("expected a non-zero exit, got 0\n%s", r)
	}
	return r
}

// MustSay fails unless every fragment appears in the combined output.
func (r Result) MustSay(t *testing.T, fragments ...string) Result {
	t.Helper()
	for _, f := range fragments {
		if !strings.Contains(r.Out(), f) {
			t.Errorf("output does not mention %q\n%s", f, r)
		}
	}
	return r
}

// MustNotSay fails if any fragment appears. Used where naming the wrong thing
// is the bug — a gate that reports old violations as new is unusable.
func (r Result) MustNotSay(t *testing.T, fragments ...string) Result {
	t.Helper()
	for _, f := range fragments {
		if strings.Contains(r.Out(), f) {
			t.Errorf("output mentions %q and should not\n%s", f, r)
		}
	}
	return r
}

// Run executes the binary in dir with no stdin. Stdin is left at /dev/null,
// which is a character device — the same thing a tool sees on an interactive
// terminal, and the case where the pipe-reading commands must complain rather
// than hang.
func (b Bin) Run(t *testing.T, dir string, args ...string) Result {
	t.Helper()
	return b.exec(t, dir, nil, args)
}

// Pipe executes the binary with stdin wired to in, as a pipeline stage would.
func (b Bin) Pipe(t *testing.T, dir, in string, args ...string) Result {
	t.Helper()
	return b.exec(t, dir, strings.NewReader(in), args)
}

func (b Bin) exec(t *testing.T, dir string, stdin *strings.Reader, args []string) Result {
	t.Helper()
	cmd := exec.Command(b.Path, args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Env = append(os.Environ(), GitEnv()...)

	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s %v: %v", b.Name, args, err)
	}
	return Result{
		Cmd:    b.Name + " " + strings.Join(args, " "),
		Stdout: out.String(),
		Stderr: errb.String(),
		Code:   code,
	}
}

// GitEnv cuts every git invocation off from the machine's own configuration.
//
// Two things depend on this. `ratchet scan --emit` falls back to the committer's
// email domain for org attribution, so a developer's identity would otherwise
// appear in a test's expected output and the test would pass on one machine
// only. And a global commit.gpgsign would make a fixture commit prompt for a
// passphrase and hang.
func GitEnv() []string {
	return []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=assay e2e",
		"GIT_AUTHOR_EMAIL=e2e@example.invalid",
		"GIT_COMMITTER_NAME=assay e2e",
		"GIT_COMMITTER_EMAIL=e2e@example.invalid",
	}
}

// Tree writes a set of relative paths under a fresh temp directory.
func Tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		WriteFile(t, root, rel, body)
	}
	return root
}

// WriteFile writes one file under root, creating parents.
func WriteFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ReadFile reads one file under root.
func ReadFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Lines returns the non-empty lines of a JSONL stream.
func Lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
