// Package docs embeds this folder, so the panel reads the same documents
// the repository has, from the binary that runs: a page written apart
// would drift from these within months, which is what V1's did.
package docs

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
)

//go:embed *.md
var files embed.FS

// Doc is one document as listed.
type Doc struct {
	Name  string `json:"name"`
	Title string `json:"title"`
}

// List is every document, by name, with its first heading.
func List() []Doc {
	entries, _ := fs.ReadDir(files, ".")
	var out []Doc
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".md")
		data, _ := files.ReadFile(e.Name())
		title := name
		for _, l := range strings.Split(string(data), "\n") {
			if t, ok := strings.CutPrefix(l, "# "); ok {
				title = strings.TrimSpace(t)
				break
			}
		}
		out = append(out, Doc{Name: name, Title: title})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Read is one document's Markdown, by name without the extension.
func Read(name string) ([]byte, bool) {
	if strings.ContainsAny(name, "/\\.") {
		return nil, false
	}
	data, err := files.ReadFile(name + ".md")
	return data, err == nil
}
