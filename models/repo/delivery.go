// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import "gitea.dev/models/db"

type DeliverySettings struct {
	RepoID       int64 `xorm:"pk"`
	Development  string
	AgentID      int64
	Instructions string `xorm:"TEXT"`
}

type DeliveryGrant struct {
	ID      int64  `xorm:"pk autoincr"`
	RepoID  int64  `xorm:"UNIQUE(delivery_scope)"`
	UserID  int64  `xorm:"UNIQUE(delivery_scope)"`
	Branch  string `xorm:"UNIQUE(delivery_scope)"`
	Buttons string
}

func init() {
	db.RegisterModel(new(DeliverySettings))
	db.RegisterModel(new(DeliveryGrant))
}

type DeliveryEnvironment struct {
	ID         int64  `xorm:"pk autoincr"`
	RepoID     int64  `xorm:"UNIQUE(delivery_environment)"`
	Branch     string `xorm:"UNIQUE(delivery_environment)"`
	Ciphertext string `xorm:"TEXT"`
}

type DeliveryAgentKey struct {
	RepoID  int64 `xorm:"pk"`
	AgentID int64
	Digest  string
}

func init() {
	db.RegisterModel(new(DeliveryEnvironment))
	db.RegisterModel(new(DeliveryAgentKey))
}
