package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// MergeLicenses writes (or replaces) the top-level `licenses:` block in the YAML
// file at path with the given config, preserving the rest of the file.
func MergeLicenses(path string, lic LicensesConfig) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	var root *yaml.Node
	if len(doc.Content) > 0 {
		root = doc.Content[0]
	} else {
		doc.Kind = yaml.DocumentNode
		root = &yaml.Node{Kind: yaml.MappingNode}
		doc.Content = []*yaml.Node{root}
	}
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("config root of %s is not a mapping", path)
	}
	var licNode yaml.Node
	if err := licNode.Encode(lic); err != nil {
		return err
	}
	found := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "licenses" {
			root.Content[i+1] = &licNode
			found = true
			break
		}
	}
	if !found {
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "licenses"}, &licNode)
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}
