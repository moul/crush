// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

//go:build windows

package gnovm

// Windows has no named pipes of this kind: no Next waits there.

type wakePipe struct{ name string }

func newWakePipe(dir string) *wakePipe { return nil }

func (p *wakePipe) rung() <-chan struct{} { return nil }

func (p *wakePipe) close() {}

func turnEnded(dir, session string) <-chan struct{} { return nil }
