package main

import (
	"sync"
)

const (
	pluginMaxDepth      = 10
	maxSkillNameLen     = 80
	maxFrontmatterBytes = 64 * 1024
	projectSkillSource  = "project"
	managedSkillSource  = "managed"
)

type manager struct {
	paths                paths
	remote               *remoteRegistry
	skillsMP             *skillsMPRegistry
	remoteStore          *remoteSkillStore
	global               bool
	runtimeOnce          sync.Once
	javascriptRuntime    string
	javascriptRuntimeErr error
}
