package world

// GenerateCastle places an incredibly large castle structure into the world
// starting at (ox, oy, oz). The castle is centered on (ox, oz) with oy as
// the ground level. It uses SetBlock for each block so callers can broadcast
// changes to connected players.
//
// Returns all (x,y,z,state) tuples that were placed so the caller can
// broadcast them.
func GenerateCastle(ox, oy, oz int) []CastleBlock {
	var blocks []CastleBlock

	place := func(x, y, z int, state uint16) {
		blocks = append(blocks, CastleBlock{X: int32(x), Y: int32(y), Z: int32(z), State: state})
	}

	// Block states (id << 4 | meta)
	const (
		air           = 0
		stoneBrick    = 98 << 4        // stone bricks
		stoneBrickM   = 98<<4 | 1      // mossy stone bricks
		stoneBrickC   = 98<<4 | 2      // cracked stone bricks
		cobble        = 4 << 4         // cobblestone
		stoneSlab     = 44 << 4        // stone slab
		stoneSlabUp   = 44<<4 | 8      // upper stone slab
		oakPlanks     = 5 << 4         // oak planks
		sprucePlanks  = 5<<4 | 1       // spruce planks
		oakLog        = 17 << 4        // oak log
		spruceLog     = 17<<4 | 1      // spruce log
		oakStairsS    = 53<<4 | 3      // oak stairs facing south
		oakStairsN    = 53<<4 | 2      // oak stairs facing north
		oakStairsE    = 53 << 4        // oak stairs facing east
		oakStairsW    = 53<<4 | 1      // oak stairs facing west
		stoneStairsS  = 109<<4 | 3     // stone brick stairs facing south
		stoneStairsN  = 109<<4 | 2     // stone brick stairs facing north
		stoneStairsE  = 109 << 4       // stone brick stairs facing east
		stoneStairsW  = 109<<4 | 1     // stone brick stairs facing west
		glass         = 20 << 4        // glass
		glassPane     = 102 << 4       // glass pane
		ironBars      = 101 << 4       // iron bars
		torch         = 50<<4 | 5      // torch (standing)
		torchE        = 50<<4 | 1      // torch east wall
		torchW        = 50<<4 | 2      // torch west wall
		torchS        = 50<<4 | 3      // torch south wall
		torchN        = 50<<4 | 4      // torch north wall
		cobbleWall    = 139 << 4       // cobblestone wall
		oakFence      = 85 << 4        // oak fence
		carpet        = 171<<4 | 14    // red carpet
		woolRed       = 35<<4 | 14     // red wool
		woolWhite     = 35 << 4        // white wool
		goldBlock     = 41 << 4        // gold block
		bookshelf     = 47 << 4        // bookshelf
		crafting      = 58 << 4        // crafting table
		furnace       = 61<<4 | 2      // furnace facing north
		chest         = 54<<4 | 2      // chest facing north
		ladder        = 65<<4 | 2      // ladder facing north
		ladderS       = 65<<4 | 3      // ladder facing south
		ladderE       = 65<<4 | 4      // ladder facing east
		ladderW       = 65<<4 | 5      // ladder facing west
		waterSrc      = 9 << 4         // still water
		glowstone     = 89 << 4        // glowstone
		netherrack    = 87 << 4        // netherrack
		fire          = 51 << 4        // fire
		bannerBlack   = 176 << 4       // standing banner
		anvilBlock    = 145 << 4       // anvil
		ironBlock     = 42 << 4        // iron block
		obsidian      = 49 << 4        // obsidian
		oakDoor       = 64 << 4        // wooden door
		oakDoorTop    = 64<<4 | 8      // wooden door top
	)

	// ===================================================================
	// CASTLE DIMENSIONS - "incredibly large"
	// ===================================================================
	// Outer walls: 65x65 blocks, 12 blocks high
	// Corner towers: 9x9, 20 blocks high with peaked roofs
	// Gate towers: 7x7, 16 blocks high
	// Inner keep: 21x21, 18 blocks high with battlements
	// Central tower: 11x11, 30 blocks high
	// Courtyard with buildings, gardens, and decorations
	// ===================================================================

	const (
		outerR     = 32 // half-width of outer wall (65x65)
		wallH      = 12 // outer wall height
		towerR     = 4  // corner tower half-width (9x9)
		towerH     = 20 // corner tower height
		gateTowerR = 3  // gate tower half-width
		gateTowerH = 16 // gate tower height
		keepR      = 10 // keep half-width (21x21)
		keepH      = 18 // keep height
		centralR   = 5  // central tower half-width (11x11)
		centralH   = 30 // central tower height
	)

	// Helper: fill a rectangular volume
	fillBox := func(x1, y1, z1, x2, y2, z2 int, state uint16) {
		for x := x1; x <= x2; x++ {
			for y := y1; y <= y2; y++ {
				for z := z1; z <= z2; z++ {
					place(x, y, z, state)
				}
			}
		}
	}

	// Helper: hollow box (walls only)
	hollowBox := func(x1, y1, z1, x2, y2, z2 int, state uint16) {
		for x := x1; x <= x2; x++ {
			for y := y1; y <= y2; y++ {
				for z := z1; z <= z2; z++ {
					isWallX := x == x1 || x == x2
					isWallZ := z == z1 || z == z2
					if isWallX || isWallZ {
						place(x, y, z, state)
					}
				}
			}
		}
	}

	// Helper: place a tower (square, hollow, with battlements)
	buildTower := func(cx, cz, r, h int, wallState uint16) {
		x1, z1 := cx-r, cz-r
		x2, z2 := cx+r, cz+r

		// Foundation
		fillBox(x1, oy-1, z1, x2, oy-1, z2, cobble)

		// Floor
		fillBox(x1, oy, z1, x2, oy, z2, stoneBrick)

		// Walls
		for dy := 1; dy <= h; dy++ {
			for x := x1; x <= x2; x++ {
				for z := z1; z <= z2; z++ {
					isWallX := x == x1 || x == x2
					isWallZ := z == z1 || z == z2
					isCorner := isWallX && isWallZ
					if isCorner {
						place(x, oy+dy, z, wallState)
					} else if isWallX || isWallZ {
						// Arrow slits at regular intervals
						if dy == 4 || dy == 8 || dy == 12 || dy == 16 {
							if (x+z)%3 == 0 {
								place(x, oy+dy, z, air)
							} else {
								place(x, oy+dy, z, wallState)
							}
						} else {
							place(x, oy+dy, z, wallState)
						}
					} else {
						place(x, oy+dy, z, air) // interior
					}
				}
			}
		}

		// Battlements (crenellations)
		for x := x1; x <= x2; x++ {
			for z := z1; z <= z2; z++ {
				isEdge := x == x1 || x == x2 || z == z1 || z == z2
				if isEdge {
					if (x+z)%2 == 0 {
						place(x, oy+h+1, z, wallState)
					} else {
						place(x, oy+h+1, z, stoneSlab)
					}
				} else {
					// Floor at top
					place(x, oy+h, z, stoneBrick)
				}
			}
		}

		// Conical/stepped roof
		for level := 1; level <= r; level++ {
			rx := r - level
			for x := cx - rx; x <= cx+rx; x++ {
				for z := cz - rx; z <= cz+rx; z++ {
					place(x, oy+h+1+level, z, stoneBrick)
				}
			}
		}
		// Spire top
		place(cx, oy+h+2+r, cz, cobbleWall)
		place(cx, oy+h+3+r, cz, cobbleWall)

		// Torches inside
		place(cx, oy+2, cz, torch)

		// Ladder up the inside
		for dy := 1; dy <= h; dy++ {
			place(cx+r-1, oy+dy, cz+r-1, ladderE)
		}
	}

	// ===================================================================
	// 1. OUTER WALLS
	// ===================================================================
	// Foundation
	fillBox(ox-outerR, oy-1, oz-outerR, ox+outerR, oy-1, oz+outerR, cobble)

	// Ground floor
	fillBox(ox-outerR, oy, oz-outerR, ox+outerR, oy, oz+outerR, stoneBrick)

	// Walls (4 sides)
	for dy := 1; dy <= wallH; dy++ {
		for i := -outerR; i <= outerR; i++ {
			wallBlock := uint16(stoneBrick)
			if dy <= 2 {
				wallBlock = stoneBrickM // mossy at base
			}

			// North wall (z = oz - outerR)
			place(ox+i, oy+dy, oz-outerR, wallBlock)
			// South wall (z = oz + outerR)
			place(ox+i, oy+dy, oz+outerR, wallBlock)
			// West wall (x = ox - outerR)
			place(ox-outerR, oy+dy, oz+i, wallBlock)
			// East wall (x = ox + outerR)
			place(ox+outerR, oy+dy, oz+i, wallBlock)

			// Arrow slits
			if dy == 5 || dy == 9 {
				if i%4 == 0 && i != -outerR && i != outerR {
					place(ox+i, oy+dy, oz-outerR, air)
					place(ox+i, oy+dy, oz+outerR, air)
					place(ox-outerR, oy+dy, oz+i, air)
					place(ox+outerR, oy+dy, oz+i, air)
				}
			}
		}
	}

	// Wall-top walkway and battlements
	for i := -outerR; i <= outerR; i++ {
		// Floor of walkway (inside edge, 2 blocks wide)
		for w := 0; w < 3; w++ {
			place(ox+i, oy+wallH, oz-outerR+w, stoneBrick)
			place(ox+i, oy+wallH, oz+outerR-w, stoneBrick)
			place(ox-outerR+w, oy+wallH, oz+i, stoneBrick)
			place(ox+outerR-w, oy+wallH, oz+i, stoneBrick)
		}

		// Crenellations on outer edge
		if (i)%2 == 0 {
			place(ox+i, oy+wallH+1, oz-outerR, stoneBrick)
			place(ox+i, oy+wallH+1, oz+outerR, stoneBrick)
			place(ox-outerR, oy+wallH+1, oz+i, stoneBrick)
			place(ox+outerR, oy+wallH+1, oz+i, stoneBrick)
		}
	}

	// Torches along the wall walkway
	for i := -outerR + 4; i <= outerR-4; i += 6 {
		place(ox+i, oy+wallH+1, oz-outerR+1, torch)
		place(ox+i, oy+wallH+1, oz+outerR-1, torch)
		place(ox-outerR+1, oy+wallH+1, oz+i, torch)
		place(ox+outerR-1, oy+wallH+1, oz+i, torch)
	}

	// ===================================================================
	// 2. CORNER TOWERS (4 towers at corners of outer wall)
	// ===================================================================
	buildTower(ox-outerR, oz-outerR, towerR, towerH, stoneBrick)
	buildTower(ox+outerR, oz-outerR, towerR, towerH, stoneBrick)
	buildTower(ox-outerR, oz+outerR, towerR, towerH, stoneBrick)
	buildTower(ox+outerR, oz+outerR, towerR, towerH, stoneBrick)

	// ===================================================================
	// 3. MAIN GATE (South wall, centered)
	// ===================================================================
	gateWidth := 5
	gateHeight := 7

	// Clear the gate opening in the south wall
	for dx := -gateWidth / 2; dx <= gateWidth/2; dx++ {
		for dy := 1; dy <= gateHeight; dy++ {
			place(ox+dx, oy+dy, oz+outerR, air)
		}
	}

	// Gate arch
	place(ox-gateWidth/2-1, oy+gateHeight+1, oz+outerR, stoneStairsE)
	place(ox+gateWidth/2+1, oy+gateHeight+1, oz+outerR, stoneStairsW)
	for dx := -gateWidth / 2; dx <= gateWidth/2; dx++ {
		place(ox+dx, oy+gateHeight+1, oz+outerR, stoneBrick)
	}

	// Iron bars portcullis (partially open effect)
	for dx := -gateWidth / 2; dx <= gateWidth/2; dx++ {
		place(ox+dx, oy+gateHeight, oz+outerR, ironBars)
		place(ox+dx, oy+gateHeight-1, oz+outerR, ironBars)
	}

	// Gate towers flanking the entrance
	buildTower(ox-gateWidth/2-gateTowerR-1, oz+outerR, gateTowerR, gateTowerH, stoneBrick)
	buildTower(ox+gateWidth/2+gateTowerR+1, oz+outerR, gateTowerR, gateTowerH, stoneBrick)

	// Secondary gate on North wall
	for dx := -gateWidth / 2; dx <= gateWidth/2; dx++ {
		for dy := 1; dy <= gateHeight-2; dy++ {
			place(ox+dx, oy+dy, oz-outerR, air)
		}
	}
	for dx := -gateWidth / 2; dx <= gateWidth/2; dx++ {
		place(ox+dx, oy+gateHeight-1, oz-outerR, stoneBrick)
	}

	// ===================================================================
	// 4. INNER KEEP
	// ===================================================================
	keepX, keepZ := ox, oz-5 // slightly north of center

	// Foundation
	fillBox(keepX-keepR, oy-1, keepZ-keepR, keepX+keepR, oy-1, keepZ+keepR, cobble)

	// Ground floor
	fillBox(keepX-keepR, oy, keepZ-keepR, keepX+keepR, oy, keepZ+keepR, stoneBrick)

	// Walls of the keep
	for dy := 1; dy <= keepH; dy++ {
		for x := keepX - keepR; x <= keepX+keepR; x++ {
			for z := keepZ - keepR; z <= keepZ+keepR; z++ {
				isWallX := x == keepX-keepR || x == keepX+keepR
				isWallZ := z == keepZ-keepR || z == keepZ+keepR
				isCorner := isWallX && isWallZ

				if isCorner {
					place(x, oy+dy, z, stoneBrickC) // cracked at corners
				} else if isWallX || isWallZ {
					// Windows on every other level
					isWindowLevel := dy == 3 || dy == 7 || dy == 11 || dy == 15
					distFromCornerX := x - (keepX - keepR)
					distFromCornerZ := z - (keepZ - keepR)
					if distFromCornerX < 0 {
						distFromCornerX = -distFromCornerX
					}
					if distFromCornerZ < 0 {
						distFromCornerZ = -distFromCornerZ
					}
					isWindowPos := false
					if isWallX {
						isWindowPos = distFromCornerZ%4 == 2 && distFromCornerZ > 0 && distFromCornerZ < keepR*2
					} else {
						isWindowPos = distFromCornerX%4 == 2 && distFromCornerX > 0 && distFromCornerX < keepR*2
					}

					if isWindowLevel && isWindowPos {
						place(x, oy+dy, z, glassPane)
					} else {
						place(x, oy+dy, z, stoneBrick)
					}
				} else {
					// Interior floors at intervals
					if dy == 6 || dy == 10 || dy == 14 {
						place(x, oy+dy, z, oakPlanks) // floor
					} else {
						place(x, oy+dy, z, air) // hollow
					}
				}
			}
		}
	}

	// Keep battlements
	for x := keepX - keepR; x <= keepX+keepR; x++ {
		for z := keepZ - keepR; z <= keepZ+keepR; z++ {
			isEdge := x == keepX-keepR || x == keepX+keepR || z == keepZ-keepR || z == keepZ+keepR
			if isEdge {
				if (x+z)%2 == 0 {
					place(x, oy+keepH+1, z, stoneBrick)
				}
			}
		}
	}

	// Keep roof
	fillBox(keepX-keepR+1, oy+keepH, keepZ-keepR+1, keepX+keepR-1, oy+keepH, keepZ+keepR-1, sprucePlanks)

	// Keep entrance (south side)
	for dy := 1; dy <= 4; dy++ {
		place(keepX-1, oy+dy, keepZ+keepR, air)
		place(keepX, oy+dy, keepZ+keepR, air)
		place(keepX+1, oy+dy, keepZ+keepR, air)
	}
	place(keepX-1, oy+5, keepZ+keepR, stoneBrick)
	place(keepX, oy+5, keepZ+keepR, stoneBrick)
	place(keepX+1, oy+5, keepZ+keepR, stoneBrick)

	// Doors
	place(keepX, oy+1, keepZ+keepR, oakDoor|3)
	place(keepX, oy+2, keepZ+keepR, oakDoorTop)

	// Keep interior: ground floor furnishings
	// Throne (gold block + stairs)
	place(keepX, oy+1, keepZ-keepR+2, goldBlock)
	place(keepX, oy+2, keepZ-keepR+2, goldBlock)
	place(keepX-1, oy+1, keepZ-keepR+2, stoneStairsN)
	place(keepX+1, oy+1, keepZ-keepR+2, stoneStairsN)
	place(keepX, oy+1, keepZ-keepR+3, stoneStairsN)

	// Red carpet leading to throne
	for z := keepZ - keepR + 4; z <= keepZ+keepR-1; z++ {
		place(keepX, oy+1, z, carpet)
	}

	// Torches inside the keep
	for dy := 1; dy <= keepH; dy += 4 {
		place(keepX-keepR+1, oy+dy+1, keepZ-keepR+1, torch)
		place(keepX+keepR-1, oy+dy+1, keepZ-keepR+1, torch)
		place(keepX-keepR+1, oy+dy+1, keepZ+keepR-1, torch)
		place(keepX+keepR-1, oy+dy+1, keepZ+keepR-1, torch)
	}

	// Ladders between keep floors
	for dy := 1; dy <= keepH-1; dy++ {
		place(keepX+keepR-2, oy+dy, keepZ-keepR+1, ladderS)
	}

	// ===================================================================
	// 5. CENTRAL TOWER (tallest structure)
	// ===================================================================
	buildTower(keepX, keepZ, centralR, centralH, stoneBrickC)

	// Grand flag pole at the very top
	place(keepX, oy+centralH+centralR+4, keepZ, oakFence)
	place(keepX, oy+centralH+centralR+5, keepZ, oakFence)
	place(keepX, oy+centralH+centralR+6, keepZ, woolRed)
	place(keepX+1, oy+centralH+centralR+6, keepZ, woolRed)
	place(keepX, oy+centralH+centralR+7, keepZ, woolWhite)
	place(keepX+1, oy+centralH+centralR+7, keepZ, woolWhite)

	// ===================================================================
	// 6. COURTYARD BUILDINGS
	// ===================================================================

	// --- Barracks (east side of courtyard) ---
	bx, bz := ox+15, oz-10
	bw, bl, bh := 12, 7, 5
	fillBox(bx, oy, bz, bx+bw, oy, bz+bl, stoneBrick)
	hollowBox(bx, oy+1, bz, bx+bw, oy+bh, bz+bl, oakPlanks)
	// Fill interior with air
	fillBox(bx+1, oy+1, bz+1, bx+bw-1, oy+bh-1, bz+bl-1, air)
	// Roof
	fillBox(bx, oy+bh, bz, bx+bw, oy+bh, bz+bl, sprucePlanks)
	// Barracks door
	place(bx+bw/2, oy+1, bz+bl, air)
	place(bx+bw/2, oy+2, bz+bl, air)
	// Barracks windows
	for dx := 2; dx < bw; dx += 3 {
		place(bx+dx, oy+2, bz, glassPane)
		place(bx+dx, oy+2, bz+bl, glassPane)
	}
	// Beds (wool blocks as beds)
	for dx := 1; dx < bw; dx += 3 {
		place(bx+dx, oy+1, bz+1, woolRed)
		place(bx+dx, oy+1, bz+bl-1, woolRed)
	}
	// Interior torches
	place(bx+1, oy+3, bz+1, torch)
	place(bx+bw-1, oy+3, bz+bl-1, torch)

	// --- Blacksmith (west side) ---
	sx, sz := ox-25, oz-10
	sw, sl, sh := 9, 8, 5
	fillBox(sx, oy, sz, sx+sw, oy, sz+sl, cobble)
	hollowBox(sx, oy+1, sz, sx+sw, oy+sh, sz+sl, cobble)
	fillBox(sx+1, oy+1, sz+1, sx+sw-1, oy+sh-1, sz+sl-1, air)
	fillBox(sx, oy+sh, sz, sx+sw, oy+sh, sz+sl, stoneBrick)
	// Blacksmith door
	place(sx+sw, oy+1, sz+sl/2, air)
	place(sx+sw, oy+2, sz+sl/2, air)
	// Forge area (lava substitute: netherrack + fire)
	place(sx+1, oy, sz+1, netherrack)
	place(sx+1, oy+1, sz+1, fire)
	place(sx+2, oy, sz+1, netherrack)
	place(sx+2, oy+1, sz+1, fire)
	// Anvil and crafting
	place(sx+4, oy+1, sz+1, anvilBlock)
	place(sx+6, oy+1, sz+1, crafting)
	// Chest
	place(sx+1, oy+1, sz+sl-1, chest)
	// Torch
	place(sx+sw-1, oy+3, sz+1, torch)

	// --- Chapel (northeast corner of courtyard) ---
	cx, cz := ox+15, oz-22
	cw, cl := 7, 10
	ch := 8
	fillBox(cx, oy, cz, cx+cw, oy, cz+cl, stoneBrick)
	hollowBox(cx, oy+1, cz, cx+cw, oy+ch, cz+cl, stoneBrick)
	fillBox(cx+1, oy+1, cz+1, cx+cw-1, oy+ch-1, cz+cl-1, air)
	// Chapel peaked roof
	for dz := -1; dz <= cl+1; dz++ {
		place(cx-1, oy+ch+1, cz+dz, stoneStairsE)
		place(cx+cw+1, oy+ch+1, cz+dz, stoneStairsW)
		place(cx, oy+ch+2, cz+dz, stoneStairsE)
		place(cx+cw, oy+ch+2, cz+dz, stoneStairsW)
		place(cx+1, oy+ch+3, cz+dz, stoneStairsE)
		place(cx+cw-1, oy+ch+3, cz+dz, stoneStairsW)
		place(cx+2, oy+ch+4, cz+dz, stoneStairsE)
		place(cx+cw-2, oy+ch+4, cz+dz, stoneStairsW)
		place(cx+3, oy+ch+5, cz+dz, stoneBrick)
	}
	// Stained glass windows
	for dz := 2; dz <= cl-2; dz += 2 {
		place(cx, oy+3, cz+dz, glassPane)
		place(cx+cw, oy+3, cz+dz, glassPane)
		place(cx, oy+5, cz+dz, glassPane)
		place(cx+cw, oy+5, cz+dz, glassPane)
	}
	// Chapel entrance
	place(cx+cw/2, oy+1, cz+cl, air)
	place(cx+cw/2, oy+2, cz+cl, air)
	// Altar
	place(cx+cw/2, oy+1, cz+1, goldBlock)
	place(cx+cw/2, oy+2, cz+1, torch)
	// Pews
	for dz := 4; dz <= cl-2; dz += 2 {
		place(cx+2, oy+1, cz+dz, oakStairsN)
		place(cx+cw-2, oy+1, cz+dz, oakStairsN)
	}

	// --- Library (northwest corner of courtyard) ---
	lx, lz := ox-25, oz-25
	lw, ll := 10, 10
	lh := 6
	fillBox(lx, oy, lz, lx+lw, oy, lz+ll, oakPlanks)
	hollowBox(lx, oy+1, lz, lx+lw, oy+lh, lz+ll, oakPlanks)
	fillBox(lx+1, oy+1, lz+1, lx+lw-1, oy+lh-1, lz+ll-1, air)
	fillBox(lx, oy+lh, lz, lx+lw, oy+lh, lz+ll, sprucePlanks)
	// Bookshelves along walls
	for dx := 1; dx < lw; dx++ {
		place(lx+dx, oy+1, lz+1, bookshelf)
		place(lx+dx, oy+2, lz+1, bookshelf)
		place(lx+dx, oy+3, lz+1, bookshelf)
	}
	for dz := 2; dz < ll-1; dz++ {
		place(lx+1, oy+1, lz+dz, bookshelf)
		place(lx+1, oy+2, lz+dz, bookshelf)
	}
	// Library door
	place(lx+lw/2, oy+1, lz+ll, air)
	place(lx+lw/2, oy+2, lz+ll, air)
	// Reading tables (crafting table as desk)
	place(lx+5, oy+1, lz+5, crafting)
	place(lx+7, oy+1, lz+5, crafting)
	// Torches
	place(lx+lw-1, oy+3, lz+2, torch)
	place(lx+lw-1, oy+3, lz+ll-2, torch)

	// ===================================================================
	// 7. COURTYARD DECORATIONS
	// ===================================================================

	// --- Garden/Fountain in courtyard center (south of keep) ---
	fx, fz := ox, oz+12
	// Water pool (7x7)
	for dx := -3; dx <= 3; dx++ {
		for dz := -3; dz <= 3; dz++ {
			place(fx+dx, oy, fz+dz, stoneBrick) // rim
			if dx >= -2 && dx <= 2 && dz >= -2 && dz <= 2 {
				place(fx+dx, oy, fz+dz, waterSrc)
			}
		}
	}
	// Fountain pillar
	place(fx, oy+1, fz, stoneBrick)
	place(fx, oy+2, fz, stoneBrick)
	place(fx, oy+3, fz, cobbleWall)
	place(fx, oy+4, fz, waterSrc)

	// --- Paths from gate to keep ---
	for z := oz + outerR - 1; z >= keepZ+keepR+1; z-- {
		for dx := -2; dx <= 2; dx++ {
			place(ox+dx, oy, z, cobble)
		}
	}
	// Cross path east-west
	for x := ox - outerR + 1; x <= ox+outerR-1; x++ {
		place(x, oy, oz+5, cobble)
		place(x, oy, oz+6, cobble)
	}

	// --- Lamp posts along paths ---
	for z := oz + outerR - 5; z >= keepZ+keepR+5; z -= 8 {
		for _, dx := range []int{-4, 4} {
			place(ox+dx, oy+1, z, oakFence)
			place(ox+dx, oy+2, z, oakFence)
			place(ox+dx, oy+3, z, oakFence)
			place(ox+dx, oy+4, z, torch)
		}
	}

	// --- Guard posts along inner walkway ---
	for _, pos := range [][2]int{
		{ox - outerR + 5, oz - outerR + 5},
		{ox + outerR - 5, oz - outerR + 5},
		{ox - outerR + 5, oz + outerR - 5},
		{ox + outerR - 5, oz + outerR - 5},
	} {
		px, pz := pos[0], pos[1]
		place(px, oy+1, pz, oakFence)
		place(px, oy+2, pz, oakFence)
		place(px, oy+3, pz, torch)
	}

	// ===================================================================
	// 8. MOAT (water channel around the outer walls)
	// ===================================================================
	moatW := 4 // moat width
	for i := -outerR - moatW; i <= outerR+moatW; i++ {
		for w := 1; w <= moatW; w++ {
			outerEdge := outerR + w

			// North moat
			place(ox+i, oy-1, oz-outerEdge, waterSrc)
			place(ox+i, oy, oz-outerEdge, waterSrc)
			// South moat (skip gate area)
			if i < -gateWidth/2-1 || i > gateWidth/2+1 {
				place(ox+i, oy-1, oz+outerEdge, waterSrc)
				place(ox+i, oy, oz+outerEdge, waterSrc)
			}
			// West moat
			place(ox-outerEdge, oy-1, oz+i, waterSrc)
			place(ox-outerEdge, oy, oz+i, waterSrc)
			// East moat
			place(ox+outerEdge, oy-1, oz+i, waterSrc)
			place(ox+outerEdge, oy, oz+i, waterSrc)
		}
	}

	// Bridge over the moat at south gate
	for w := 1; w <= moatW; w++ {
		for dx := -gateWidth/2 - 1; dx <= gateWidth/2+1; dx++ {
			place(ox+dx, oy, oz+outerR+w, oakPlanks)
		}
	}

	// ===================================================================
	// 9. INTERIOR WALL TOWERS (mid-wall towers)
	// ===================================================================
	// Mid-wall towers on each side
	midTowerR := 3
	midTowerH := 14
	buildTower(ox, oz-outerR, midTowerR, midTowerH, stoneBrick)   // North mid
	buildTower(ox-outerR, oz, midTowerR, midTowerH, stoneBrick)   // West mid
	buildTower(ox+outerR, oz, midTowerR, midTowerH, stoneBrick)   // East mid

	// ===================================================================
	// 10. GLOWSTONE LIGHTING throughout courtyard
	// ===================================================================
	// Elevated lamp posts with glowstone in key areas
	for _, pos := range [][2]int{
		{ox - 10, oz + 5}, {ox + 10, oz + 5},
		{ox - 10, oz + 20}, {ox + 10, oz + 20},
		{ox - 10, oz - 15}, {ox + 10, oz - 15},
	} {
		px, pz := pos[0], pos[1]
		place(px, oy+1, pz, oakFence)
		place(px, oy+2, pz, oakFence)
		place(px, oy+3, pz, glowstone)
	}

	return blocks
}

// CastleBlock represents a single block placement in the castle.
type CastleBlock struct {
	X, Y, Z int32
	State   uint16
}
