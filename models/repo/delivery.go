// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"gitea.dev/models/db"
	"time"
)

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
	ID           int64  `xorm:"pk autoincr"`
	RepoID       int64  `xorm:"UNIQUE(delivery_environment)"`
	Branch       string `xorm:"UNIQUE(delivery_environment)"`
	Name         string
	Cloud        string
	Region       string
	Workflow     string
	TriggerEvent string
	Ciphertext   string `xorm:"TEXT"`
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

type DeliveryRecord struct {
	ID       int64 `xorm:"pk autoincr"`
	RepoID   int64 `xorm:"index"`
	Branch   string
	Kind     string `xorm:"index"`
	Subject  string
	Revision string
	Actor    string
	Detail   string `xorm:"TEXT"`
	Created  int64  `xorm:"created"`
}

func init() { db.RegisterModel(new(DeliveryRecord)) }

type DeliveryToolGrant struct {
	ID      int64 `xorm:"pk autoincr"`
	RepoID  int64 `xorm:"index"`
	AgentID int64
	Login   string
	Service string
	Tools   string
	Digest  string `xorm:"unique"`
}

func init() { db.RegisterModel(new(DeliveryToolGrant)) }

func (r DeliveryRecord) CreatedAt() string { return time.Unix(r.Created, 0).Format("01-02 15:04") }
