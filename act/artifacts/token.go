// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifacts

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"strings"
)

// BackendIDs reads the run and job IDs the results service expects from the unverified claims
// of a runtime token, the same way the toolkit does.
func BackendIDs(token string) (runID, jobID string, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", errors.New("the runtime token is not a JWT, artifacts v4 need Gitea 1.22 or later")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", "", err
	}
	var claims struct {
		Scp string `json:"scp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", err
	}
	for scope := range strings.FieldsSeq(claims.Scp) {
		if fields := strings.Split(scope, ":"); fields[0] == "Actions.Results" && len(fields) == 3 {
			return fields[1], fields[2], nil
		}
	}
	return "", "", errors.New("the runtime token has no Actions.Results scope")
}

// LocalRuntimeToken is an unsigned token the local artifact server accepts, shaped like Gitea's
// so that the toolkit finds its backend IDs in it.
func LocalRuntimeToken(runID string) string {
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]string{"scp": "Actions.Results:" + runID + ":1", "jti": hex.EncodeToString(nonce)})
	return header + "." + base64.RawURLEncoding.EncodeToString(claims) + "." + hex.EncodeToString(nonce)
}
