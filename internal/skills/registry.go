package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Registry searches a public index of skills. csm uses it only to find skills: an install always comes from the
// skill's own GitHub repository, pinned to a commit, through the same checks as any other source.
type Registry struct {
	URL    string
	Client *http.Client
}

const RegistryPrefix = "skills.sh:"

func NewRegistry() *Registry {
	base := os.Getenv("CSM_SKILLS_REGISTRY_URL")

	if base == "" {
		base = "https://skills.sh"
	}

	return &Registry{URL: strings.TrimSuffix(base, "/"), Client: &http.Client{Timeout: 20 * time.Second}}
}

// RegistryEntry is one search result; install counts and other popularity fields are deliberately not read.
type RegistryEntry struct {
	Repo  string `json:"source"`
	Skill string `json:"skillId"`
	Name  string `json:"name"`
}

var (
	repoName  = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	skillName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// SourceRef is the source string that installs the entry: its repository, narrowed to the one skill.
func (e RegistryEntry) SourceRef() string { return RegistryPrefix + e.Repo + "/" + e.Skill }

func (r *Registry) Search(ctx context.Context, query string, limit int) ([]RegistryEntry, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}

	u := fmt.Sprintf("%s/api/search?q=%s&limit=%d", r.URL, url.QueryEscape(query), limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)

	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "csm-skills")

	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the skills.sh registry: %w", redactURLError(err))
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("the skills.sh registry is rate limiting searches; try again later")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the skills.sh registry answered %s", resp.Status)
	}

	var body struct {
		Skills []RegistryEntry `json:"skills"`
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("unexpected answer from the skills.sh registry: %w", err)
	}

	var out []RegistryEntry

	for _, e := range body.Skills {
		if repoName.MatchString(e.Repo) && skillName.MatchString(e.Skill) {
			out = append(out, e)
		}
	}

	return out, nil
}

// ParseRegistryRef turns skills.sh:owner/repo/skill into the repository to read and the skill to take from it.
func ParseRegistryRef(src string) (repo, skill string, ok bool) {
	rest, found := strings.CutPrefix(src, RegistryPrefix)
	if !found {
		return "", "", false
	}

	i := strings.LastIndex(rest, "/")
	if i <= 0 {
		return "", "", false
	}

	repo, skill = rest[:i], rest[i+1:]

	return repo, skill, repoName.MatchString(repo) && skillName.MatchString(skill)
}
