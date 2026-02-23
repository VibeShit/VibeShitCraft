package world

import "testing"

func TestGenerateCastleReturnsBlocks(t *testing.T) {
	blocks := GenerateCastle(0, 64, 0)
	if len(blocks) == 0 {
		t.Fatal("GenerateCastle returned no blocks")
	}
	// The castle should be "incredibly large" — expect many thousands of blocks
	if len(blocks) < 5000 {
		t.Errorf("Castle too small: got %d blocks, want at least 5000", len(blocks))
	}
}

func TestGenerateCastleSpan(t *testing.T) {
	blocks := GenerateCastle(0, 64, 0)

	var minX, maxX, minZ, maxZ int32
	minX, maxX = blocks[0].X, blocks[0].X
	minZ, maxZ = blocks[0].Z, blocks[0].Z

	for _, b := range blocks {
		if b.X < minX {
			minX = b.X
		}
		if b.X > maxX {
			maxX = b.X
		}
		if b.Z < minZ {
			minZ = b.Z
		}
		if b.Z > maxZ {
			maxZ = b.Z
		}
	}

	spanX := maxX - minX
	spanZ := maxZ - minZ

	// The castle outer walls are 65 blocks wide plus moat; expect at least 70 block span
	if spanX < 70 {
		t.Errorf("Castle X span too small: %d (min=%d, max=%d)", spanX, minX, maxX)
	}
	if spanZ < 70 {
		t.Errorf("Castle Z span too small: %d (min=%d, max=%d)", spanZ, minZ, maxZ)
	}
}

func TestGenerateCastleHeight(t *testing.T) {
	oy := 64
	blocks := GenerateCastle(0, oy, 0)

	var maxY int32
	for _, b := range blocks {
		if b.Y > maxY {
			maxY = b.Y
		}
	}

	// Central tower is 30 high + roof + flag, should reach well above oy+30
	if int(maxY)-oy < 30 {
		t.Errorf("Castle not tall enough: maxY=%d, oy=%d, height=%d", maxY, oy, int(maxY)-oy)
	}
}

func TestGenerateCastleAtDifferentOrigins(t *testing.T) {
	// Castle should work at any origin
	for _, origin := range [][3]int{{100, 64, 200}, {-50, 70, -50}, {0, 4, 0}} {
		blocks := GenerateCastle(origin[0], origin[1], origin[2])
		if len(blocks) == 0 {
			t.Errorf("GenerateCastle(%d,%d,%d) returned no blocks", origin[0], origin[1], origin[2])
		}
	}
}

func TestGenerateCastleDeterministic(t *testing.T) {
	blocks1 := GenerateCastle(0, 64, 0)
	blocks2 := GenerateCastle(0, 64, 0)

	if len(blocks1) != len(blocks2) {
		t.Fatalf("Non-deterministic: first call %d blocks, second call %d blocks", len(blocks1), len(blocks2))
	}

	for i := range blocks1 {
		if blocks1[i] != blocks2[i] {
			t.Fatalf("Block %d differs: %v vs %v", i, blocks1[i], blocks2[i])
		}
	}
}
