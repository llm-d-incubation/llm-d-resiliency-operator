// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

// Package journal stores the controller's record. See docs/design/contracts/journal.md.
package journal

import (
	"context"
	"errors"
)

type Journal interface {
	Load(ctx context.Context) (data []byte, version string, err error)
	Save(ctx context.Context, data []byte, version string) (string, error)
}

var ErrConflict = errors.New("journal conflict")
