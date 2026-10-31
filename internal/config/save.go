package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

// Save writes the configuration back to config.yaml. The file is the user's as
// much as the page's: it may hold comments, keys this version does not know and
// edits made since Bazel read it. So only the top-level keys this process
// changed are replaced, on top of what is on disk now, and the rest of the file
// stays as it is. A value edited by hand survives a click on the page, and
// that includes a gate such as mcp_allow_publish. Comments inside a replaced
// value go with it.
//
// The file is replaced in one step: a full disk or a killed process leaves the
// old one whole, and a reader never sees half of it.
func (c *Config) Save() error {
	if err := c.validateFormats(); err != nil {
		return err
	}
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, base, err := c.merged(path)
	if err != nil {
		return err
	}
	if err := writeAtomic(path, data); err != nil {
		return err
	}
	c.base = base
	return nil
}

// topLevel is a config as yaml: its mapping node and each key's encoded value.
func topLevel(data []byte) (*yaml.Node, map[string]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("not a mapping")
	}
	root := doc.Content[0]
	values := make(map[string]string, len(root.Content)/2)
	for i := 0; i+1 < len(root.Content); i += 2 {
		enc, err := yaml.Marshal(root.Content[i+1])
		if err != nil {
			return nil, nil, err
		}
		values[root.Content[i].Value] = string(enc)
	}
	return &doc, values, nil
}

// snapshot is what c looks like key by key, the base of the next Save.
func (c *Config) snapshot() map[string]string {
	data, err := yaml.Marshal(c)
	if err != nil {
		return nil
	}
	_, values, err := topLevel(data)
	if err != nil {
		return nil
	}
	return values
}

// merged is the file to write: what is on disk with this process's changes on
// top, plus the new base. A file that is there and does not parse is not
// replaced: it may be someone's edit in progress.
func (c *Config) merged(path string) ([]byte, map[string]string, error) {
	fresh, err := yaml.Marshal(c)
	if err != nil {
		return nil, nil, err
	}
	ours, values, err := topLevel(fresh)
	if err != nil {
		return nil, nil, err
	}
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if len(bytes.TrimSpace(old)) == 0 {
		return fresh, values, nil
	}
	disk, _, err := topLevel(old)
	if err != nil {
		return nil, nil, fmt.Errorf("%s does not parse, and saving over it would lose it: %w", path, err)
	}
	root, ourRoot := disk.Content[0], ours.Content[0]
	root.Style &^= yaml.FlowStyle // a JSON-style file takes block values
	for i := 0; i+1 < len(ourRoot.Content); i += 2 {
		key := ourRoot.Content[i].Value
		if c.base[key] == values[key] {
			continue
		}
		setKey(root, ourRoot.Content[i], ourRoot.Content[i+1])
	}
	for key := range c.base {
		if _, still := values[key]; !still {
			dropKey(root, key)
		}
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(disk); err != nil {
		return nil, nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, nil, err
	}
	return out.Bytes(), values, nil
}

// setKey replaces a key's value, or adds the key, keeping the key's comments.
func setKey(root, key, value *yaml.Node) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key.Value {
			root.Content[i+1] = value
			return
		}
	}
	root.Content = append(root.Content, key, value)
}

func dropKey(root *yaml.Node, key string) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			root.Content = slices.Delete(root.Content, i, i+2)
			return
		}
	}
}

// writeAtomic puts data at path through a temporary file next to it.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	// CreateTemp's 0600 is what it should be: an agent's env may hold a token.
	return os.Rename(tmp.Name(), path)
}

// Redacted is config.yaml safe to show or hand over: the values under every
// `env:` go, and the names stay, so that it still says what an agent needs. It
// works on the text, not the struct, because this is the file the page shows.
func Redacted(data []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || !redactEnv(doc.Content[0]) {
		return data, nil
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// redactEnv blanks what it finds and says whether it found anything: with no
// env the file goes out as it is.
func redactEnv(n *yaml.Node) bool {
	found := false
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "env" && n.Content[i+1].Kind == yaml.MappingNode {
				env := n.Content[i+1]
				for j := 1; j < len(env.Content); j += 2 {
					env.Content[j] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "<redacted>"}
					found = true
				}
				continue
			}
			found = redactEnv(n.Content[i+1]) || found
		}
		return found
	}
	for _, c := range n.Content {
		found = redactEnv(c) || found
	}
	return found
}

// Copy is a config that shares no list with c: what an edit of the page can
// change, so that a failed save can put it back.
func (c *Config) Copy() Config {
	out := *c
	out.Repos = slices.Clone(c.Repos)
	out.Authors = slices.Clone(c.Authors)
	out.Agents = slices.Clone(c.Agents)
	out.Pipelines = slices.Clone(c.Pipelines)
	return out
}
