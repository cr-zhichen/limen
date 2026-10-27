package limen

import _ "embed"

//go:embed config.example.toml
var ConfigExample string

//go:embed internal/skill/SKILL.md
var Skill string

//go:embed .agents/skills/limen/SKILL.md
var SkillStub string
