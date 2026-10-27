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
	"strings"
	"testing"

	"github.com/cr-zhichen/limen/internal/config"
)

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
