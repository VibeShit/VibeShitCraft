package world

import "sync"

// VillageGrid handles deterministic village placement on a sparse grid.
// Every VILLAGE_CELL_SIZE blocks in X and Z forms a grid cell; each cell
// independently rolls whether it contains a village center.
const villageCellSize = 96

// roadSegment represents one axis-aligned road segment in the village road tree.
type roadSegment struct {
	sx, sz, ex, ez int
	horizontal     bool
	children       []roadSegment
	wobbleSeed     int
}

// bbox represents a 2D axis-aligned bounding box.
type bbox struct {
	minX, minZ, maxX, maxZ int
}

// VillagePlan encapsulates all deterministic layout data for a village.
type VillagePlan struct {
	VX, VZ    int // Well center
	Roads     []roadSegment
	Buildings []struct {
		Site buildingSite
		Type int
		Box  bbox
	}
	Farms []struct {
		X, Z int
		Box  bbox
	}
}

type buildingSite struct {
	doorX, doorZ int
	facing       int // 0=+Z, 1=-Z, 2=+X, 3=-X
	smallOnly    bool
}

// VillageGrid carries the world seed used for per-cell rolls.
type cellKey struct {
	x, z int
}

type centerResult struct {
	wx, wz int
	ok     bool
}

type VillageGrid struct {
	seed       int64
	tempNoise  *Perlin
	rainNoise  *Perlin
	heightFunc func(x, z int) int

	mu          sync.RWMutex
	centerCache map[cellKey]centerResult
	planCache   map[cellKey]*VillagePlan
}

// NewVillageGrid creates a VillageGrid for the given world seed and biome noise.
func NewVillageGrid(seed int64, tempNoise, rainNoise *Perlin, heightFunc func(x, z int) int) *VillageGrid {
	return &VillageGrid{
		seed:        seed,
		tempNoise:   tempNoise,
		rainNoise:   rainNoise,
		heightFunc:  heightFunc,
		centerCache: make(map[cellKey]centerResult),
		planCache:   make(map[cellKey]*VillagePlan),
	}
}

// cellHash returns a deterministic value in [0, mod) for cell (cx, cz).
func (v *VillageGrid) cellHash(cx, cz, mod int64) int64 {
	const k1 int64 = -7046029254386353131 // splitmix64 step 1
	const k2 int64 = -4265267296055464877 // splitmix64 step 2
	h := v.seed ^ (cx * k1) ^ (cz * 7823434773480878946)
	h ^= h >> 33
	h *= k1
	h ^= h >> 27
	h *= k2
	h ^= h >> 31
	if h < 0 {
		h = -h
	}
	return h % mod
}

// villageCenter returns the world-space (x, z) center of a village in grid cell
// (cellX, cellZ), and ok=true if that cell contains a village (25% chance)
// AND no neighboring village is too close (minimum 80 blocks apart).
func (v *VillageGrid) villageCenter(cellX, cellZ int) (int, int, bool) {
	key := cellKey{cellX, cellZ}
	v.mu.RLock()
	if res, ok := v.centerCache[key]; ok {
		v.mu.RUnlock()
		return res.wx, res.wz, res.ok
	}
	v.mu.RUnlock()

	wx, wz, ok := v.calculateVillageCenter(cellX, cellZ)

	v.mu.Lock()
	v.centerCache[key] = centerResult{wx, wz, ok}
	v.mu.Unlock()

	return wx, wz, ok
}

func (v *VillageGrid) calculateVillageCenter(cellX, cellZ int) (wx, wz int, ok bool) {
	cx := int64(cellX)
	cz := int64(cellZ)
	// ~25% of cells get a village
	if v.cellHash(cx, cz, 4) != 0 {
		return 0, 0, false
	}
	// Offset within the cell so villages aren't always at the corner
	ox := int(v.cellHash(cx^0xDEAD, cz^0xBEEF, int64(villageCellSize-20))) + 10
	oz := int(v.cellHash(cx^0xCAFE, cz^0xF00D, int64(villageCellSize-20))) + 10
	wx = cellX*villageCellSize + ox
	wz = cellZ*villageCellSize + oz

	// Check neighboring cells — suppress this village if a neighbor is too close.
	// Only yield to neighbors with a lower cell hash (deterministic tie-breaking).
	const minDist = 80
	myPriority := v.cellHash(cx^0x1234, cz^0x5678, 1000)
	for dx := -1; dx <= 1; dx++ {
		for dz := -1; dz <= 1; dz++ {
			if dx == 0 && dz == 0 {
				continue
			}
			ncx := int64(cellX + dx)
			ncz := int64(cellZ + dz)
			if v.cellHash(ncx, ncz, 4) != 0 {
				continue // neighbor cell has no village
			}
			nox := int(v.cellHash(ncx^0xDEAD, ncz^0xBEEF, int64(villageCellSize-20))) + 10
			noz := int(v.cellHash(ncx^0xCAFE, ncz^0xF00D, int64(villageCellSize-20))) + 10
			nwx := (cellX+dx)*villageCellSize + nox
			nwz := (cellZ+dz)*villageCellSize + noz

			ddx := wx - nwx
			ddz := wz - nwz
			if ddx < 0 {
				ddx = -ddx
			}
			if ddz < 0 {
				ddz = -ddz
			}
			if ddx+ddz < minDist {
				// Too close — suppress the one with higher priority value
				neighborPriority := v.cellHash(ncx^0x1234, ncz^0x5678, 1000)
				if myPriority >= neighborPriority {
					return 0, 0, false
				}
			}
		}
	}

	// Check terrain height at center — don't build if overhanging water!
	if v.heightFunc != nil {
		// Check a 2-block radius around the center (well area)
		for dx := -2; dx <= 2; dx++ {
			for dz := -2; dz <= 2; dz++ {
				if v.heightFunc(wx+dx, wz+dz) < 62 { // 62 is WaterLevel
					return 0, 0, false
				}
			}
		}
	}

	return wx, wz, true
}

// divFloor returns a / b, rounding towards negative infinity.
func divFloor(a, b int) int {
	if a < 0 && a%b != 0 {
		return a/b - 1
	}
	return a / b
}

// generateVillage writes village structures into sections for chunk (chunkX, chunkZ).
// All coordinates stay inside the section array exactly like generateTrees does.
func (v *VillageGrid) generateVillage(chunkX, chunkZ int, sections *[SectionsPerChunk][ChunkSectionSize]uint16) {
	// Chunk world-space bounds
	minWX := chunkX * 16
	minWZ := chunkZ * 16
	maxWX := minWX + 15
	maxWZ := minWZ + 15

	// Village influence radius (structures can reach this many blocks from center)
	// Increased to 75 to capture longer branching paths and offset buildings.
	const radius = 75

	// Determine which grid cells could have villages whose structures overlap this chunk.
	// We use floor division to ensure correct mapping in all coordinate quadrants.
	cellMinX := divFloor(minWX-radius, villageCellSize)
	cellMaxX := divFloor(maxWX+radius, villageCellSize)
	cellMinZ := divFloor(minWZ-radius, villageCellSize)
	cellMaxZ := divFloor(maxWZ+radius, villageCellSize)

	for cx := cellMinX; cx <= cellMaxX; cx++ {
		for cz := cellMinZ; cz <= cellMaxZ; cz++ {
			vx, vz, ok := v.villageCenter(cx, cz)
			if !ok {
				continue
			}

			// Determine surface height at village center to avoid floating/buried towns
			surfY := v.heightFunc(vx, vz)
			if surfY < WaterLevel {
				surfY = WaterLevel // Don't build villages underwater
			}

			v.buildVillage(vx, vz, surfY, chunkX, chunkZ, sections)
		}
	}
}

// IsInVillage returns true if (wx, wz) is within the influence area of a village's ACTUAL structures.
func (v *VillageGrid) IsInVillage(wx, wz int) bool {
	// A rough "generous" radius to find likely candidate villages.
	// 75 is the max extent of our road segments.
	const searchRadius = 80
	cellMinX := divFloor(wx-searchRadius, villageCellSize)
	cellMaxX := divFloor(wx+searchRadius, villageCellSize)
	cellMinZ := divFloor(wz-searchRadius, villageCellSize)
	cellMaxZ := divFloor(wz+searchRadius, villageCellSize)

	for cx := cellMinX; cx <= cellMaxX; cx++ {
		for cz := cellMinZ; cz <= cellMaxZ; cz++ {
			vx, vz, ok := v.villageCenter(cx, cz)
			if !ok {
				continue
			}

			// Deterministically plan the village to check structure overlap
			plan := v.planVillage(vx, vz)

			// 1. Check Buildings & Farms (proximity 4 blocks)
			for _, b := range plan.Buildings {
				if wx >= b.Box.minX-4 && wx <= b.Box.maxX+4 &&
					wz >= b.Box.minZ-4 && wz <= b.Box.maxZ+4 {
					return true
				}
			}
			for _, f := range plan.Farms {
				if wx >= f.Box.minX-4 && wx <= f.Box.maxX+4 &&
					wz >= f.Box.minZ-4 && wz <= f.Box.maxZ+4 {
					return true
				}
			}

			// 2. Check Roads (proximity 4 blocks)
			if v.isNearRoad(wx, wz, plan.Roads, 4) {
				return true
			}

			// 3. Check Well (at center, 4x4 + margin)
			if wx >= vx-5 && wx <= vx+4 && wz >= vz-5 && wz <= vz+4 {
				return true
			}
		}
	}
	return false
}

// ChunkInVillage returns true if the 16x16 chunk intersects any village structures.
func (v *VillageGrid) ChunkInVillage(chunkX, chunkZ int) bool {
	minWX := chunkX * 16
	minWZ := chunkZ * 16
	maxWX := minWX + 15
	maxWZ := minWZ + 15

	const searchRadius = 80
	cellMinX := divFloor(minWX-searchRadius, villageCellSize)
	cellMaxX := divFloor(maxWX+searchRadius, villageCellSize)
	cellMinZ := divFloor(minWZ-searchRadius, villageCellSize)
	cellMaxZ := divFloor(maxWZ+searchRadius, villageCellSize)

	for cx := cellMinX; cx <= cellMaxX; cx++ {
		for cz := cellMinZ; cz <= cellMaxZ; cz++ {
			vx, vz, ok := v.villageCenter(cx, cz)
			if !ok {
				continue
			}

			plan := v.planVillage(vx, vz)

			// 1. Check Buildings & Farms (proximity 4 blocks)
			for _, b := range plan.Buildings {
				if maxWX >= b.Box.minX-4 && minWX <= b.Box.maxX+4 &&
					maxWZ >= b.Box.minZ-4 && minWZ <= b.Box.maxZ+4 {
					return true
				}
			}
			for _, f := range plan.Farms {
				if maxWX >= f.Box.minX-4 && minWX <= f.Box.maxX+4 &&
					maxWZ >= f.Box.minZ-4 && minWZ <= f.Box.maxZ+4 {
					return true
				}
			}

			// 2. Check Roads (proximity 4 blocks)
			// For chunk box, just check the four corners and the center to be safe or use simple bounding box.
			// Accurate intersection of AABB and road segments.
			if v.isNearRoadChunk(minWX, minWZ, maxWX, maxWZ, plan.Roads, 4) {
				return true
			}

			// 3. Check Well (at center, 4x4 + margin)
			if maxWX >= vx-5 && minWX <= vx+4 && maxWZ >= vz-5 && minWZ <= vz+4 {
				return true
			}
		}
	}
	return false
}

// isNearRoadChunk checks if any road segment overlaps the given chunk bounding box.
func (v *VillageGrid) isNearRoadChunk(minWX, minWZ, maxWX, maxWZ int, segments []roadSegment, dist int) bool {
	for _, seg := range segments {
		minX, maxX := seg.sx, seg.ex
		if minX > maxX {
			minX, maxX = maxX, minX
		}
		minZ, maxZ := seg.sz, seg.ez
		if minZ > maxZ {
			minZ, maxZ = maxZ, minZ
		}

		if maxWX >= minX-dist && minWX <= maxX+dist && maxWZ >= minZ-dist && minWZ <= maxZ+dist {
			return true
		}

		if v.isNearRoadChunk(minWX, minWZ, maxWX, maxWZ, seg.children, dist) {
			return true
		}
	}
	return false
}

// isNearRoad is a recursive helper to check if a point is near any road segment in the tree.
func (v *VillageGrid) isNearRoad(wx, wz int, segments []roadSegment, dist int) bool {
	abs := func(x int) int {
		if x < 0 {
			return -x
		}
		return x
	}
	for _, seg := range segments {
		minX, maxX := seg.sx, seg.ex
		if minX > maxX {
			minX, maxX = maxX, minX
		}
		minZ, maxZ := seg.sz, seg.ez
		if minZ > maxZ {
			minZ, maxZ = maxZ, minZ
		}

		if seg.horizontal {
			if wx >= minX-dist && wx <= maxX+dist && abs(wz-seg.sz) <= dist {
				return true
			}
		} else {
			if wz >= minZ-dist && wz <= maxZ+dist && abs(wx-seg.sx) <= dist {
				return true
			}
		}

		if v.isNearRoad(wx, wz, seg.children, dist) {
			return true
		}
	}
	return false
}

// planVillage creates the layout of a village deterministically.
func (v *VillageGrid) planVillage(vx, vz int) *VillagePlan {
	key := cellKey{vx, vz}
	v.mu.RLock()
	if plan, ok := v.planCache[key]; ok {
		v.mu.RUnlock()
		return plan
	}
	v.mu.RUnlock()

	plan := v.calculateVillagePlan(vx, vz)

	v.mu.Lock()
	v.planCache[key] = plan
	v.mu.Unlock()

	return plan
}

func (v *VillageGrid) calculateVillagePlan(vx, vz int) *VillagePlan {
	abs := func(x int) int {
		if x < 0 {
			return -x
		}
		return x
	}
	// Decide main arms
	armPosX := v.cellHash(int64(vx)^0xA1, int64(vz)^0xB2, 4) < 3
	armNegX := v.cellHash(int64(vx)^0xC3, int64(vz)^0xD4, 4) < 3
	armPosZ := v.cellHash(int64(vx)^0xE5, int64(vz)^0xF6, 4) < 3
	armNegZ := v.cellHash(int64(vx)^0x17, int64(vz)^0x28, 4) < 3
	if !armPosX && !armNegX && !armPosZ && !armNegZ {
		armPosX, armNegZ = true, true
	}

	var allSegmentsFlat []roadSegment
	checkRoadCollision := func(psx, psz, pex, pez int, parentSx, parentSz int) bool {
		minX, maxX := psx, pex
		if minX > maxX {
			minX, maxX = maxX, minX
		}
		minZ, maxZ := psz, pez
		if minZ > maxZ {
			minZ, maxZ = maxZ, minZ
		}
		pMinX, pMaxX := minX-12, maxX+12
		pMinZ, pMaxZ := minZ-12, maxZ+12
		for _, exSeg := range allSegmentsFlat {
			eMinX, eMaxX := exSeg.sx, exSeg.ex
			if eMinX > eMaxX {
				eMinX, eMaxX = eMaxX, eMinX
			}
			eMinZ, eMaxZ := exSeg.sz, exSeg.ez
			if eMinZ > eMaxZ {
				eMinZ, eMaxZ = eMaxZ, eMinZ
			}
			if pMinX <= eMaxX && pMaxX >= eMinX && pMinZ <= eMaxZ && pMaxZ >= eMinZ {
				if parentSx >= eMinX && parentSx <= eMaxX && parentSz >= eMinZ && parentSz <= eMaxZ {
					continue
				}
				return true
			}
		}
		return false
	}

	var generateBranch func(sx, sz int, horizontal bool, length, dir, depth, salt int, out *roadSegment) bool
	generateBranch = func(sx, sz int, horizontal bool, length, dir, depth, salt int, out *roadSegment) bool {
		var ex, ez int
		if horizontal {
			ex = sx + length*dir
			ez = sz
		} else {
			ex = sx
			ez = sz + length*dir
		}
		if depth < 2 && checkRoadCollision(sx, sz, ex, ez, sx, sz) {
			return false
		}
		seg := roadSegment{sx: sx, sz: sz, ex: ex, ez: ez, horizontal: horizontal, wobbleSeed: salt}
		allSegmentsFlat = append(allSegmentsFlat, seg)
		if depth <= 0 || length < 10 {
			*out = seg
			return true
		}
		numAttempts := 1
		if length >= 20 {
			numAttempts = 2
		}
		for b := 0; b < numAttempts; b++ {
			branchSalt := salt*31 + b*17 + depth*7
			if v.cellHash(int64(branchSalt)^0xBB, int64(sx+sz+b), 10) < 4 {
				continue
			}
			frac := int(v.cellHash(int64(branchSalt)^0xCC, int64(ex+ez), 40)) + 40
			dist := length * frac / 100
			if dist < 8 {
				dist = 8
			}
			var bsx, bsz int
			if horizontal {
				bsx = sx + dist*dir
				bsz = sz
			} else {
				bsx = sx
				bsz = sz + dist*dir
			}
			bdir := 1
			if v.cellHash(int64(branchSalt)^0xDD, int64(bsx+bsz), 2) == 0 {
				bdir = -1
			}
			blen := int(v.cellHash(int64(branchSalt)^0xEE, int64(bsx-bsz), 15)) + 14
			var child roadSegment
			if generateBranch(bsx, bsz, !horizontal, blen, bdir, depth-1, branchSalt+1000, &child) {
				seg.children = append(seg.children, child)
			}
		}
		*out = seg
		return true
	}

	mainLen := func(salt int) int {
		return int(v.cellHash(int64(vx)^int64(salt), int64(vz)^int64(salt*3), 22)) + 24
	}

	var roads []roadSegment
	if armPosX {
		var s roadSegment
		if generateBranch(vx, vz, true, mainLen(0x111), 1, 2, vx+1, &s) {
			roads = append(roads, s)
		}
	}
	if armNegX {
		var s roadSegment
		if generateBranch(vx, vz, true, mainLen(0x222), -1, 2, vx+2, &s) {
			roads = append(roads, s)
		}
	}
	if armPosZ {
		var s roadSegment
		if generateBranch(vx, vz, false, mainLen(0x333), 1, 2, vz+3, &s) {
			roads = append(roads, s)
		}
	}
	if armNegZ {
		var s roadSegment
		if generateBranch(vx, vz, false, mainLen(0x444), -1, 2, vz+4, &s) {
			roads = append(roads, s)
		}
	}

	var sites []buildingSite
	var collectSites func(seg *roadSegment, depth int)
	collectSites = func(seg *roadSegment, depth int) {
		slen := abs(seg.ex-seg.sx) + abs(seg.ez-seg.sz)
		if slen >= 10 {
			sideHash := v.cellHash(int64(seg.ex)^0xF1, int64(seg.ez)^0xF2, 2)
			off := 5
			if sideHash == 0 {
				off = -5
			}
			if seg.horizontal {
				facing := 0
				if off > 0 {
					facing = 1
				}
				sites = append(sites, buildingSite{doorX: seg.ex, doorZ: seg.ez + off, facing: facing, smallOnly: depth > 0})
			} else {
				facing := 2
				if off > 0 {
					facing = 3
				}
				sites = append(sites, buildingSite{doorX: seg.ex + off, doorZ: seg.ez, facing: facing, smallOnly: depth > 0})
			}
		}
		if slen >= 22 {
			midHash := v.cellHash(int64(seg.sx+seg.ex)^0xA5, int64(seg.sz+seg.ez)^0xB6, 2)
			off := 5
			if midHash == 0 {
				off = -5
			}
			mx, mz := (seg.sx+seg.ex)/2, (seg.sz+seg.ez)/2
			if seg.horizontal {
				facing := 0
				if off > 0 {
					facing = 1
				}
				sites = append(sites, buildingSite{doorX: mx, doorZ: mz + off, facing: facing, smallOnly: true})
			} else {
				facing := 2
				if off > 0 {
					facing = 3
				}
				sites = append(sites, buildingSite{doorX: mx + off, doorZ: mz, facing: facing, smallOnly: true})
			}
		}
		for i := range seg.children {
			collectSites(&seg.children[i], depth+1)
		}
	}
	for i := range roads {
		collectSites(&roads[i], 0)
	}

	plan := &VillagePlan{VX: vx, VZ: vz, Roads: roads}
	var placed []bbox
	var roadBBs []bbox
	var collectRoadBBs func(seg *roadSegment)
	collectRoadBBs = func(seg *roadSegment) {
		if seg.horizontal {
			minX, maxX := seg.sx, seg.ex
			if minX > maxX {
				minX, maxX = maxX, minX
			}
			roadBBs = append(roadBBs, bbox{minX, seg.sz - 2, maxX, seg.sz + 2})
		} else {
			minZ, maxZ := seg.sz, seg.ez
			if minZ > maxZ {
				minZ, maxZ = maxZ, minZ
			}
			roadBBs = append(roadBBs, bbox{seg.sx - 2, minZ, seg.sx + 2, maxZ})
		}
		for i := range seg.children {
			collectRoadBBs(&seg.children[i])
		}
	}
	for i := range roads {
		collectRoadBBs(&roads[i])
	}

	var fallback []buildingSite
	hasHall, hasChurch, hasMarket := false, false, false
	for i, site := range sites {
		h := v.cellHash(int64(site.doorX+i*7), int64(site.doorZ+i*13+3), 100)
		if h < 20 {
			fallback = append(fallback, site)
			continue
		}
		stype := 1
		if !site.smallOnly {
			if h >= 90 && !hasHall {
				stype = 2
			} else if h >= 80 && !hasChurch {
				stype = 3
			} else if h >= 70 && !hasMarket {
				stype = 4
			}
		}
		var bx, bz, ex, ez int
		switch stype {
		case 1:
			ax, az := doorToAnchorHouse(site.doorX, site.doorZ, site.facing)
			bx, bz, ex, ez = ax-1, az-1, ax+6, az+6
		case 2:
			ax, az := doorToAnchorHall(site.doorX, site.doorZ, site.facing)
			bx, bz, ex, ez = ax-1, az-1, ax+8, az+10
		case 3:
			ax, az := doorToAnchorChurch(site.doorX, site.doorZ, site.facing)
			bx, bz, ex, ez = ax-1, az-1, ax+6, az+12
		case 4:
			ax, az := doorToAnchorMarketplace(site.doorX, site.doorZ, site.facing)
			bx, bz, ex, ez = ax-1, az-1, ax+14, az+14
		}
		newB := bbox{bx, bz, ex, ez}
		collides := false
		for _, b := range placed {
			if newB.minX <= b.maxX && newB.maxX >= b.minX && newB.minZ <= b.maxZ && newB.maxZ >= b.minZ {
				collides = true
				break
			}
		}
		if !collides {
			for _, r := range roadBBs {
				if newB.minX <= r.maxX && newB.maxX >= r.minX && newB.minZ <= r.maxZ && newB.maxZ >= r.minZ {
					collides = true
					break
				}
			}
		}
		if collides {
			fallback = append(fallback, site)
			continue
		}
		if stype == 2 {
			hasHall = true
		} else if stype == 3 {
			hasChurch = true
		} else if stype == 4 {
			hasMarket = true
		}
		placed = append(placed, newB)
		plan.Buildings = append(plan.Buildings, struct {
			Site buildingSite
			Type int
			Box  bbox
		}{site, stype, newB})
	}

	for _, site := range fallback {
		if len(placed) >= 5 {
			break
		}
		ax, az := doorToAnchorHouse(site.doorX, site.doorZ, site.facing)
		newB := bbox{ax - 1, az - 1, ax + 6, az + 6}
		collides := false
		for _, b := range placed {
			if newB.minX <= b.maxX && newB.maxX >= b.minX && newB.minZ <= b.maxZ && newB.maxZ >= b.minZ {
				collides = true
				break
			}
		}
		if !collides {
			for _, r := range roadBBs {
				if newB.minX <= r.maxX && newB.maxX >= r.minX && newB.minZ <= r.maxZ && newB.maxZ >= r.minZ {
					collides = true
					break
				}
			}
		}
		if !collides {
			placed = append(placed, newB)
			plan.Buildings = append(plan.Buildings, struct {
				Site buildingSite
				Type int
				Box  bbox
			}{site, 1, newB})
		}
	}

	floc := []struct{ dx, dz int }{{-24, 14}, {8, -26}, {24, 14}, {-8, -32}, {30, -14}, {-28, -16}}
	nfarms := int(v.cellHash(int64(vx), int64(vz), 4)) + 1
	for i := len(floc) - 1; i > 0; i-- {
		j := int(v.cellHash(int64(vx+i), int64(vz-i), int64(i+1)))
		floc[i], floc[j] = floc[j], floc[i]
	}
	for i := 0; i < len(floc) && len(plan.Farms) < nfarms; i++ {
		fx, fz := vx+floc[i].dx, vz+floc[i].dz
		fB := bbox{fx - 4, fz - 4, fx + 4, fz + 4}
		collides := false
		for _, b := range placed {
			if fB.minX <= b.maxX && fB.maxX >= b.minX && fB.minZ <= b.maxZ && fB.maxZ >= b.minZ {
				collides = true
				break
			}
		}
		if !collides {
			for _, r := range roadBBs {
				if fB.minX <= r.maxX && fB.maxX >= r.minX && fB.minZ <= r.maxZ && fB.maxZ >= r.minZ {
					collides = true
					break
				}
			}
		}
		if !collides {
			plan.Farms = append(plan.Farms, struct {
				X, Z int
				Box  bbox
			}{fx, fz, fB})
		}
	}

	pruneRoad := func(seg *roadSegment, isMain bool) bool {
		var recurse func(s *roadSegment, main bool) bool
		recurse = func(s *roadSegment, main bool) bool {
			kept := s.children[:0]
			for i := range s.children {
				if recurse(&s.children[i], false) {
					kept = append(kept, s.children[i])
				}
			}
			s.children = kept
			maxD := 0
			checkP := func(px, pz int) {
				if s.horizontal {
					if abs(pz-s.sz) <= 12 {
						d := px - s.sx
						if s.ex < s.sx {
							d = s.sx - px
						}
						if d >= -4 && d > maxD {
							maxD = d
						}
					}
				} else {
					if abs(px-s.sx) <= 12 {
						d := pz - s.sz
						if s.ez < s.sz {
							d = s.sz - pz
						}
						if d >= -4 && d > maxD {
							maxD = d
						}
					}
				}
			}
			for _, c := range s.children {
				checkP(c.sx, c.sz)
			}
			for _, b := range plan.Buildings {
				checkP(b.Site.doorX, b.Site.doorZ)
			}
			for _, f := range plan.Farms {
				checkP(f.X, f.Z)
			}
			origL := abs(s.ex - s.sx)
			if !s.horizontal {
				origL = abs(s.ez - s.sz)
			}
			if maxD <= 0 && len(s.children) == 0 {
				if main {
					maxD = 3
				} else {
					return false
				}
			}
			nL := maxD + 2
			if nL > origL {
				nL = origL
			}
			if s.horizontal {
				if s.ex < s.sx {
					s.ex = s.sx - nL
				} else {
					s.ex = s.sx + nL
				}
			} else {
				if s.ez < s.sz {
					s.ez = s.sz - nL
				} else {
					s.ez = s.sx + nL // BUG HERE in original? Should be seg.sz
					s.ez = s.sz + nL
				}
			}
			return true
		}
		return recurse(seg, isMain)
	}
	for i := range plan.Roads {
		pruneRoad(&plan.Roads[i], true)
	}

	return plan
}

// setBlock is a helper that writes block state into the sections array, clamping
// to valid section / local coordinates within the chunk.
func setBlock(lx, y, lz int, state uint16, sections *[SectionsPerChunk][ChunkSectionSize]uint16) {
	if y < 0 || y > 255 || lx < 0 || lx > 15 || lz < 0 || lz > 15 {
		return
	}
	sec := y / 16
	sy := y % 16
	sections[sec][(sy*16+lz)*16+lx] = state
}

// worldToLocal converts a world X or Z to the local coordinate within chunkX/chunkZ.
// Returns the local coord and whether it falls within [0,15].
func worldToLocal(world, chunkOrigin int) (int, bool) {
	l := world - chunkOrigin
	return l, l >= 0 && l <= 15
}

// doorToAnchorHouse computes the building anchor (hx, hz) for a 5x5 house
// given the door's world position and facing direction.
// facing: 0=door on +Z wall, 1=door on -Z wall, 2=door on +X wall, 3=door on -X wall
func doorToAnchorHouse(doorX, doorZ, facing int) (int, int) {
	switch facing {
	case 0: // door at (hx+2, hz+4)
		return doorX - 2, doorZ - 4
	case 1: // door at (hx+2, hz)
		return doorX - 2, doorZ
	case 2: // door at (hx+4, hz+2)
		return doorX - 4, doorZ - 2
	case 3: // door at (hx, hz+2)
		return doorX, doorZ - 2
	}
	return doorX, doorZ
}

// doorToAnchorHall computes the building anchor for a 7x9 large hall.
// facing 0: door at (hx+3, hz+8); facing 1: door at (hx+3, hz)
func doorToAnchorHall(doorX, doorZ, facing int) (int, int) {
	switch facing {
	case 0: // door at (hx+3, hz+l-1=hz+8)
		return doorX - 3, doorZ - 8
	case 1: // door at (hx+3, hz)
		return doorX - 3, doorZ
	}
	return doorX, doorZ
}

// doorToAnchorChurch computes the building anchor for the church.
// facing 0: door at (hx+2, hz+10); facing 1: door at (hx+2, hz)
func doorToAnchorChurch(doorX, doorZ, facing int) (int, int) {
	switch facing {
	case 0: // door at (hx+2, hz+hallMaxZ=hz+10)
		return doorX - 2, doorZ - 10
	case 1: // door at (hx+2, hz)
		return doorX - 2, doorZ
	}
	return doorX, doorZ
}

// doorToAnchorMarketplace computes the anchor for the 13x13 marketplace.
// The anchor is the minimum x,z corner. The "door" is the entry to the central cross-path.
// facing 0: entry on +Z wall; facing 1: entry on -Z wall
func doorToAnchorMarketplace(doorX, doorZ, facing int) (int, int) {
	switch facing {
	case 0: // entry at (hx+6, hz+12)
		return doorX - 6, doorZ - 12
	case 1: // entry at (hx+6, hz)
		return doorX - 6, doorZ
	case 2: // entry at (hx+12, hz+6)
		return doorX - 12, doorZ - 6
	case 3: // entry at (hx, hz+6)
		return doorX, doorZ - 6
	}
	return doorX, doorZ
}

func (v *VillageGrid) buildVillage(vx, vz, surfY, chunkX, chunkZ int, sections *[SectionsPerChunk][ChunkSectionSize]uint16) {
	originX := chunkX * 16
	originZ := chunkZ * 16
	maxWX := originX + 15
	maxWZ := originZ + 15

	// Determine biome at village center for styling
	biome := BiomeAt(v.tempNoise, v.rainNoise, vx, vz)

	// Block IDs (state = id << 4)
	const (
		air          uint16 = 0
		cobble       uint16 = 4 << 4   // cobblestone
		glass        uint16 = 20 << 4  // glass pane (window)
		fence        uint16 = 85 << 4  // oak fence (TODO: biome specific?)
		stoneBrick   uint16 = 98 << 4  // stone bricks (well walls)
		cobbleWall   uint16 = 139 << 4 // cobblestone wall
		waterSrc     uint16 = 9 << 4   // water (stationary)
		torch        uint16 = 50 << 4  // torch
		slab         uint16 = 44 << 4  // stone slab
		woodenDoor   uint16 = 64 << 4  // wooden door
		farmland     uint16 = 60 << 4  // farmland
		wheat        uint16 = 59 << 4  // wheat
		carrot       uint16 = 141 << 4 // carrot
		potato       uint16 = 142 << 4 // potato
		melonStem    uint16 = 105 << 4 // melon stem
		pumpkinStem  uint16 = 104 << 4 // pumpkin stem
		melonBlock   uint16 = 103 << 4 // melon block
		pumpkinBlock uint16 = 86 << 4  // pumpkin block
		dandelion    uint16 = 37 << 4  // dandelion flower
		poppy        uint16 = 38 << 4  // poppy flower
		goldBlock    uint16 = 41 << 4  // gold block (bell)
		bed          uint16 = 26 << 4  // bed
	)

	log := biome.VillageLog
	planks := biome.VillagePlanks
	pathBlock := biome.VillagePath
	woodSlab := biome.VillageSlab
	woodStairs := biome.VillageStairs
	woodFence := biome.VillageFence
	deco1 := biome.VillageDeco1
	deco2 := biome.VillageDeco2

	abs := func(x int) int {
		if x < 0 {
			return -x
		}
		return x
	}

	placeBlock := func(wx, y, wz int, state uint16) {
		lx := wx - originX
		lz := wz - originZ
		setBlock(lx, y, lz, state, sections)

		// Foundation support: if this is at the base floor level (surfY),
		// fill down to solid ground to avoid floating buildings.
		if y == surfY {
			for fy := y - 1; fy >= 0; fy-- {
				sec, sy := fy/16, fy%16
				if fy >= 0 && fy < 256 && lx >= 0 && lx < 16 && lz >= 0 && lz < 16 {
					current := sections[sec][(sy*16+lz)*16+lx] >> 4
					if current == 0 || current == 8 || current == 9 { // Air or Water
						// Use cobblestone for foundation (or sandstone for desert)
						fState := cobble
						setBlock(lx, fy, lz, fState, sections)
					} else {
						break // Hit solid ground
					}
				} else {
					break
				}
			}
		}
	}

	isSafeForPath := func(wx, y, wz int) bool {
		lx := wx - originX
		lz := wz - originZ
		if y < 0 || y > 255 || lx < 0 || lx > 15 || lz < 0 || lz > 15 {
			return false
		}
		sec := y / 16
		sy := y % 16
		currentState := sections[sec][(sy*16+lz)*16+lx]
		id := currentState >> 4
		return id == 0 || id == 2 || id == 3 || id == 13 || id == 12 || id == 80 || currentState == pathBlock // Air, Grass, Dirt, Gravel, Sand, Snow, or current path
	}

	placePath := func(wx, y, wz int, state uint16) {
		if isSafeForPath(wx, y, wz) {
			placeBlock(wx, y, wz, state)
		} else {
			// If not safe, it might be above ground on a slope.
			// Force place the path at surfY and fill below.
			placeBlock(wx, y, wz, state)
		}
	}

	isSafeToDecorate := func(wx, y, wz int) bool {
		lx := wx - originX
		lz := wz - originZ
		if y < 0 || y > 255 || lx < 0 || lx > 15 || lz < 0 || lz > 15 {
			return false
		}
		sec := y / 16
		sy := y % 16
		currentState := sections[sec][(sy*16+lz)*16+lx]
		id := currentState >> 4
		return id == 0 || id == 12 || id == 80 || id == 13 || currentState == pathBlock // Air, Sand, Snow, Gravel, or current path
	}

	placeDecoration := func(wx, y, wz int, state uint16) {
		if isSafeToDecorate(wx, y, wz) {
			placeBlock(wx, y, wz, state)
		}
	}

	// ------------------------------------------------------------------
	// 1. Plan roads, buildings, and farms
	// ------------------------------------------------------------------
	plan := v.planVillage(vx, vz)

	// ------------------------------------------------------------------
	// 2. Render Well (at center)
	// ------------------------------------------------------------------
	if vx+2 >= originX && vx-3 <= maxWX && vz+2 >= originZ && vz-3 <= maxWZ {
		for dx := -3; dx <= 2; dx++ {
			for dz := -3; dz <= 2; dz++ {
				placePath(vx+dx, surfY, vz+dz, pathBlock)
			}
		}
		for dx := -2; dx <= 1; dx++ {
			for dz := -2; dz <= 1; dz++ {
				isCorner := (dx == -2 || dx == 1) && (dz == -2 || dz == 1)
				isInner := (dx == -1 || dx == 0) && (dz == -1 || dz == 0)
				placeBlock(vx+dx, surfY+1, vz+dz, stoneBrick)
				if isCorner {
					placeBlock(vx+dx, surfY+2, vz+dz, cobbleWall)
					placeBlock(vx+dx, surfY+3, vz+dz, cobbleWall)
					placeBlock(vx+dx, surfY+4, vz+dz, cobbleWall)
				} else if isInner {
					placeBlock(vx+dx, surfY+1, vz+dz, stoneBrick)
					placeBlock(vx+dx, surfY+2, vz+dz, waterSrc)
				} else {
					placeBlock(vx+dx, surfY+2, vz+dz, stoneBrick)
				}
				placeBlock(vx+dx, surfY+5, vz+dz, slab)
			}
		}
	}

	// ------------------------------------------------------------------
	// 3. Render all road segments
	// ------------------------------------------------------------------
	wobble := func(d, seed int) int {
		drift := 0
		for seg := 1; seg <= d/16; seg++ {
			h := v.cellHash(int64(seed), int64(seg), 10)
			if h < 3 {
				drift--
			} else if h >= 7 {
				drift++
			}
		}
		if drift > 2 {
			drift = 2
		} else if drift < -2 {
			drift = -2
		}
		return drift
	}

	var renderRoad func(seg *roadSegment)
	renderRoad = func(seg *roadSegment) {
		startX, endX := seg.sx, seg.ex
		if startX > endX {
			startX, endX = endX, startX
		}
		startZ, endZ := seg.sz, seg.ez
		if startZ > endZ {
			startZ, endZ = endZ, startZ
		}

		// Render this segment only if it might overlap the chunk (+/- 4 padding for paths and wobble)
		if !(endX+4 < originX || startX-4 > maxWX || endZ+4 < originZ || startZ-4 > maxWZ) {
			if seg.horizontal {
				for x := startX; x <= endX; x++ {
					d := abs(x - seg.sx)
					wo := wobble(d, seg.wobbleSeed)
					for w := -1; w <= 1; w++ {
						placePath(x, surfY, seg.sz+w+wo, pathBlock)
					}
				}
			} else {
				for z := startZ; z <= endZ; z++ {
					d := abs(z - seg.sz)
					wo := wobble(d, seg.wobbleSeed)
					for w := -1; w <= 1; w++ {
						placePath(seg.sx+w+wo, surfY, z, pathBlock)
					}
				}
			}
		}

		for i := range seg.children {
			renderRoad(&seg.children[i])
		}
	}
	for i := range plan.Roads {
		renderRoad(&plan.Roads[i])
	}

	// ------------------------------------------------------------------
	// 4. Place buildings at decided sites
	// ------------------------------------------------------------------
	for _, b := range plan.Buildings {
		// AABB chunk cull
		if b.Box.maxX < originX || b.Box.minX > maxWX || b.Box.maxZ < originZ || b.Box.minZ > maxWZ {
			continue
		}

		dx, dz, fac := b.Site.doorX, b.Site.doorZ, b.Site.facing
		switch b.Type {
		case 1:
			hx, hz := doorToAnchorHouse(dx, dz, fac)
			v.buildHouse(hx, hz, surfY, fac, placeBlock, log, planks, cobble, glass, woodFence, torch, woodStairs, woodenDoor, air, woodSlab, bed)
		case 2:
			hx, hz := doorToAnchorHall(dx, dz, fac)
			v.buildLargeHall(hx, hz, surfY, fac, placeBlock, log, planks, cobble, glass, torch, woodStairs, woodenDoor, air, woodSlab, bed)
		case 3:
			hx, hz := doorToAnchorChurch(dx, dz, fac)
			v.buildChurch(hx, hz, surfY, fac, placeBlock, log, planks, cobble, glass, torch, woodStairs, woodenDoor, slab, air, goldBlock, woodFence)
		case 4:
			hx, hz := doorToAnchorMarketplace(dx, dz, fac)
			v.buildMarketplace(hx, hz, surfY, fac, placeBlock, log, woodFence, woodSlab, pathBlock, torch, air)
		}
		pw := 1
		switch fac {
		case 0:
			for z := dz; z <= dz+6; z++ {
				for w := -pw; w <= pw; w++ {
					placePath(dx+w, surfY, z, pathBlock)
				}
			}
		case 1:
			for z := dz - 6; z <= dz; z++ {
				for w := -pw; w <= pw; w++ {
					placePath(dx+w, surfY, z, pathBlock)
				}
			}
		case 2:
			for x := dx; x <= dx+6; x++ {
				for w := -pw; w <= pw; w++ {
					placePath(x, surfY, dz+w, pathBlock)
				}
			}
		case 3:
			for x := dx - 6; x <= dx; x++ {
				for w := -pw; w <= pw; w++ {
					placePath(x, surfY, dz+w, pathBlock)
				}
			}
		}
	}

	// ------------------------------------------------------------------
	// 5. Place farms
	// ------------------------------------------------------------------
	for _, f := range plan.Farms {
		// AABB chunk cull
		if f.Box.maxX < originX || f.Box.minX > maxWX || f.Box.maxZ < originZ || f.Box.minZ > maxWZ {
			continue
		}

		fx, fz := f.X, f.Z
		fh := v.cellHash(int64(fx), int64(fz), 10)
		if fh < 7 {
			cr := []uint16{wheat, carrot, potato}
			v.buildFarm(fx, fz, surfY, placeBlock, log, waterSrc, farmland, cr[fh%3])
		} else {
			fr := []uint16{melonBlock, pumpkinBlock}
			st := []uint16{melonStem, pumpkinStem}
			idx := int(fh % 2)
			v.buildFruitFarm(fx, fz, surfY, placeBlock, log, waterSrc, farmland, st[idx], fr[idx])
		}
	}

	// ------------------------------------------------------------------
	// 6. Decorations
	// ------------------------------------------------------------------
	for i := -40; i <= 40; i += 7 {
		for j := -40; j <= 40; j += 7 {
			decX := vx + i
			decZ := vz + j
			if decX >= originX && decX <= maxWX && decZ >= originZ && decZ <= maxWZ {
				h := int64(i*131 + j*17)
				if h%5 == 0 {
					var fl uint16 = deco1
					if h%10 == 0 {
						fl = deco2
					}
					placeDecoration(decX, surfY+1, decZ, fl)
				}
			}
		}
	}
	var placeLamps func(seg *roadSegment)
	placeLamps = func(seg *roadSegment) {
		startX, endX := seg.sx, seg.ex
		if startX > endX {
			startX, endX = endX, startX
		}
		startZ, endZ := seg.sz, seg.ez
		if startZ > endZ {
			startZ, endZ = endZ, startZ
		}

		if !(endX+4 < originX || startX-4 > maxWX || endZ+4 < originZ || startZ-4 > maxWZ) {
			slen := abs(seg.ex-seg.sx) + abs(seg.ez-seg.sz)
			for dist := 8; dist <= slen-2; dist += 10 {
				var px, pz int
				if seg.horizontal {
					dir := 1
					if seg.ex < seg.sx {
						dir = -1
					}
					px, pz = seg.sx+dist*dir, seg.sz+2
				} else {
					dir := 1
					if seg.ez < seg.sz {
						dir = -1
					}
					px, pz = seg.sx+2, seg.sz+dist*dir
				}
				placeDecoration(px, surfY+1, pz, fence)
				placeDecoration(px, surfY+2, pz, fence)
				placeDecoration(px, surfY+3, pz, fence)
				placeDecoration(px, surfY+4, pz, torch|5)
			}
		}

		for i := range seg.children {
			placeLamps(&seg.children[i])
		}
	}
	for i := range plan.Roads {
		placeLamps(&plan.Roads[i])
	}
}

// buildMarketplace creates an open market with 4 stall structures and a central path cross.
func (v *VillageGrid) buildMarketplace(hx, hz, surfY, facing int, place func(wx, y, wz int, state uint16), log, fence, woodSlab, pathBlock, torch, air uint16) {
	// 13x13 bounding box. Clear footprint first.
	for dx := 0; dx < 13; dx++ {
		for dz := 0; dz < 13; dz++ {
			for dy := 1; dy <= 4; dy++ {
				place(hx+dx, surfY+dy, hz+dz, air)
			}
		}
	}

	// 1. Draw the gravel footprint
	// Instead of a cross, draw a solid rectangle that covers the stalls and the center path.
	if facing == 0 || facing == 1 {
		// Vertical layout. Gravel from X=2..10, Z=0..12
		for dx := 2; dx <= 10; dx++ {
			for dz := 0; dz <= 12; dz++ {
				place(hx+dx, surfY, hz+dz, pathBlock)
			}
		}
	} else {
		// Horizontal layout. Gravel from X=0..12, Z=2..10
		for dx := 0; dx <= 12; dx++ {
			for dz := 2; dz <= 10; dz++ {
				place(hx+dx, surfY, hz+dz, pathBlock)
			}
		}
	}

	upperSlab := woodSlab | 8 // meta 8 to make it upper slab

	// Function to generate a dynamic stall facing the specified axis
	buildStall := func(startX, startZ, w, l, frontDx, frontDz int) {
		for dx := 0; dx < w; dx++ {
			for dz := 0; dz < l; dz++ {
				// Logs at corners
				isCorner := (dx == 0 || dx == w-1) && (dz == 0 || dz == l-1)

				if isCorner {
					place(hx+startX+dx, surfY+1, hz+startZ+dz, log)
					place(hx+startX+dx, surfY+2, hz+startZ+dz, fence)
					place(hx+startX+dx, surfY+3, hz+startZ+dz, fence)
				}

				// Roof (covers entire stall)
				place(hx+startX+dx, surfY+4, hz+startZ+dz, woodSlab)

				// Counter (upper slab). Forms a U-shape open at the back.
				if !isCorner {
					isFront := false
					if frontDx >= 0 && dx == frontDx {
						isFront = true
					}
					if frontDz >= 0 && dz == frontDz {
						isFront = true
					}

					isEdge := false
					if frontDx >= 0 { // Stall faces along X-axis
						if dz == 0 || dz == l-1 {
							isEdge = true
						}
					} else if frontDz >= 0 { // Stall faces along Z-axis
						if dx == 0 || dx == w-1 {
							isEdge = true
						}
					}

					if isFront || isEdge {
						place(hx+startX+dx, surfY+1, hz+startZ+dz, upperSlab)
					}
				}
			}
		}
	}

	if facing == 0 || facing == 1 {
		// Main road enters along Z-axis. Center path is X=5..7.
		// Stalls face the vertical path (X=5..7).
		// Stalls are 3 wide (X) by 5 long (Z).
		buildStall(2, 0, 3, 5, 2, -1) // Top-Left, front facing +X (dx=2)
		buildStall(8, 0, 3, 5, 0, -1) // Top-Right, front facing -X (dx=0)
		buildStall(2, 8, 3, 5, 2, -1) // Bottom-Left, front facing +X (dx=2)
		buildStall(8, 8, 3, 5, 0, -1) // Bottom-Right, front facing -X (dx=0)
	} else {
		// Main road enters along X-axis. Center path is Z=5..7.
		// Stalls face the horizontal path (Z=5..7).
		// Stalls are 5 wide (X) by 3 long (Z).
		buildStall(0, 2, 5, 3, -1, 2) // Top-Left, front facing +Z (dz=2)
		buildStall(8, 2, 5, 3, -1, 2) // Top-Right, front facing +Z (dz=2)
		buildStall(0, 8, 5, 3, -1, 0) // Bottom-Left, front facing -Z (dz=0)
		buildStall(8, 8, 5, 3, -1, 0) // Bottom-Right, front facing -Z (dz=0)
	}
}

// buildFarm creates a 7x7 farm with a water trench in the middle.
func (v *VillageGrid) buildFarm(fx, fz, surfY int, place func(wx, y, wz int, state uint16), log, water, farmland, crop uint16) {
	for dx := -3; dx <= 3; dx++ {
		for dz := -3; dz <= 3; dz++ {
			isBorder := dx == -3 || dx == 3 || dz == -3 || dz == 3
			if isBorder {
				place(fx+dx, surfY, fz+dz, log)
			} else if dx == 0 {
				place(fx+dx, surfY, fz+dz, water)
			} else {
				place(fx+dx, surfY, fz+dz, farmland)
				place(fx+dx, surfY+1, fz+dz, crop|7) // fully grown (meta 7)
			}
		}
	}
}

// buildFruitFarm creates a 7x7 farm designed for melons or pumpkins.
func (v *VillageGrid) buildFruitFarm(fx, fz, surfY int, place func(wx, y, wz int, state uint16), log, water, farmland, stem, fruit uint16) {
	for dx := -3; dx <= 3; dx++ {
		for dz := -3; dz <= 3; dz++ {
			isBorder := dx == -3 || dx == 3 || dz == -3 || dz == 3
			if isBorder {
				place(fx+dx, surfY, fz+dz, log)
			} else if dx == 0 {
				place(fx+dx, surfY, fz+dz, water)
			} else if dx == -1 || dx == 1 {
				// Farmland for stems
				place(fx+dx, surfY, fz+dz, farmland)
				place(fx+dx, surfY+1, fz+dz, stem|7) // fully grown stem
			} else {
				// Dirt-like foundation for fruit to spawn on
				// In villages, this is usually grass or dirt. We'll use grass.
				place(fx+dx, surfY, fz+dz, 2<<4) // grass
				// Randomly place a few fruits already
				if (dx+dz)%3 == 0 {
					place(fx+dx, surfY+1, fz+dz, fruit)
				}
			}
		}
	}
}

// buildLargeHall creates a 7x9 building with a solid symmetrical facade.
// facing: 0=door on +Z wall, 1=door on -Z wall
func (v *VillageGrid) buildLargeHall(hx, hz, surfY, facing int, place func(wx, y, wz int, state uint16), log, planks, cobble, glass, torch, stairs, door, air, woodSlab, bed uint16) {
	const w, l = 7, 9
	const h = 4

	// Door and back wall Z positions depend on facing
	doorZ := l - 1 // facing=0: door on +Z
	backZ := 0
	doorMeta := uint16(1)
	if facing == 1 {
		doorZ = 0 // facing=1: door on -Z
		backZ = l - 1
		doorMeta = 3
	}

	for dx := 0; dx < w; dx++ {
		for dz := 0; dz < l; dz++ {
			place(hx+dx, surfY, hz+dz, cobble)
			isCorner := (dx == 0 || dx == w-1) && (dz == 0 || dz == l-1)
			isWallX := dx == 0 || dx == w-1
			isWallZ := dz == 0 || dz == l-1
			isWall := isWallX || isWallZ

			for dy := 1; dy <= h; dy++ {
				if isCorner {
					place(hx+dx, surfY+dy, hz+dz, log)
				} else if isWall {
					block := planks

					if dz == doorZ {
						isDoor := dx == 3 && (dy == 1 || dy == 2)
						isWindow := (dx == 1 || dx == 5) && dy == 2
						if isDoor {
							if dy == 1 {
								block = door | doorMeta
							} else {
								block = door | 8
							}
						} else if isWindow {
							block = glass
						}
					} else if dz == backZ {
						if (dx == 2 || dx == 4) && dy == 2 {
							block = glass
						}
					} else if isWallX {
						if (dz == 2 || dz == 4 || dz == 6) && dy == 2 {
							block = glass
						}
					}
					place(hx+dx, surfY+dy, hz+dz, block)
				} else {
					place(hx+dx, surfY+dy, hz+dz, air)
				}
			}
		}
	}

	// Interior Furniture (Bed) - placed at the back of the hall
	if facing == 0 {
		place(hx+1, surfY+1, hz+2, bed|2)
		place(hx+1, surfY+1, hz+1, bed|10)
	} else {
		place(hx+1, surfY+1, hz+l-3, bed|0)
		place(hx+1, surfY+1, hz+l-2, bed|8)
	}

	// Pitched Roof
	for dz := -1; dz <= l; dz++ {
		place(hx-1, surfY+h+1, hz+dz, stairs|0)
		place(hx+w, surfY+h+1, hz+dz, stairs|1)

		place(hx, surfY+h+2, hz+dz, stairs|0)
		place(hx+w-1, surfY+h+2, hz+dz, stairs|1)

		place(hx+1, surfY+h+3, hz+dz, stairs|0)
		place(hx+w-2, surfY+h+3, hz+dz, stairs|1)

		place(hx+2, surfY+h+4, hz+dz, stairs|0)
		place(hx+w-3, surfY+h+4, hz+dz, stairs|1)

		place(hx+3, surfY+h+5, hz+dz, woodSlab)

		if dz == 0 || dz == l-1 {
			for dy := 1; dy <= 4; dy++ {
				for dx := 0; dx < w; dx++ {
					if (dy == 1) || (dy == 2 && dx >= 1 && dx <= 5) || (dy == 3 && dx >= 2 && dx <= 4) || (dy == 4 && dx == 3) {
						place(hx+dx, surfY+h+dy, hz+dz, planks)
					}
				}
			}
		}
	}
	// Torch above door
	if facing == 0 {
		place(hx+3, surfY+3, hz+l, torch)
	} else {
		place(hx+3, surfY+3, hz-1, torch)
	}
}

// buildHouse places a 5x5 plank house with a solid symmetrical facade.
// facing: 0=door on +Z wall, 1=door on -Z wall, 2=door on +X wall, 3=door on -X wall
func (v *VillageGrid) buildHouse(hx, hz, surfY, facing int, place func(wx, y, wz int, state uint16), log, planks, cobble, glass, fence, torch, stairs, door, air, woodSlab, bed uint16) {
	const w = 5
	const h = 3

	// Door metadata per facing direction
	doorMetas := [4]uint16{1, 3, 2, 0} // +Z, -Z, +X, -X
	doorMeta := doorMetas[facing]

	for dx := 0; dx < w; dx++ {
		for dz := 0; dz < w; dz++ {
			place(hx+dx, surfY, hz+dz, cobble)
			isCorner := (dx == 0 || dx == w-1) && (dz == 0 || dz == w-1)
			isWallX := dx == 0 || dx == w-1
			isWallZ := dz == 0 || dz == w-1
			isWall := isWallX || isWallZ

			for dy := 1; dy <= h; dy++ {
				if isCorner {
					place(hx+dx, surfY+dy, hz+dz, log)
				} else if isWall {
					block := planks

					// Determine front wall based on facing
					var isFrontWall bool
					switch facing {
					case 0:
						isFrontWall = dz == w-1
					case 1:
						isFrontWall = dz == 0
					case 2:
						isFrontWall = dx == w-1
					case 3:
						isFrontWall = dx == 0
					}

					if isFrontWall {
						var isDoor, isWindow bool
						switch facing {
						case 0, 1: // door on Z wall, centered on X
							isDoor = dx == 2 && (dy == 1 || dy == 2)
							isWindow = (dx == 1 || dx == 3) && dy == 2
						case 2, 3: // door on X wall, centered on Z
							isDoor = dz == 2 && (dy == 1 || dy == 2)
							isWindow = (dz == 1 || dz == 3) && dy == 2
						}
						if isDoor {
							if dy == 1 {
								block = door | doorMeta
							} else {
								block = door | 8
							}
						} else if isWindow {
							block = glass
						}
					} else {
						// Other walls: centered window
						if dy == 2 {
							if (isWallX && dz == 2) || (isWallZ && dx == 2) {
								block = glass
							}
						}
					}
					place(hx+dx, surfY+dy, hz+dz, block)
				} else {
					place(hx+dx, surfY+dy, hz+dz, air)
				}
			}
		}
	}

	// Interior Furniture (Bed) - placed opposite the door
	switch facing {
	case 0: // door at +Z, bed at back (low Z)
		place(hx+1, surfY+1, hz+2, bed|2)
		place(hx+1, surfY+1, hz+1, bed|10)
	case 1: // door at -Z, bed at back (high Z)
		place(hx+3, surfY+1, hz+2, bed|0)
		place(hx+3, surfY+1, hz+3, bed|8)
	case 2: // door at +X, bed at back (low X)
		place(hx+2, surfY+1, hz+1, bed|1)
		place(hx+1, surfY+1, hz+1, bed|9)
	case 3: // door at -X, bed at back (high X)
		place(hx+2, surfY+1, hz+3, bed|3)
		place(hx+3, surfY+1, hz+3, bed|11)
	}

	// Pitched Roof - ridge perpendicular to door wall for variety
	if facing == 0 || facing == 1 {
		// Ridge runs along X axis, slopes face N/S
		for dx := -1; dx <= w; dx++ {
			place(hx+dx, surfY+h+1, hz-1, stairs|2)
			place(hx+dx, surfY+h+1, hz+w, stairs|3)
			place(hx+dx, surfY+h+2, hz, stairs|2)
			place(hx+dx, surfY+h+2, hz+w-1, stairs|3)
			place(hx+dx, surfY+h+3, hz+1, stairs|2)
			place(hx+dx, surfY+h+3, hz+3, stairs|3)
			place(hx+dx, surfY+h+4, hz+2, woodSlab)

			if dx == 0 || dx == w-1 {
				for dy := 1; dy <= 3; dy++ {
					for dz := 1; dz < w-1; dz++ {
						if (dy == 1) || (dy == 2 && dz >= 1 && dz <= 3) || (dy == 3 && dz == 2) {
							place(hx+dx, surfY+h+dy, hz+dz, planks)
						}
					}
					place(hx+dx, surfY+h+1, hz, planks)
					place(hx+dx, surfY+h+1, hz+w-1, planks)
				}
			}
		}
	} else {
		// Ridge runs along Z axis, slopes face E/W
		for dz := -1; dz <= w; dz++ {
			place(hx-1, surfY+h+1, hz+dz, stairs|0)
			place(hx+w, surfY+h+1, hz+dz, stairs|1)
			place(hx, surfY+h+2, hz+dz, stairs|0)
			place(hx+w-1, surfY+h+2, hz+dz, stairs|1)
			place(hx+1, surfY+h+3, hz+dz, stairs|0)
			place(hx+3, surfY+h+3, hz+dz, stairs|1)
			place(hx+2, surfY+h+4, hz+dz, woodSlab)

			if dz == 0 || dz == w-1 {
				for dy := 1; dy <= 3; dy++ {
					for dx := 1; dx < w-1; dx++ {
						if (dy == 1) || (dy == 2 && dx >= 1 && dx <= 3) || (dy == 3 && dx == 2) {
							place(hx+dx, surfY+h+dy, hz+dz, planks)
						}
					}
					place(hx, surfY+h+1, hz+dz, planks)
					place(hx+w-1, surfY+h+1, hz+dz, planks)
				}
			}
		}
	}
}

// buildChurch creates a polished village church with a ground-level entrance and open belfry.
// facing: 0=door on +Z wall, 1=door on -Z wall.
// For facing=0 the entire structure is mirrored along Z.
func (v *VillageGrid) buildChurch(hx, hz, surfY, facing int, place func(wx, y, wz int, state uint16), log, planks, cobble, glass, torch, stairs, door, slab, air, bell, fence uint16) {
	// For facing=0, mirror the Z axis. The church extends 11 blocks in Z (0..10).
	// We mirror: dz -> 10 - dz, and swap door meta and stair orientations.
	const totalZ = 10 // hallMaxZ
	actualPlace := place
	if facing == 0 {
		place = func(wx, y, wz int, state uint16) {
			// Mirror Z around center of church
			dz := wz - hz
			mirroredZ := hz + totalZ - dz
			// Rotate block metadata for doors and stairs
			blockID := state >> 4
			meta := state & 0xF
			if blockID == 64 { // wooden door
				if meta == 3 {
					meta = 1 // south -> north
				} else if meta == 1 {
					meta = 3
				}
				state = (blockID << 4) | meta
			} else if blockID == 67 { // cobblestone stairs
				if meta == 2 {
					meta = 3 // south -> north
				} else if meta == 3 {
					meta = 2
				}
				state = (blockID << 4) | meta
			} else if blockID == stairs>>4 {
				if meta == 2 {
					meta = 3
				} else if meta == 3 {
					meta = 2
				}
				state = (blockID << 4) | meta
			}
			actualPlace(wx, y, mirroredZ, state)
		}
	}
	// Footprint split into two areas:
	// Tower: [0, 5) x [0, 5)
	// Hall:  [0, 5) x [4, 11) (Overlaps at dz=4)
	const towerMaxX, towerMaxZ = 4, 4
	const hallMaxX, hallMaxZ = 4, 10
	const hallHeight = 4
	const towerHeight = 10 // Solid part ends here

	// Raise the whole church one block so it sits on the surface instead of sunk.
	surfY++

	// 1. Tower Construction
	for dx := 0; dx <= towerMaxX; dx++ {
		for dz := 0; dz <= towerMaxZ; dz++ {
			isWallX := dx == 0 || dx == towerMaxX
			isWallZ := dz == 0 || dz == towerMaxZ
			isCorner := isWallX && isWallZ

			for dy := -1; dy <= towerHeight+6; dy++ {
				if dy == -1 {
					place(hx+dx, surfY+dy, hz+dz, cobble) // Hidden Foundation
				} else if dy == 0 {
					// Place floor, but let corner logs extend down to touch the ground.
					if isCorner {
						place(hx+dx, surfY+dy, hz+dz, log)
					} else {
						place(hx+dx, surfY+dy, hz+dz, cobble) // Solid Floor at ground level
					}
				} else if dy == towerHeight {
					place(hx+dx, surfY+dy, hz+dz, cobble) // Belfry Floor
				} else {
					// Corners go up to dy = towerHeight + 4 (Belfry Pillars)
					isBelfrySpace := dy > towerHeight && dy <= towerHeight+4
					if isCorner && dy <= towerHeight+4 {
						place(hx+dx, surfY+dy, hz+dz, log)
					} else if dy > towerHeight+4 {
						// Belfry Peaked Roof
						isRoofWallX := dx == 0 || dx == towerMaxX
						isRoofWallZ := dz == 0 || dz == towerMaxZ
						if dy == towerHeight+5 {
							if isRoofWallX || isRoofWallZ {
								place(hx+dx, surfY+dy, hz+dz, cobble)
							}
						} else if dy == towerHeight+6 {
							if dx >= 1 && dx <= 3 && dz >= 1 && dz <= 3 && (dx == 2 || dz == 2) {
								place(hx+dx, surfY+dy, hz+dz, cobble)
							}
							if dx == 2 && dz == 2 {
								place(hx+dx, surfY+dy+1, hz+dz, cobble) // Tip
							}
						}
					} else if !isBelfrySpace {
						// Solid Walls
						if isWallX || (isWallZ && (dy > hallHeight || dz == 0)) {
							block := cobble
							// Door and Cross at front
							if dz == 0 {
								if dx == 2 {
									if dy == 1 {
										block = door | 3
									} else if dy == 2 {
										block = door | 8
									} else if dy >= 4 && dy <= 6 {
										block = glass
									}
								} else if (dx == 1 || dx == 3) && dy == 5 {
									block = glass
								}
							}
							// Upper windows
							if dy == towerHeight-2 && (isWallX || dz == 0) {
								block = glass
							}
							place(hx+dx, surfY+dy, hz+dz, block)
						} else {
							if dy > 0 {
								place(hx+dx, surfY+dy, hz+dz, air) // Hollow interior
							}
						}
					} else {
						// Open belfry space
						if dy == towerHeight+4 && isWallZ && (dx == 1 || dx == 3) {
							place(hx+dx, surfY+dy, hz+dz, cobble) // Arch connectors
						} else if dy == towerHeight+4 && isWallX && (dz == 1 || dz == 3) {
							place(hx+dx, surfY+dy, hz+dz, cobble) // Arch connectors
						} else {
							place(hx+dx, surfY+dy, hz+dz, air)
						}
					}
				}
			}
		}
	}

	// 2. Hanging Bell
	place(hx+2, surfY+towerHeight+5, hz+2, fence) // Extra fence to attach to roof
	place(hx+2, surfY+towerHeight+4, hz+2, fence) // Fence hanging from belfry roof center
	place(hx+2, surfY+towerHeight+3, hz+2, bell)  // The Gold Bell

	// Cobblestone stair in front of door so entrance is accessible.
	cobbleStairs := uint16(67 << 4) // stone/cobblestone stairs
	place(hx+2, surfY, hz-1, cobbleStairs|2)

	// 3. Hall Construction
	for dx := 0; dx <= hallMaxX; dx++ {
		for dz := 5; dz <= hallMaxZ; dz++ {
			isWallX := dx == 0 || dx == hallMaxX
			isWallZ := dz == hallMaxZ
			isCorner := isWallX && (dz == hallMaxZ)

			for dy := -1; dy <= hallHeight; dy++ {
				if dy == -1 {
					place(hx+dx, surfY+dy, hz+dz, cobble) // Hidden Foundation
				} else if dy == 0 {
					// Place floor; allow back-corner logs to start at floor level so they
					// touch the ground instead of floating.
					if isCorner {
						place(hx+dx, surfY+dy, hz+dz, log)
					} else {
						place(hx+dx, surfY+dy, hz+dz, cobble) // Solid Floor
					}
				} else if isCorner {
					place(hx+dx, surfY+dy, hz+dz, log) // Back corners
				} else if isWallX || isWallZ {
					block := cobble
					// Windows
					if dy == 1 || dy == 2 {
						if isWallX && (dz == 6 || dz == 8) {
							block = glass
						} else if isWallZ && dx == 2 {
							block = glass
						}
					}
					place(hx+dx, surfY+dy, hz+dz, block)
				} else {
					if dy > 0 {
						place(hx+dx, surfY+dy, hz+dz, air)
					}
				}
			}
		}
	}

	// 4. Junction Closure (dz=4)
	for dx := 1; dx < hallMaxX; dx++ {
		for dy := 1; dy <= hallHeight; dy++ {
			place(hx+dx, surfY+dy, hz+4, air)
		}
	}

	// 5. Hall Gables and Roof
	// Helper to avoid placing roof blocks where the hall roof would intrude
	// into the tower footprint at dz==4 (overlap column). If a roof block
	// would land inside the tower (tz == hz+4 and tx in [hx, hx+towerMaxX])
	// skip it.
	placeRoof := func(tx, y, tz int, state uint16) {
		if tz == hz+4 {
			if tx >= hx && tx <= hx+towerMaxX {
				return
			}
		}
		place(tx, y, tz, state)
	}

	for dz := 4; dz <= hallMaxZ; dz++ {
		// Back Gable (dz=10)
		if dz == hallMaxZ {
			place(hx+1, surfY+hallHeight+1, hz+dz, cobble)
			place(hx+2, surfY+hallHeight+1, hz+dz, cobble)
			place(hx+3, surfY+hallHeight+1, hz+dz, cobble)
			place(hx+2, surfY+hallHeight+2, hz+dz, cobble)
		}

		// Slopes
		placeRoof(hx-1, surfY+hallHeight+1, hz+dz, stairs|0)
		placeRoof(hx+hallMaxX+1, surfY+hallHeight+1, hz+dz, stairs|1)

		placeRoof(hx, surfY+hallHeight+1, hz+dz, cobble)
		placeRoof(hx, surfY+hallHeight+2, hz+dz, stairs|0)

		placeRoof(hx+hallMaxX, surfY+hallHeight+1, hz+dz, cobble)
		placeRoof(hx+hallMaxX, surfY+hallHeight+2, hz+dz, stairs|1)

		placeRoof(hx+1, surfY+hallHeight+2, hz+dz, cobble)
		placeRoof(hx+1, surfY+hallHeight+3, hz+dz, stairs|0)

		placeRoof(hx+hallMaxX-1, surfY+hallHeight+2, hz+dz, cobble)
		placeRoof(hx+hallMaxX-1, surfY+hallHeight+3, hz+dz, stairs|1)

		// Ridge - Extends into the tower wall (skip overlap)
		placeRoof(hx+2, surfY+hallHeight+3, hz+dz, cobble)
		placeRoof(hx+2, surfY+hallHeight+4, hz+dz, slab)
	}

	// 6. Interior Furniture
	// Fixed pews: 3 = North, 2 = South, 0 = East, 1 = West
	// We want them facing the altar (dz=hallMaxZ), which is North in our coordinate system?
	// Actually dz increases. Altar is at dz=9/10. So they should face North (3).
	// Let's re-verify:
	// dz = 0 is Front (Entrance)
	// dz = 11 is Back (Altar)
	// So facing "Back" is facing North.
	place(hx+1, surfY+1, hz+6, stairs|3)
	place(hx+3, surfY+1, hz+6, stairs|3)
	place(hx+1, surfY+1, hz+8, stairs|3)
	place(hx+3, surfY+1, hz+8, stairs|3)

	place(hx+2, surfY+1, hz+hallMaxZ-1, cobble) // Altar
	place(hx+2, surfY+2, hz+hallMaxZ-1, torch|5)
}
