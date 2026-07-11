// helm-values-gen generates docs/reference/helm-values.md from
// helm/values.yaml by extracting YAML keys and their preceding comments.
//
// Usage from repo root:
//
//	go run ./cmd/helm-values-gen -w
//
// The generator replaces content between <!-- hv-gen:start --> and
// <!-- hv-gen:end --> markers. Everything outside is preserved.

package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

func main() {
	writeMode := len(os.Args) > 1 && os.Args[1] == "-w"

	src, err := os.ReadFile("helm/values.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "helm-values-gen: %v\n", err)
		os.Exit(1)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(src, &root); err != nil {
		fmt.Fprintf(os.Stderr, "helm-values-gen: parse error: %v\n", err)
		os.Exit(1)
	}

	table := generateTable(&root, "", "")

	target := "docs/reference/helm-values.md"
	content := ""
	if b, err := os.ReadFile(target); err == nil {
		content = string(b)
	}

	start := "<!-- hv-gen:start -->"
	end := "<!-- hv-gen:end -->"
	idxStart := strings.Index(content, start)
	idxEnd := strings.Index(content, end)

	if idxStart >= 0 && idxEnd >= 0 {
		out := content[:idxStart+len(start)] + "\n" + table + "\n" + content[idxEnd:]
		if writeMode {
			os.WriteFile(target, []byte(out), 0644)
		} else {
			fmt.Print(out)
		}
	} else {
		// No markers: emit full file with markers wrapping the table.
		if writeMode {
			fmt.Fprintf(os.Stderr, "helm-values-gen: no markers found in %s; add <!-- hv-gen:start --> and <!-- hv-gen:end -->\n", target)
			os.Exit(1)
		}
		fmt.Print(table)
	}
}

func generateTable(node *yaml.Node, prefix, section string) string {
	if node.Kind != yaml.DocumentNode && node.Kind != yaml.MappingNode {
		return ""
	}

	var b strings.Builder

	// Collect top-level mapping pairs.
	items := node.Content
	if node.Kind == yaml.DocumentNode && len(items) > 0 {
		items = items[0].Content
	}

	for i := 0; i+1 < len(items); i += 2 {
		keyNode := items[i]
		valNode := items[i+1]

		key := keyNode.Value
		comment := strings.TrimSpace(keyNode.HeadComment)
		if comment == "" {
			comment = strings.TrimSpace(keyNode.LineComment)
		}
		fullKey := key
		if prefix != "" {
			fullKey = prefix + "." + key
		}

		defaultVal := valueToString(valNode)
		typeStr := typeFromValue(valNode)

		// If this is a mapping with children (not a leaf), emit a section header.
		if valNode.Kind == yaml.MappingNode {
			sectionName := fullKey
			if comment != "" {
				b.WriteString(fmt.Sprintf("### `%s`\n\n", sectionName))
				b.WriteString(comment + "\n\n")
			} else {
				b.WriteString(fmt.Sprintf("### `%s`\n\n", sectionName))
			}
			b.WriteString("| Key | Type | Default | Description |\n")
			b.WriteString("|---|---|---|---|\n")
			// Render children.
			for j := 0; j+1 < len(valNode.Content); j += 2 {
				ck := valNode.Content[j]
				cv := valNode.Content[j+1]
				childKey := ck.Value
				childComment := strings.TrimSpace(ck.HeadComment)
				if childComment == "" {
					childComment = strings.TrimSpace(ck.LineComment)
				}
				b.WriteString(fmt.Sprintf("| `%s.%s` | %s | `%s` | %s |\n",
					fullKey, childKey, typeFromValue(cv), valueToString(cv),
					escapePipe(childComment)))
			}
			b.WriteString("\n")
			continue
		}

		// Leaf key: emit a single row.
		if b.Len() == 0 || !strings.Contains(b.String()[:min(b.Len(), 20)], "|---|---|") {
			b.WriteString("| Key | Type | Default | Description |\n")
			b.WriteString("|---|---|---|---|\n")
		}

		b.WriteString(fmt.Sprintf("| `%s` | %s | `%s` | %s |\n",
			fullKey, typeStr, defaultVal, escapePipe(comment)))
	}

	return b.String()
}

func typeFromValue(node *yaml.Node) string {
	switch node.Kind {
	case yaml.MappingNode:
		return "object"
	case yaml.SequenceNode:
		if len(node.Content) > 0 {
			return "[]" + typeFromValue(node.Content[0])
		}
		return "list"
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!bool":
			return "bool"
		case "!!int":
			return "int"
		case "!!float":
			return "float"
		case "!!null":
			return "string"
		}
		return "string"
	}
	return "any"
}

func valueToString(node *yaml.Node) string {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == "!!null" || node.Value == "" {
			return `""`
		}
		return node.Value
	case yaml.SequenceNode:
		var parts []string
		for _, c := range node.Content {
			parts = append(parts, c.Value)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case yaml.MappingNode:
		return "{}"
	default:
		return fmt.Sprintf("<%d>", node.Kind)
	}
}

func escapePipe(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
