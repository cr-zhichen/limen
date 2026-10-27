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
		{"read credentials", Facts{"none", "no", "no", "simple"}, true, true},
		{"scratch report", Facts{"scratch", "no", "no", "simple"}, true, true},
		{"ordinary delete", Facts{"other", "no", "no", "simple"}, false, true},
		{"service restart", Facts{"none", "yes", "no", "simple"}, false, true},
		{"disk wipe", Facts{"other", "no", "yes", "simple"}, false, false},
		{"script without meaningful cues", Facts{"unknown", "unknown", "no", "simple"}, true, true},
		{"unknown executable effects", Facts{"unknown", "unknown", "unknown", "complex"}, true, true},
		{"unknown path", Facts{"unknown", "no", "unknown", "complex"}, true, true},
		{"known outside write with unknown state", Facts{"other", "unknown", "unknown", "complex"}, false, true},
		{"known state change with unknown files", Facts{"unknown", "yes", "unknown", "complex"}, false, true},
		{"known destruction with unknown other effects", Facts{"unknown", "unknown", "yes", "complex"}, false, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if Authorize(config.ReadOnly, tt.facts).Allowed != tt.ro || Authorize(config.ReadWrite, tt.facts).Allowed != tt.rw || !Authorize(config.Unrestricted, tt.facts).Allowed {
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
func TestReviewConfidenceAndHybridRecheck(t *testing.T) {
	read := Facts{"none", "no", "no", "simple"}
	write := Facts{"other", "no", "no", "simple"}
	wipe := Facts{"other", "no", "yes", "simple"}
	script := Facts{"unknown", "unknown", "unknown", "complex"}
	for _, tt := range []struct {
		name, mode   string
		facts        Facts
		p            float64
		llmFacts     Facts
		wantReviewer string
		allow        bool
	}{
		{"clear reject", "hybrid", wipe, .99, read, "jev", false},
		{"clear allow", "hybrid", read, .99, wipe, "jev", true},
		{"uncertain allow confirmed", "hybrid", read, .7, read, "llm", true},
		{"uncertain allow rejected", "hybrid", read, .7, wipe, "llm", false},
		{"uncertain write rejection corrected", "hybrid", write, .7, read, "llm", true},
		{"uncertain reject confirmed", "hybrid", wipe, .7, wipe, "llm", false},
		{"complex script remains unknown", "hybrid", script, .99, script, "llm", true},
		{"jev uncertain allow accepted", "jev", read, .7, read, "jev", true},
		{"jev unknown script accepted", "jev", script, .7, read, "jev", true},
		{"jev uncertain reject retained", "jev", wipe, .7, read, "jev", false},
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
				content, _ := json.Marshal(tt.llmFacts)
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
					"finish_reason": "stop", "message": map[string]string{"content": string(content)},
				}}})
			}))
			defer srv.Close()
			c := config.Defaults()
			c.Review.Mode = tt.mode
			c.Models.Jev.Endpoint = srv.URL + "/jev"
			c.Models.Jev.APIKey.Value = "key"
			c.Models.LLM.Endpoint = srv.URL + "/llm"
			c.Models.LLM.APIKey.Value = "key"
			d, e := (&Engine{Config: c}).Check(context.Background(), config.ReadOnly, "actual command")
			if e != nil || d.Allowed != tt.allow || d.Reviewer != tt.wantReviewer || llm != (tt.wantReviewer == "llm") {
				t.Fatal(d, e, llm)
			}
			if llm && (d.Facts == nil || *d.Facts != tt.llmFacts) {
				t.Fatal("final decision did not use LLM classification", d)
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
