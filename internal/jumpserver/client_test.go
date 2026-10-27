package jumpserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/cr-zhichen/limen/internal/config"
)

func TestAssetsPreservesMetadataAndResolvesGroupPaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("X-JMS-ORG") != rootOrg {
			t.Errorf("unexpected asset query: %s, org %s", r.Method, r.Header.Get("X-JMS-ORG"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.RequestURI() {
		case "/api/v1/perms/users/self/assets/?limit=100&offset=0":
			fmt.Fprint(w, `{"count":2,"results":[{
				"id":"a","name":"web","address":"10.0.0.1","org_id":"org-a","org_name":"Acme",
				"nodes":[{"id":"prod","name":"Web"},{"id":"shared","name":"Shared"}],
				"platform":{"id":33,"name":"Linux"},"category":{"value":"host","label":"Host"},
				"type":{"value":"linux","label":"Linux"},"is_active":true,
				"labels":[{"id":"label-1","name":"env","value":"production","color":"red"}],"comment":"primary"
			}]}`)
		case "/api/v1/perms/users/self/assets/?limit=100&offset=1":
			fmt.Fprint(w, `{"count":2,"results":[{
				"id":"b","name":"db","address":"10.0.0.2","org_id":"org-b","org_name":"Other",
				"nodes":[{"id":"stage","name":"Web"},{"id":"direct","name":"Direct"}],
				"platform":null,"category":{"value":"database","label":"Database"},
				"type":{"value":"mysql","label":"MySQL"},"is_active":false,"labels":[],"comment":""
			}]}`)
		case "/api/v1/perms/users/self/nodes/?limit=100&offset=0":
			fmt.Fprint(w, `{"count":3,"results":[{"id":"stage","name":"Web","full_value":"/Other/Staging/Web"}]}`)
		case "/api/v1/perms/users/self/nodes/?limit=100&offset=1":
			fmt.Fprint(w, `{"count":3,"results":[
				{"id":"shared","name":"Shared","full_value":"/Acme/Shared"},
				{"id":"prod","name":"Web","full_value":"/Acme/Production/Web"}
			]}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := New(&config.JumpServer{URL: srv.URL, AccessKeyID: config.Secret{Value: "id"}, AccessKeySecret: config.Secret{Value: "secret"}})
	assets, err := c.Assets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	output, err := json.Marshal(assets)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON := `[
		{
			"id":"a","name":"web","address":"10.0.0.1","org_id":"org-a","org_name":"Acme",
			"nodes":[{"id":"prod","name":"Web","full_value":"/Acme/Production/Web"},{"id":"shared","name":"Shared","full_value":"/Acme/Shared"}],
			"platform":{"id":33,"name":"Linux"},"category":{"value":"host","label":"Host"},
			"type":{"value":"linux","label":"Linux"},"is_active":true,
			"labels":[{"id":"label-1","name":"env","value":"production","color":"red"}],"comment":"primary"
		},
		{
			"id":"b","name":"db","address":"10.0.0.2","org_id":"org-b","org_name":"Other",
			"nodes":[{"id":"stage","name":"Web","full_value":"/Other/Staging/Web"},{"id":"direct","name":"Direct"}],
			"platform":null,"category":{"value":"database","label":"Database"},
			"type":{"value":"mysql","label":"MySQL"},"is_active":false,"labels":[],"comment":""
		}
	]`
	var got, want any
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("asset output lost or mismatched metadata: %s", output)
	}
}

func TestAssetsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/perms/users/self/assets/" {
			t.Errorf("unexpected request for empty asset list: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"count":0,"results":[]}`)
	}))
	defer srv.Close()
	c := New(&config.JumpServer{URL: srv.URL, AccessKeyID: config.Secret{Value: "id"}, AccessKeySecret: config.Secret{Value: "secret"}})
	assets, err := c.Assets(context.Background())
	if err != nil || assets == nil || len(assets) != 0 {
		t.Fatalf("expected empty asset array, got %v, %v", assets, err)
	}
}

func TestAssetsRejectsIncompleteGroupPages(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"request failed", http.StatusForbidden, ""},
		{"invalid response", http.StatusOK, `{"count":2,"results":[`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.RequestURI() {
				case "/api/v1/perms/users/self/assets/?limit=100&offset=0":
					fmt.Fprint(w, `{"count":1,"results":[{"id":"a","nodes":[{"id":"n","name":"Web"}]}]}`)
				case "/api/v1/perms/users/self/nodes/?limit=100&offset=0":
					fmt.Fprint(w, `{"count":2,"results":[{"id":"n","name":"Web","full_value":"/Acme/Web"}]}`)
				case "/api/v1/perms/users/self/nodes/?limit=100&offset=1":
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				default:
					t.Errorf("unexpected request: %s", r.URL.RequestURI())
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			c := New(&config.JumpServer{URL: srv.URL, AccessKeyID: config.Secret{Value: "id"}, AccessKeySecret: config.Secret{Value: "secret"}})
			assets, err := c.Assets(context.Background())
			if err == nil || assets != nil {
				t.Fatalf("returned incomplete asset metadata: %v, %v", assets, err)
			}
		})
	}
}

func TestAssetsPaginationSignatureAndToken(t *testing.T) {
	pages, posts := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		canonical := strings.ToLower(r.Method) + " " + r.URL.RequestURI()
		h := hmac.New(sha256.New, []byte("secret"))
		fmt.Fprintf(h, "(request-target): %s\naccept: application/json\ndate: %s", canonical, r.Header.Get("Date"))
		if !strings.Contains(r.Header.Get("Authorization"), base64.StdEncoding.EncodeToString(h.Sum(nil))) {
			t.Error("invalid request signature")
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/perms/users/self/assets/":
			pages++
			if r.URL.Query().Get("offset") == "0" {
				fmt.Fprint(w, `{"count":2,"results":[{"id":"a","org_id":"org-a"}]}`)
			} else {
				fmt.Fprint(w, `{"count":2,"results":[{"id":"b"}]}`)
			}
		case "/api/v1/perms/users/self/assets/a/":
			if r.Header.Get("X-JMS-ORG") != "org-a" {
				t.Error("organization not propagated")
			}
			fmt.Fprint(w, `{"id":"a","permed_accounts":[{"alias":"alias","username":"alice"}]}`)
		case "/api/v1/authentication/connection-token/":
			posts++
			var v map[string]any
			json.NewDecoder(r.Body).Decode(&v)
			if v["asset"] != "a" || v["account"] != "alias" || v["protocol"] != "ssh" || v["connect_method"] != "ssh_guide" {
				t.Errorf("wrong token request: %v", v)
			}
			if r.Header.Get("X-JMS-ORG") != "org-a" {
				t.Error("token has wrong org")
			}
			w.WriteHeader(201)
			fmt.Fprint(w, `{"id":"token","value":"one-time-password"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := New(&config.JumpServer{URL: srv.URL, AccessKeyID: config.Secret{Value: "id"}, AccessKeySecret: config.Secret{Value: "secret"}})
	a, e := c.Asset(context.Background(), "a")
	if e != nil {
		t.Fatal(e)
	}
	if pages != 2 || a.OrgID != "org-a" {
		t.Fatal("pagination/detail failed")
	}
	if _, e = c.Token(context.Background(), a, "alice"); e == nil {
		t.Fatal("username used instead of authorized alias")
	}
	token, e := c.Token(context.Background(), a, "alias")
	if e != nil || token.ID != "token" || posts != 1 {
		t.Fatal(token.ID, e, posts)
	}
}
func TestRedirectDoesNotForwardCredential(t *testing.T) {
	leaked := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer srv.Close()
	c := New(&config.JumpServer{URL: srv.URL, AccessKeyID: config.Secret{Value: "id"}, AccessKeySecret: config.Secret{Value: "secret"}})
	if _, e := c.Assets(context.Background()); e == nil || leaked {
		t.Fatal("redirect allowed")
	}
}
