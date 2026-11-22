// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"strings"
	"testing"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMaxParallelStrategy(t *testing.T) {
	for limit, want := range map[string]int{"1": 1, "": 4, "10": 5, "tow": 4} {
		assert.Equal(t, want, maxParallelFor(&model.Strategy{MaxParallelString: limit}, 5), limit)
	}

	t.Run("deferred strategy", func(t *testing.T) {
		workflow, err := model.ReadWorkflow(strings.NewReader(`
jobs:
  test:
    if: false
    strategy: ${{ fromJSON('{"max-parallel":2,"fail-fast":false,"matrix":{"os":["ubuntu","windows"]}}') }}
`))
		require.NoError(t, err)
		job := workflow.Jobs["test"]
		rawStrategy := model.CloneYamlNode(job.RawStrategy)
		require.NoError(t, (&runnerImpl{config: &Config{Env: testCheckoutEnv()}}).NewPlanExecutor(&model.Plan{Stages: []*model.Stage{{Runs: []*model.Run{{Workflow: workflow, JobID: "test"}}}}})(t.Context()))
		require.NotNil(t, job.Strategy)
		assert.False(t, job.Strategy.GetFailFast())
		assert.Equal(t, 2, maxParallelFor(job.Strategy, 5))
		matrixes, err := job.GetMatrixes()
		require.NoError(t, err)
		assert.Equal(t, []map[string]any{{"os": "ubuntu"}, {"os": "windows"}}, matrixes)
		assert.Equal(t, rawStrategy, job.RawStrategy)
	})
}

func TestNewPlanExecutorInvalidMatrix(t *testing.T) {
	runner := &runnerImpl{config: &Config{Env: testCheckoutEnv()}}
	for strategy, want := range map[string]string{
		`{matrix: {config: {nested: value}}}`:               "could not get job matrix:",
		`${{ fromJSON('invalid') }}`:                        "job strategy",
		`${{ '${{ inputs.unresolved }}' }}`:                 "job strategy",
		`{matrix: {x: [a, "${{ fromJSON('not json') }}"]}}`: "Error when evaluating 'strategy' for job 'job1'.",
	} {
		t.Run(strategy, func(t *testing.T) {
			workflow, err := model.ReadWorkflow(strings.NewReader("jobs:\n  job1:\n    strategy: " + strategy))
			require.NoError(t, err)
			plan, err := model.CombineWorkflowPlanner(workflow).PlanAll()
			require.NoError(t, err)
			require.ErrorContains(t, runner.NewPlanExecutor(plan)(t.Context()), want)
			assert.Equal(t, "failure", workflow.Jobs["job1"].Result)
		})
	}
}
