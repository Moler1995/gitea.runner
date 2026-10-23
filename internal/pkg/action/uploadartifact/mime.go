// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package uploadartifact

import (
	"path"
	"strings"
)

// mimeTypes is the toolkit's table for files uploaded without archiving.
var mimeTypes = map[string]string{
	".txt":   "text/plain",
	".html":  "text/html",
	".htm":   "text/html",
	".css":   "text/css",
	".csv":   "text/csv",
	".xml":   "text/xml",
	".md":    "text/markdown",
	".js":    "application/javascript",
	".mjs":   "application/javascript",
	".json":  "application/json",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".svg":   "image/svg+xml",
	".webp":  "image/webp",
	".ico":   "image/x-icon",
	".bmp":   "image/bmp",
	".tiff":  "image/tiff",
	".tif":   "image/tiff",
	".mp3":   "audio/mpeg",
	".wav":   "audio/wav",
	".ogg":   "audio/ogg",
	".flac":  "audio/flac",
	".mp4":   "video/mp4",
	".webm":  "video/webm",
	".avi":   "video/x-msvideo",
	".mov":   "video/quicktime",
	".pdf":   "application/pdf",
	".doc":   "application/msword",
	".docx":  "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":   "application/vnd.ms-excel",
	".xlsx":  "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":   "application/vnd.ms-powerpoint",
	".pptx":  "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".zip":   "application/zip",
	".tar":   "application/x-tar",
	".gz":    "application/gzip",
	".rar":   "application/vnd.rar",
	".7z":    "application/x-7z-compressed",
	".wasm":  "application/wasm",
	".yaml":  "application/x-yaml",
	".yml":   "application/x-yaml",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
	".eot":   "application/vnd.ms-fontobject",
}

func mimeTypeOf(name string) string {
	if mimeType, ok := mimeTypes[strings.ToLower(path.Ext(name))]; ok {
		return mimeType
	}
	return "application/octet-stream"
}
