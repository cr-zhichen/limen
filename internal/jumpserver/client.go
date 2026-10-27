package jumpserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cr-zhichen/limen/internal/config"
)

const rootOrg = "00000000-0000-0000-0000-000000000002"
const assetsPath = "/api/v1/perms/users/self/assets/"

type Client struct {
	Config *config.JumpServer
	HTTP   *http.Client
}
type Account struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
	Alias    string `json:"alias"`
}
type Asset struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Address  string    `json:"address"`
	OrgID    string    `json:"org_id"`
	OrgName  string    `json:"org_name"`
	Nodes    []Node    `json:"nodes"`
	Platform *Platform `json:"platform"`
	Category Choice    `json:"category"`
	Type     Choice    `json:"type"`
	IsActive bool      `json:"is_active"`
	Labels   []Label   `json:"labels"`
	Comment  string    `json:"comment"`
	Accounts []Account `json:"permed_accounts,omitempty"`
}
type Node struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	FullValue string `json:"full_value,omitempty"`
}
type Platform struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}
type Choice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}
type Label struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Value string `json:"value"`
	Color string `json:"color"`
}
type Token struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

func New(c *config.JumpServer) *Client {
	return &Client{Config: c, HTTP: &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Client) request(ctx context.Context, method, path, org string, payload, out any) error {
	id, e := c.Config.AccessKeyID.Resolve(ctx)
	if e != nil {
		return e
	}
	secret, e := c.Config.AccessKeySecret.Resolve(ctx)
	if e != nil {
		return e
	}
	var body []byte
	if payload != nil {
		body, e = json.Marshal(payload)
		if e != nil {
			return e
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Config.URL, "/")+path, bytes.NewReader(body))
	if e != nil {
		return e
	}
	date := time.Now().UTC().Format(http.TimeFormat)
	signed := "(request-target): " + strings.ToLower(method) + " " + req.URL.RequestURI() + "\naccept: application/json\ndate: " + date
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signed))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Date", date)
	req.Header.Set("Content-Type", "application/json")
	if org == "" {
		org = rootOrg
	}
	req.Header.Set("X-JMS-ORG", org)
	req.Header.Set("Authorization", fmt.Sprintf(`Signature keyId=%q,algorithm="hmac-sha256",headers="(request-target) accept date",signature=%q`, id, base64.StdEncoding.EncodeToString(mac.Sum(nil))))
	resp, e := c.HTTP.Do(req)
	if e != nil {
		return fmt.Errorf("无法连接 JumpServer：%w", e)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("JumpServer 请求失败（HTTP %d），请检查 Access Key 和资产授权", resp.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if e != nil {
		return e
	}
	if e = json.Unmarshal(b, out); e != nil {
		return fmt.Errorf("JumpServer 返回的数据无法读取：%w", e)
	}
	return nil
}
func (c *Client) Assets(ctx context.Context) ([]Asset, error) {
	assets, e := list[Asset](ctx, c, assetsPath)
	if e != nil || len(assets) == 0 {
		return assets, e
	}
	nodes, e := list[Node](ctx, c, "/api/v1/perms/users/self/nodes/")
	if e != nil {
		return nil, e
	}
	paths := make(map[string]string, len(nodes))
	for _, node := range nodes {
		paths[node.ID] = node.FullValue
	}
	for i := range assets {
		for j := range assets[i].Nodes {
			node := &assets[i].Nodes[j]
			node.FullValue = paths[node.ID]
		}
	}
	return assets, nil
}
func list[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	all := []T{}
	for offset := 0; ; {
		var page struct {
			Count   int `json:"count"`
			Results []T `json:"results"`
		}
		if e := c.request(ctx, "GET", path+"?limit=100&offset="+strconv.Itoa(offset), "", nil, &page); e != nil {
			return nil, e
		}
		all = append(all, page.Results...)
		offset += len(page.Results)
		if len(page.Results) == 0 || offset >= page.Count {
			return all, nil
		}
	}
}
func (c *Client) Asset(ctx context.Context, id string) (Asset, error) {
	assets, e := list[Asset](ctx, c, assetsPath)
	if e != nil {
		return Asset{}, e
	}
	for _, a := range assets {
		if a.ID == id {
			var detail Asset
			e = c.request(ctx, "GET", assetsPath+url.PathEscape(id)+"/", a.OrgID, nil, &detail)
			if detail.OrgID == "" {
				detail.OrgID = a.OrgID
			}
			return detail, e
		}
	}
	return Asset{}, fmt.Errorf("未找到已授权资产 %s，请先运行 limen assets", id)
}
func (c *Client) Token(ctx context.Context, a Asset, account string) (Token, error) {
	found := false
	for _, v := range a.Accounts {
		if v.Alias == account {
			found = true
		}
	}
	if !found {
		return Token{}, fmt.Errorf("该账号不在资产授权列表中，请运行 limen accounts %s 并使用 alias", a.ID)
	}
	payload := map[string]any{"asset": a.ID, "account": account, "protocol": "ssh", "connect_method": "ssh_guide", "connect_options": map[string]any{}}
	var t Token
	e := c.request(ctx, "POST", "/api/v1/authentication/connection-token/", a.OrgID, payload, &t)
	if e == nil && (t.ID == "" || t.Value == "") {
		e = fmt.Errorf("JumpServer 未返回有效连接凭据，请检查资产连接权限")
	}
	return t, e
}
