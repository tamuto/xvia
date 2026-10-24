//go:build !linux

package detector

import "github.com/tamuto/xvia/internal/logx"

// Detector is only implemented on Linux.
type Detector struct{}

func New(root int, log *logx.Logger) *Detector { return &Detector{} }

func (d *Detector) Scan() map[int][]string { return map[int][]string{} }
