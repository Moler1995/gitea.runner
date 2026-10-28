// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifacts

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json/v2"
	"time"
)

func LocalRuntimeToken(runID string, lifetime time.Duration) string { // an unsecured JWT with the claims the toolkit and docker buildx read, its jti making it unguessable as the cache server's bearer
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{
		"scp": "Actions.Results:" + runID + ":1",
		"ac":  `[{"Scope":"","Permission":3}]`,
		"nbf": now.Unix(),
		"exp": now.Add(lifetime).Unix(),
		"jti": rand.Text(),
	})
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims) + "."
}
