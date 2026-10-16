package skills

import (
	"bytes"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityHigh    Severity = "high"
)

const (
	InspectionClean = "clean"
	InspectionCheck = "review"
	InspectionRisky = "risky"
)

type Finding struct {
	Severity Severity `json:"severity"`
	File     string   `json:"file"`
	Line     int      `json:"line,omitempty"`
	Message  string   `json:"message"`
}

// Inspection is what a static read of the files found. It cannot prove a skill is safe, only point at what to read.
type Inspection struct {
	Status   string    `json:"status"`
	Findings []Finding `json:"findings,omitempty"`
	Scripts  []string  `json:"scripts,omitempty"`
	Binary   []string  `json:"binary,omitempty"`
}

type rule struct {
	severity Severity
	pattern  *regexp.Regexp
	message  string
	// quoted rules only note a match inside quotation marks, as when a skill warns about the phrase.
	quoted bool
}

func newRule(sev Severity, re *regexp.Regexp, msg string) rule {
	return rule{severity: sev, pattern: re, message: msg}
}

func quotedRule(sev Severity, re *regexp.Regexp, msg string) rule {
	return rule{severity: sev, pattern: re, message: msg, quoted: true}
}

var rules = []rule{
	newRule(SeverityHigh, regexp.MustCompile(`(?i)\b(curl|wget|iwr|invoke-webrequest)\b[^\n|]*\|\s*(sudo\s+)?(ba|z|da)?sh\b`), "downloads a script and pipes it into a shell"),
	newRule(SeverityHigh, regexp.MustCompile(`(?i)base64\s+(-d|--decode)\b[^\n]*\|\s*(ba|z|da)?sh\b|\beval\s*\(\s*(atob|Buffer\.from)\b`), "decodes hidden text and runs it"),
	newRule(SeverityHigh, regexp.MustCompile(`(?i)\b(curl|wget)\b[^\n]*(-d|--data|-F|--upload-file|-T)\b[^\n]*(\$\(?(cat|env|printenv)|~/\.|\.env\b|id_rsa|credentials)`), "sends local files or environment variables to a server"),
	quotedRule(SeverityHigh, regexp.MustCompile(`(?i)\b(ignore|disregard|forget)\s+(all\s+)?(previous|prior|above|earlier|other)\s+(instructions|rules|guidance)`), "tells the agent to ignore its other instructions"),
	newRule(SeverityHigh, regexp.MustCompile(`(?i)\b(hide|conceal)\s+(this|these|it|that)\b[^.\n]{0,40}\bfrom\s+the\s+user|\b(do not|don't|never)\s+(tell|inform)\s+the\s+user\s+about\s+(this|these)\s+(skill|instructions?)`), "tells the agent to hide something from the user"),
	newRule(SeverityHigh, regexp.MustCompile(`(?i)--dangerously-skip-permissions|--yolo\b|--approve-mcps\b|\bbypassPermissions\b|\bskip\s+(the\s+)?(permission|approval)\s+(prompts?|checks?)`), "asks to bypass the agent's permission checks"),
	newRule(SeverityHigh, regexp.MustCompile(`(?i)~/\.csm\b|\$CSM_HOME|\.csm/(config|accounts|settings)`), "touches csm's own settings or accounts"),
	newRule(SeverityWarning, regexp.MustCompile(`(?i)~/\.ssh\b|id_rsa|id_ed25519|\.aws/credentials|\.netrc\b|\.git-credentials|security\s+find-(generic|internet)-password|keychain`), "refers to credentials or keys"),
	newRule(SeverityWarning, regexp.MustCompile(`(?i)\brm\s+-[a-z]*r[a-z]*f?[a-z]*\s+(/|~|\$HOME)(\s|$)|git\s+push\s+(-f|--force)\b|git\s+reset\s+--hard\b`), "contains a destructive command"),
	newRule(SeverityInfo, regexp.MustCompile(`\b[A-Z][A-Z0-9_]*(_API_KEY|_TOKEN|_SECRET|_PASSWORD)\b`), "names a secret environment variable"),
	newRule(SeverityInfo, regexp.MustCompile(`(?i)\b(npm|pnpm|yarn)\s+(install|add|i)\s+-g\b|\bpip3?\s+install\b|\bbrew\s+install\b|\bgo\s+install\b|\bcargo\s+install\b`), "asks to install software"),
}

// secretPatterns match credentials by shape; findings name the kind and line, never the value.
var secretPatterns = []struct {
	kind     string
	severity Severity
	pattern  *regexp.Regexp
}{
	{"private key", SeverityHigh, regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`)},
	{"AWS access key", SeverityHigh, regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"GitHub token", SeverityHigh, regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{60,})\b`)},
	{"Anthropic API key", SeverityHigh, regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{"OpenAI API key", SeverityHigh, regexp.MustCompile(`\bsk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{32,}`)},
	{"Google API key", SeverityHigh, regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"Slack token", SeverityHigh, regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{"Stripe secret key", SeverityHigh, regexp.MustCompile(`\b(?:sk|rk)_live_[A-Za-z0-9]{20,}`)},
	{"secret assigned in the text", SeverityWarning, regexp.MustCompile(`(?i)\b(?:api[_-]?key|secret|token|passwd|password)["']?\s*[:=]\s*["']([A-Za-z0-9+/_\-.]{20,})["']`)},
}

var placeholderWords = regexp.MustCompile(`(?i)your|example|placeholder|changeme|dummy|sample|xxxx|redacted|test|fake|replace|here`)

// realSecret filters the generic assignment pattern down to values that look random rather than descriptive.
func realSecret(kind, m string, sub []string) bool {
	if kind != "secret assigned in the text" {
		return true
	}

	v := sub[1]

	return !placeholderWords.MatchString(v) && strings.IndexAny(v, "0123456789") >= 0 && strings.ContainsAny(strings.ToLower(v), "abcdefghijklmnopqrstuvwxyz")
}

// RedactSecrets hides credential-shaped strings in text csm shows or stores.
func RedactSecrets(text string) string {
	for _, sp := range secretPatterns {
		text = sp.pattern.ReplaceAllStringFunc(text, func(m string) string {
			sub := sp.pattern.FindStringSubmatch(m)

			if !realSecret(sp.kind, m, sub) {
				return m
			}

			if len(sub) > 1 {
				return strings.Replace(m, sub[1], "[redacted]", 1)
			}

			return "[redacted " + sp.kind + "]"
		})
	}

	return text
}

// hiddenRune reports characters that render as nothing or reorder text, which can hide instructions from a reader.
func hiddenRune(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
		return true
	case r >= 0x200B && r <= 0x200F, r == 0x2060, r == 0x2062, r == 0x2063, r == 0x2064, r == 0xFEFF:
		return true
	case r >= 0xE0000 && r <= 0xE007F:
		return true
	}

	return false
}

var hiddenComment = regexp.MustCompile(`(?is)<!--(.*?)-->`)

var directiveWords = regexp.MustCompile(`(?i)\b(ignore|disregard|instead|you must|always|never|do not|don't|system prompt|assistant|run|execute|curl|send|upload)\b`)

var scriptExt = map[string]bool{".sh": true, ".bash": true, ".zsh": true, ".fish": true, ".py": true, ".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".rb": true, ".pl": true, ".php": true, ".ps1": true, ".bat": true, ".cmd": true, ".exe": true}

// Inspect reads every file of a skill without running anything.
func Inspect(s *Skill, contents map[string][]byte) Inspection {
	in := Inspection{Status: InspectionClean}

	for _, f := range s.Files {
		data := contents[f.Path]

		if f.Executable || scriptExt[strings.ToLower(path.Ext(f.Path))] {
			in.Scripts = append(in.Scripts, f.Path)
		}

		if !isText(data) {
			in.Binary = append(in.Binary, f.Path)

			continue
		}

		text := string(data)

		for _, sp := range secretPatterns {
			for _, loc := range sp.pattern.FindAllStringSubmatchIndex(text, -1) {
				sub := make([]string, len(loc)/2)

				for k := range sub {
					if loc[2*k] >= 0 {
						sub[k] = text[loc[2*k]:loc[2*k+1]]
					}
				}

				if realSecret(sp.kind, sub[0], sub) {
					in.Findings = append(in.Findings, Finding{Severity: sp.severity, File: f.Path, Line: lineAt(text, loc[0]), Message: "contains what looks like a credential (" + sp.kind + "; value hidden)"})

					break
				}
			}
		}

		for i, r := range text {
			if hiddenRune(r) && !(r == 0xFEFF && i == 0) {
				in.Findings = append(in.Findings, Finding{Severity: SeverityHigh, File: f.Path, Line: lineAt(text, i), Message: fmt.Sprintf("contains invisible or reordering characters (U+%04X) that can hide text from a reader", r)})

				break
			}
		}

		for _, m := range hiddenComment.FindAllStringSubmatchIndex(text, -1) {
			if directiveWords.MatchString(text[m[2]:m[3]]) {
				in.Findings = append(in.Findings, Finding{Severity: SeverityWarning, File: f.Path, Line: lineAt(text, m[0]), Message: "an HTML comment, invisible when rendered, holds instructions"})

				break
			}
		}

		for _, r := range rules {
			first, count, quotedOnly := 0, 0, true

			for i, line := range strings.Split(string(data), "\n") {
				for _, loc := range r.pattern.FindAllStringIndex(line, -1) {
					if count++; first == 0 {
						first = i + 1
					}

					if !r.quoted || !insideQuotes(line, loc[0]) {
						quotedOnly = false
					}
				}
			}

			if count == 0 {
				continue
			}

			sev, msg := r.severity, r.message

			if r.quoted && quotedOnly {
				sev, msg = SeverityInfo, "quotes a phrase used in prompt injection: "+r.message
			}

			if count > 1 {
				msg = fmt.Sprintf("%s (%d places)", msg, count)
			}

			in.Findings = append(in.Findings, Finding{Severity: sev, File: f.Path, Line: first, Message: msg})
		}
	}

	in.Findings = append(in.Findings, executableFindings(s, contents)...)

	if len(in.Scripts) > 0 {
		in.Findings = append(in.Findings, Finding{Severity: SeverityInfo, File: strings.Join(in.Scripts, ", "), Message: fmt.Sprintf("contains %d script(s); csm never runs them, an agent may, under its own approval rules", len(in.Scripts))})
	}

	if len(in.Binary) > 0 {
		in.Findings = append(in.Findings, Finding{Severity: SeverityWarning, File: strings.Join(in.Binary, ", "), Message: "contains binary files that cannot be read before use"})
	}

	for _, f := range in.Findings {
		switch {
		case f.Severity == SeverityHigh:
			in.Status = InspectionRisky
		case f.Severity == SeverityWarning && in.Status == InspectionClean:
			in.Status = InspectionCheck
		}
	}

	return in
}

// insideQuotes reports whether the text before offset opens a quotation that is still open.
func insideQuotes(line string, offset int) bool {
	before := line[:offset]

	if strings.Count(before, "\u201c") > strings.Count(before, "\u201d") {
		return true
	}

	for _, q := range []string{`"`, "`", "'"} {
		if strings.Count(before, q)%2 == 1 {
			return true
		}
	}

	return false
}

// executableFindings name what makes an agent run something on its own once the skill is loaded.
func executableFindings(s *Skill, contents map[string][]byte) []Finding {
	var out []Finding

	if _, ok := s.Extra["hooks"]; ok {
		out = append(out, Finding{Severity: SeverityWarning, File: ManifestFile, Message: "defines hooks, commands Claude Code runs on its own while the skill is active; csm leaves them out until you approve them and never carries them in a handoff"})
	}

	if text := string(contents[ManifestFile]); preprocessExpr.MatchString(s.Body) {
		line := 0

		if loc := preprocessExpr.FindStringIndex(text); loc != nil {
			line = lineAt(text, loc[0])
		}

		out = append(out, Finding{Severity: SeverityWarning, File: ManifestFile, Line: line, Message: "has !`command` lines Claude Code runs as soon as it loads the skill; csm never carries such a skill to Claude Code in a handoff"})
	}

	if len(s.AllowedTools) > 0 {
		out = append(out, Finding{Severity: SeverityInfo, File: ManifestFile, Message: "asks to use tools without prompting: " + strings.Join(s.AllowedTools, ", ") + "; csm removes this until you approve it"})
	}

	if data, ok := contents["agents/openai.yaml"]; ok && strings.Contains(string(data), "dependencies") {
		out = append(out, Finding{Severity: SeverityInfo, File: "agents/openai.yaml", Message: "declares tool dependencies that Codex may offer to install"})
	}

	return out
}

func lineAt(text string, offset int) int { return strings.Count(text[:offset], "\n") + 1 }

func isText(data []byte) bool {
	if bytes.IndexByte(data, 0) >= 0 {
		return false
	}

	return utf8.Valid(data)
}

// Summary is one line for a terminal or a list.
func (in Inspection) Summary() string {
	counts := map[Severity]int{}

	for _, f := range in.Findings {
		counts[f.Severity]++
	}

	switch in.Status {
	case InspectionRisky:
		return fmt.Sprintf("risky: %d serious finding(s)", counts[SeverityHigh])
	case InspectionCheck:
		return fmt.Sprintf("read before use: %d warning(s)", counts[SeverityWarning])
	default:
		if counts[SeverityInfo] > 0 {
			return fmt.Sprintf("no warnings; %d note(s)", counts[SeverityInfo])
		}

		return "no findings"
	}
}

const ScanLimitation = "A static read cannot prove a skill is safe: it only points at instructions and files worth reading before you install."
