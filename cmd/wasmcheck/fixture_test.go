package main

import "github.com/me/metaai2api/internal/meta/dgw"

// frameFixture is the frame describe() is pinned against.
func frameFixture() dgw.Frame {
	return dgw.Frame{
		Direction: dgw.DirectionInbound,
		Type:      dgw.FramePing,
		StreamID:  7,
	}
}
