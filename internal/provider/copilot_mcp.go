package provider

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/onoja123/csm/internal/mcp"
)

const copilotMCPFile = "mcp-config.json"

var copilotTransports = []mcp.Transport{mcp.Stdio, mcp.HTTP, mcp.SSE}

type copilotMCP struct{}

func (c Copilot) MCP() mcp.Adapter { return copilotMCP{} }

func (copilotMCP) Provider() string { return CopilotID }

func (copilotMCP) EnvByName() bool { return false }

func (copilotMCP) Validate(s mcp.Server) error {
	if err := mcp.ValidName(s.Name); err != nil {
		return err
	}

	return transportError("GitHub Copilot CLI", s.Transport, copilotTransports...)
}

func copilotMCPPath(profileDir string) string { return filepath.Join(profileDir, copilotMCPFile) }

func (a copilotMCP) Parse(profileDir, projectDir string) ([]mcp.Server, error) {
	path := copilotMCPPath(profileDir)
	config, err := readJSONObject(path)

	if err != nil {
		return nil, err
	}

	defs, err := rawObject(config[claudeMCPServersKey])

	if err != nil {
		return nil, fmt.Errorf("read %s: mcpServers: %w", path, err)
	}

	var servers []mcp.Server

	for _, name := range mcp.Names(defs) {
		s, def, err := portableFields(name, defs[name])

		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}

		switch def.Type {
		case "local", "stdio", "":
			s.Transport = mcp.Stdio
		default:
			s.Transport = mcp.Transport(def.Type)
		}

		if def.Type == "" && def.Command == "" && s.URL != "" {
			s.Transport = mcp.HTTP
		}

		s.Scope, s.Source, s.Provider = mcp.ScopeUser, path, CopilotID
		servers = append(servers, s)
	}

	return servers, nil
}

func (a copilotMCP) Prepare(profileDir, projectDir string, servers []mcp.Server) error {
	path := copilotMCPPath(profileDir)
	config, err := readJSONObject(path)

	if err != nil {
		return err
	}

	defs, err := rawObject(config[claudeMCPServersKey])

	if err != nil {
		return err
	}

	add := map[string]json.RawMessage{}

	for _, s := range servers {
		if s.Provider == CopilotID && len(s.Native) > 0 {
			add[s.Name] = s.Native

			continue
		}

		add[s.Name] = copilotDefinition(s)
	}

	if !mergeServers(defs, add) {
		return nil
	}

	config[claudeMCPServersKey] = marshalRaw(defs)

	return writeConfigFile(path, marshalIndent(config))
}

// copilotDefinition builds a Copilot CLI entry from the portable fields alone; every tool is allowed, as Copilot's own `/mcp add` does.
func copilotDefinition(s mcp.Server) json.RawMessage {
	def := map[string]any{"tools": []string{"*"}}

	if s.Transport == mcp.Stdio {
		def["type"] = "local"
		def["command"] = s.Command
		def["args"] = nonNil(s.Args)
	} else {
		def["type"] = string(s.Transport)
		def["url"] = s.URL
	}

	return marshalRaw(def)
}
