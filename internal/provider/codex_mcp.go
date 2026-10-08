package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/onoja123/csm/internal/mcp"
)

const (
	codexConfigFile    = "config.toml"
	codexMCPServersKey = "mcp_servers"
)

var codexTransports = []mcp.Transport{mcp.Stdio, mcp.HTTP}

type codexMCP struct{}

func (c Codex) MCP() mcp.Adapter { return codexMCP{} }

func (codexMCP) Provider() string { return CodexID }

func (codexMCP) EnvByName() bool { return true }

func (codexMCP) Validate(s mcp.Server) error {
	if err := mcp.ValidName(s.Name); err != nil {
		return err
	}

	return transportError("Codex", s.Transport, codexTransports...)
}

func codexConfigPath(profileDir string) string { return filepath.Join(profileDir, codexConfigFile) }

func readCodexConfig(profileDir string) (tomlTable, error) {
	path := codexConfigPath(profileDir)
	data, err := os.ReadFile(path)

	if errors.Is(err, os.ErrNotExist) {
		return tomlTable{}, nil
	}

	if err != nil {
		return nil, err
	}

	config, err := parseTOML(string(data))

	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return config, nil
}

func (a codexMCP) Parse(profileDir, projectDir string) ([]mcp.Server, error) {
	config, err := readCodexConfig(profileDir)

	if err != nil {
		return nil, err
	}

	path := codexConfigPath(profileDir)
	tables, ok := config[codexMCPServersKey].(tomlTable)

	if config[codexMCPServersKey] != nil && !ok {
		return nil, fmt.Errorf("read %s: mcp_servers is not a table", path)
	}

	var servers []mcp.Server

	for _, name := range mcp.Names(tables) {
		table, ok := tables[name].(tomlTable)

		if !ok {
			return nil, fmt.Errorf("read %s: mcp_servers.%s is not a table", path, name)
		}

		s, err := codexServer(name, table)

		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}

		s.Scope, s.Source, s.Provider = mcp.ScopeUser, path, CodexID
		servers = append(servers, s)
	}

	return servers, nil
}

func codexServer(name string, table tomlTable) (mcp.Server, error) {
	s := mcp.Server{Name: name}
	s.Command, _ = table["command"].(string)
	s.URL, _ = table["url"].(string)

	if args, ok := table["args"].([]any); ok {
		for _, a := range args {
			str, ok := a.(string)

			if !ok {
				return s, fmt.Errorf("mcp_servers.%s.args must be strings", name)
			}

			s.Args = append(s.Args, str)
		}
	}

	switch {
	case s.URL != "":
		s.Transport = mcp.HTTP
	case s.Command != "":
		s.Transport = mcp.Stdio
	default:
		return s, fmt.Errorf("mcp_servers.%s has neither command nor url", name)
	}

	if env, ok := table["env"].(tomlTable); ok {
		s.EnvNames = mcp.Names(env)
	}

	if v, ok := table["bearer_token_env_var"].(string); ok && v != "" {
		s.EnvNames = append(s.EnvNames, v)
	}

	if names, ok := table["env_vars"].([]any); ok {
		for _, n := range names {
			if str, ok := n.(string); ok {
				s.EnvNames = append(s.EnvNames, str)
			}
		}
	}

	if headers, ok := table["http_headers"].(tomlTable); ok {
		s.HeaderNames = mcp.Names(headers)
	}

	if envHeaders, ok := table["env_http_headers"].(tomlTable); ok {
		for _, header := range mcp.Names(envHeaders) {
			s.HeaderNames = append(s.HeaderNames, header)

			if v, ok := envHeaders[header].(string); ok {
				s.EnvNames = append(s.EnvNames, v)
			}
		}
	}

	slices.Sort(s.EnvNames)
	s.EnvNames = slices.Compact(s.EnvNames)
	slices.Sort(s.HeaderNames)
	s.HeaderNames = slices.Compact(s.HeaderNames)

	if enabled, ok := table["enabled"].(bool); ok && !enabled {
		s.Disabled = true
	}

	native, err := json.Marshal(table)

	if err != nil {
		return s, err
	}

	s.Native = native

	return s, nil
}

// Prepare appends tables so the user's own config.toml text and comments survive byte for byte. Another agent's environment variables become env_vars, which Codex reads from its own environment.
func (a codexMCP) Prepare(profileDir, projectDir string, servers []mcp.Server) error {
	config, err := readCodexConfig(profileDir)

	if err != nil {
		return err
	}

	existing, _ := config[codexMCPServersKey].(tomlTable)
	path := codexConfigPath(profileDir)
	current, err := os.ReadFile(path)

	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	var b strings.Builder

	b.Write(current)

	if len(current) > 0 && !strings.HasSuffix(string(current), "\n") {
		b.WriteString("\n")
	}

	added := 0

	for _, s := range servers {
		if _, ok := existing[s.Name]; ok {
			continue
		}

		table, err := codexDefinition(s)

		if err != nil {
			return err
		}

		if b.Len() > 0 {
			b.WriteString("\n")
		}

		if err := writeTOMLTable(&b, []string{codexMCPServersKey, s.Name}, table); err != nil {
			return fmt.Errorf("mcp_servers.%s: %w", s.Name, err)
		}

		added++
	}

	if added == 0 {
		return nil
	}

	return writeConfigFile(path, []byte(b.String()))
}

// codexDefinition is the server's own table when it comes from Codex, otherwise the portable fields with no environment values.
func codexDefinition(s mcp.Server) (tomlTable, error) {
	if s.Provider == CodexID && len(s.Native) > 0 {
		table := tomlTable{}

		if err := json.Unmarshal(s.Native, &table); err != nil {
			return nil, err
		}

		return table, nil
	}

	table := tomlTable{}

	switch s.Transport {
	case mcp.Stdio:
		table["command"] = s.Command
		table["args"] = s.Args

		if len(s.EnvNames) > 0 {
			table["env_vars"] = s.EnvNames
		}
	case mcp.HTTP:
		table["url"] = s.URL
	default:
		return nil, transportError("Codex", s.Transport, codexTransports...)
	}

	if s.Disabled {
		table["enabled"] = false
	}

	return table, nil
}
