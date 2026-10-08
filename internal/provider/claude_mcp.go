package provider

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/onoja123/csm/internal/mcp"
)

const (
	claudeConfigFile    = ".claude.json"
	claudeProjectMCP    = ".mcp.json"
	claudeMCPAuthCache  = "mcp-needs-auth-cache.json"
	claudeCredentials   = ".credentials.json"
	claudeEnabledKey    = "enabledMcpjsonServers"
	claudeDisabledKey   = "disabledMcpjsonServers"
	claudeTrustKey      = "hasTrustDialogAccepted"
	claudeMCPServersKey = "mcpServers"
)

var claudeTransports = []mcp.Transport{mcp.Stdio, mcp.HTTP, mcp.SSE, mcp.WebSocket}

// claudeMCP reads and writes the three places Claude Code keeps MCP servers: the user scope and the per-project local scope in <profile>/.claude.json, and the project scope in <cwd>/.mcp.json.
type claudeMCP struct{}

func (c Claude) MCP() mcp.Adapter { return claudeMCP{} }

func (claudeMCP) Provider() string { return ClaudeID }

func (claudeMCP) EnvByName() bool { return false }

func (claudeMCP) Validate(s mcp.Server) error {
	if err := mcp.ValidName(s.Name); err != nil {
		return err
	}

	if s.Provider == ClaudeID && len(s.Native) > 0 {
		return nil
	}

	return transportError("Claude Code", s.Transport, claudeTransports...)
}

func projectKeys(projectDir string) []string {
	keys := []string{projectDir}

	if real, err := filepath.EvalSymlinks(projectDir); err == nil && real != projectDir {
		keys = append(keys, real)
	}

	return keys
}

func (a claudeMCP) Parse(profileDir, projectDir string) ([]mcp.Server, error) {
	configPath := filepath.Join(profileDir, claudeConfigFile)
	config, err := readJSONObject(configPath)

	if err != nil {
		return nil, err
	}

	oauth := claudeOAuthServers(profileDir)
	var servers []mcp.Server

	add := func(scope, source string, defs map[string]json.RawMessage, approved, disabled []string) error {
		for _, name := range mcp.Names(defs) {
			s, err := claudeServer(name, defs[name])

			if err != nil {
				return fmt.Errorf("%s: %w", source, err)
			}

			s.Scope, s.Source, s.Provider = scope, source, ClaudeID
			s.Approved = slices.Contains(approved, name)
			s.Disabled = slices.Contains(disabled, name)
			s.OAuth = slices.Contains(oauth, name)
			servers = append(servers, s)
		}

		return nil
	}

	user, err := rawObject(config[claudeMCPServersKey])

	if err != nil {
		return nil, fmt.Errorf("read %s: mcpServers: %w", configPath, err)
	}

	if err := add(mcp.ScopeUser, configPath, user, nil, nil); err != nil {
		return nil, err
	}

	project, err := claudeProjectEntry(config, projectDir)

	if err != nil {
		return nil, fmt.Errorf("read %s: %w", configPath, err)
	}

	local, err := rawObject(project[claudeMCPServersKey])

	if err != nil {
		return nil, fmt.Errorf("read %s: project mcpServers: %w", configPath, err)
	}

	if err := add(mcp.ScopeLocal, configPath, local, nil, nil); err != nil {
		return nil, err
	}

	mcpPath := filepath.Join(projectDir, claudeProjectMCP)
	projectConfig, err := readJSONObject(mcpPath)

	if err != nil {
		return nil, err
	}

	shared, err := rawObject(projectConfig[claudeMCPServersKey])

	if err != nil {
		return nil, fmt.Errorf("read %s: mcpServers: %w", mcpPath, err)
	}

	return servers, add(mcp.ScopeProject, mcpPath, shared, rawStrings(project[claudeEnabledKey]), rawStrings(project[claudeDisabledKey]))
}

func claudeServer(name string, raw json.RawMessage) (mcp.Server, error) {
	s, def, err := portableFields(name, raw)

	if err != nil {
		return s, err
	}

	switch {
	case def.Type != "":
		s.Transport = mcp.Transport(def.Type)
	case def.Command == "" && def.URL != "":
		s.Transport = mcp.HTTP
	default:
		s.Transport = mcp.Stdio
	}

	return s, nil
}

// claudeProjectEntry returns the <profile>/.claude.json entry Claude Code keeps for a project, under whichever path spelling it used.
func claudeProjectEntry(config map[string]json.RawMessage, projectDir string) (map[string]json.RawMessage, error) {
	projects, err := rawObject(config["projects"])

	if err != nil {
		return nil, fmt.Errorf("projects: %w", err)
	}

	for _, key := range projectKeys(projectDir) {
		if raw, ok := projects[key]; ok {
			entry, err := rawObject(raw)

			if err != nil {
				return nil, fmt.Errorf("projects[%q]: %w", key, err)
			}

			return entry, nil
		}
	}

	return map[string]json.RawMessage{}, nil
}

// claudeOAuthServers names the servers this profile has logged in to, or still needs to; only names are read.
func claudeOAuthServers(profileDir string) []string {
	var names []string

	if cache, err := readJSONObject(filepath.Join(profileDir, claudeMCPAuthCache)); err == nil {
		names = append(names, mcp.Names(cache)...)
	}

	if creds, err := readJSONObject(filepath.Join(profileDir, claudeCredentials)); err == nil {
		if oauth, err := rawObject(creds["mcpOAuth"]); err == nil {
			for _, key := range mcp.Names(oauth) {
				name, _, _ := strings.Cut(key, "|")
				names = append(names, name)
			}
		}
	}

	slices.Sort(names)

	return slices.Compact(names)
}

// Project scope servers are shared through .mcp.json, so only their approval moves, and Claude Code only honours it in a profile that already trusts the directory; servers from other agents land at user scope without secrets.
func (a claudeMCP) Prepare(profileDir, projectDir string, servers []mcp.Server) error {
	configPath := filepath.Join(profileDir, claudeConfigFile)
	config, err := readJSONObject(configPath)

	if err != nil {
		return err
	}

	user, err := rawObject(config[claudeMCPServersKey])

	if err != nil {
		return err
	}

	projects, err := rawObject(config["projects"])

	if err != nil {
		return err
	}

	projectKey := projectDir

	for _, key := range projectKeys(projectDir) {
		if _, ok := projects[key]; ok {
			projectKey = key
		}
	}

	entry, err := rawObject(projects[projectKey])

	if err != nil {
		return err
	}

	local, err := rawObject(entry[claudeMCPServersKey])

	if err != nil {
		return err
	}

	addUser, addLocal := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	var enable, disable []string

	for _, s := range servers {
		switch {
		case s.Provider != ClaudeID || len(s.Native) == 0:
			addUser[s.Name] = claudeDefinition(s)

		case s.Scope == mcp.ScopeLocal:
			addLocal[s.Name] = s.Native

		case s.Scope == mcp.ScopeProject:
			switch {
			case s.Disabled:
				disable = append(disable, s.Name)
			case s.Approved:
				enable = append(enable, s.Name)
			}

		default:
			addUser[s.Name] = s.Native
		}
	}

	changed := mergeServers(user, addUser)

	if mergeServers(local, addLocal) {
		entry[claudeMCPServersKey] = marshalRaw(local)
		changed = true
	}

	trusted, _ := rawBool(entry[claudeTrustKey])

	if trusted && len(enable)+len(disable) > 0 && len(rawStrings(entry[claudeEnabledKey]))+len(rawStrings(entry[claudeDisabledKey])) == 0 {
		entry[claudeEnabledKey] = marshalRaw(nonNil(enable))
		entry[claudeDisabledKey] = marshalRaw(nonNil(disable))
		changed = true
	}

	if !changed {
		return nil
	}

	if len(user) > 0 {
		config[claudeMCPServersKey] = marshalRaw(user)
	}

	if len(entry) > 0 {
		projects[projectKey] = marshalRaw(entry)
		config["projects"] = marshalRaw(projects)
	}

	return writeConfigFile(configPath, marshalIndent(config))
}

func claudeDefinition(s mcp.Server) json.RawMessage {
	def := map[string]any{"type": string(s.Transport)}

	if s.Transport == mcp.Stdio {
		def["command"] = s.Command
		def["args"] = nonNil(s.Args)
	} else {
		def["url"] = s.URL
	}

	return marshalRaw(def)
}

func marshalRaw(v any) json.RawMessage {
	data, _ := json.Marshal(v)

	return data
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}

	return list
}
