package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/sites"
)

// A site can be made from a repository. The module lists the repositories
// its token can see, to choose from, and inspects one (its default branch
// and what its root holds) so the site is made as the repository is: a
// Node app, a front end that is built, a Go program or files. Without a
// token only public repositories can be inspected, and nothing is listed.

// ErrNoToken is listing without a token: there is nobody to list for.
var ErrNoToken = errors.New("the GitHub module has no access token: your repositories cannot be listed. A public repository can still be given by name or address")

// RepoSummary is one repository in the list.
type RepoSummary struct {
	FullName      string    `json:"full_name"`
	Private       bool      `json:"private"`
	Description   string    `json:"description,omitempty"`
	DefaultBranch string    `json:"default_branch"`
	PushedAt      time.Time `json:"pushed_at"`
}

func (m *Module) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(m.APIBase, "/")+path, nil)
	if err != nil {
		return err
	}
	if t := m.token(""); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "middleware")
	client := m.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return fmt.Errorf("not found on GitHub (or private, and the token cannot see it)")
	default:
		return fmt.Errorf("github answered %s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

// ListRepos is every repository the token can see, most recently pushed
// first, up to three hundred.
func (m *Module) ListRepos(ctx context.Context) ([]RepoSummary, error) {
	if m.token("") == "" {
		return nil, ErrNoToken
	}
	var out []RepoSummary
	for page := 1; page <= 3; page++ {
		var batch []RepoSummary
		if err := m.get(ctx, fmt.Sprintf("/user/repos?per_page=100&sort=pushed&affiliation=owner,collaborator,organization_member&page=%d", page), &batch); err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < 100 {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].PushedAt.After(out[j].PushedAt) })
	return out, nil
}

// Inspect reads what a site made from the repository needs to know.
func (m *Module) Inspect(ctx context.Context, ref string) (sites.Repo, error) {
	full, err := sites.ParseRepo(ref)
	if err != nil {
		return sites.Repo{}, err
	}
	var info struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Private       bool   `json:"private"`
		Description   string `json:"description"`
	}
	if err := m.get(ctx, "/repos/"+full, &info); err != nil {
		return sites.Repo{}, fmt.Errorf("%s: %w", full, err)
	}
	r := sites.Repo{FullName: info.FullName, DefaultBranch: info.DefaultBranch, Private: info.Private, Description: info.Description}
	var root []struct {
		Name string `json:"name"`
	}
	// An empty repository has no contents: a 404 here, and nothing to
	// look at, which makes a static site that the first push fills.
	if err := m.get(ctx, "/repos/"+r.FullName+"/contents/", &root); err == nil {
		for _, e := range root {
			r.Files = append(r.Files, e.Name)
		}
	}
	for _, f := range r.Files {
		if f != "package.json" {
			continue
		}
		var file struct {
			Content string `json:"content"`
		}
		if err := m.get(ctx, "/repos/"+r.FullName+"/contents/package.json", &file); err == nil {
			r.PackageJSON, _ = base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
		}
	}
	return r, nil
}

func (m *Module) handleRepos(w http.ResponseWriter, r *http.Request) {
	list, err := m.ListRepos(r.Context())
	if errors.Is(err, ErrNoToken) {
		writeJSON(w, http.StatusOK, map[string]any{"repos": []RepoSummary{}, "error": err.Error()})
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if list == nil {
		list = []RepoSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": list})
}

func (m *Module) handleInspect(w http.ResponseWriter, r *http.Request) {
	repo, err := m.Inspect(r.Context(), r.PathValue("owner")+"/"+r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	mf, kind := sites.RepoManifest(repo, "", "")
	writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "kind": kind, "manifest": mf})
}
