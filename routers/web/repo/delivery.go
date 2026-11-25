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
		if g.Branch != "*" && g.Branch != branch {
			continue
		}
		keys := strings.Fields(g.Buttons)
		if slices.Contains(keys, "scoped") {
			if !slices.Contains(keys, button) {
				continue
			}
			guard := map[string]string{"review-view": "read", "branches": "read", "release-view": "read", "task": "submit-pr", "merge": "merge-pr", "publish": "deploy", "revoke": "deploy"}[button]
			if guard == "" || slices.Contains(keys, guard) {
				return true
			}
		} else if slices.Contains(keys, button) || (button == "branches" && slices.Contains(keys, "review-view")) {
			return true
		}
	}
	return false
}

func (d *deliveryContext) manage(ctx *context.Context, page string) bool {
	return d.admin || (ctx.Repo.Permission.IsAdmin() && d.allowed("*", page))
}

type deliveryChange struct {
	Feedback                    string
	SHA, Title, Applied, Status string
	Number                      int64
	Common                      bool
	Acceptance                  string
	Blocked                     bool
}
type deliveryPull struct {
	Number                          int64
	Title, Head, Description, State string
	History                         []repo_model.DeliveryRecord
	Mine                            bool
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
	canBranches := d.allowed(d.settings.Development, "branches") && ctx.Repo.Permission.CanRead(unit.TypePullRequests)
	ctx.Data["CanBranches"] = canBranches
	tech := d.admin || (canReview && d.allowed(d.settings.Development, "expert") && ctx.Repo.Permission.CanWrite(unit.TypePullRequests))
	for _, page := range []string{"users", "settings", "agents", "environment"} {
		ok := d.manage(ctx, page)
		ctx.Data["manage"+page] = ok
		tech = tech || ok
	}
	if !canReview && !canBranches && len(targets) == 0 && !tech {
		ctx.HTTPError(403)
		return
	}
	tab := ctx.FormString("tab")
	if tab == "" {
		tab = "overview"
	}
	ctx.Data["Title"] = "业务交付"
	ctx.Data["PageIsDelivery"] = true
	ctx.Data["DeliveryAdmin"] = d.admin
	ctx.Data["DeliverySettings"] = d.settings
	ctx.Data["DeliveryTab"] = tab
	repos, e := workbenchRepos(ctx, tab)
	if e != nil {
		ctx.ServerErrorAuto(e)
		return
	}
	ctx.Data["WorkspaceRepos"] = repos
	ctx.Data["EditRepo"] = ctx.FormBool("edit")
	ctx.Data["Crumb"] = map[string]string{"overview": "交付总览", "reviews": "Agent 合并审批", "branches": "需求与分支", "releases": "发版操作", "users": "用户管理", "settings": "仓库管理", "agents": "MCP 管理及授权", "environment": "部署环境"}[tab]
	ctx.Data["TechRole"] = tech
	c, cookieErr := ctx.Req.Cookie("delivery-view")
	tech = tech && cookieErr == nil && c.Value == "tech"
	ctx.Data["TechView"] = tech
	ctx.Data["MCPURL"] = setting.AppURL + "-/delivery/mcp/" + strconv.FormatInt(d.settings.RepoID, 10)
	ctx.Data["CanReview"] = canReview
	ctx.Data["Targets"] = targets
	ctx.Data["Target"] = branch
	page := max(1, min(100000, ctx.FormInt("page")))
	ctx.Data["Prev"] = page - 1
	switch tab {
	case "overview":
		if e := deliveryOverview(ctx, d, targets, canReview); e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
	case "branches":
		if !canBranches {
			ctx.HTTPError(403)
			return
		}
		ctx.Data["CreateTask"] = d.allowed(d.settings.Development, "task") && ctx.Repo.Permission.CanWrite(unit.TypeCode)
		if e := deliveryTasks(ctx, d); e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
	case "reviews":
		if !canReview {
			ctx.HTTPError(403)
			return
		}
		var prs []*issues_model.PullRequest
		defaultQueue := "pending"
		if tech {
			defaultQueue = "expert"
		}
		queue := ctx.FormString("queue", defaultQueue)
		if n := ctx.FormInt64("pr"); n > 0 {
			pr, e := issues_model.GetPullRequestByIndex(ctx, d.settings.RepoID, n)
			if e != nil {
				ctx.NotFound()
				return
			}
			if e = pr.LoadIssue(ctx); e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			if pr.Issue.IsClosed {
				queue = "done"
			} else {
				requested, e := db.GetEngine(ctx).Where("issue_id=? AND type=?", pr.IssueID, issues_model.ReviewTypeRequest).Exist(new(issues_model.Review))
				if e != nil {
					ctx.ServerErrorAuto(e)
					return
				}
				if requested {
					queue = "expert"
				} else {
					queue = "pending"
				}
			}
		}
		ctx.Data["Queue"] = queue
		q := db.GetEngine(ctx).Table("pull_request").Select("pull_request.*").Join("INNER", "issue", "issue.id=pull_request.issue_id").Where("pull_request.base_repo_id=? AND pull_request.base_branch=? AND "+deliveryPullAuthor+" AND issue.is_closed=?", d.settings.RepoID, d.settings.Development, d.settings.AgentID, queue == "done")
		switch queue {
		case "expert":
			q = q.Where("EXISTS (SELECT 1 FROM review WHERE review.issue_id=issue.id AND review.type=?)", issues_model.ReviewTypeRequest)
		case "pending":
			q = q.Where("NOT EXISTS (SELECT 1 FROM review WHERE review.issue_id=issue.id AND review.type=?)", issues_model.ReviewTypeRequest)
		case "done":
		default:
			ctx.HTTPError(400)
			return
		}
		if n := ctx.FormInt64("pr"); n > 0 {
			q = q.Where("pull_request.`index`=?", n)
			ctx.Data["OpenPR"] = n
		}
		err = q.Desc("pull_request.id").Limit(21, (page-1)*20).Find(&prs)
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
			state := "待我审批"
			if queue == "expert" {
				state = "专家复核"
			}
			if queue == "done" {
				state = "已关闭"
				if pr.HasMerged {
					state = "已合并"
				}
			}
			description := pr.Issue.Content
			if pr.HeadRepoID == pr.BaseRepoID {
				var task repo_model.DeliveryRecord
				found, e := db.GetEngine(ctx).Where("repo_id=? AND kind=? AND revision=? AND branch=?", d.settings.RepoID, "创建需求分支", pr.HeadBranch, d.settings.Development).Desc("id").Get(&task)
				if e != nil {
					ctx.ServerErrorAuto(e)
					return
				}
				if found {
					description = task.Detail
				}
			}
			if pr.Issue.IsClosed && !pr.HasMerged {
				var outcome repo_model.DeliveryRecord
				found, e := db.GetEngine(ctx).Where("repo_id=? AND subject=? AND kind IN (?,?)", d.settings.RepoID, fmt.Sprintf("PR #%d", pr.Index), "要求修改", "驳回").Desc("id").Get(&outcome)
				if e != nil {
					ctx.ServerErrorAuto(e)
					return
				}
				if found {
					state = outcome.Kind + " · " + outcome.Detail
				}
			}
			var history []repo_model.DeliveryRecord
			if e = db.GetEngine(ctx).Where("repo_id=? AND subject=?", d.settings.RepoID, fmt.Sprintf("PR #%d", pr.Index)).Asc("id").Find(&history); e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			mine, e := db.GetEngine(ctx).Where("issue_id=? AND reviewer_id=? AND type=?", pr.IssueID, ctx.Doer.ID, issues_model.ReviewTypeRequest).Exist(new(issues_model.Review))
			if e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			rows = append(rows, deliveryPull{pr.Index, pr.Issue.Title, head, description, state, history, mine})
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
		var prs []issues_model.PullRequest
		if e := db.GetEngine(ctx).Where("base_repo_id=? AND base_branch=? AND has_merged=? AND merged_commit_id<>?", d.settings.RepoID, d.settings.Development, true, "").Desc("merged_unix", "id").Limit(21, (page-1)*20).Find(&prs); e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		if len(prs) > 20 {
			ctx.Data["Next"] = page + 1
			prs = prs[:20]
		}
		acceptance, e := loadAcceptance(ctx, d)
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		deployedLedger := map[string]string{}
		if ctx.Repo.Permission.CanRead(unit.TypeActions) {
			deployed, e := deployedRevision(ctx, branch)
			if e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			if deployed != "" {
				deployedLedger, e = deliveryLedger(ctx, deployed)
				if e != nil {
					ctx.ServerErrorAuto(e)
					return
				}
			}
		}
		var changes []deliveryChange
		for _, pr := range prs {
			sha := pr.MergedCommitID
			if e := pr.LoadIssue(ctx); e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			common, e := git.IsCommitInBranch(ctx, ctx.Repo.GitRepo, sha, branch)
			if e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			applied, recorded := ledger[sha]
			status := "待发布"
			switch {
			case applied != "" && deployedLedger[sha] == applied:
				status = "已合并 · 已部署"
			case applied != "":
				status = "已合并 · 等待部署"
			case recorded:
				status = "已撤销"
			case common:
				status = "分支共有"
			}
			changes = append(changes, deliveryChange{SHA: sha, Title: pr.Issue.Title, Applied: applied, Number: pr.Index, Status: status, Common: common, Acceptance: acceptance.passed[sha], Feedback: acceptance.notes[sha], Blocked: branch == "main" && acceptance.gate && acceptance.passed[sha] != "验收通过"})
		}
		ctx.Data["Changes"] = changes
		ctx.Data["TargetHead"] = head
		writable := ctx.Repo.CanWriteToBranch(ctx, ctx.Doer, branch) && ctx.Repo.Repository.CanContentChange()
		ctx.Data["publish"] = writable && d.allowed(branch, "publish")
		ctx.Data["revoke"] = writable && d.allowed(branch, "revoke")
		if ctx.Repo.Permission.CanRead(unit.TypeActions) {
			deployed, e := deployedRevision(ctx, branch)
			if e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			ctx.Data["Deployed"] = deployed
			if slices.Contains(targets, "main") {
				prod, e := deployedRevision(ctx, "main")
				if e != nil {
					ctx.ServerErrorAuto(e)
					return
				}
				ctx.Data["Production"] = prod
			}
		}
	case "settings", "users", "agents", "environment":
		if !d.manage(ctx, tab) {
			ctx.HTTPError(403)
			return
		}
		if d.settings.AgentID != 0 {
			u, e := user_model.GetUserByID(ctx, d.settings.AgentID)
			if e == nil {
				ctx.Data["AgentName"] = u.Name
			}
		}
		if tab == "agents" {
			var scoped []repo_model.DeliveryToolGrant
			if e := db.GetEngine(ctx).Where("repo_id=?", d.settings.RepoID).Asc("id").Find(&scoped); e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			ctx.Data["ToolGrants"] = scoped
		}
		if tab == "environment" {
			envs, e := deliveryEnvironments(ctx, targets)
			if e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			ctx.Data["Environments"] = envs
		}
		if tab == "users" {
			var accounts []user_model.User
			if e := db.GetEngine(ctx).Cols("id", "name").Where("is_active=? AND prohibit_login=?", true, false).In("type", []user_model.UserType{user_model.UserTypeIndividual, user_model.UserTypeBot}).Asc("lower_name").Limit(101, (page-1)*100).Find(&accounts); e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			if len(accounts) > 100 {
				ctx.Data["Next"] = page + 1
				accounts = accounts[:100]
			}
			ctx.Data["Accounts"] = accounts
			var rows []repo_model.DeliveryGrant
			err = db.GetEngine(ctx).Where("repo_id=?", d.settings.RepoID).Find(&rows)
			if err != nil {
				ctx.ServerErrorAuto(err)
				return
			}
			type grantView struct{ Login, Branch, Buttons string }
			labels := map[string]string{"review-view": "审批列表", "merge": "合并", "reject": "驳回", "expert": "邀请专家", "release-view": "发版页", "publish": "发版", "revoke": "撤销", "task": "新建需求分支", "read": "读取", "submit-pr": "提交 PR", "merge-pr": "合并 PR", "deploy": "发布", "settings": "仓库管理", "users": "用户管理", "environment": "部署环境", "agents": "MCP 管理及授权", "branches": "需求与分支"}
			var views []grantView
			selected := map[string]bool{}
			ctx.Data["ManagementMenus"] = map[string]string{"settings": "仓库管理", "users": "用户管理", "agents": "MCP 管理及授权", "environment": "部署环境"}
			ctx.Data["BranchOps"] = map[string]string{"read": "读取", "submit-pr": "提交 PR", "merge-pr": "合并 PR", "deploy": "发布"}
			ctx.Data["EditLogin"] = ctx.FormTrim("user")
			ctx.Data["EditBranch"] = ctx.FormTrim("scope")
			ctx.Data["SelectedButtons"] = selected
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
				keys := strings.Fields(g.Buttons)
				if !slices.Contains(keys, "scoped") {
					keys = append(keys, "read")
					for k, v := range map[string]string{"review-view": "branches", "task": "submit-pr", "merge": "merge-pr", "publish": "deploy", "revoke": "deploy"} {
						if slices.Contains(keys, k) && !slices.Contains(keys, v) {
							keys = append(keys, v)
						}
					}
				}
				for _, key := range keys {
					if key == "scoped" {
						continue
					}
					ops = append(ops, labels[key])
					if u.Name == ctx.FormTrim("user") && g.Branch == ctx.FormTrim("scope") {
						selected[key] = true
					}
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
	case "init-repo", "save-repo":
		if !d.manage(ctx, "settings") || (op == "init-repo" && !d.admin) {
			ctx.HTTPError(403)
			return
		}
		var link string
		link, err = saveWorkbenchRepo(ctx, op == "init-repo")
		if link != "" {
			dest = link
		} else {
			dest += "tab=settings"
		}
	case "submit-task":
		if !d.allowed(d.settings.Development, "branches") || !d.allowed(d.settings.Development, "task") || !ctx.Repo.Permission.CanWrite(unit.TypeCode) || !ctx.Repo.Repository.CanContentChange() {
			ctx.HTTPError(403)
			return
		}
		err = submitDeliveryPR(ctx, d)
		dest += "tab=branches"
	case "create-task":
		if !d.allowed(d.settings.Development, "task") || !d.allowed(d.settings.Development, "branches") || !ctx.Repo.Permission.CanWrite(unit.TypeCode) || !ctx.Repo.Repository.CanContentChange() {
			ctx.HTTPError(403)
			return
		}
		err = createDeliveryTask(ctx, d)
		dest += "tab=branches"
	case "accept":
		if branch != "uat" || !d.allowed(branch, "release-view") || !d.allowed(branch, "publish") || !ctx.Repo.Permission.CanRead(unit.TypeActions) || !ctx.Repo.CanWriteToBranch(ctx, ctx.Doer, branch) {
			ctx.HTTPError(403)
			return
		}
		state, e := loadAcceptance(ctx, d)
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		sha := ctx.FormString("sha")
		applied := state.applied[sha]
		if applied == "" || applied != ctx.FormString("applied") {
			ctx.HTTPError(409)
			return
		}
		deployed, e := deployedRevision(ctx, branch)
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		if deployed == "" {
			ctx.HTTPError(409, "尚无成功部署记录")
			return
		}
		live, e := deliveryLedger(ctx, deployed)
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		if live[sha] != applied {
			ctx.HTTPError(409, "该变更尚未成功部署")
			return
		}
		result, note := ctx.FormTrim("result"), ctx.FormTrim("note")
		if (result != "验收通过" && result != "验收未通过") || note == "" {
			ctx.HTTPError(400)
			return
		}
		err = recordDelivery(ctx, result, branch, sha, applied, note)
		dest += "tab=releases&branch=uat"
	case "tool-grant":
		if !d.manage(ctx, "agents") {
			ctx.HTTPError(403)
			return
		}
		if ctx.FormString("mode") == "revoke" {
			_, err = db.GetEngine(ctx).Where("id=? AND repo_id=?", ctx.FormInt64("grant"), d.settings.RepoID).Delete(new(repo_model.DeliveryToolGrant))
			dest += "tab=agents"
			break
		}
		actor, e := user_model.GetUserByName(ctx, ctx.FormTrim("agent"))
		if e != nil {
			ctx.HTTPError(400, "账号不存在")
			return
		}
		tools := ctx.FormStrings("tools")
		service := ctx.FormTrim("service")
		if len(tools) == 0 || service == "" {
			ctx.HTTPError(400)
			return
		}
		for _, tool := range tools {
			if tool != "coding_instructions" && tool != "repository_info" {
				ctx.HTTPError(400)
				return
			}
		}
		value := rand.Text()
		hash := sha256.Sum256([]byte(value))
		_, err = db.GetEngine(ctx).Insert(&repo_model.DeliveryToolGrant{RepoID: d.settings.RepoID, AgentID: actor.ID, Login: actor.Name, Service: service, Tools: strings.Join(tools, " "), Digest: hex.EncodeToString(hash[:])})
		if err != nil {
			ctx.ServerErrorAuto(err)
			return
		}
		ctx.Resp.Header().Set("Cache-Control", "no-store")
		ctx.Data["NewKey"] = value
		ctx.Req.Form.Set("tab", "agents")
		Delivery(ctx)
		return
	case "mcp-key":
		if !d.manage(ctx, "agents") || d.settings.AgentID == 0 {
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
		if !d.manage(ctx, "environment") {
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
			if values[field] == "" && field != "DEPLOY_SSH_KEY" {
				ctx.HTTPError(400, "请填写完整的连接信息")
				return
			}
		}
		for _, field := range []string{"DEPLOY_PORT", "DEPLOY_DIR", "DEPLOY_ARTIFACT", "DEPLOY_TIMEOUT", "DEPLOY_HEALTH"} {
			values[field] = ctx.FormTrim(field)
		}
		values["DEPLOY_KEY_ALIAS"] = ctx.FormTrim("DEPLOY_KEY_ALIAS")
		if !validDeploymentOptions(values) {
			ctx.HTTPError(400, "请检查端口、超时及路径")
			return
		}
		env := repo_model.DeliveryEnvironment{RepoID: d.settings.RepoID, Branch: branch}
		found, e := db.GetEngine(ctx).Get(&env)
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		if values["DEPLOY_SSH_KEY"] == "" {
			if !found {
				ctx.HTTPError(400, "首次配置需要 SSH 私钥")
				return
			}
			old, e := secret.DecryptSecret(setting.SecretKey, env.Ciphertext)
			if e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			var saved map[string]string
			if e = json.Unmarshal([]byte(old), &saved); e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			values["DEPLOY_SSH_KEY"] = saved["DEPLOY_SSH_KEY"]
		}
		raw, e := json.Marshal(values)
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		env.Name = ctx.FormTrim("name")
		env.Cloud = ctx.FormTrim("cloud")
		env.Region = ctx.FormTrim("region")
		env.Workflow = ctx.FormTrim("workflow")
		env.TriggerEvent = ctx.FormString("trigger", "push")
		if (env.Workflow != "" && !workflowFilename.MatchString(env.Workflow)) || (env.TriggerEvent != "push" && env.TriggerEvent != "workflow_dispatch") {
			ctx.HTTPError(400, "工作流配置无效")
			return
		}
		env.Ciphertext, e = secret.EncryptSecret(setting.SecretKey, string(raw))
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		if found {
			_, err = db.GetEngine(ctx).ID(env.ID).Cols("ciphertext", "name", "cloud", "region", "workflow", "trigger_event").Update(&env)
		} else {
			_, err = db.GetEngine(ctx).Insert(&env)
		}
		dest += "tab=environment"
	case "settings", "grant":
		page := "users"
		if op == "settings" {
			page = "settings"
		}
		if !d.manage(ctx, page) {
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
				if !slices.Contains([]string{"review-view", "merge", "reject", "expert", "release-view", "publish", "revoke", "task", "read", "merge-pr", "submit-pr", "deploy", "branches", "settings", "users", "agents", "environment"}, v) {
					ctx.HTTPError(400)
					return
				}
			}
			if len(buttons) > 0 && ctx.FormBool("scoped") {
				buttons = append(buttons, "scoped")
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
	case "merge", "reject", "expert", "revise", "return":
		perm := op
		if op == "revise" {
			perm = "reject"
		}
		if op == "return" {
			perm = "expert"
		}
		if !d.allowed(d.settings.Development, "review-view") || !d.allowed(d.settings.Development, perm) || !ctx.Repo.Permission.CanWrite(unit.TypePullRequests) || !ctx.Repo.Repository.CanContentChange() {
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
		eligible := issue.PosterID == d.settings.AgentID
		if !eligible && pr.HeadRepoID == pr.BaseRepoID {
			eligible, e = db.GetEngine(ctx).Where("repo_id=? AND branch=? AND kind=? AND revision=?", d.settings.RepoID, d.settings.Development, "创建需求分支", pr.HeadBranch).Exist(new(repo_model.DeliveryRecord))
			if e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
		}
		if issue.IsClosed || pr.HasMerged || pr.BaseBranch != d.settings.Development || !eligible {
			ctx.HTTPError(409)
			return
		}
		head, e := ctx.Repo.GitRepo.GetRefCommitID(ctx, pr.GetGitHeadRefName())
		if e != nil || head != ctx.FormString("head") {
			ctx.HTTPError(409, "变更已更新，请刷新")
			return
		}
		note := ctx.FormTrim("note")
		pending, e := db.GetEngine(ctx).Where("issue_id=? AND type=?", issue.ID, issues_model.ReviewTypeRequest).Exist(new(issues_model.Review))
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		if pending && (op == "reject" || op == "revise") {
			mine, e := db.GetEngine(ctx).Where("issue_id=? AND reviewer_id=? AND type=?", issue.ID, ctx.Doer.ID, issues_model.ReviewTypeRequest).Exist(new(issues_model.Review))
			if e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			if !mine {
				ctx.HTTPError(403)
				return
			}
		}
		if pending && op == "merge" {
			ctx.HTTPError(409, "尚有专家复核请求")
			return
		}
		if (op == "revise" || op == "reject") && note == "" {
			ctx.HTTPError(400, "请填写处理意见")
			return
		}
		switch op {
		case "merge":
			style := ctx.Repo.Repository.MustGetUnit(ctx, unit.TypePullRequests).PullRequestsConfig().DefaultMergeStyle
			err = pull_service.CheckPullMergeable(ctx, ctx.Doer, &ctx.Repo.Permission, pr, pull_service.MergeCheckTypeGeneral, style, false)
			if err == nil {
				err = pull_service.Merge(ctx, pr.ID, ctx.Doer, style, head, issue.Title, false)
			}
		case "reject", "revise":
			_, err = issue_service.CreateIssueComment(ctx, ctx.Doer, ctx.Repo.Repository, issue, note, nil)
			if err == nil {
				err = issue_service.CloseIssue(ctx, issue, ctx.Doer, "")
			}
		case "return":
			mine, e := db.GetEngine(ctx).Where("issue_id=? AND reviewer_id=? AND type=?", issue.ID, ctx.Doer.ID, issues_model.ReviewTypeRequest).Exist(new(issues_model.Review))
			if e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			if !mine {
				ctx.HTTPError(403)
				return
			}
			_, err = issue_service.ReviewRequest(ctx, issue, ctx.Doer, &ctx.Repo.Permission, ctx.Doer, false)
		case "expert":
			expert, e := user_model.GetUserByName(ctx, ctx.FormTrim("expert"))
			if e != nil {
				ctx.HTTPError(400, "专家账号不存在")
				return
			}
			_, err = issue_service.ReviewRequest(ctx, issue, ctx.Doer, &ctx.Repo.Permission, expert, true)
		}
		if err == nil {
			err = recordDelivery(ctx, map[string]string{"merge": "确认合并", "revise": "要求修改", "reject": "驳回", "expert": "转交专家", "return": "交还业务确认"}[op], d.settings.Development, fmt.Sprintf("PR #%d", pr.Index), head, note)
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
		if op == "publish" {
			ok, e := db.GetEngine(ctx).Where("base_repo_id=? AND base_branch=? AND has_merged=? AND merged_commit_id=?", d.settings.RepoID, d.settings.Development, true, sha).Exist(new(issues_model.PullRequest))
			if e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			if !ok {
				ctx.HTTPError(409, "仅可发布已合并的 PR")
				return
			}
		}
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
		if op == "publish" && branch == "main" {
			a, e := loadAcceptance(ctx, d)
			if e != nil {
				ctx.ServerErrorAuto(e)
				return
			}
			if a.gate && a.passed[sha] != "验收通过" {
				ctx.HTTPError(409, "请先完成 UAT 验收")
				return
			}
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
		if err == nil {
			err = recordDelivery(ctx, map[string]string{"publish": "合并发版", "revoke": "撤销发版"}[op], branch, sha, head, "")
		}
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
	tools := []string{"coding_instructions"}
	legacy := found && subtle.ConstantTimeCompare([]byte(credential.Digest), []byte(hex.EncodeToString(digest[:]))) == 1
	if !legacy {
		var grant repo_model.DeliveryToolGrant
		exists, e := db.GetEngine(ctx).Where("repo_id=? AND digest=?", repoID, hex.EncodeToString(digest[:])).Get(&grant)
		if e != nil {
			ctx.ServerErrorAuto(e)
			return
		}
		if !exists {
			ctx.HTTPError(401)
			return
		}
		credential.AgentID = grant.AgentID
		tools = strings.Fields(grant.Tools)
	}
	var cfg repo_model.DeliverySettings
	found, err = db.GetEngine(ctx).ID(repoID).Get(&cfg)
	if err != nil {
		ctx.ServerErrorAuto(err)
		return
	}
	if !found || (legacy && cfg.AgentID != credential.AgentID) {
		ctx.HTTPError(403)
		return
	}
	actor, err := user_model.GetUserByID(ctx, credential.AgentID)
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
		list := []any{}
		for _, name := range tools {
			list = append(list, map[string]any{"name": name, "description": map[string]string{"coding_instructions": "读取仓库编码约束", "repository_info": "读取仓库名称和开发分支"}[name], "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}})
		}
		result = map[string]any{"tools": list}
	case "tools/call":
		if slices.Contains(tools, req.Params.Name) && req.Params.Name == "repository_info" {
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": repository.FullName() + " / " + cfg.Development}}}
		}
		if slices.Contains(tools, req.Params.Name) && req.Params.Name == "coding_instructions" {
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
	if ctx.FormString("open") == "1" {
		for _, r := range visible {
			if !r.IsEmpty {
				ctx.Redirect(r.Link() + "/delivery")
				return
			}
		}
	}
	ctx.Data["Title"] = "业务工作台"
	ctx.Data["PageIsDelivery"] = true
	ctx.Data["DeliveryRepos"] = visible
	ctx.Data["Prev"] = page - 1
	ctx.HTML(200, "repo/delivery_home")
}
