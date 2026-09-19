package docket

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Provider is the whole surface an issue tracker needs to expose.
//
// Deliberately three methods. Every tracker has some notion of create, close and
// a link, and nothing else here is worth abstracting — a bigger interface would
// make each new provider a project rather than an afternoon.
type Provider interface {
	Name() string
	Create(title, body string, labels []string) (id, url string, err error)
	Close(id, comment string) error
}

// DryRun prints what would happen and touches nothing.
//
// It is the DEFAULT, not a debugging aid. Creating tickets is an outward-facing
// action against someone else's tracker, and a tool that does that on first run
// without being asked deserves to be uninstalled.
type DryRun struct{ W io.Writer }

func (d DryRun) Name() string { return "dry-run" }

func (d DryRun) Create(title, body string, labels []string) (string, string, error) {
	fmt.Fprintf(d.W, "\n─── would create ──────────────────────────────────────────\n")
	fmt.Fprintf(d.W, "title:  %s\n", title)
	if len(labels) > 0 {
		fmt.Fprintf(d.W, "labels: %s\n", strings.Join(labels, ", "))
	}
	fmt.Fprintf(d.W, "\n%s\n", indent(truncate(body, 1200), "  "))
	return "DRY-RUN", "", nil
}

func (d DryRun) Close(id, comment string) error {
	fmt.Fprintf(d.W, "would close %s: %s\n", id, comment)
	return nil
}

func indent(s, pad string) string {
	return pad + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+pad)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n  … (truncated for preview)"
}

// Files writes tickets as markdown on disk.
//
// Not a test double — a real provider. It makes docket useful with no tracker,
// no token and no network, which matters for anyone evaluating this before
// wiring it to their workspace, and for teams whose tracker has no usable API.
type Files struct {
	Dir string
	n   int
}

func (f *Files) Name() string { return "file" }

func (f *Files) Create(title, body string, labels []string) (string, string, error) {
	if err := os.MkdirAll(f.Dir, 0o755); err != nil {
		return "", "", err
	}
	f.n++
	id := fmt.Sprintf("T-%03d", f.n)
	name := id + "-" + slug(title) + ".md"
	path := filepath.Join(f.Dir, name)

	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	if len(labels) > 0 {
		fmt.Fprintf(&b, "`%s`\n\n", strings.Join(labels, "` `"))
	}
	b.WriteString(body)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", "", err
	}
	return id, path, nil
}

func (f *Files) Close(id, comment string) error {
	matches, _ := filepath.Glob(filepath.Join(f.Dir, id+"-*.md"))
	for _, m := range matches {
		cur, err := os.ReadFile(m)
		if err != nil {
			return err
		}
		// Prepend rather than delete: the record of what was cleaned up is
		// worth more than a tidy directory.
		out := "> **CLOSED** — " + comment + "\n\n" + string(cur)
		if err := os.WriteFile(m, []byte(out), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func slug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	return strings.Trim(truncate(b.String(), 50), "-")
}

// Linear talks to the Linear GraphQL API.
type Linear struct {
	Token  string
	TeamID string
	client *http.Client
}

func NewLinear(token, teamID string) (*Linear, error) {
	if token == "" {
		token = os.Getenv("LINEAR_API_KEY")
	}
	if token == "" {
		return nil, fmt.Errorf("no Linear token: set LINEAR_API_KEY or pass --token")
	}
	if teamID == "" {
		return nil, fmt.Errorf("no Linear team: pass --team (the team key, e.g. ENG)")
	}
	return &Linear{Token: token, TeamID: teamID,
		client: &http.Client{Timeout: 30 * time.Second}}, nil
}

func (l *Linear) Name() string { return "linear" }

func (l *Linear) gql(query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.linear.app/graphql", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", l.Token)

	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("linear http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	// GraphQL reports failures in the body with a 200, so the status code alone
	// is not enough to know whether anything happened.
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("linear: bad response: %w", err)
	}
	if len(env.Errors) > 0 {
		return fmt.Errorf("linear: %s", env.Errors[0].Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

// teamUUID resolves a human team key like "ENG" to the UUID the API wants.
func (l *Linear) teamUUID() (string, error) {
	if strings.Count(l.TeamID, "-") == 4 {
		return l.TeamID, nil // already a UUID
	}
	var out struct {
		Teams struct {
			Nodes []struct{ ID, Key string } `json:"nodes"`
		} `json:"teams"`
	}
	if err := l.gql(`query { teams { nodes { id key } } }`, nil, &out); err != nil {
		return "", err
	}
	for _, t := range out.Teams.Nodes {
		if strings.EqualFold(t.Key, l.TeamID) {
			return t.ID, nil
		}
	}
	return "", fmt.Errorf("linear: no team with key %q", l.TeamID)
}

// labelIDs resolves label names, creating any that do not exist.
func (l *Linear) labelIDs(teamUUID string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var out struct {
		Team struct {
			Labels struct {
				Nodes []struct{ ID, Name string } `json:"nodes"`
			} `json:"labels"`
		} `json:"team"`
	}
	err := l.gql(`query($id:String!){ team(id:$id){ labels { nodes { id name } } } }`,
		map[string]any{"id": teamUUID}, &out)
	if err != nil {
		return nil, err
	}
	have := map[string]string{}
	for _, n := range out.Team.Labels.Nodes {
		have[strings.ToLower(n.Name)] = n.ID
	}
	var ids []string
	for _, want := range names {
		if id, ok := have[strings.ToLower(want)]; ok {
			ids = append(ids, id)
			continue
		}
		var made struct {
			IssueLabelCreate struct {
				IssueLabel struct{ ID string } `json:"issueLabel"`
			} `json:"issueLabelCreate"`
		}
		err := l.gql(`mutation($in:IssueLabelCreateInput!){ issueLabelCreate(input:$in){ issueLabel { id } } }`,
			map[string]any{"in": map[string]any{"name": want, "teamId": teamUUID}}, &made)
		if err != nil {
			return nil, fmt.Errorf("create label %q: %w", want, err)
		}
		ids = append(ids, made.IssueLabelCreate.IssueLabel.ID)
	}
	return ids, nil
}

func (l *Linear) Create(title, body string, labels []string) (string, string, error) {
	team, err := l.teamUUID()
	if err != nil {
		return "", "", err
	}
	labelIDs, err := l.labelIDs(team, labels)
	if err != nil {
		return "", "", err
	}
	in := map[string]any{"teamId": team, "title": title, "description": body}
	if len(labelIDs) > 0 {
		in["labelIds"] = labelIDs
	}
	var out struct {
		IssueCreate struct {
			Success bool `json:"success"`
			Issue   struct {
				ID         string `json:"id"`
				Identifier string `json:"identifier"`
				URL        string `json:"url"`
			} `json:"issue"`
		} `json:"issueCreate"`
	}
	err = l.gql(`mutation($in:IssueCreateInput!){ issueCreate(input:$in){ success issue { id identifier url } } }`,
		map[string]any{"in": in}, &out)
	if err != nil {
		return "", "", err
	}
	if !out.IssueCreate.Success {
		return "", "", fmt.Errorf("linear: issueCreate returned success=false")
	}
	// Return the UUID as the id — identifiers like ENG-123 can change if an
	// issue moves team, and the stored mapping must survive that.
	return out.IssueCreate.Issue.ID, out.IssueCreate.Issue.URL, nil
}

func (l *Linear) Close(id, comment string) error {
	if comment != "" {
		_ = l.gql(`mutation($in:CommentCreateInput!){ commentCreate(input:$in){ success } }`,
			map[string]any{"in": map[string]any{"issueId": id, "body": comment}}, nil)
	}
	// Find the team's completed state, since the UUID differs per workspace.
	var st struct {
		Issue struct {
			Team struct {
				States struct {
					Nodes []struct{ ID, Type string } `json:"nodes"`
				} `json:"states"`
			} `json:"team"`
		} `json:"issue"`
	}
	if err := l.gql(`query($id:String!){ issue(id:$id){ team { states { nodes { id type } } } } }`,
		map[string]any{"id": id}, &st); err != nil {
		return err
	}
	var done string
	for _, s := range st.Issue.Team.States.Nodes {
		if s.Type == "completed" {
			done = s.ID
			break
		}
	}
	if done == "" {
		return fmt.Errorf("linear: no completed state found for issue %s", id)
	}
	return l.gql(`mutation($id:String!,$in:IssueUpdateInput!){ issueUpdate(id:$id,input:$in){ success } }`,
		map[string]any{"id": id, "in": map[string]any{"stateId": done}}, nil)
}
