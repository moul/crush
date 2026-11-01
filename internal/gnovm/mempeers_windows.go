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

// Windows has no named pipes of this kind: no session is taken for working,
// and Next answers as it does for one session.

func turnHeld(dir, session string) bool { return false }

func (h *memHost) ring() {}

func (h *memHost) activate(name string) bool { return false }

func (h *memHost) pendingPipe(name string) bool { return false }
