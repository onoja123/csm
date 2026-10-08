package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/onoja123/csm/internal/mcp"
)

func MCPAdapter(p Provider) (mcp.Adapter, bool) {
	if m, ok := p.(interface{ MCP() mcp.Adapter }); ok {
		return m.MCP(), true
	}

	return nil, false
}

var errSymlinkedConfig = errors.New("is a symlink to your own agent settings; csm does not write through it")

// writeConfigFile replaces a configuration file atomically, keeping its mode, and refuses to write through a symlink.
func writeConfigFile(path string, data []byte) error {
	mode := os.FileMode(0o600)

	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s %w", path, errSymlinkedConfig)
		}

		mode = fi.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.csm-tmp")
	if err != nil {
		return err
	}

	_, err = tmp.Write(data)

	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}

	if err == nil {
		err = os.Chmod(tmp.Name(), mode)
	}

	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}

	if err != nil {
		os.Remove(tmp.Name())
	}

	return err
}

// readJSONObject reads a JSON file as a map so unknown keys survive a rewrite. A missing file is an empty object.
func readJSONObject(path string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)

	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}

	if err != nil {
		return nil, err
	}

	obj := map[string]json.RawMessage{}

	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return obj, nil
}

func rawObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	obj := map[string]json.RawMessage{}

	if len(raw) == 0 || string(raw) == "null" {
		return obj, nil
	}

	return obj, json.Unmarshal(raw, &obj)
}

func rawStrings(raw json.RawMessage) []string {
	var list []string

	json.Unmarshal(raw, &list)

	return list
}

func rawString(raw json.RawMessage) string {
	var s string

	json.Unmarshal(raw, &s)

	return s
}

func rawBool(raw json.RawMessage) (bool, bool) {
	var b bool

	if len(raw) == 0 || json.Unmarshal(raw, &b) != nil {
		return false, false
	}

	return b, true
}

// rawKeys returns the sorted keys of a JSON object, for env and header maps whose values must never be read.
func rawKeys(raw json.RawMessage) []string {
	obj, err := rawObject(raw)

	if err != nil {
		return nil
	}

	return mcp.Names(obj)
}

// jsonServer is the shape Claude Code, Copilot CLI and Gemini CLI share for one server; each adapter maps its own type names.
type jsonServer struct {
	Type    string          `json:"type,omitempty"`
	Command string          `json:"command,omitempty"`
	Args    []string        `json:"args,omitempty"`
	Env     json.RawMessage `json:"env,omitempty"`
	URL     string          `json:"url,omitempty"`
	HTTPURL string          `json:"httpUrl,omitempty"`
	Headers json.RawMessage `json:"headers,omitempty"`
}

func portableFields(name string, raw json.RawMessage) (mcp.Server, jsonServer, error) {
	var def jsonServer

	if err := json.Unmarshal(raw, &def); err != nil {
		return mcp.Server{}, def, fmt.Errorf("MCP server %q: %w", name, err)
	}

	s := mcp.Server{
		Name:        name,
		Command:     def.Command,
		Args:        slices.Clone(def.Args),
		URL:         def.URL,
		EnvNames:    rawKeys(def.Env),
		HeaderNames: rawKeys(def.Headers),
		Native:      slices.Clone(raw),
	}

	if s.URL == "" {
		s.URL = def.HTTPURL
	}

	s.EnvRefs = envRefsOf(s, def.Env, def.Headers)

	return s, def, nil
}

// envRefsOf finds ${VAR} references the agent expands at start; values are inspected in memory only and never kept.
func envRefsOf(s mcp.Server, env, headers json.RawMessage) []string {
	values := append([]string{s.Command, s.URL}, s.Args...)

	for _, raw := range []json.RawMessage{env, headers} {
		obj, err := rawObject(raw)

		if err != nil {
			continue
		}

		for _, v := range obj {
			values = append(values, rawString(v))
		}
	}

	refs := mcp.EnvRefs(values...)
	sort.Strings(refs)

	return refs
}

func transportError(name string, t mcp.Transport, supported ...mcp.Transport) error {
	if slices.Contains(supported, t) {
		return nil
	}

	return fmt.Errorf("%s transport is not supported by %s", t, name)
}

func mergeServers(servers map[string]json.RawMessage, add map[string]json.RawMessage) (changed bool) {
	for _, name := range mcp.Names(add) {
		if _, exists := servers[name]; exists {
			continue
		}

		servers[name] = add[name]
		changed = true
	}

	return changed
}

func marshalIndent(v any) []byte {
	data, _ := json.MarshalIndent(v, "", "  ")

	return append(data, '\n')
}
