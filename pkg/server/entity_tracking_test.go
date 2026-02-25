package server

import (
	"net"
	"testing"
)

func TestShouldTrack(t *testing.T) {
	srv := New(DefaultConfig())
	viewer := &Player{X: 0, Y: 0, Z: 0}

	tests := []struct {
		name       string
		ex, ey, ez float64
		want       bool
	}{
		{"In range", 10, 0, 0, true},
		{"Exactly at range", EntityTrackingRange, 0, 0, true},
		{"Out of range", EntityTrackingRange + 1, 0, 0, false},
		{"Far away", 100, 100, 100, false},
	}

	for _, tt := range tests {
		got := srv.shouldTrack(viewer, tt.ex, tt.ey, tt.ez)
		if got != tt.want {
			t.Errorf("%s: shouldTrack = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestUpdateEntityTracking_Spawn(t *testing.T) {
	srv := New(DefaultConfig())

	p1 := &Player{EntityID: 1, X: 0, Y: 0, Z: 0, trackedEntities: make(map[int32]bool), loadedChunks: make(map[ChunkPos]bool)}
	p2 := &Player{EntityID: 2, X: 10, Y: 0, Z: 0, trackedEntities: make(map[int32]bool), loadedChunks: make(map[ChunkPos]bool)}

	srv.mu.Lock()
	srv.players[1] = p1
	srv.players[2] = p2
	srv.mu.Unlock()

	// Initial state: p1 does not track p2
	if p1.trackedEntities[2] {
		t.Fatal("p1 should not track p2 initially")
	}

	// Update tracking
	srv.updateEntityTracking(p1)

	// Result: p1 should now track p2
	if !p1.trackedEntities[2] {
		t.Error("p1 should track p2 after update")
	}
}

func TestUpdateEntityTracking_Despawn(t *testing.T) {
	srv := New(DefaultConfig())

	p1 := &Player{EntityID: 1, X: 0, Y: 0, Z: 0, trackedEntities: make(map[int32]bool), loadedChunks: make(map[ChunkPos]bool)}
	p2 := &Player{EntityID: 2, X: EntityTrackingRange + 10, Y: 0, Z: 0, trackedEntities: make(map[int32]bool), loadedChunks: make(map[ChunkPos]bool)}

	// Pre-set p1 tracking p2 even though p2 is far away
	p1.trackedEntities[2] = true

	srv.mu.Lock()
	srv.players[1] = p1
	srv.players[2] = p2
	srv.mu.Unlock()

	// Update tracking
	srv.updateEntityTracking(p1)

	// Result: p1 should no longer track p2
	if p1.trackedEntities[2] {
		t.Error("p1 should NOT track p2 after update (out of range)")
	}
}

func TestTeleportSync(t *testing.T) {
	srv := New(DefaultConfig())

	p1 := &Player{EntityID: 1, X: 0, Y: 0, Z: 0, trackedEntities: make(map[int32]bool), loadedChunks: make(map[ChunkPos]bool)}
	p2 := &Player{EntityID: 2, X: 100, Y: 0, Z: 0, trackedEntities: make(map[int32]bool), loadedChunks: make(map[ChunkPos]bool)}

	srv.mu.Lock()
	srv.players[1] = p1
	srv.players[2] = p2
	srv.mu.Unlock()

	// Initial tracking check
	srv.updateEntityTracking(p1)
	if p1.trackedEntities[2] {
		t.Fatal("p1 should not track p2 while far away")
	}

	// Teleport p1 closer to p2
	srv.teleportPlayer(p1, 90, 0, 0)

	// teleportPlayer calls updateEntityTracking(p1)
	if !p1.trackedEntities[2] {
		t.Error("p1 should track p2 after teleporting closer")
	}
}

func TestSpawnMobRegistersTracking(t *testing.T) {
	srv := New(DefaultConfig())
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	p1 := &Player{
		EntityID:        1,
		UUID:            [16]byte{1},
		Username:        "SpawnObserver",
		Conn:            c1,
		X:               0,
		Y:               0,
		Z:               0,
		trackedEntities: make(map[int32]bool),
	}
	srv.players[p1.EntityID] = p1

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := c2.Read(buf); err != nil {
				return
			}
		}
	}()

	// Spawn mob near player - should be tracked
	srv.SpawnMob(5, 5, 5, 90)

	// Spawn mob far away - should NOT be tracked
	srv.SpawnMob(1000, 5, 1000, 90)

	p1.mu.Lock()
	tracked := len(p1.trackedEntities)
	p1.mu.Unlock()

	if tracked != 1 {
		t.Errorf("Expected exactly 1 tracked entity (the near mob), got %d", tracked)
	}
}
