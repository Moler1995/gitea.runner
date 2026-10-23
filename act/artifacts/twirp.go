// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifacts

import (
	"encoding/json/jsontext"
	"strconv"
	"time"
)

// ArtifactServicePath is the twirp service the v4 artifact clients talk to.
const ArtifactServicePath = "/twirp/github.actions.results.api.v1.ArtifactService"

// The ArtifactService messages in the protobuf JSON mapping. Requests are written with the proto
// names as the toolkit sends them, responses in camelCase as Gitea answers, and either reads both.
type (
	CreateArtifactRequest struct {
		WorkflowRunBackendID    string     `json:"workflow_run_backend_id,case:ignore"`
		WorkflowJobRunBackendID string     `json:"workflow_job_run_backend_id,case:ignore"`
		Name                    string     `json:"name"`
		ExpiresAt               *time.Time `json:"expires_at,omitempty,case:ignore"`
		Version                 int32      `json:"version"`
		MimeType                *string    `json:"mime_type,omitempty,case:ignore"`
	}
	CreateArtifactResponse struct {
		Ok              bool   `json:"ok"`
		SignedUploadURL string `json:"signedUploadUrl,case:ignore"`
	}

	FinalizeArtifactRequest struct {
		WorkflowRunBackendID    string  `json:"workflow_run_backend_id,case:ignore"`
		WorkflowJobRunBackendID string  `json:"workflow_job_run_backend_id,case:ignore"`
		Name                    string  `json:"name"`
		Size                    Int64   `json:"size"`
		Hash                    *string `json:"hash,omitempty"`
	}
	FinalizeArtifactResponse struct {
		Ok         bool  `json:"ok"`
		ArtifactID Int64 `json:"artifactId,case:ignore"`
	}

	ListArtifactsRequest struct {
		WorkflowRunBackendID    string  `json:"workflow_run_backend_id,case:ignore"`
		WorkflowJobRunBackendID string  `json:"workflow_job_run_backend_id,case:ignore"`
		NameFilter              *string `json:"name_filter,omitempty,case:ignore"`
		IDFilter                *Int64  `json:"id_filter,omitempty,case:ignore"`
	}
	ListArtifactsResponse struct {
		Artifacts []MonolithArtifact `json:"artifacts"`
	}
	MonolithArtifact struct {
		WorkflowRunBackendID    string     `json:"workflowRunBackendId,case:ignore"`
		WorkflowJobRunBackendID string     `json:"workflowJobRunBackendId,case:ignore"`
		DatabaseID              Int64      `json:"databaseId,case:ignore"`
		Name                    string     `json:"name"`
		Size                    Int64      `json:"size"`
		CreatedAt               *time.Time `json:"createdAt,omitempty,case:ignore"`
		Digest                  *string    `json:"digest,omitempty"`
	}

	GetSignedArtifactURLRequest struct {
		WorkflowRunBackendID    string `json:"workflow_run_backend_id,case:ignore"`
		WorkflowJobRunBackendID string `json:"workflow_job_run_backend_id,case:ignore"`
		Name                    string `json:"name"`
	}
	GetSignedArtifactURLResponse struct {
		SignedURL string `json:"signedUrl,case:ignore"`
	}

	DeleteArtifactRequest struct {
		WorkflowRunBackendID    string `json:"workflow_run_backend_id,case:ignore"`
		WorkflowJobRunBackendID string `json:"workflow_job_run_backend_id,case:ignore"`
		Name                    string `json:"name"`
	}
	DeleteArtifactResponse struct {
		Ok         bool  `json:"ok"`
		ArtifactID Int64 `json:"artifactId,case:ignore"`
	}
)

// Int64 is written as the JSON string the mapping prescribes, and read from a string or a number.
type Int64 int64

func (n Int64) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.String(strconv.FormatInt(int64(n), 10)))
}

func (n *Int64) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	val, err := dec.ReadValue()
	if err != nil {
		return err
	}
	digits := []byte(val)
	switch val.Kind() {
	case 'n':
		return nil
	case '"':
		if digits, err = jsontext.AppendUnquote(nil, val); err != nil {
			return err
		}
	}
	parsed, err := strconv.ParseInt(string(digits), 10, 64)
	*n = Int64(parsed)
	return err
}
