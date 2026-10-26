package api

import (
	"testing"

	"github.com/dcimorra/middleware-public/internal/installer"
)

// The panel reads the same checks the command prints, each with its mark.
func TestTheDoctorAnswersInThePanel(t *testing.T) {
	f := newFixture(t).withToken()
	f.srv.RegisterDoctor(func() ([]installer.Check, bool) {
		return []installer.Check{{Name: "helper", OK: false, Detail: "unreachable"}, {Name: "disk", OK: true, Detail: "40 GB free"}}, false
	})
	resp, out := f.call("GET", "/api/v1/system/doctor", nil)
	if resp.StatusCode != 200 || out["ok"] != false {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	checks, _ := out["checks"].([]any)
	if len(checks) != 2 || checks[0].(map[string]any)["name"] != "helper" || checks[0].(map[string]any)["ok"] != false || checks[1].(map[string]any)["detail"] != "40 GB free" {
		t.Fatalf("checks: %v", checks)
	}
}
