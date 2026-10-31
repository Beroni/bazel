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
// A key this process has to replace and that was also edited by hand is refused:
// `publishable: false` lives inside `agents`, and replacing the list would drop
// it without a word.
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
	c.wrote(data, base)
	c.base = base
	return nil
}

// wrote records what a save put in the file. Only the keys it changed: a hand
// edit in another key is still one, and that save did not look at it.
func (c *Config) wrote(data []byte, base map[string]string) {
	_, now, err := topLevel(data)
	if err != nil || c.disk == nil {
		c.disk = now
		return
	}
	for _, values := range []map[string]string{base, c.base} {
		for key := range values {
			if v, ok := now[key]; ok && c.base[key] != base[key] {
				c.disk[key] = v
			} else if !ok {
				delete(c.disk, key)
			}
		}
	}
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
	disk, now, err := topLevel(old)
	if err != nil {
		return nil, nil, fmt.Errorf("%s does not parse, and saving over it would lose it: %w", path, err)
	}
	for key := range c.base {
		if _, still := values[key]; !still && c.editedByHand(key, now) {
			return nil, nil, editedError(path, key)
		}
	}
	for key := range values {
		if c.base[key] != values[key] && c.editedByHand(key, now) {
			return nil, nil, editedError(path, key)
		}
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
	data, err := encodeYAML(disk)
	if err != nil {
		return nil, nil, err
	}
	// The last word before the file is replaced: whatever came out has to be a
	// config the next boot can load.
	if err := yaml.Unmarshal(data, Default()); err != nil {
		return nil, nil, fmt.Errorf("saving would break %s: %w", path, err)
	}
	return data, values, nil
}

// editedByHand says the file's key is no longer what this process last saw.
func (c *Config) editedByHand(key string, now map[string]string) bool {
	return c.disk != nil && c.disk[key] != now[key]
}

func editedError(path, key string) error {
	return fmt.Errorf("%q changed in %s since Bazel read it, and saving would undo that: restart Bazel to pick it up, then do this again", key, path)
}

func encodeYAML(doc *yaml.Node) ([]byte, error) {
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// keepAliases writes out, in place, the aliases that point inside a value about
// to go: an unknown key that said `*watched` keeps what it meant, and the file
// is not left with an alias to an anchor that no longer exists.
func keepAliases(root, gone *yaml.Node) {
	inside := map[*yaml.Node]bool{}
	var mark func(*yaml.Node)
	mark = func(n *yaml.Node) {
		inside[n] = true
		for _, c := range n.Content {
			mark(c)
		}
	}
	mark(gone)
	seen := map[*yaml.Node]bool{}
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		if seen[n] || inside[n] {
			return
		}
		seen[n] = true
		if n.Kind == yaml.AliasNode && inside[n.Alias] {
			*n = *clearAnchors(n.Alias)
			return
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(root)
}

// clearAnchors copies a value without its anchors: they stay with the original.
func clearAnchors(n *yaml.Node) *yaml.Node {
	out := *n
	out.Anchor = ""
	out.Content = make([]*yaml.Node, len(n.Content))
	for i, c := range n.Content {
		out.Content[i] = clearAnchors(c)
	}
	return &out
}

// setKey replaces a key's value, or adds the key, keeping the key's comments.
func setKey(root, key, value *yaml.Node) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key.Value {
			keepAliases(root, root.Content[i+1])
			root.Content[i+1] = value
			return
		}
	}
	root.Content = append(root.Content, key, value)
}

func dropKey(root *yaml.Node, key string) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			keepAliases(root, root.Content[i+1])
			root.Content = slices.Delete(root.Content, i, i+2)
			return
		}
	}
}

// writeAtomic puts data at path through a temporary file next to it. A path that
// is a link stays one: the rename goes to its target, or a config kept in a
// dotfiles repository would stop being updated without a word.
func writeAtomic(path string, data []byte) error {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
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
// `env:` go, and the names stay, so that it still says what an agent needs. So do
// all the comments, since `# GH_TOKEN: x` is a value nobody can tell from a note.
// It works on the text, not the struct, because this is the file the page shows.
func Redacted(data []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	found := redactEnv(doc.Content[0], map[*yaml.Node]bool{})
	found = dropComments(&doc, map[*yaml.Node]bool{}) || found
	if !found {
		return data, nil
	}
	return encodeYAML(&doc)
}

// dropComments clears the comments under n and says whether there were any.
func dropComments(n *yaml.Node, seen map[*yaml.Node]bool) bool {
	if seen[n] {
		return false
	}
	seen[n] = true
	found := n.HeadComment != "" || n.LineComment != "" || n.FootComment != ""
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	for _, c := range n.Content {
		found = dropComments(c, seen) || found
	}
	if n.Alias != nil {
		found = dropComments(n.Alias, seen) || found
	}
	return found
}

// redactEnv blanks the value of every env it finds and says whether it found
// any: with no env the file goes out as it is. An env can get its values from an
// alias or a merge, and they go where they are written.
func redactEnv(n *yaml.Node, seen map[*yaml.Node]bool) bool {
	if seen[n] {
		return false
	}
	seen[n] = true
	found := false
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "env" {
				found = blank(n.Content[i+1], map[*yaml.Node]bool{}) || found
				continue
			}
			found = redactEnv(n.Content[i+1], seen) || found
		}
		return found
	}
	for _, c := range n.Content {
		found = redactEnv(c, seen) || found
	}
	return found
}

// blank replaces every scalar under n, following aliases. Mapping keys stay, so
// that the file still says what an agent needs.
func blank(n *yaml.Node, seen map[*yaml.Node]bool) bool {
	if seen[n] {
		return false
	}
	seen[n] = true
	found := false
	switch n.Kind {
	case yaml.ScalarNode:
		n.Value, n.Tag, n.Style = "<redacted>", "!!str", yaml.DoubleQuotedStyle
		return true
	case yaml.AliasNode:
		return blank(n.Alias, seen)
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			found = blank(n.Content[i], seen) || found
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			found = blank(c, seen) || found
		}
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
