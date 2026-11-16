// Copyright 2026 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"cmp"
	"testing"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStepFactoryNewStepAppliesReplaceUsesToUsesOnly(t *testing.T) {
	replacements := map[string]string{"": "docker://image:tag", "remote/replaced@v1": "docker://image:tag"}
	rc := &RunContext{Config: &Config{ReplaceUses: func(uses string) string { return cmp.Or(replacements[uses], uses) }}}
	for _, test := range []struct {
		name  string
		model *model.Step
		want  step
	}{
		{"StepRemoteAction", &model.Step{Uses: "remote/action@v1"}, &stepActionRemote{}},
		{"StepLocalAction", &model.Step{Uses: "./action@v1"}, &stepActionLocal{}},
		{"StepDocker", &model.Step{Uses: "docker://image:tag"}, &stepDocker{}},
		{"StepReplaced", &model.Step{Uses: "remote/replaced@v1"}, &stepDocker{}},
		{"StepRun", &model.Step{Run: "cmd"}, &stepRun{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			step, err := (&stepFactoryImpl{}).newStep(test.model, rc)
			require.NoError(t, err)
			assert.IsType(t, test.want, step)
		})
	}
}

func TestStepFactoryInvalidStepStaysInvalidWhenReplaceUsesEmptiesUses(t *testing.T) {
	rc := &RunContext{Name: "build", Config: &Config{ReplaceUses: func(string) string { return "" }}}
	for _, stepModel := range []*model.Step{
		{Uses: "remote/action@v1", Run: "cmd"},
	} {
		_, err := (&stepFactoryImpl{}).newStep(stepModel, rc)
		assert.ErrorContains(t, err, "job:build ", stepModel.Uses)
	}
}
