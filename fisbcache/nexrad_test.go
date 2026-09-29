package fisbcache

import "testing"

func TestNexradTileIdentity_SameTileSameIdentity(t *testing.T) {
	a := NexradTileIdentity(63, 0, 40.0001, -120.0001, 1.0, 1.0)
	b := NexradTileIdentity(63, 0, 40.0001, -120.0001, 1.0, 1.0)
	if a != b {
		t.Errorf("identical tiles produced different identities: %q vs %q", a, b)
	}
}

func TestNexradTileIdentity_DifferentTileDifferentIdentity(t *testing.T) {
	a := NexradTileIdentity(63, 0, 40.0, -120.0, 1.0, 1.0)
	b := NexradTileIdentity(63, 0, 41.0, -120.0, 1.0, 1.0)
	if a == b {
		t.Error("different tile locations produced the same identity")
	}
	c := NexradTileIdentity(64, 0, 40.0, -120.0, 1.0, 1.0)
	if a == c {
		t.Error("different radar types produced the same identity")
	}
}

func TestNexradKey_ClassIsNexradTile(t *testing.T) {
	k := NexradKey(63, 0, 40.0, -120.0, 1.0, 1.0)
	if k.Class != ClassNexradTile {
		t.Errorf("got class %q", k.Class)
	}
}
