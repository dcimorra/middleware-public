package vpn

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Routes implements Module. The keys are net/http patterns relative to the
// module's mount point (/api/v1/modules/vpn); the registry prefixes them.
func (v *VPN) Routes() map[string]http.Handler {
	// "GET /{$}" rather than "GET /": the latter is a prefix match in a
	// ServeMux and would swallow every unknown path with the status page.
	return map[string]http.Handler{
		"GET /{$}":             http.HandlerFunc(v.handleStatus),
		"POST /peers":          http.HandlerFunc(v.handleAddPeer),
		"DELETE /peers/{name}": http.HandlerFunc(v.handleRemovePeer),
		"POST /only":           http.HandlerFunc(v.handleOnly),
	}
}

// Handler mounts Routes on a mux, for the CLI and for tests; the registry
// does the same with its own prefix.
func (v *VPN) Handler() http.Handler {
	mux := http.NewServeMux()
	for pattern, h := range v.Routes() {
		mux.Handle(pattern, h)
	}
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrPeerNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrPeerExists):
		status = http.StatusConflict
	case errors.Is(err, ErrNotEnabled):
		status = http.StatusConflict
	case strings.Contains(err.Error(), "must match"), strings.Contains(err.Error(), "add a peer"):
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (v *VPN) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, v.Status(r.Context()))
}

func (v *VPN) handleAddPeer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"name\": \"...\"}"})
		return
	}
	text, err := v.AddPeer(r.Context(), body.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": strings.TrimSpace(body.Name), "config": text})
}

func (v *VPN) handleRemovePeer(w http.ResponseWriter, r *http.Request) {
	if err := v.RemovePeer(r.Context(), r.PathValue("name")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (v *VPN) handleOnly(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"enabled\": true|false}"})
		return
	}
	if err := v.SetOnly(r.Context(), body.Enabled); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "enabled": body.Enabled})
}
