package eshttp

import (
	"encoding/json"
	"net/http"
)

// Stubs serves the management probe endpoints that log shippers call before
// sending bulk data.
type Stubs struct {
	cfg Config
}

func NewStubs(cfg Config) (*Stubs, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Stubs{cfg: cfg}, nil
}

func (s *Stubs) XPackInfo(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]interface{}{
		"build": map[string]interface{}{
			"hash": "lynxdb",
			"date": "2025-01-01T00:00:00.000Z",
		},
		"license":  licensePayload(),
		"features": map[string]interface{}{},
	})
}

func (s *Stubs) XPackLicense(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]interface{}{
		"license": licensePayload(),
	})
}

func (s *Stubs) EmptyArray(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, []interface{}{})
}

func (s *Stubs) IndexTemplates(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]interface{}{
		"index_templates": []interface{}{},
	})
}

func (s *Stubs) Acknowledged(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]interface{}{
		"acknowledged": true,
	})
}

func (s *Stubs) NotFound(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusNotFound, map[string]interface{}{})
}

func (s *Stubs) NodesHTTP(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]interface{}{
		"cluster_name": s.cfg.ClusterName,
		"nodes": map[string]interface{}{
			"lynxdb-node": map[string]interface{}{
				"name":    "lynxdb",
				"version": s.cfg.AdvertisedVersion,
				"roles":   []interface{}{},
				"http": map[string]interface{}{
					"bound_address":               []string{"127.0.0.1:9200"},
					"publish_address":             "127.0.0.1:9200",
					"max_content_length_in_bytes": 100 * 1024 * 1024,
					"max_content_length":          "100mb",
				},
			},
		},
	})
}

func (s *Stubs) EmptyAliases(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]interface{}{})
}

func (s *Stubs) EmptyDataStreams(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]interface{}{
		"data_streams": []interface{}{},
	})
}

func licensePayload() map[string]interface{} {
	return map[string]interface{}{
		"status":                "active",
		"uid":                   "lynxdb-basic",
		"type":                  "basic",
		"mode":                  "basic",
		"issue_date_in_millis":  0,
		"expiry_date_in_millis": -1,
	}
}

func respond(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("X-Elastic-Product", "Elasticsearch")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
