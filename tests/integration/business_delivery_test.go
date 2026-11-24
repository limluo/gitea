// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
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
	for _, path := range []string{"/-/delivery", "/user2/repo1/delivery?tab=reviews", "/user2/repo1/delivery?tab=users", "/user2/repo1/delivery?tab=agents", "/user2/repo1/delivery?tab=settings", "/user2/repo1/delivery?tab=environment"} {
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
