package relocate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnAppFolderSaysHowItRuns(t *testing.T) {
	for _, c := range []struct {
		files   map[string]string
		command []string
		want    string
		fails   bool
	}{
		{files: map[string]string{"Dockerfile": "FROM node:22\nCMD [\"node\",\"server.js\"]\n"}, want: `"runtime": "docker"`},
		{files: map[string]string{"package.json": `{"scripts":{"start":"node index.js"}}`}, want: `"npm"`},
		{files: map[string]string{"index.html": "<h1>hi</h1>"}, want: `"static"`},
		{files: map[string]string{"public/index.html": "<h1>hi</h1>"}, want: `"root": "public"`},
		{files: map[string]string{"main.py": "print(1)"}, command: []string{"python3", "main.py"}, want: `"python3"`},
		{files: map[string]string{"main.py": "print(1)"}, fails: true},
		{files: map[string]string{"Dockerfile": "FROM x\nCMD node server.js\n"}, fails: true},
	} {
		dir := t.TempDir()
		for name, body := range c.files {
			os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
			os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
		}
		s := &GenericSource{Dir: dir, Command: c.command}
		_, err := s.Detect("app.test")
		if c.fails {
			if err == nil {
				t.Errorf("%v: detected %s", c.files, s.manifest)
			}
			continue
		}
		if err != nil || !strings.Contains(string(s.manifest), c.want) {
			t.Errorf("%v: %v\n%s", c.files, err, s.manifest)
		}
	}
}
