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
