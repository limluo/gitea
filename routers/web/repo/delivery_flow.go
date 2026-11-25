// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	actions_model "gitea.dev/models/actions"
	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/modules/git"
	"gitea.dev/modules/secret"
	"gitea.dev/modules/setting"
	"gitea.dev/services/context"
	repository_service "gitea.dev/services/repository"
)

const deliveryPullAuthor = "(issue.poster_id=? OR (pull_request.head_repo_id=pull_request.base_repo_id AND EXISTS (SELECT 1 FROM delivery_record dr WHERE dr.repo_id=pull_request.base_repo_id AND dr.branch=pull_request.base_branch AND dr.revision=pull_request.head_branch AND dr.kind='创建需求分支')))"

func recordDelivery(ctx *context.Context, kind, branch, subject, revision, note string) error {
	_, err := db.GetEngine(ctx).Insert(&repo_model.DeliveryRecord{RepoID: ctx.Repo.Repository.ID, Kind: kind, Branch: branch, Subject: subject, Revision: revision, Actor: ctx.Doer.Name, Detail: note})
	return err
}
func deployedRevision(ctx *context.Context, branch string) (string, error) {
	var env repo_model.DeliveryEnvironment
	_, e := db.GetEngine(ctx).Where("repo_id=? AND branch=?", ctx.Repo.Repository.ID, branch).Get(&env)
	if e != nil {
		return "", e
	}
	workflow, event := env.Workflow, env.TriggerEvent
	if workflow == "" {
		workflow = "deploy-" + strings.ReplaceAll(branch, "/", "-") + ".yml"
	}
	if event == "" {
		event = "push"
	}
	var r actions_model.ActionRun
	_, err := db.GetEngine(ctx).Where("repo_id=? AND ref=? AND workflow_id=? AND event=? AND is_scoped_run=? AND status=?", ctx.Repo.Repository.ID, git.RefNameFromBranch(branch).String(), workflow, event, false, actions_model.StatusSuccess).Desc("stopped", "id").Get(&r)
	return r.CommitSHA, err
}

type deliveryVersion struct{ Branch, SHA string }

func deliveryOverview(ctx *context.Context, d *deliveryContext, targets []string, review bool) error {
	var versions []deliveryVersion
	for _, b := range targets {
		sha := ""
		if ctx.Repo.Permission.CanRead(unit.TypeActions) {
			var err error
			sha, err = deployedRevision(ctx, b)
			if err != nil {
				return err
			}
		}
		versions = append(versions, deliveryVersion{b, sha})
	}
	ctx.Data["Versions"] = versions
	if review {
		var inbox []issues_model.Issue
		q := db.GetEngine(ctx).Table("issue").Select("issue.*").Join("INNER", "pull_request", "issue.id=pull_request.issue_id").Where("issue.repo_id=? AND "+deliveryPullAuthor+" AND issue.is_closed=? AND pull_request.base_branch=?", d.settings.RepoID, d.settings.AgentID, false, d.settings.Development)
		filter := "NOT EXISTS (SELECT 1 FROM review WHERE review.issue_id=issue.id AND review.type=?)"
		if ctx.Data["TechView"] == true {
			filter = "EXISTS (SELECT 1 FROM review WHERE review.issue_id=issue.id AND review.type=?)"
		}
		count, err := q.Where(filter, issues_model.ReviewTypeRequest).Desc("issue.id").Limit(6).FindAndCount(&inbox)
		ctx.Data["InboxCount"] = count
		if err != nil {
			return err
		}
		ctx.Data["Inbox"] = inbox
	}
	branches := slices.Clone(targets)
	if review {
		branches = append(branches, d.settings.Development)
	}
	var events []repo_model.DeliveryRecord
	if len(branches) > 0 {
		if err := db.GetEngine(ctx).Where("repo_id=?", d.settings.RepoID).In("branch", branches).Desc("id").Limit(5).Find(&events); err != nil {
			return err
		}
	}
	ctx.Data["Events"] = events
	return nil
}

var requirementBranch = regexp.MustCompile(`^feature/[A-Za-z0-9_-]+$`)

func createDeliveryTask(ctx *context.Context, d *deliveryContext) error {
	branch, title, criteria := ctx.FormTrim("feature"), ctx.FormTrim("title"), ctx.FormTrim("criteria")
	if !requirementBranch.MatchString(branch) || title == "" || criteria == "" {
		return fmt.Errorf("请填写需求、验收标准及有效的 feature/ 分支名")
	}
	protected, err := git_model.IsBranchProtected(ctx, d.settings.RepoID, branch)
	if err != nil {
		return err
	}
	if protected {
		return fmt.Errorf("需求分支不能匹配保护规则")
	}
	if err = repository_service.CreateNewBranch(ctx, ctx.Doer, ctx.Repo.Repository, ctx.Repo.GitRepo, d.settings.Development, branch); err != nil {
		return err
	}
	return recordDelivery(ctx, "创建需求分支", d.settings.Development, title, branch, criteria)
}

type taskRow struct {
	Title, Criteria, Branch, State string
	Number                         int64
	PRs                            []int64
	Note                           string
}

func deliveryTasks(ctx *context.Context, d *deliveryContext) error {
	var tasks []repo_model.DeliveryRecord
	if err := db.GetEngine(ctx).Where("repo_id=? AND kind=? AND branch=?", d.settings.RepoID, "创建需求分支", d.settings.Development).Desc("id").Find(&tasks); err != nil {
		return err
	}
	var rows []taskRow
	counts := map[string]int{"全部": len(tasks)}
	stage := ctx.FormString("stage")
	ctx.Data["Stage"] = stage
	for _, t := range tasks {
		row := taskRow{Title: t.Subject, Criteria: t.Detail, Branch: t.Revision, State: "开发中"}
		var pr issues_model.PullRequest
		found, err := db.GetEngine(ctx).Where("base_repo_id=? AND head_repo_id=? AND base_branch=? AND head_branch=?", d.settings.RepoID, d.settings.RepoID, d.settings.Development, t.Revision).Desc("id").Get(&pr)
		if err != nil {
			return err
		}
		if found {
			if err = pr.LoadIssue(ctx); err != nil {
				return err
			}
			row.Number = pr.Index
			if e := db.GetEngine(ctx).Table("pull_request").Where("base_repo_id=? AND head_repo_id=? AND base_branch=? AND head_branch=?", d.settings.RepoID, d.settings.RepoID, d.settings.Development, t.Revision).Desc("id").Cols("index").Find(&row.PRs); e != nil {
				return e
			}
			switch {
			case pr.HasMerged:
				row.State = "已并入开发分支"
			case pr.Issue.IsClosed:
				row.State = "已关闭"
			default:
				row.State = "已提交 PR"
			}
			var last repo_model.DeliveryRecord
			ok, e := db.GetEngine(ctx).Where("repo_id=? AND subject=? AND kind=?", d.settings.RepoID, fmt.Sprintf("PR #%d", pr.Index), "要求修改").Desc("id").Get(&last)
			if e != nil {
				return e
			}
			if ok && pr.Issue.IsClosed && !pr.HasMerged {
				row.State = "修改中"
				row.Note = last.Detail
			}
		}
		counts[row.State]++
		if filter := ctx.FormString("stage"); filter == "" || row.State == filter {
			rows = append(rows, row)
		}
	}
	type stageTab struct {
		Name, Value string
		Count       int
	}
	var tabs []stageTab
	for _, v := range []string{"全部", "开发中", "修改中", "已提交 PR", "已并入开发分支", "已关闭"} {
		if counts[v] > 0 || v == "全部" {
			value := v
			if v == "全部" {
				value = ""
			}
			tabs = append(tabs, stageTab{v, value, counts[v]})
		}
	}
	ctx.Data["Stages"] = tabs
	ctx.Data["Tasks"] = rows
	return nil
}

type acceptanceState struct {
	applied, passed, notes map[string]string
	gate                   bool
}

func loadAcceptance(ctx *context.Context, d *deliveryContext) (acceptanceState, error) {
	state := acceptanceState{passed: map[string]string{}}
	if d.settings.Development == "uat" {
		return state, nil
	}
	ok, err := git_model.IsBranchProtected(ctx, d.settings.RepoID, "uat")
	if err != nil || !ok {
		return state, err
	}
	exists, err := git_model.IsBranchExist(ctx, d.settings.RepoID, "uat")
	if err != nil || !exists {
		return state, err
	}
	state.gate = true
	head, err := ctx.Repo.GitRepo.GetBranchCommitID(ctx, "uat")
	if err != nil {
		return state, err
	}
	state.applied, err = deliveryLedger(ctx, head)
	if err != nil {
		return state, err
	}
	var rows []repo_model.DeliveryRecord
	err = db.GetEngine(ctx).Where("repo_id=? AND branch=? AND kind IN (?,?)", d.settings.RepoID, "uat", "验收通过", "验收未通过").Asc("id").Find(&rows)
	for _, r := range rows {
		if r.Revision == state.applied[r.Subject] {
			state.passed[r.Subject] = r.Kind
			if state.notes == nil {
				state.notes = map[string]string{}
			}
			state.notes[r.Subject] = r.Detail
		}
	}
	return state, err
}

var workflowFilename = regexp.MustCompile(`^[A-Za-z0-9_-]+\.ya?ml$`)
var deployPath = regexp.MustCompile(`^[a-zA-Z0-9_./-]+$`)

var keyAlias = regexp.MustCompile(`^DEPLOY_KEY_[A-Z0-9_]+$`)

func validDeploymentOptions(v map[string]string) bool {
	alias := v["DEPLOY_KEY_ALIAS"]
	if alias != "" && alias != "DEPLOY_SSH_KEY" && (!keyAlias.MatchString(alias) || alias == "DEPLOY_KEY_ALIAS") {
		return false
	}
	for key, maximum := range map[string]int{"DEPLOY_PORT": 65535, "DEPLOY_TIMEOUT": 86400} {
		if raw := v[key]; raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil || n < 1 || n > maximum {
				return false
			}
		}
	}
	for _, k := range []string{"DEPLOY_DIR", "DEPLOY_ARTIFACT", "DEPLOY_HEALTH"} {
		p := v[k]
		if p == "" {
			continue
		}
		if !deployPath.MatchString(p) || slices.Contains(strings.Split(p, "/"), "..") || strings.HasPrefix(p, "-") {
			return false
		}
		if k == "DEPLOY_ARTIFACT" && strings.HasPrefix(p, "/") {
			return false
		}
		if k != "DEPLOY_ARTIFACT" && !strings.HasPrefix(p, "/") {
			return false
		}
	}
	return true
}

type environmentView struct {
	Branch, Name, Cloud, Region, Workflow, Event string
	Values                                       map[string]string
}

func deliveryEnvironments(ctx *context.Context, branches []string) ([]environmentView, error) {
	var rows []environmentView
	for _, b := range branches {
		var env repo_model.DeliveryEnvironment
		ok, err := db.GetEngine(ctx).Where("repo_id=? AND branch=?", ctx.Repo.Repository.ID, b).Get(&env)
		if err != nil {
			return nil, err
		}
		values := map[string]string{}
		if ok {
			plain, e := secret.DecryptSecret(setting.SecretKey, env.Ciphertext)
			if e != nil {
				return nil, e
			}
			if e = json.Unmarshal([]byte(plain), &values); e != nil {
				return nil, e
			}
			delete(values, "DEPLOY_SSH_KEY")
		}
		if env.Name == "" {
			env.Name = b
		}
		if env.Workflow == "" {
			env.Workflow = "deploy-" + strings.ReplaceAll(b, "/", "-") + ".yml"
		}
		rows = append(rows, environmentView{b, env.Name, env.Cloud, env.Region, env.Workflow, env.TriggerEvent, values})
	}
	return rows, nil
}
