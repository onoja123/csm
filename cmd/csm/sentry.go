package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/onoja123/csm/engrun"
)

const sentryTimeout = 20 * time.Second

// sentryConfig lets CSM read issues from Sentry's API with the user's own auth token, so a single developer can fix
// Sentry issues from the terminal without hosting anything. The token is stored with the AutoFix settings (0600) and is
// sent only to the configured Sentry host.
type sentryConfig struct {
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
	Org   string `json:"org"`
	// Projects maps each connected project slug to Sentry's numeric id, which the issues endpoint filters on.
	Projects map[string]string `json:"projects,omitempty"`
}

type sentryClient struct {
	cfg  sentryConfig
	http *http.Client
}

type sentryOrg struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type sentryProject struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
}

type sentryIssueRow struct {
	ID        string    `json:"id"`
	ShortID   string    `json:"shortId"`
	Title     string    `json:"title"`
	Culprit   string    `json:"culprit"`
	Level     string    `json:"level"`
	Count     string    `json:"count"`
	UserCount int       `json:"userCount"`
	LastSeen  time.Time `json:"lastSeen"`
	Permalink string    `json:"permalink"`
	Project   struct {
		Slug string `json:"slug"`
	} `json:"project"`
}

var sentryShortID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*-[A-Za-z0-9]+$`)

func validSentryURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")

	if raw == "" {
		raw = "https://sentry.io"
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("invalid Sentry URL %q", raw)
	}

	host := u.Hostname()

	if u.Scheme != "https" && !(u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")) {
		return "", errors.New("Sentry must be reached over https")
	}

	return u.String(), nil
}

func newSentryClient(cfg sentryConfig) sentryClient {
	return sentryClient{cfg: cfg, http: &http.Client{Timeout: sentryTimeout}}
}

func (c sentryClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.cfg.URL+path, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Sentry could not be reached at %s: %v", c.cfg.URL, err)
	}

	defer resp.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	switch {
	case resp.StatusCode == 401:
		return errors.New("Sentry rejected the auth token; create one with org:read, project:read and event:read")
	case resp.StatusCode == 403:
		return errors.New("the auth token cannot read this; it needs org:read, project:read and event:read")
	case resp.StatusCode == 404:
		return errNotFoundInSentry
	case resp.StatusCode >= 400:
		return fmt.Errorf("Sentry answered %d: %s", resp.StatusCode, firstLine(string(data)))
	}

	if raw, ok := out.(*json.RawMessage); ok {
		*raw = data

		return nil
	}

	return json.Unmarshal(data, out)
}

var errNotFoundInSentry = errors.New("not found in Sentry")

func (c sentryClient) orgs(ctx context.Context) ([]sentryOrg, error) {
	var out []sentryOrg

	return out, c.get(ctx, "/api/0/organizations/", &out)
}

func (c sentryClient) projects(ctx context.Context, org string) ([]sentryProject, error) {
	var out []sentryProject

	return out, c.get(ctx, "/api/0/organizations/"+url.PathEscape(org)+"/projects/", &out)
}

func (c sentryClient) issues(ctx context.Context, limit int) ([]sentryIssueRow, error) {
	q := url.Values{"query": {"is:unresolved"}, "statsPeriod": {"14d"}, "sort": {"date"}, "limit": {fmt.Sprint(limit)}}

	for _, id := range c.cfg.Projects {
		q.Add("project", id)
	}

	var out []sentryIssueRow

	return out, c.get(ctx, "/api/0/organizations/"+url.PathEscape(c.cfg.Org)+"/issues/?"+q.Encode(), &out)
}

// issue fetches one issue by its short id (ORDERS-API-3F) or numeric id, with its latest event, as a Signal.
func (c sentryClient) issue(ctx context.Context, ref string) (engrun.Signal, error) {
	org := url.PathEscape(c.cfg.Org)
	ref = strings.TrimSpace(ref)

	var issue json.RawMessage

	switch {
	case regexp.MustCompile(`^\d+$`).MatchString(ref):
		if err := c.get(ctx, "/api/0/organizations/"+org+"/issues/"+ref+"/", &issue); errors.Is(err, errNotFoundInSentry) {
			err = c.get(ctx, "/api/0/issues/"+ref+"/", &issue)

			if err != nil {
				return engrun.Signal{}, err
			}
		} else if err != nil {
			return engrun.Signal{}, err
		}

	case sentryShortID.MatchString(ref):
		var resolved struct {
			Group json.RawMessage `json:"group"`
		}

		if err := c.get(ctx, "/api/0/organizations/"+org+"/shortids/"+url.PathEscape(strings.ToUpper(ref))+"/", &resolved); err != nil {
			return engrun.Signal{}, err
		}

		issue = resolved.Group

	default:
		return engrun.Signal{}, fmt.Errorf("%q is not a Sentry issue id; use the short id from csm issues, like ORDERS-API-3F", ref)
	}

	var head struct {
		ID string `json:"id"`
	}

	if json.Unmarshal(issue, &head) != nil || head.ID == "" {
		return engrun.Signal{}, errors.New("Sentry returned an issue CSM could not read")
	}

	var event json.RawMessage

	if err := c.get(ctx, "/api/0/organizations/"+org+"/issues/"+head.ID+"/events/latest/", &event); errors.Is(err, errNotFoundInSentry) {
		if err := c.get(ctx, "/api/0/issues/"+head.ID+"/events/latest/", &event); err != nil {
			return engrun.Signal{}, err
		}
	} else if err != nil {
		return engrun.Signal{}, err
	}

	return engrun.ParseSentryIssue(issue, event, time.Now())
}

func (d *daemon) sentry() (sentryClient, error) {
	cfg := d.autofixConfig()

	if cfg.Sentry == nil || cfg.Sentry.Token == "" {
		return sentryClient{}, errors.New("Sentry is not connected; run csm sentry connect")
	}

	return newSentryClient(*cfg.Sentry), nil
}

func (e *buildEngine) activeSummary(id string) (buildSummary, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	b, ok := e.builds[id]
	if !ok || b.Status.Terminal() {
		return buildSummary{}, false
	}

	return b.summary(), true
}

// postSentryFix pulls one Sentry issue and starts an AutoFix run for it, whatever the automatic-start setting: the user
// asked for this fix. An incident that already has a run in progress gets that run back instead of a second one.
func (d *daemon) postSentryFix(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Issue string `json:"issue"`
	}

	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, err)

		return
	}

	c, err := d.sentry()
	if err != nil {
		writeErr(w, 409, err)

		return
	}

	sig, err := c.issue(r.Context(), in.Issue)
	if err != nil {
		status := 502

		if errors.Is(err, errNotFoundInSentry) {
			status, err = 404, fmt.Errorf("Sentry has no issue %q in %s", in.Issue, c.cfg.Org)
		}

		writeErr(w, status, err)

		return
	}

	m, mapped := d.autofixConfig().mapping(sig.Source, sig.Repository.Project)
	if !mapped {
		writeErr(w, 409, fmt.Errorf("Sentry project %q is not mapped to a repository; run csm sentry connect in that repository", sig.Repository.Project))

		return
	}

	_, inc, _, err := d.signals.record(sig, true, time.Now())
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	if inc == nil {
		found, ok := d.signals.byFingerprint(sig.Fingerprint)
		if !ok {
			writeErr(w, 500, errors.New("the incident for this issue could not be found"))

			return
		}

		inc = &found
	}

	d.bus.publish("signals", nil)

	for i := len(inc.Runs) - 1; i >= 0; i-- {
		if sum, ok := d.builds.activeSummary(inc.Runs[i]); ok {
			writeOK(w, map[string]any{"incident": inc.ID, "run": sum, "existing": true})

			return
		}
	}

	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	b, err := d.builds.createAutoFix(s, sig, inc.ID, m)
	if err != nil {
		d.signals.attachRun(inc.ID, "", "Could not start a run: "+err.Error())
		writeErr(w, 400, err)

		return
	}

	d.signals.attachRun(inc.ID, b.ID, "")
	d.rememberProject(b.Repo)
	d.log.add(Activity{Kind: "signal.received", Title: "Fixing Sentry issue: " + sig.Title, Detail: "csm fix", Refs: map[string]string{"incident": inc.ID, "build": b.ID}})
	writeOK(w, map[string]any{"incident": inc.ID, "run": b.summary()})
}
