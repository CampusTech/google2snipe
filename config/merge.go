package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// yamlUnmarshal is a thin wrapper so test helpers can call it without importing
// gopkg.in/yaml.v3 directly.
func yamlUnmarshal(data []byte, v any) error { return yaml.Unmarshal(data, v) }

// MergeFieldMapping reads a YAML config file, merges new field mappings into
// sync.field_mapping, and writes it back. Entries with an empty Transform are
// written as bare-string YAML values; entries with a Transform get the
// {path, transform} object form. Comments are preserved. If the
// sync.field_mapping node does not exist it is created. For each db_column_name
// key, any existing entry is replaced (set/overwrite semantics).
func MergeFieldMapping(path string, entries map[string]FieldMappingEntry) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parsing config file: %w", err)
	}

	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("unexpected YAML structure")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("expected mapping at root")
	}

	syncNode := findOrCreateMapping(root, "sync")
	fmNode := findOrCreateMapping(syncNode, "field_mapping")

	// Build a lookup of existing keys so we can replace them.
	existing := make(map[string]int) // key -> index of value node in fmNode.Content
	for i := 0; i < len(fmNode.Content)-1; i += 2 {
		existing[fmNode.Content[i].Value] = i + 1
	}

	for dbCol, entry := range entries {
		if dbCol == "" {
			continue
		}
		var valNode *yaml.Node
		if entry.Transform == "" {
			valNode = &yaml.Node{Kind: yaml.ScalarNode, Value: entry.Path, Tag: "!!str"}
		} else {
			valNode = &yaml.Node{
				Kind: yaml.MappingNode, Tag: "!!map",
				Content: []*yaml.Node{
					{Kind: yaml.ScalarNode, Value: "path", Tag: "!!str"},
					{Kind: yaml.ScalarNode, Value: entry.Path, Tag: "!!str"},
					{Kind: yaml.ScalarNode, Value: "transform", Tag: "!!str"},
					{Kind: yaml.ScalarNode, Value: entry.Transform, Tag: "!!str"},
				},
			}
		}
		if idx, ok := existing[dbCol]; ok {
			// Replace existing value node in place.
			fmNode.Content[idx] = valNode
		} else {
			keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: dbCol, Tag: "!!str"}
			fmNode.Content = append(fmNode.Content, keyNode, valNode)
			// Update index in case the same key appears again in entries.
			existing[dbCol] = len(fmNode.Content) - 1
		}
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("writing config file: %w", err)
	}
	return nil
}

// findOrCreateMapping finds (or creates) a mapping child of parent identified
// by key. It returns the child mapping node.
func findOrCreateMapping(parent *yaml.Node, key string) *yaml.Node {
	for i := 0; i < len(parent.Content)-1; i += 2 {
		if parent.Content[i].Value == key {
			val := parent.Content[i+1]
			if val.Kind != yaml.MappingNode {
				val.Kind = yaml.MappingNode
				val.Tag = "!!map"
				val.Value = ""
				val.Content = nil
			}
			return val
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"}
	valNode := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	parent.Content = append(parent.Content, keyNode, valNode)
	return valNode
}
