package engrun

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type sentryAPIEvent struct {
	EventID     string          `json:"eventID"`
	ID          string          `json:"id"`
	GroupID     flexString      `json:"groupID"`
	Title       string          `json:"title"`
	Message     string          `json:"message"`
	Culprit     string          `json:"culprit"`
	Platform    string          `json:"platform"`
	DateCreated flexTime        `json:"dateCreated"`
	Tags        json.RawMessage `json:"tags"`
	Release     json.RawMessage `json:"release"`
	Entries     []struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	} `json:"entries"`
}

type sentryAPIException struct {
	Values []struct {
		Type       string `json:"type"`
		Value      string `json:"value"`
		Module     string `json:"module"`
		Stacktrace *struct {
			Frames []struct {
				Filename string            `json:"filename"`
				AbsPath  string            `json:"absPath"`
				Function string            `json:"function"`
				LineNo   int               `json:"lineNo"`
				InApp    bool              `json:"inApp"`
				Context  []json.RawMessage `json:"context"`
			} `json:"frames"`
		} `json:"stacktrace"`
	} `json:"values"`
}

// ParseSentryIssue turns an issue and one of its events, as Sentry's REST API returns them, into a Signal. It yields the
// same external id and fingerprint as the webhook for the same event, so a pulled issue and a delivered one deduplicate.
func ParseSentryIssue(issueJSON, eventJSON []byte, now time.Time) (Signal, error) {
	var iss sentryIssue

	if err := json.Unmarshal(issueJSON, &iss); err != nil || iss.ID == "" {
		return Signal{}, fmt.Errorf("the Sentry issue could not be read")
	}

	var e sentryAPIEvent

	if err := json.Unmarshal(eventJSON, &e); err != nil {
		return Signal{}, fmt.Errorf("the Sentry event could not be read: %w", err)
	}

	eventID := orValue(e.EventID, e.ID)
	s := Signal{Source: SourceSentry, EventType: "issue.pulled", IssueID: iss.ID, ReceivedAt: now.UTC(), Evidence: Evidence{Kind: SourceSentry}}
	s.ExternalID = "issue:" + iss.ID + ":pulled:" + iss.LastSeen.String()

	if eventID != "" {
		s.ExternalID = "event:" + eventID
	}

	s.Title = orValue(iss.Title, e.Title)
	s.Culprit = orValue(iss.Culprit, e.Culprit)
	s.Message = orValue(e.Message, iss.Metadata.Value)
	s.Repository.Project = iss.Project.String()
	s.Link = iss.Permalink
	s.OccurredAt = orTime(e.DateCreated.Time, iss.LastSeen.Time)

	ev := &SentryEvidence{Platform: orValue(e.Platform, iss.Platform), Count: int(iss.Count), UserCount: int(iss.UserCount), FirstSeen: iss.FirstSeen.Time, LastSeen: iss.LastSeen.Time}
	ev.Tags = sentryTags(e.Tags)
	s.Severity = orValue(ev.Tags["level"], iss.Level)
	s.Environment = ev.Tags["environment"]
	s.Release = orValue(ev.Tags["release"], releaseVersion(e.Release))

	for _, entry := range e.Entries {
		switch entry.Type {
		case "exception":
			var x sentryAPIException

			if json.Unmarshal(entry.Data, &x) != nil {
				continue
			}

			for _, v := range x.Values {
				ex := Exception{Type: v.Type, Value: v.Value, Module: v.Module}

				if v.Stacktrace != nil {
					for _, f := range v.Stacktrace.Frames {
						ex.Frames = append(ex.Frames, Frame{File: orValue(f.Filename, f.AbsPath), Function: f.Function, Line: f.LineNo, InApp: f.InApp, Context: contextLine(f.Context, f.LineNo)})
					}
				}

				ev.Exceptions = append(ev.Exceptions, ex)
			}

		case "request":
			var r struct {
				Method string `json:"method"`
				URL    string `json:"url"`
			}

			if json.Unmarshal(entry.Data, &r) == nil && r.URL != "" {
				ev.Request = strings.TrimSpace(r.Method + " " + strings.SplitN(r.URL, "?", 2)[0])
			}
		}
	}

	if len(ev.Exceptions) == 0 && iss.Metadata.Type != "" {
		ev.Exceptions = []Exception{{Type: iss.Metadata.Type, Value: iss.Metadata.Value}}
	}

	if c := ev.Tags["commit"]; c != "" {
		s.Repository.Commit = c
	}

	if s.Title == "" {
		return Signal{}, fmt.Errorf("%w: the issue has no title", ErrIgnored)
	}

	s.Evidence.Sentry = ev

	return Normalize(s), nil
}

// contextLine picks the source line a frame points at from Sentry's [[lineno, "code"], …] context.
func contextLine(ctx []json.RawMessage, line int) string {
	for _, raw := range ctx {
		var pair []json.RawMessage

		if json.Unmarshal(raw, &pair) != nil || len(pair) != 2 {
			continue
		}

		var n int
		var code string

		if json.Unmarshal(pair[0], &n) == nil && n == line && json.Unmarshal(pair[1], &code) == nil {
			return code
		}
	}

	return ""
}

func releaseVersion(raw json.RawMessage) string {
	var r struct {
		Version string `json:"version"`
	}

	if json.Unmarshal(raw, &r) == nil && r.Version != "" {
		return r.Version
	}

	var s string

	json.Unmarshal(raw, &s)

	return s
}
