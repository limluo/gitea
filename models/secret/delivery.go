// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package secret

import (
	"context"
	"strings"

	actions_model "gitea.dev/models/actions"
	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/json"
	"gitea.dev/modules/secret"
	"gitea.dev/modules/setting"
)

func loadDeliverySecrets(ctx context.Context, run *actions_model.ActionRun, dst map[string]string) error {
	branch, ok := strings.CutPrefix(run.Ref, "refs/heads/")
	if !ok || run.Event != "push" || run.IsScopedRun || run.IsForkPullRequest || run.WorkflowRepoID != run.RepoID || run.WorkflowID != "deploy-"+strings.ReplaceAll(branch, "/", "-")+".yml" {
		return nil
	}
	var env repo_model.DeliveryEnvironment
	exists, err := db.GetEngine(ctx).Where("repo_id=? AND branch=?", run.RepoID, branch).Get(&env)
	if err != nil || !exists {
		return err
	}
	protected, err := git_model.IsBranchProtected(ctx, run.RepoID, branch)
	if err != nil || !protected {
		return err
	}
	value, err := secret.DecryptSecret(setting.SecretKey, env.Ciphertext)
	if err != nil {
		return err
	}
	var values map[string]string
	if err = json.Unmarshal([]byte(value), &values); err != nil {
		return err
	}
	for k, v := range values {
		dst[k] = v
	}
	return nil
}
