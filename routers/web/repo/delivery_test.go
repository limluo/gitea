// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	repo_model "gitea.dev/models/repo"
	"testing"
)

func TestDeliveryGrantScope(t *testing.T) {
	d := deliveryContext{grants: []repo_model.DeliveryGrant{{Branch: "*", Buttons: "review-view"}, {Branch: "uat", Buttons: "release-view publish"}}}
	for _, tc := range []struct {
		branch, button string
		want           bool
	}{{"uat", "publish", true}, {"main", "publish", false}, {"uat", "revoke", false}, {"develop", "review-view", true}, {"uat", "view", false}} {
		if got := d.allowed(tc.branch, tc.button); got != tc.want {
			t.Errorf("allowed(%s,%s)=%t", tc.branch, tc.button, got)
		}
	}
}
func TestWorkbenchScopedGrant(t *testing.T) {
	d := deliveryContext{grants: []repo_model.DeliveryGrant{{Branch: "dev", Buttons: "scoped branches read merge"}, {Branch: "uat", Buttons: "scoped release-view read publish deploy"}}}
	if !d.allowed("dev", "branches") || d.allowed("dev", "merge") || !d.allowed("uat", "publish") || d.allowed("main", "publish") || d.allowed("uat", "revoke") {
		t.Fatal("menu, button and branch operations must respect each grant scope")
	}
}
