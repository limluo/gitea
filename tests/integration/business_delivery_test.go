// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	actions_model "gitea.dev/models/actions"
	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	"github.com/PuerkitoBio/goquery"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gitea.dev/tests"
	"github.com/stretchr/testify/assert"
)

func TestBusinessDeliveryPages(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	admin := loginUser(t, "user1")
	for _, path := range []string{"/-/delivery", "/user2/repo1/delivery?tab=overview", "/user2/repo1/delivery?tab=branches", "/user2/repo1/delivery?tab=reviews&queue=expert", "/user2/repo1/delivery?tab=reviews&queue=done", "/user2/repo1/delivery?tab=reviews", "/user2/repo1/delivery?tab=users", "/user2/repo1/delivery?tab=agents", "/user2/repo1/delivery?tab=settings", "/user2/repo1/delivery?tab=environment"} {
		r := admin.MakeRequest(t, NewRequest(t, http.MethodGet, path), http.StatusOK)
		assert.Contains(t, r.Body.String(), "业务")
	}
	reader := loginUser(t, "user2")
	reader.MakeRequest(t, NewRequest(t, http.MethodGet, "/user2/repo1/delivery?tab=users"), http.StatusForbidden)
	reader.MakeRequest(t, NewRequestWithValues(t, "POST", "/user2/repo1/delivery", map[string]string{"op": "grant", "login": "user2", "branch": "*"}), http.StatusForbidden)
}

func TestBusinessReleaseAndRevoke(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, _ *url.URL) {
		admin := loginUser(t, "user1")
		path := "/user2/repo1/delivery"
		admin.MakeRequest(t, NewRequestWithValues(t, "POST", path, map[string]string{"op": "settings", "development": "branch2", "agent": "user2"}), 303)
		_, err := db.GetEngine(t.Context()).Insert(&git_model.ProtectedBranch{RepoID: 1, RuleName: "master", ProtectedBranchConfig: git_model.ProtectedBranchConfig{CanPush: true}})
		require.NoError(t, err)
		visit := func() *HTMLDoc {
			resp := admin.MakeRequest(t, NewRequest(t, "GET", path+"?tab=releases&branch=master"), 200)
			return NewHTMLParser(t, resp.Body)
		}
		page := visit()
		form := page.Find("tr").FilterFunction(func(_ int, s *goquery.Selection) bool { return strings.Contains(s.Text(), "a change") }).Find("form")
		require.Equal(t, 1, form.Length())
		sha := form.Find("[name=sha]").AttrOr("value", "")
		head := form.Find("[name=head]").AttrOr("value", "")
		require.NotEmpty(t, sha)
		require.NotEmpty(t, head)
		values := map[string]string{"op": "publish", "branch": "master", "head": head, "sha": sha}
		admin.MakeRequest(t, NewRequestWithValues(t, "POST", path, values), 303)
		page = visit()
		revoke := page.Find("button[value=revoke]").ParentsFiltered("form")
		require.Equal(t, 1, revoke.Length(), page.Find("main").Text())
		admin.MakeRequest(t, NewRequestWithValues(t, "POST", path, values), 409)
		values["op"] = "revoke"
		values["head"] = revoke.Find("[name=head]").AttrOr("value", "")
		admin.MakeRequest(t, NewRequestWithValues(t, "POST", path, values), 303)
		require.Equal(t, 0, visit().Find("button[value=revoke]").Length())
	})
}

func TestBusinessUATAcceptance(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, _ *url.URL) {
		admin := loginUser(t, "user1")
		path := "/user2/repo1/delivery"
		post := func(b map[string]string, status int) {
			admin.MakeRequest(t, NewRequestWithValues(t, "POST", path, b), status)
		}
		post(map[string]string{"op": "settings", "development": "branch2", "agent": "user2"}, 303)
		for _, b := range []string{"uat", "main"} {
			testCreateBranch(t, admin, "user2", "repo1", "branch/master", b, 303)
			_, err := db.GetEngine(t.Context()).Insert(&git_model.ProtectedBranch{RepoID: 1, RuleName: b, ProtectedBranchConfig: git_model.ProtectedBranchConfig{CanPush: true}})
			require.NoError(t, err)
		}
		form := func(b string) *goquery.Selection {
			resp := admin.MakeRequest(t, NewRequest(t, "GET", path+"?tab=releases&branch="+b), 200)
			s := NewHTMLParser(t, resp.Body).Find("tr").FilterFunction(func(_ int, row *goquery.Selection) bool { return strings.Contains(row.Text(), "a change") }).Find("form")
			require.Equal(t, 1, s.Length())
			return s
		}
		val := func(s *goquery.Selection, k string) string { return s.Find("[name="+k+"]").AttrOr("value", "") }
		f := form("main")
		sha := val(f, "sha")
		main := map[string]string{"op": "publish", "branch": "main", "sha": sha, "head": val(f, "head")}
		post(main, 409)
		u := form("uat")
		post(map[string]string{"op": "publish", "branch": "uat", "sha": sha, "head": val(u, "head")}, 303)
		u = form("uat")
		applied := val(u, "applied")
		require.NotEmpty(t, applied)
		accept := map[string]string{"op": "accept", "branch": "uat", "sha": sha, "applied": applied, "result": "验收通过", "note": "验收环境核对完成"}
		post(accept, 409)
		_, err := db.GetEngine(t.Context()).Insert(&actions_model.ActionRun{RepoID: 1, Index: 9999, WorkflowID: "deploy-uat.yml", Ref: "refs/heads/uat", Event: "push", Status: actions_model.StatusSuccess, CommitSHA: applied})
		require.NoError(t, err)
		post(accept, 303)
		post(main, 303)
		require.NotEqual(t, main["head"], val(form("main"), "head"))
		post(map[string]string{"op": "revoke", "branch": "uat", "sha": sha, "head": val(u, "head")}, 303)
		post(accept, 409)
	})
}

func TestBusinessToolGrant(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	admin := loginUser(t, "user1")
	path := "/user2/repo1/delivery"
	admin.MakeRequest(t, NewRequestWithValues(t, "POST", path, map[string]string{"op": "settings", "development": "master", "agent": "user2"}), 303)
	response := admin.MakeRequest(t, NewRequestWithValues(t, "POST", path, map[string]string{"op": "tool-grant", "agent": "user2", "service": "需求查询", "tools": "repository_info"}), 200)
	doc := NewHTMLParser(t, response.Body)
	key := doc.Find("p").FilterFunction(func(_ int, s *goquery.Selection) bool { return strings.Contains(s.Text(), "请立即复制") }).Next().Text()
	require.NotEmpty(t, key)
	call := func(method, tool string, status int) string {
		r := NewRequestWithJSON(t, "POST", "/-/delivery/mcp/1", map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": map[string]string{"name": tool}}).SetHeader("Authorization", "Bearer "+key)
		return MakeRequest(t, r, status).Body.String()
	}
	assert.Contains(t, call("tools/list", "", 200), "repository_info")
	assert.NotContains(t, call("tools/list", "", 200), "coding_instructions")
	assert.Contains(t, call("tools/call", "coding_instructions", 200), "error")
	assert.Contains(t, call("tools/call", "repository_info", 200), "repo1")
	id := doc.Find("[name=grant]").AttrOr("value", "")
	require.NotEmpty(t, id)
	admin.MakeRequest(t, NewRequestWithValues(t, "POST", path, map[string]string{"op": "tool-grant", "mode": "revoke", "grant": id}), 303)
	call("tools/list", "", 401)
}
