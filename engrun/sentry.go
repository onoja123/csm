package engrun

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sentry integration webhooks carry their kind in the Sentry-Hook-Resource header: "issue" for issue state changes,
// "error" for error events and "event_alert" when an alert rule fires on an event.
const (
	SentryIssue      = "issue"
	SentryError      = "error"
	SentryEventAlert = "event_alert"
	// SentryTestResource marks a signed test delivery from CSM; receivers verify it and create nothing.
	SentryTestResource = "csm.test"
)

var (
	ErrBadSignature = errors.New("the webhook signature does not match")
	ErrIgnored      = errors.New("this webhook does not describe an error to fix")
)

// VerifySentrySignature checks Sentry-Hook-Signature: the hex HMAC-SHA256 of the raw body with the integration's client
// secret. An empty secret never verifies, so an unconfigured integration cannot accept unsigned events.
func VerifySentrySignature(secret string, body []byte, signature string) bool {
	if secret == "" || signature == "" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(want), []byte(strings.ToLower(strings.TrimSpace(signature))))
}

// SignSentry is the signature a Sentry integration would send; used to simulate events and in tests.
func SignSentry(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)

	return hex.EncodeToString(mac.Sum(nil))
}

type sentryPayload struct {
	Action string `json:"action"`
	Data   struct {
		Issue *sentryIssue `json:"issue"`
		Error *sentryEvent `json:"error"`
		Event *sentryEvent `json:"event"`
		Rule  string       `json:"triggered_rule"`
	} `json:"data"`
}

type sentryIssue struct {
	ID        string   `json:"id"`
	ShortID   string   `json:"shortId"`
	Title     string   `json:"title"`
	Culprit   string   `json:"culprit"`
	Level     string   `json:"level"`
	Platform  string   `json:"platform"`
	Permalink string   `json:"permalink"`
	Count     flexInt  `json:"count"`
	UserCount flexInt  `json:"userCount"`
	FirstSeen flexTime `json:"firstSeen"`
	LastSeen  flexTime `json:"lastSeen"`
	Project   flexProj `json:"project"`
	Metadata  struct {
		Type     string `json:"type"`
		Value    string `json:"value"`
		Filename string `json:"filename"`
		Function string `json:"function"`
	} `json:"metadata"`
}

type sentryEvent struct {
	EventID     string          `json:"event_id"`
	IssueID     flexString      `json:"issue_id"`
	GroupID     flexString      `json:"group_id"`
	Project     flexProj        `json:"project"`
	Title       string          `json:"title"`
	Message     string          `json:"message"`
	Culprit     string          `json:"culprit"`
	Level       string          `json:"level"`
	Platform    string          `json:"platform"`
	Environment string          `json:"environment"`
	Release     string          `json:"release"`
	Timestamp   flexTime        `json:"timestamp"`
	Datetime    flexTime        `json:"datetime"`
	WebURL      string          `json:"web_url"`
	IssueURL    string          `json:"issue_url"`
	Tags        json.RawMessage `json:"tags"`
	Request     *struct {
		Method string `json:"method"`
		URL    string `json:"url"`
	} `json:"request"`
	Exception *struct {
		Values []struct {
			Type       string `json:"type"`
			Value      string `json:"value"`
			Module     string `json:"module"`
			Stacktrace *struct {
				Frames []struct {
					Filename    string `json:"filename"`
					AbsPath     string `json:"abs_path"`
					Function    string `json:"function"`
					Lineno      int    `json:"lineno"`
					InApp       bool   `json:"in_app"`
					ContextLine string `json:"context_line"`
				} `json:"frames"`
			} `json:"stacktrace"`
		} `json:"values"`
	} `json:"exception"`
}

// ParseSentry turns a Sentry integration webhook into a Signal. The raw body is never kept: only the fields CSM uses,
// bounded and redacted by Normalize. requestID is the delivery id (the Request-ID header) used for idempotency when the
// event has none of its own.
func ParseSentry(resource string, body []byte, requestID string, now time.Time) (Signal, error) {
	var p sentryPayload

	if err := json.Unmarshal(body, &p); err != nil {
		return Signal{}, fmt.Errorf("the payload is not valid JSON: %w", err)
	}

	s := Signal{Source: SourceSentry, ReceivedAt: now.UTC(), Evidence: Evidence{Kind: SourceSentry}}
	ev := &SentryEvidence{}

	switch {
	case resource == SentryIssue || (resource == "" && p.Data.Issue != nil):
		iss := p.Data.Issue

		if iss == nil || iss.ID == "" {
			return Signal{}, fmt.Errorf("%w: no issue in the payload", ErrIgnored)
		}

		if p.Action != "created" && p.Action != "unresolved" && p.Action != "" {
			return Signal{}, fmt.Errorf("%w: issue %s", ErrIgnored, p.Action)
		}

		s.EventType = "issue." + orValue(p.Action, "created")
		s.IssueID = iss.ID
		s.ExternalID = "issue:" + iss.ID + ":" + orValue(p.Action, "created") + ":" + iss.LastSeen.String()
		s.Title, s.Culprit, s.Severity, s.Link = iss.Title, iss.Culprit, iss.Level, iss.Permalink
		s.Message = iss.Metadata.Value
		s.Repository.Project = iss.Project.String()
		s.OccurredAt = iss.LastSeen.Time
		ev.Platform, ev.Count, ev.UserCount, ev.FirstSeen, ev.LastSeen = iss.Platform, int(iss.Count), int(iss.UserCount), iss.FirstSeen.Time, iss.LastSeen.Time

		if iss.Metadata.Type != "" {
			ex := Exception{Type: iss.Metadata.Type, Value: iss.Metadata.Value}

			if iss.Metadata.Filename != "" {
				ex.Frames = []Frame{{File: iss.Metadata.Filename, Function: iss.Metadata.Function, InApp: true}}
			}

			ev.Exceptions = []Exception{ex}
		}

	case resource == SentryError || resource == SentryEventAlert || (resource == "" && (p.Data.Error != nil || p.Data.Event != nil)):
		e := p.Data.Error
		s.EventType = "error." + orValue(p.Action, "created")

		if e == nil {
			e = p.Data.Event
			s.EventType = "event_alert." + orValue(p.Action, "triggered")
		}

		if e == nil || e.EventID == "" {
			return Signal{}, fmt.Errorf("%w: no event in the payload", ErrIgnored)
		}

		s.ExternalID = "event:" + e.EventID
		s.IssueID = orValue(e.IssueID.String(), e.GroupID.String())
		s.Title, s.Culprit, s.Severity = e.Title, e.Culprit, e.Level
		s.Message = e.Message
		s.Environment, s.Release = e.Environment, e.Release
		s.Repository.Project = e.Project.String()
		s.Link = orValue(e.WebURL, e.IssueURL)
		s.OccurredAt = orTime(e.Timestamp.Time, e.Datetime.Time)
		ev.Platform = e.Platform
		ev.Tags = sentryTags(e.Tags)

		if e.Request != nil && e.Request.URL != "" {
			ev.Request = e.Request.Method + " " + strings.SplitN(e.Request.URL, "?", 2)[0]
		}

		if e.Exception != nil {
			for _, v := range e.Exception.Values {
				ex := Exception{Type: v.Type, Value: v.Value, Module: v.Module}

				if v.Stacktrace != nil {
					for _, f := range v.Stacktrace.Frames {
						ex.Frames = append(ex.Frames, Frame{File: orValue(f.Filename, f.AbsPath), Function: f.Function, Line: f.Lineno, InApp: f.InApp, Context: f.ContextLine})
					}
				}

				ev.Exceptions = append(ev.Exceptions, ex)
			}
		}

		if s.Title == "" && len(ev.Exceptions) > 0 {
			last := ev.Exceptions[len(ev.Exceptions)-1]
			s.Title = last.Type + ": " + last.Value
		}

		if c := ev.Tags["commit"]; c != "" {
			s.Repository.Commit = c
		}

	default:
		return Signal{}, fmt.Errorf("%w: resource %q", ErrIgnored, resource)
	}

	if s.ExternalID == "" && requestID != "" {
		s.ExternalID = "delivery:" + requestID
	}

	if s.Title == "" {
		return Signal{}, fmt.Errorf("%w: the event has no title or exception", ErrIgnored)
	}

	s.Evidence.Sentry = ev

	return Normalize(s), nil
}

func sentryTags(raw json.RawMessage) map[string]string {
	out := map[string]string{}

	var pairs [][]string

	if json.Unmarshal(raw, &pairs) == nil {
		for _, p := range pairs {
			if len(p) == 2 {
				out[p[0]] = p[1]
			}
		}

		return out
	}

	var objs []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}

	if json.Unmarshal(raw, &objs) == nil {
		for _, o := range objs {
			out[o.Key] = o.Value
		}
	}

	return out
}

func orValue(a, b string) string {
	if a != "" {
		return a
	}

	return b
}

func orTime(a, b time.Time) time.Time {
	if !a.IsZero() {
		return a
	}

	return b
}

type flexInt int

func (f *flexInt) UnmarshalJSON(data []byte) error {
	var n float64

	if json.Unmarshal(data, &n) == nil {
		*f = flexInt(n)

		return nil
	}

	var s string

	if err := json.Unmarshal(data, &s); err != nil {
		return nil
	}

	v, _ := strconv.Atoi(s)
	*f = flexInt(v)

	return nil
}

type flexString string

func (f *flexString) UnmarshalJSON(data []byte) error {
	var s string

	if json.Unmarshal(data, &s) == nil {
		*f = flexString(s)

		return nil
	}

	var n json.Number

	if json.Unmarshal(data, &n) == nil {
		*f = flexString(n.String())
	}

	return nil
}

func (f flexString) String() string { return string(f) }

// flexProj accepts a project as {"slug": …}, a bare slug or a numeric id.
type flexProj string

func (f *flexProj) UnmarshalJSON(data []byte) error {
	var obj struct {
		Slug string     `json:"slug"`
		ID   flexString `json:"id"`
	}

	if json.Unmarshal(data, &obj) == nil && (obj.Slug != "" || obj.ID != "") {
		*f = flexProj(orValue(obj.Slug, obj.ID.String()))

		return nil
	}

	var s flexString

	s.UnmarshalJSON(data)
	*f = flexProj(s)

	return nil
}

func (f flexProj) String() string { return string(f) }

// flexTime accepts RFC 3339 strings and Unix seconds, the two forms Sentry payloads use.
type flexTime struct{ time.Time }

func (f *flexTime) UnmarshalJSON(data []byte) error {
	var s string

	if json.Unmarshal(data, &s) == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			f.Time = t.UTC()
		}

		return nil
	}

	var secs float64

	if json.Unmarshal(data, &secs) == nil && secs > 0 {
		f.Time = time.Unix(int64(secs), int64((secs-float64(int64(secs)))*1e9)).UTC()
	}

	return nil
}

func (f flexTime) String() string {
	if f.IsZero() {
		return ""
	}

	return f.UTC().Format(time.RFC3339)
}
