// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package imagefactory

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
)

// RenderDockerfile renders the deterministic Dockerfile for a workspace
// image from frozen resolved values + a base. Pure function of
// (ResolvedValues, Base): identical inputs always render an identical
// Dockerfile byte-for-byte, so a content-addressed tag maps to exactly one
// reproducible build (design/0046 build-execution section).
//
// The Dockerfile only ever layers onto the operator-approved base
// (base.Ref()). It cannot reference an arbitrary image, and every layer is
// derived from operator-authored catalog values (no user free text) — this
// is the structural guard against the injection surface.
func RenderDockerfile(rv ResolvedValues, base Base) (string, error) {
	if err := ValidateResolved(rv); err != nil {
		return "", fmt.Errorf("render: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# schematic: base=%s@%s\n", base.Name, base.Version)
	fmt.Fprintf(&b, "FROM %s\n", base.Ref())
	b.WriteString("USER root\n\n")

	// Group by type so the output is deterministic and the Dockerfile is
	// readable: apt block, then mise block, then per-file block, then ENV.
	renderAptBlock(&b, rv)
	renderMiseBlock(&b, rv)
	renderFileBlock(&b, rv)

	b.WriteString("USER sandbox\n")
	b.WriteString("WORKDIR /workspace\n")
	b.WriteString(`ENTRYPOINT ["/usr/local/bin/entrypoint-opencode.sh"]` + "\n")
	return b.String(), nil
}

func renderAptBlock(b *strings.Builder, rv ResolvedValues) {
	var pkgs []string
	for _, v := range rv {
		if v.Type == ExtensionTypeApt {
			pkgs = append(pkgs, v.Value)
		}
	}
	if len(pkgs) == 0 {
		return
	}
	sort.Strings(pkgs)
	b.WriteString("RUN apt-get update && apt-get install -y --no-install-recommends \\\n")
	for _, p := range pkgs {
		fmt.Fprintf(b, "    %s \\\n", p)
	}
	b.WriteString("    && rm -rf /var/lib/apt/lists/*\n\n")
}

func renderMiseBlock(b *strings.Builder, rv ResolvedValues) {
	var tools []string
	for _, v := range rv {
		if v.Type == ExtensionTypeMise {
			tools = append(tools, v.Value)
		}
	}
	if len(tools) == 0 {
		return
	}
	sort.Strings(tools)
	b.WriteString("RUN set -eux; \\\n")
	fmt.Fprintf(b, "    for tool in %s; do \\\n", strings.Join(tools, " "))
	b.WriteString("        MISE_YES=1 MISE_GITHUB_ATTESTATIONS=1 mise install --system \"$tool\"; \\\n")
	b.WriteString("    done && mise reshim\n\n")
}

func renderFileBlock(b *strings.Builder, rv ResolvedValues) {
	// Sort by path so map-iteration randomness never affects output.
	type fileEntry struct {
		path string
		v    ResolvedValue
	}
	var files []fileEntry
	for _, v := range rv {
		if v.Type == ExtensionTypeFile && v.FileSpec != nil {
			files = append(files, fileEntry{path: v.FileSpec.Path, v: v})
		}
	}
	if len(files) == 0 {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	for _, f := range files {
		mode := f.v.FileSpec.Mode
		if mode == "" {
			mode = "0644"
		}
		enc := base64.StdEncoding.EncodeToString([]byte(f.v.Value))
		fmt.Fprintf(b, "RUN mkdir -p %q && printf %%s %q | base64 -d > %q && chmod %s %q\n",
			dirOf(f.v.FileSpec.Path), enc, f.v.FileSpec.Path, mode, f.v.FileSpec.Path)
	}
	b.WriteString("\n")
}

func dirOf(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx <= 0 {
		return "/"
	}
	return path[:idx]
}
