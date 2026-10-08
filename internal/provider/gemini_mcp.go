package provider

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/onoja123/csm/internal/mcp"
)

const geminiSettingsFile = "settings.json"

var geminiTransports = []mcp.Transport{mcp.Stdio, mcp.HTTP, mcp.SSE}

type geminiMCP struct{}

func (g Gemini) MCP() mcp.Adapter { return geminiMCP{} }

func (geminiMCP) Provider() string { return GeminiID }

func (geminiMCP) EnvByName() bool { return true }

func (geminiMCP) Validate(s mcp.Server) error {
	if err := mcp.ValidName(s.Name); err != nil {
		return err
	}

	return transportError("Gemini CLI", s.Transport, geminiTransports...)
}

func geminiSettingsPath(profileDir string) string {
	return filepath.Join(geminiDir(profileDir), geminiSettingsFile)
}

func (a geminiMCP) Parse(profileDir, projectDir string) ([]mcp.Server, error) {
	path := geminiSettingsPath(profileDir)
	settings, err := readJSONObject(path)

	if err != nil {
		return nil, err
	}

	defs, err := rawObject(settings[claudeMCPServersKey])

	if err != nil {
		return nil, fmt.Errorf("read %s: mcpServers: %w", path, err)
	}

	var servers []mcp.Server

	for _, name := range mcp.Names(defs) {
		s, def, err := portableFields(name, defs[name])

		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}

		switch {
		case def.Type == "http" || def.Type == "sse":
			s.Transport = mcp.Transport(def.Type)
		case def.Command != "":
			s.Transport = mcp.Stdio
		case def.HTTPURL != "":
			s.Transport = mcp.HTTP
		case def.URL != "":
			s.Transport = mcp.SSE
		default:
			return nil, fmt.Errorf("read %s: MCP server %q has neither command, httpUrl nor url", path, name)
		}

		s.Scope, s.Source, s.Provider = mcp.ScopeUser, path, GeminiID
		servers = append(servers, s)
	}

	return servers, nil
}

func (a geminiMCP) Prepare(profileDir, projectDir string, servers []mcp.Server) error {
	path := geminiSettingsPath(profileDir)
	settings, err := readJSONObject(path)

	if err != nil {
		return err
	}

	defs, err := rawObject(settings[claudeMCPServersKey])

	if err != nil {
		return err
	}

	add := map[string]json.RawMessage{}

	for _, s := range servers {
		if s.Provider == GeminiID && len(s.Native) > 0 {
			add[s.Name] = s.Native

			continue
		}

		add[s.Name] = geminiDefinition(s)
	}

	if !mergeServers(defs, add) {
		return nil
	}

	settings[claudeMCPServersKey] = marshalRaw(defs)

	return writeConfigFile(path, marshalIndent(settings))
}

// geminiDefinition writes what `gemini mcp add` writes; Gemini CLI expands $NAME in env values from its own environment, so no value is stored.
func geminiDefinition(s mcp.Server) json.RawMessage {
	def := map[string]any{}

	switch s.Transport {
	case mcp.Stdio:
		def["command"] = s.Command
		def["args"] = nonNil(s.Args)

		if len(s.EnvNames) > 0 {
			env := map[string]string{}

			for _, name := range s.EnvNames {
				env[name] = "$" + name
			}

			def["env"] = env
		}
	default:
		def["type"] = string(s.Transport)
		def["url"] = s.URL
	}

	return marshalRaw(def)
}
