package review

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cr-zhichen/limen/internal/config"
)

func TestPermissionBoundaries(t *testing.T) {
	cases := []struct {
		name   string
		facts  Facts
		ro, rw bool
	}{
		{"read credentials", Facts{"none", "no", "no", "no", "simple"}, true, true},
		{"scratch report", Facts{"scratch", "no", "no", "no", "simple"}, true, true},
		{"ordinary delete", Facts{"other", "no", "no", "no", "simple"}, false, true},
		{"service restart", Facts{"none", "yes", "no", "no", "simple"}, false, true},
		{"disk wipe", Facts{"other", "no", "yes", "no", "simple"}, false, false},
		{"missing script", Facts{"unknown", "unknown", "unknown", "yes", "complex"}, false, false},
		{"unknown path", Facts{"unknown", "no", "unknown", "no", "complex"}, false, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if Authorize(config.ReadOnly, tt.facts).Allowed != tt.ro || Authorize(config.ReadWrite, tt.facts).Allowed != tt.rw {
				t.Fatal("wrong authorization")
			}
		})
	}
}
func jevResponse(f Facts, p float64) any {
	b, _ := json.Marshal(f)
	var fields map[string]string
	json.Unmarshal(b, &fields)
	a := map[string]any{}
	for k, q := range questions() {
		prob := map[string]float64{}
		for v := range q.Criteria {
			prob[v] = (1 - p) / float64(len(q.Criteria)-1)
		}
		prob[fields[k]] = p
		a[k] = map[string]any{"choice": fields[k], "probabilities": prob}
	}
	return map[string]any{"answers": a}
}
func TestHybridRejectsWithoutEscalatingAndRechecksUncertainAllow(t *testing.T) {
	for _, tt := range []struct {
		name           string
		facts          Facts
		p              float64
		wantLLM, allow bool
	}{
		{"reject", Facts{"other", "no", "yes", "no", "simple"}, .6, false, false},
		{"clear", Facts{"none", "no", "no", "no", "simple"}, .99, false, true},
		{"uncertain", Facts{"scratch", "no", "no", "no", "simple"}, .7, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			llm := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var v map[string]any
				json.NewDecoder(r.Body).Decode(&v)
				if r.URL.Path == "/jev" {
					state := v["state"].(map[string]any)
					if len(state) != 1 || state["command"] != "actual command" {
						t.Error("non-command context sent")
					}
					json.NewEncoder(w).Encode(jevResponse(tt.facts, tt.p))
					return
				}
				llm = true
				fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"{\"file_scope\":\"scratch\",\"state_change\":\"no\",\"destructive\":\"no\",\"opaque\":\"no\",\"complexity\":\"complex\"}"}}]}`)
			}))
			defer srv.Close()
			c := config.Defaults()
			c.Review.Mode = "hybrid"
			c.Models.Jev.Endpoint = srv.URL + "/jev"
			c.Models.Jev.APIKey.Value = "key"
			c.Models.LLM.Endpoint = srv.URL + "/llm"
			c.Models.LLM.APIKey.Value = "key"
			d, e := (&Engine{Config: c}).Check(context.Background(), config.ReadOnly, "actual command")
			if e != nil || d.Allowed != tt.allow || llm != tt.wantLLM {
				t.Fatal(d, e, llm)
			}
		})
	}
}
func TestUnrestrictedDoesNotResolveModelKeys(t *testing.T) {
	c := config.Defaults()
	d, e := (&Engine{Config: c}).Check(context.Background(), config.Unrestricted, "anything")
	if e != nil || !d.Allowed {
		t.Fatal(d, e)
	}
}
func TestMalformedReviewFailsClosed(t *testing.T) {
	for _, response := range []string{`{}`, `{"answers":{"file_scope":{"choice":"none","probabilities":{"none":1}}}}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, response) }))
		c := config.Defaults()
		c.Review.Mode = "jev"
		c.Models.Jev.Endpoint = srv.URL
		c.Models.Jev.APIKey.Value = "key"
		d, e := (&Engine{Config: c}).Check(context.Background(), config.ReadOnly, "cat data")
		srv.Close()
		if e == nil || d.Allowed {
			t.Fatal("invalid response allowed")
		}
	}
}
