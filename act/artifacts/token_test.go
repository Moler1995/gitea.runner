// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifacts

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackendIDs(t *testing.T) {
	run, job, err := BackendIDs(LocalRuntimeToken("42"))
	require.NoError(t, err)
	assert.Equal(t, []string{"42", "1"}, []string{run, job})

	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"scp":"Actions.GenericRead:x Actions.Results:7:9","TaskID":3}`))
	run, job, err = BackendIDs("e30." + claims + ".sig")
	require.NoError(t, err)
	assert.Equal(t, []string{"7", "9"}, []string{run, job})

	_, _, err = BackendIDs("0123456789abcdef") // the task token older Gitea hands out
	require.ErrorContains(t, err, "Gitea 1.22")
	_, _, err = BackendIDs("e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"scp":"Actions.GenericRead:x"}`)) + ".sig")
	require.ErrorContains(t, err, "Actions.Results")
}
