package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestVerifiedUpdateAndTamperRejection(t *testing.T) {
	var archive bytes.Buffer
	name := "limen"
	if runtime.GOOS == "windows" {
		name += ".exe"
		z := zip.NewWriter(&archive)
		w, _ := z.Create(name)
		w.Write([]byte("new executable"))
		z.Close()
	} else {
		g := gzip.NewWriter(&archive)
		tw := tar.NewWriter(g)
		tw.WriteHeader(&tar.Header{Name: name, Size: 14, Mode: 0755})
		tw.Write([]byte("new executable"))
		tw.Close()
		g.Close()
	}
	for _, tampered := range []bool{false, true} {
		t.Run(fmt.Sprint(tampered), func(t *testing.T) {
			asset := archiveName("v0.2.0", runtime.GOOS, runtime.GOARCH)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/latest":
					fmt.Fprint(w, `{"tag_name":"v0.2.0","draft":false,"prerelease":false}`)
				case "/v0.2.0/SHA256SUMS":
					sum := sha256.Sum256(archive.Bytes())
					if tampered {
						sum[0] ^= 1
					}
					fmt.Fprintf(w, "%x  %s\n", sum, asset)
				default:
					w.Write(archive.Bytes())
				}
			}))
			defer srv.Close()
			c := New()
			c.API = srv.URL + "/latest"
			c.Downloads = srv.URL
			target := filepath.Join(t.TempDir(), name)
			os.WriteFile(target, []byte("old executable"), 0755)
			result, e := c.Check(context.Background(), "0.1.0")
			if e != nil || !result.Available {
				t.Fatal(result, e)
			}
			e = c.Install(context.Background(), result, target)
			if (e != nil) != tampered {
				t.Fatal(e)
			}
			got, _ := os.ReadFile(target)
			want := "new executable"
			if tampered {
				want = "old executable"
			}
			if string(got) != want {
				t.Fatal("unexpected installed content")
			}
		})
	}
}
func TestPrereleaseNotInstalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v1.0.0-rc.1","prerelease":true}`)
	}))
	defer srv.Close()
	c := New()
	c.API = srv.URL
	if _, e := c.Check(context.Background(), "0.1.0"); e == nil {
		t.Fatal("prerelease accepted")
	}
}
