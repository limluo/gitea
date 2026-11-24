// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	access_model "gitea.dev/models/perm/access"
	"gitea.dev/modules/secret"
	"gitea.dev/modules/setting"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	actions_model "gitea.dev/models/actions"
	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/services/context"
	issue_service "gitea.dev/services/issue"
	pull_service "gitea.dev/services/pull"
	"gitea.dev/services/repository/files"
)

type deliveryContext struct {
	settings repo_model.DeliverySettings
	grants   []repo_model.DeliveryGrant
	admin    bool
}

func loadDelivery(ctx *context.Context) (*deliveryContext, error) {
	d := &deliveryContext{admin: ctx.Doer.IsAdmin, settings: repo_model.DeliverySettings{RepoID: ctx.Repo.Repository.ID}}
	found, err := db.GetEngine(ctx).ID(d.settings.RepoID).Get(&d.settings)
	if err != nil {
		return nil, err
	}
	if !found {
		d.settings.Development = ctx.Repo.Repository.DefaultBranch
	}
	err = db.GetEngine(ctx).Where("repo_id=? AND user_id=?", d.settings.RepoID, ctx.Doer.ID).Find(&d.grants)
	return d, err
}
func (d *deliveryContext) allowed(branch, button string) bool {
	if d.admin {
		return true
	}
	for _, g := range d.grants {
		if (g.Branch == "*" || g.Branch == branch) && slices.Contains(strings.Fields(g.Buttons), button) {
			return true
		}
	}
	return false
}

type deliveryChange struct {
	SHA, Title, Applied string
	Common              bool
}
type deliveryPull struct {
	Number      int64
	Title, Head string
}

func deliveryLedger(ctx *context.Context, head string) (map[string]string, error) {
	raw, _, err := gitcmd.NewCommand("log", "--first-parent", "--format=%H%x09%s", "--grep=^Delivery-Source: ").AddDynamicArguments(head).WithRepo(ctx.Repo.GitRepo).RunStdString(ctx)
	states := map[string]string{}
	for line := range strings.SplitSeq(raw, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		if _, ok := states[f[2]]; ok {
			continue
		}
		switch f[1] {
		case "Publish":
			states[f[2]] = f[0]
		case "Revoke":
			states[f[2]] = ""
		}
	}
	return states, err
}
func Delivery(ctx *context.Context) {
	d, err := loadDelivery(ctx)
	if err != nil {
		ctx.ServerErrorAuto(err)
		return
	}
	branch := ctx.FormString("branch")
	rules, err := git_model.FindRepoProtectedBranchRules(ctx, d.settings.RepoID)
	if err != nil {
		ctx.ServerErrorAuto(err)
		return
	}
	names, _, err := ctx.Repo.GitRepo.GetBranchNames(ctx, 0, 0)
	if err != nil {
		ctx.ServerErrorAuto(err)
		return
	}
	var targets []string
	for _, n := range names {
		if n != d.settings.Development && rules.GetFirstMatched(n) != nil && d.allowed(n, "release-view") {
			targets = append(targets, n)
		}
	}
	if !slices.Contains(targets, branch) {
		branch = ""
		if len(targets) > 0 {
			branch = targets[0]
		}
	}
	canReview := d.allowed(d.settings.Development, "review-view") && ctx.Repo.Permission.CanRead(unit.TypePullRequests)
	if !canReview && len(targets) == 0 && !d.admin {
		ctx.HTTPError(403)
		return
	}
	tab := ctx.FormString("tab")
	if tab == "" {
		switch {
		case canReview:
			tab = "reviews"
		case len(targets) > 0:
			tab = "releases"
		default:
			tab = "settings"
		}
	}
	ctx.Data["Title"] = "业务交付"
	ctx.Data["PageIsDelivery"] = true
	ctx.Data["DeliveryAdmin"] = d.admin
	ctx.Data["DeliverySettings"] = d.settings
	ctx.Data["DeliveryTab"] = tab
	ctx.Data["MCPURL"] = setting.AppURL + "-/delivery/mcp/" + strconv.FormatInt(d.settings.RepoID, 10)
	ctx.Data["CanReview"] = canReview
	ctx.Data["Targets"] = targets
	ctx.Data["Target"] = branch
	page := max(1, min(100000, ctx.FormInt("page")))
	ctx.Data["Prev"] = page - 1
	switch tab {
	case "reviews":
		if !canReview {
			ctx.HTTPError(403)
			return
		}
		var prs []*issues_model.PullRequest
		err = db.GetEngine(ctx).Table("pull_request").Select("pull_request.*").Join("INNER", "issue", "issue.id=pull_request.issue_id").Where("pull_request.base_repo_id=? AND pull_request.base_branch=? AND issue.poster_id=? AND issue.is_closed=? AND pull_request.has_merged=?", d.settings.RepoID, d.settings.Development, d.settings.AgentID, false, false).Desc("pull_request.id").Limit(21, (page-1)*20).Find(&prs)
		if err != nil {
			ctx.ServerErrorAuto(err)
			return
		}
		if len(prs) > 20 {
			ctx.Data["Next"] = page + 1
			prs = prs[:20]
		}
		var rows []deliveryPull
		for _, pr := range prs {
			if err = pr.LoadIssue(ctx); err != nil {
				ctx.ServerErrorAuto(err)
				return
			}
			head, e := ctx.Repo.GitRepo.GetRefCommitID(ctx, pr.GetGitHeadRefName())
			if e != nil {
				head = ""
			}
			rows = append(rows, deliveryPull{pr.Index, pr.Issue.Title, head})
		}
		ctx.Data["Pulls"] = rows
		for _, k := range []string{"merge", "reject", "expert"} {
			ctx.Data[k] = d.allowed(d.settings.Development, k) && ctx.Repo.Permission.CanWrite(unit.TypePullRequests)
		}
	case "releases":
		if branch == "" {
			ctx.HTTPError(403)
			return
		}
		head, e := ctx.Repo.GitRepo.GetBranchCommitID(ctx, branch)
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		ledger, e := deliveryLedger(ctx, head)
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		raw, _, e := gitcmd.NewCommand("log", "--first-parent", "--max-count=21", "--format=%H%x09%s").AddOptionFormat("--skip=%d", (page-1)*20).AddDynamicArguments(git.RefNameFromBranch(d.settings.Development).String()).WithRepo(ctx.Repo.GitRepo).RunStdString(ctx)
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		var changes []deliveryChange
		for line := range strings.SplitSeq(strings.TrimSuffix(raw, "\n"), "\n") {
			sha, title, ok := strings.Cut(line, "\t")
			if !ok {
				continue
			}
			if len(changes) == 20 {
				ctx.Data["Next"] = page + 1
				break
			}
			common, e := git.IsCommitInBranch(ctx, ctx.Repo.GitRepo, sha, branch)
			if e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			changes = append(changes, deliveryChange{sha, title, ledger[sha], common})
		}
		ctx.Data["Changes"] = changes
		ctx.Data["TargetHead"] = head
		writable := ctx.Repo.CanWriteToBranch(ctx, ctx.Doer, branch) && ctx.Repo.Repository.CanContentChange()
		ctx.Data["publish"] = writable && d.allowed(branch, "publish")
		ctx.Data["revoke"] = writable && d.allowed(branch, "revoke")
		if ctx.Repo.Permission.CanRead(unit.TypeActions) {
			var run actions_model.ActionRun
			ok, e := db.GetEngine(ctx).Where("repo_id=? AND ref=? AND workflow_id=? AND event=? AND is_scoped_run=? AND status=?", d.settings.RepoID, git.RefNameFromBranch(branch).String(), "deploy-"+strings.ReplaceAll(branch, "/", "-")+".yml", "push", false, actions_model.StatusSuccess).Desc("stopped", "id").Get(&run)
			if e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			if ok {
				ctx.Data["Deployed"] = run.CommitSHA
			}
		}
	case "settings", "users", "agents", "environment":
		if !d.admin {
			ctx.HTTPError(403)
			return
		}
		if d.settings.AgentID != 0 {
			u, e := user_model.GetUserByID(ctx, d.settings.AgentID)
			if e == nil {
				ctx.Data["AgentName"] = u.Name
			}
		}
		if tab == "users" {
			var rows []repo_model.DeliveryGrant
			err = db.GetEngine(ctx).Where("repo_id=?", d.settings.RepoID).Find(&rows)
			if err != nil {
				ctx.ServerErrorAuto(err)
				return
			}
			type grantView struct{ Login, Branch, Buttons string }
			labels := map[string]string{"review-view": "审批列表", "merge": "合并", "reject": "驳回", "expert": "邀请专家", "release-view": "发版页", "publish": "发版", "revoke": "撤销"}
			var views []grantView
			for _, g := range rows {
				if g.Buttons == "" {
					continue
				}
				u, e := user_model.GetUserByID(ctx, g.UserID)
				if e != nil {
					if user_model.IsErrUserNotExist(e) {
						continue
					}
					ctx.ServerErrorAuto(e)
					return
				}
				var ops []string
				for _, key := range strings.Fields(g.Buttons) {
					ops = append(ops, labels[key])
				}
				views = append(views, grantView{u.Name, g.Branch, strings.Join(ops, "、")})
			}
			ctx.Data["Grants"] = views
		}
	default:
		ctx.NotFound()
		return
	}
	ctx.HTML(http.StatusOK, "repo/delivery")
}

func DeliveryPost(ctx *context.Context) {
	d, err := loadDelivery(ctx)
	if err != nil {
		ctx.ServerErrorAuto(err)
		return
	}
	op := ctx.FormString("op")
	branch := ctx.FormString("branch")
	dest := ctx.Repo.RepoLink + "/delivery?"
	switch op {
	case "mcp-key":
		if !d.admin || d.settings.AgentID == 0 {
			ctx.HTTPError(403)
			return
		}
		key := repo_model.DeliveryAgentKey{RepoID: d.settings.RepoID, AgentID: d.settings.AgentID}
		found, e := db.GetEngine(ctx).ID(key.RepoID).Exist(new(repo_model.DeliveryAgentKey))
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		var value string
		if ctx.FormString("mode") == "issue" {
			value = rand.Text()
			digest := sha256.Sum256([]byte(value))
			key.Digest = hex.EncodeToString(digest[:])
		}
		if found {
			_, err = db.GetEngine(ctx).ID(key.RepoID).Cols("agent_id", "digest").Update(&key)
		} else {
			_, err = db.GetEngine(ctx).Insert(&key)
		}
		if err != nil {
			ctx.ServerErrorAuto(err)
			return
		}
		ctx.Resp.Header().Set("Cache-Control", "no-store")
		ctx.Data["NewKey"] = value
		ctx.Req.URL.RawQuery = "tab=agents"
		ctx.Req.Form.Set("tab", "agents")
		Delivery(ctx)
		return
	case "environment":
		if !d.admin {
			ctx.HTTPError(403)
			return
		}
		protected, e := git_model.IsBranchProtected(ctx, d.settings.RepoID, branch)
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		if !protected {
			ctx.HTTPError(400, "请选择保护分支")
			return
		}
		values := map[string]string{}
		for _, field := range []string{"DEPLOY_HOST", "DEPLOY_USER", "DEPLOY_SSH_KEY", "DEPLOY_KNOWN_HOSTS"} {
			values[field] = ctx.FormTrim(field)
			if values[field] == "" {
				ctx.HTTPError(400, "请填写完整的连接信息")
				return
			}
		}
		raw, e := json.Marshal(values)
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		env := repo_model.DeliveryEnvironment{RepoID: d.settings.RepoID, Branch: branch}
		found, e := db.GetEngine(ctx).Get(&env)
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		env.Ciphertext, e = secret.EncryptSecret(setting.SecretKey, string(raw))
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		if found {
			_, err = db.GetEngine(ctx).ID(env.ID).Cols("ciphertext").Update(&env)
		} else {
			_, err = db.GetEngine(ctx).Insert(&env)
		}
		dest += "tab=environment"
	case "settings", "grant":
		if !d.admin {
			ctx.HTTPError(403)
			return
		}
		if op == "settings" {
			dev := ctx.FormString("development")
			if _, err = ctx.Repo.GitRepo.GetBranchCommitID(ctx, dev); err != nil {
				ctx.HTTPError(400, "开发分支不存在")
				return
			}
			agent, e := user_model.GetUserByName(ctx, ctx.FormTrim("agent"))
			if e != nil {
				ctx.HTTPError(400, "Agent 账号不存在")
				return
			}
			cfg := repo_model.DeliverySettings{RepoID: d.settings.RepoID, Development: dev, AgentID: agent.ID, Instructions: ctx.FormString("instructions")}
			found, e := db.GetEngine(ctx).ID(cfg.RepoID).Exist(new(repo_model.DeliverySettings))
			if e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			if found {
				_, err = db.GetEngine(ctx).ID(cfg.RepoID).Cols("development", "agent_id", "instructions").Update(&cfg)
			} else {
				_, err = db.GetEngine(ctx).Insert(&cfg)
			}
			dest += "tab=settings"
		} else {
			u, e := user_model.GetUserByName(ctx, ctx.FormTrim("login"))
			if e != nil {
				ctx.HTTPError(400, "账号不存在")
				return
			}
			if branch != "*" {
				if _, e = ctx.Repo.GitRepo.GetBranchCommitID(ctx, branch); e != nil {
					ctx.HTTPError(400, "分支不存在")
					return
				}
			}
			buttons := ctx.FormStrings("buttons")
			for _, v := range buttons {
				if !slices.Contains([]string{"review-view", "merge", "reject", "expert", "release-view", "publish", "revoke"}, v) {
					ctx.HTTPError(400)
					return
				}
			}
			g := repo_model.DeliveryGrant{RepoID: d.settings.RepoID, Branch: branch, UserID: u.ID}
			exists, e := db.GetEngine(ctx).Get(&g)
			if e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			g.Buttons = strings.Join(buttons, " ")
			if exists {
				_, err = db.GetEngine(ctx).ID(g.ID).Cols("buttons").Update(&g)
			} else {
				_, err = db.GetEngine(ctx).Insert(&g)
			}
			dest += "tab=users"
		}
	case "merge", "reject", "expert":
		if !d.allowed(d.settings.Development, "review-view") || !d.allowed(d.settings.Development, op) || !ctx.Repo.Permission.CanWrite(unit.TypePullRequests) || !ctx.Repo.Repository.CanContentChange() {
			ctx.HTTPError(403)
			return
		}
		pr, e := issues_model.GetPullRequestByIndex(ctx, d.settings.RepoID, ctx.FormInt64("number"))
		if e != nil {
			ctx.NotFound()
			return
		}
		if e = pr.LoadIssue(ctx); e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		issue := pr.Issue
		issue.Repo = ctx.Repo.Repository
		issue.PullRequest = pr
		if issue.IsClosed || pr.HasMerged || pr.BaseBranch != d.settings.Development || issue.PosterID != d.settings.AgentID {
			ctx.HTTPError(409)
			return
		}
		head, e := ctx.Repo.GitRepo.GetRefCommitID(ctx, pr.GetGitHeadRefName())
		if e != nil || head != ctx.FormString("head") {
			ctx.HTTPError(409, "变更已更新，请刷新")
			return
		}
		switch op {
		case "merge":
			style := ctx.Repo.Repository.MustGetUnit(ctx, unit.TypePullRequests).PullRequestsConfig().DefaultMergeStyle
			err = pull_service.CheckPullMergeable(ctx, ctx.Doer, &ctx.Repo.Permission, pr, pull_service.MergeCheckTypeGeneral, style, false)
			if err == nil {
				err = pull_service.Merge(ctx, pr.ID, ctx.Doer, style, head, issue.Title, false)
			}
		case "reject":
			err = issue_service.CloseIssue(ctx, issue, ctx.Doer, "")
		case "expert":
			expert, e := user_model.GetUserByName(ctx, ctx.FormTrim("expert"))
			if e != nil {
				ctx.HTTPError(400, "专家账号不存在")
				return
			}
			_, err = issue_service.ReviewRequest(ctx, issue, ctx.Doer, &ctx.Repo.Permission, expert, true)
		}
		dest += "tab=reviews"
	case "publish", "revoke":
		if !d.allowed(branch, "release-view") || !d.allowed(branch, op) || branch == d.settings.Development || !ctx.Repo.CanWriteToBranch(ctx, ctx.Doer, branch) {
			ctx.HTTPError(403)
			return
		}
		protected, e := git_model.IsBranchProtected(ctx, d.settings.RepoID, branch)
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		if !protected {
			ctx.HTTPError(403)
			return
		}
		sha := ctx.FormString("sha")
		commit, e := ctx.Repo.GitRepo.GetCommit(ctx, sha)
		if e != nil || commit.ID.String() != sha {
			ctx.HTTPError(400)
			return
		}
		head, e := ctx.Repo.GitRepo.GetBranchCommitID(ctx, branch)
		if e != nil || head != ctx.FormString("head") {
			ctx.HTTPError(409, "目标版本已更新，请刷新")
			return
		}
		raw, _, e := gitcmd.NewCommand("rev-list", "--first-parent").AddDynamicArguments(git.RefNameFromBranch(d.settings.Development).String()).WithRepo(ctx.Repo.GitRepo).RunStdString(ctx)
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		if !slices.Contains(strings.Fields(raw), sha) {
			ctx.HTTPError(400)
			return
		}
		ledger, e := deliveryLedger(ctx, head)
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		source, verb := sha, "Publish"
		revert := op == "revoke"
		if (ledger[sha] != "") != revert {
			ctx.HTTPError(409)
			return
		}
		if revert {
			source = ledger[sha]
			verb = "Revoke"
		} else {
			common, e := git.IsCommitInBranch(ctx, ctx.Repo.GitRepo, sha, branch)
			if e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			if common {
				ctx.HTTPError(409)
				return
			}
		}
		_, err = files.CherryPick(ctx, ctx.Repo.Repository, ctx.Doer, revert, &files.ApplyDiffPatchOptions{OldBranch: branch, NewBranch: branch, LastCommitID: head, Content: source, Message: fmt.Sprintf("%s %s\n\nDelivery-Source: %s", verb, sha, sha)})
		dest += "tab=releases&branch=" + url.QueryEscape(branch) + "&page=" + strconv.Itoa(max(1, ctx.FormInt("page")))
	default:
		ctx.HTTPError(400)
		return
	}
	if err != nil {
		ctx.Flash.Error(err.Error())
	}
	ctx.Redirect(dest)
}

func DeliveryMCP(ctx *context.Context) {
	repoID := ctx.PathParamInt64("repoid")
	key, ok := strings.CutPrefix(ctx.Req.Header.Get("Authorization"), "Bearer ")
	if !ok || key == "" {
		ctx.HTTPError(401)
		return
	}
	var credential repo_model.DeliveryAgentKey
	found, err := db.GetEngine(ctx).ID(repoID).Get(&credential)
	if err != nil {
		ctx.ServerErrorAuto(err)
		return
	}
	digest := sha256.Sum256([]byte(key))
	if !found || subtle.ConstantTimeCompare([]byte(credential.Digest), []byte(hex.EncodeToString(digest[:]))) != 1 {
		ctx.HTTPError(401)
		return
	}
	var cfg repo_model.DeliverySettings
	found, err = db.GetEngine(ctx).ID(repoID).Get(&cfg)
	if err != nil {
		ctx.ServerErrorAuto(err)
		return
	}
	if !found || cfg.AgentID != credential.AgentID {
		ctx.HTTPError(403)
		return
	}
	actor, err := user_model.GetUserByID(ctx, cfg.AgentID)
	if err != nil || !actor.IsActive || actor.ProhibitLogin {
		ctx.HTTPError(403)
		return
	}
	repository, err := repo_model.GetRepositoryByID(ctx, repoID)
	if err != nil {
		ctx.NotFound()
		return
	}
	perm, err := access_model.GetDoerRepoPermission(ctx, repository, actor)
	if err != nil {
		ctx.ServerErrorAuto(err)
		return
	}
	if !perm.CanRead(unit.TypeCode) {
		ctx.HTTPError(403)
		return
	}
	var req struct {
		ID     json.RawMessage
		Method string
		Params struct{ Name string }
	}
	if err = json.NewDecoder(io.LimitReader(ctx.Req.Body, 64*1024)).Decode(&req); err != nil {
		ctx.HTTPError(400)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2025-03-26", "serverInfo": map[string]string{"name": "gitea-delivery", "version": "1"}, "capabilities": map[string]any{"tools": map[string]any{}}}
	case "notifications/initialized":
		ctx.Status(202)
		return
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{"name": "coding_instructions", "description": "读取授权仓库、目标开发分支和编码约束", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}}
	case "tools/call":
		if req.Params.Name == "coding_instructions" {
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": fmt.Sprintf("%s\n开发分支 %s\n%s", repository.FullName(), cfg.Development, cfg.Instructions)}}}
		}
	}
	if result == nil {
		ctx.JSON(200, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "Unknown method or tool"}})
		return
	}
	ctx.JSON(200, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func DeliveryHome(ctx *context.Context) {
	page := max(1, min(100000, ctx.FormInt("page")))
	var repos []*repo_model.Repository
	q := db.GetEngine(ctx).Table("repository")
	if !ctx.Doer.IsAdmin {
		q = q.Where("id IN (SELECT repo_id FROM delivery_grant WHERE user_id=? AND buttons<>?)", ctx.Doer.ID, "")
	}
	if err := q.Desc("id").Limit(25, (page-1)*24).Find(&repos); err != nil {
		ctx.ServerErrorAuto(err)
		return
	}
	if len(repos) > 24 {
		ctx.Data["Next"] = page + 1
		repos = repos[:24]
	}
	visible := make([]*repo_model.Repository, 0, len(repos))
	for _, r := range repos {
		perm, e := access_model.GetDoerRepoPermission(ctx, r, ctx.Doer)
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		if perm.CanRead(unit.TypeCode) {
			visible = append(visible, r)
		}
	}
	ctx.Data["Title"] = "业务工作台"
	ctx.Data["PageIsDelivery"] = true
	ctx.Data["DeliveryRepos"] = visible
	ctx.Data["Prev"] = page - 1
	ctx.HTML(200, "repo/delivery_home")
}
