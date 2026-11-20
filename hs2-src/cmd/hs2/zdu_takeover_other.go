//go:build !linux

package main

import "context"

// ZDU S6 is linux-only (fd passing); elsewhere these are no-ops.

func zduStart(context.Context, func(string, ...any), func()) {}

func zduTakeReady(func(string, ...any)) {}

func zduNoteTun(any) {}

func zduSetDrainSink(func(int)) {}
