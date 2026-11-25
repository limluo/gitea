// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"fmt"
	"slices"
	"strings"

	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/setting"
	"gitea.dev/services/context"
	pull_service "gitea.dev/services/pull"
	repository_service "gitea.dev/services/repository"
)

type workbenchRepo struct {
	ID                                                            int64
	Name, Link, Description, Protected, Exact, Dev, Prompt, Agent string
}

func workbenchRepos(ctx *context.Context, tab string) ([]workbenchRepo, error) {
	var repos []*repo_model.Repository
	q := db.GetEngine(ctx).Table("repository")
	if !ctx.Doer.IsAdmin {
		q = q.Where("id IN (SELECT repo_id FROM delivery_grant WHERE user_id=? AND buttons<>?)", ctx.Doer.ID, "")
	}
	if err := q.Asc("owner_name", "lower_name").Find(&repos); err != nil {
		return nil, err
	}
	var list []workbenchRepo
	for _, r := range repos {
		p, e := access_model.GetDoerRepoPermission(ctx, r, ctx.Doer)
		if e != nil {
			return nil, e
		}
		if !p.CanRead(unit.TypeCode) || r.IsEmpty {
			continue
		}
		row := workbenchRepo{ID: r.ID, Name: r.FullName(), Link: r.Link(), Description: r.Description}
		if tab == "releases" {
			rules, e := git_model.FindRepoProtectedBranchRules(ctx, r.ID)
			if e != nil {
				return nil, e
			}
			if len(rules) == 0 {
				continue
			}
		}
		if tab == "settings" {
			if !ctx.Doer.IsAdmin && (r.ID != ctx.Repo.Repository.ID || !p.IsAdmin()) {
				continue
			}
			rules, e := git_model.FindRepoProtectedBranchRules(ctx, r.ID)
			if e != nil {
				return nil, e
			}
			cfg := repo_model.DeliverySettings{}
			found, err := db.GetEngine(ctx).ID(r.ID).Get(&cfg)
			if err != nil {
				return nil, err
			}
			row.Dev = r.DefaultBranch
			if found {
				row.Dev = cfg.Development
				row.Prompt = cfg.Instructions
			}
			if cfg.AgentID != 0 {
				u, e := user_model.GetUserByID(ctx, cfg.AgentID)
				if e != nil && !user_model.IsErrUserNotExist(e) {
					return nil, e
				}
				if u != nil {
					row.Agent = u.Name
				}
			}
			var names, exact []string
			for _, rule := range rules {
				names = append(names, rule.RuleName)
				if !strings.ContainsAny(rule.RuleName, "*?[") {
					exact = append(exact, rule.RuleName)
				}
			}
			row.Exact = strings.Join(exact, ", ")
			row.Protected = strings.Join(names, ", ")
		}
		list = append(list, row)
	}
	return list, nil
}

func submitDeliveryPR(ctx *context.Context, d *deliveryContext) error {
	repo := ctx.Repo.Repository
	branch := ctx.FormTrim("feature")
	var task repo_model.DeliveryRecord
	found, err := db.GetEngine(ctx).Where("repo_id=? AND kind=? AND revision=? AND branch=?", repo.ID, "创建需求分支", branch, d.settings.Development).Desc("id").Get(&task)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("需求分支不存在")
	}
	var revisedHead string
	var prior issues_model.PullRequest
	found, err = db.GetEngine(ctx).Where("base_repo_id=? AND head_repo_id=? AND head_branch=? AND base_branch=?", repo.ID, repo.ID, branch, d.settings.Development).Desc("id").Get(&prior)
	if err != nil {
		return err
	}
	if found {
		if err = prior.LoadIssue(ctx); err != nil {
			return err
		}
		if prior.HasMerged || !prior.Issue.IsClosed {
			return fmt.Errorf("此需求已经提交或合并")
		}
		var decision repo_model.DeliveryRecord
		ok, e := db.GetEngine(ctx).Where("repo_id=? AND subject=? AND kind IN (?,?)", repo.ID, fmt.Sprintf("PR #%d", prior.Index), "要求修改", "驳回").Desc("id").Get(&decision)
		if e != nil {
			return e
		}
		revisedHead = decision.Revision
		if !ok || decision.Kind != "要求修改" {
			return fmt.Errorf("此需求已经关闭")
		}
	}
	base, err := ctx.Repo.GitRepo.GetBranchCommitID(ctx, d.settings.Development)
	if err != nil {
		return err
	}
	head, err := ctx.Repo.GitRepo.GetBranchCommitID(ctx, branch)
	if err != nil {
		return err
	}
	if head == revisedHead {
		return fmt.Errorf("请先根据修改意见提交新的代码变更")
	}
	mergeBase, err := git.MergeBase(ctx, repo, base, head)
	if err != nil {
		return err
	}
	if mergeBase == head {
		return fmt.Errorf("请先向需求分支提交开发成果")
	}
	pr := &issues_model.PullRequest{HeadRepoID: repo.ID, BaseRepoID: repo.ID, HeadRepo: repo, BaseRepo: repo, HeadBranch: branch, BaseBranch: d.settings.Development, MergeBase: mergeBase, Type: issues_model.PullRequestGitea}
	issue := &issues_model.Issue{RepoID: repo.ID, Title: task.Subject, Content: task.Detail, Poster: ctx.Doer, PosterID: ctx.Doer.ID, IsPull: true}
	return pull_service.NewPullRequest(ctx, ctx.Repo.Permission, &pull_service.NewPullRequestOptions{Repo: repo, Issue: issue, PullRequest: pr})
}

func CreateWorkbenchRepo(ctx *context.Context) {
	if !ctx.Doer.IsAdmin {
		ctx.HTTPError(403)
		return
	}
	next, err := saveWorkbenchRepo(ctx, true)
	if err != nil {
		ctx.Flash.Error(err.Error())
	}
	if next == "" {
		next = setting.AppSubURL + "/-/delivery"
	}
	ctx.Redirect(next)
}

func saveWorkbenchRepo(ctx *context.Context, create bool) (string, error) {
	var repo *repo_model.Repository
	if !create {
		repo = ctx.Repo.Repository
	}
	dev := ctx.FormTrim("development")
	if dev == "" {
		dev = "dev"
	}
	description := ctx.FormTrim("description")
	prompt := ctx.FormTrim("instructions")
	wanted := strings.FieldsFunc(ctx.FormTrim("protected"), func(r rune) bool { return r == ',' || r == '，' })
	if len(wanted) == 0 {
		return "", fmt.Errorf("请配置至少一个保护分支")
	}
	if !git.IsValidRefPattern(dev) || strings.HasPrefix(dev, "-") || dev == "HEAD" {
		return "", fmt.Errorf("无效的开发分支名")
	}
	for i, b := range wanted {
		b = strings.TrimSpace(b)
		if b == "" || b == "HEAD" || b == dev || strings.ContainsAny(b, "*?[") || !git.IsValidRefPattern(b) || strings.HasPrefix(b, "-") {
			return "", fmt.Errorf("请输入与开发分支不同的有效环境分支名")
		}
		wanted[i] = b
	}
	slices.Sort(wanted)
	wanted = slices.Compact(wanted)
	for i, b := range wanted {
		for _, other := range append(slices.Clone(wanted[:i]), dev) {
			if strings.HasPrefix(b, other+"/") || strings.HasPrefix(other, b+"/") {
				return "", fmt.Errorf("分支名称之间存在路径冲突")
			}
		}
	}
	var agentID int64
	if login := ctx.FormTrim("agent"); login != "" {
		u, e := user_model.GetUserByName(ctx, login)
		if e != nil {
			return "", fmt.Errorf("找不到 Agent 账号 %s", login)
		}
		agentID = u.ID
	}
	if create {
		opts := repository_service.CreateRepoOptions{Name: ctx.FormTrim("name"), Description: description, DefaultBranch: dev, AutoInit: true, IsPrivate: true, Readme: "Default"}
		if err := repository_service.CheckCreateRepository(ctx, ctx.Doer, ctx.Doer, opts.Name, false); err != nil {
			return "", err
		}
		var err error
		repo, err = repository_service.CreateRepository(ctx, ctx.Doer, ctx.Doer, opts)
		if err != nil {
			return "", err
		}
	}
	target := repo.Link() + "/delivery?tab=settings"
	gr, err := git.OpenRepository(ctx, repo)
	if err != nil {
		return target, err
	}
	defer gr.Close()
	if !gr.IsBranchExist(ctx, dev) {
		return target, fmt.Errorf("开发分支不存在")
	}
	rules, err := git_model.FindRepoProtectedBranchRules(ctx, repo.ID)
	if err != nil {
		return target, err
	}
	if !create {
		var current []string
		for _, rule := range rules {
			if !strings.ContainsAny(rule.RuleName, "*?[") {
				current = append(current, rule.RuleName)
			}
		}
		if strings.Join(current, ", ") != ctx.FormString("original") {
			return target, fmt.Errorf("保护规则已变更，请刷新后重试")
		}
	}
	for _, b := range wanted {
		if !gr.IsBranchExist(ctx, b) {
			if err = repository_service.CreateNewBranch(ctx, ctx.Doer, repo, gr, dev, b); err != nil {
				return target, err
			}
		}
		if rules.GetFirstMatched(b) == nil {
			if err = git_model.UpdateProtectBranch(ctx, repo, &git_model.ProtectedBranch{RepoID: repo.ID, RuleName: b, ProtectedBranchConfig: git_model.ProtectedBranchConfig{CanPush: true, EnableWhitelist: true, WhitelistUserIDs: []int64{ctx.Doer.ID}}}, git_model.WhitelistOptions{UserIDs: []int64{ctx.Doer.ID}}); err != nil {
				return target, err
			}
		}
	}
	// Wildcard protection policies are maintained in Gitea's native branch settings.
	for _, r := range rules {
		if !strings.ContainsAny(r.RuleName, "*?[") && !slices.Contains(wanted, r.RuleName) {
			if err = git_model.DeleteProtectedBranch(ctx, repo, r.ID); err != nil {
				return target, err
			}
		}
	}
	repo.Description = description
	if err = repo_model.UpdateRepositoryColsWithAutoTime(ctx, repo, "description"); err != nil {
		return target, err
	}
	cfg := repo_model.DeliverySettings{RepoID: repo.ID}
	exists, err := db.GetEngine(ctx).ID(repo.ID).Get(&cfg)
	if err != nil {
		return target, err
	}
	cfg.AgentID = agentID
	cfg.Development = dev
	cfg.Instructions = prompt
	if exists {
		_, err = db.GetEngine(ctx).ID(repo.ID).Cols("development", "instructions", "agent_id").Update(&cfg)
	} else {
		_, err = db.GetEngine(ctx).Insert(&cfg)
	}
	return target, err
}
