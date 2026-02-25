package world

import (
	"bytes"
	"math"
)

// Generator produces terrain data from a seed using Perlin noise.
type Generator struct {
	Seed         int64
	terrain      *Perlin      // broad height map noise
	roughness    *Perlin      // fine detail / roughness noise
	tempNoise    *Perlin      // biome temperature
	rainNoise    *Perlin      // biome rainfall
	caveNoise    *Perlin      // 3D cave carving
	cave2        *Perlin      // secondary 3D cave noise for spaghetti caves
	treeNoise    *Perlin      // tree placement
	boulderNoise *Perlin      // boulder placement
	villageGen   *VillageGrid // village placement grid
	lakeNoise    *Perlin      // lake carving noise
	riverNoise   *Perlin      // river carving noise
}

// NewGenerator creates a terrain generator from a seed.
func NewGenerator(seed int64) *Generator {
	g := &Generator{
		Seed:         seed,
		terrain:      NewPerlin(seed),
		roughness:    NewPerlin(seed + 100),
		tempNoise:    NewPerlin(seed + 1),
		rainNoise:    NewPerlin(seed + 2),
		caveNoise:    NewPerlin(seed + 3),
		cave2:        NewPerlin(seed + 5),
		treeNoise:    NewPerlin(seed + 4),
		boulderNoise: NewPerlin(seed + 200),
	}
	g.villageGen = NewVillageGrid(seed, g.tempNoise, g.rainNoise, g.SurfaceHeight)
	g.lakeNoise = NewPerlin(seed + 300)
	g.riverNoise = NewPerlin(seed + 400)
	return g
}

// SurfaceHeight returns the solid surface Y for the given world-space x, z.
func (g *Generator) SurfaceHeight(x, z int) int {
	// Biome Blending: sample biomes in a neighborhood to smooth transitions
	// ------------------------------------------------------------------
	var totalBase float64
	var totalVar float64
	var totalWeight float64

	const radius = 4
	const stride = 4 // Increased from 2 to 4 for 3x3 sampling

	for dx := -radius; dx <= radius; dx += stride {
		for dz := -radius; dz <= radius; dz += stride {
			// Get biome at sample point
			b := BiomeAt(g.tempNoise, g.rainNoise, x+dx, z+dz)

			// Simple distance-based weight (closer points matter more)
			// Using 1.0 for now for simple linear blending, but could be Gaussian
			weight := 1.0
			totalBase += float64(b.BaseHeight) * weight
			totalVar += b.HeightVariation * weight
			totalWeight += weight
		}
	}

	blendedBase := totalBase / totalWeight
	blendedVar := totalVar / totalWeight

	// Base height noise
	const noiseScale = 0.015
	h := g.terrain.OctaveNoise2D(float64(x)*noiseScale, float64(z)*noiseScale, 3, 2.0, 0.5)

	// Combine blended base height and variation
	height := blendedBase + h*blendedVar

	// 1. Rare Rivers (Ridged Noise)
	// ------------------------------------------------------------------
	const riverScale = 0.003
	rv := g.riverNoise.Noise2D(float64(x)*riverScale, float64(z)*riverScale)
	rv = math.Abs(rv)

	if rv < 0.04 {
		factor := (0.04 - rv) / 0.04
		depth := factor * 15.0
		height -= depth
	}

	// 2. Rare Lakes (Threshold Noise)
	// ------------------------------------------------------------------
	const lakeScale = 0.01
	lv := g.lakeNoise.Noise2D(float64(x)*lakeScale, float64(z)*lakeScale)
	if lv > 0.82 {
		factor := (lv - 0.82) / (1.0 - 0.82)
		depth := factor * 12.0
		height -= depth
	}

	return int(height)
}

// isCave returns true if the block at (x,y,z) should be carved into a cave.
func (g *Generator) isCave(x, y, z int) bool {
	// Simple 3D Perlin for caves, starting below surface
	lowRes := g.caveNoise.Noise3D(float64(x)*0.03, float64(y)*0.03, float64(z)*0.03)
	if lowRes > 0.5 {
		spaghetti := g.cave2.Noise3D(float64(x)*0.08, float64(y)*0.08, float64(z)*0.08)
		return spaghetti > 0.3
	}
	return false
}

// shouldPlaceTree returns true if a tree should be placed at (x, z) given the biome's density.
func (g *Generator) shouldPlaceTree(x, z int, biome *Biome) bool {
	// Density already checked by caller, but just in case
	if biome.TreeDensity <= 0 {
		return false
	}

	// Low frequency noise to create macro "forest clusters" and "clearings"
	const clusterScale = 0.02
	clusterVal := g.treeNoise.Noise2D(float64(x)*clusterScale, float64(z)*clusterScale)
	clusterVal = (clusterVal + 1) / 2 // [0,1]

	// If we are in a dense cluster region, increase density. If in a clearing, reduce it.
	// Multiply base density by a cluster factor (e.g. 0.0 to 1.5)
	effectiveDensity := biome.TreeDensity * (clusterVal * 1.5)

	// Pseudo-random value for this specific (x,z) coordinate
	hash := uint32(x*73856093 ^ z*191152071 ^ int(g.Seed))
	hash ^= hash >> 16
	hash *= 0x85ebca6b
	hash ^= hash >> 13
	hash *= 0xc2b2ae35
	hash ^= hash >> 16

	randVal := float64(hash) / float64(0xFFFFFFFF)

	return randVal < effectiveDensity
}

// WaterLevel is the sea level.
const WaterLevel = 62

func (g *Generator) BlockAt(x, y, z int) uint16 {
	if y < 0 || y > 255 {
		return 0
	}
	if y == 0 {
		return 7 << 4 // bedrock
	}

	surfH := g.SurfaceHeight(x, z)
	if y > surfH {
		if y <= WaterLevel {
			return 8 << 4 // water
		}
		return 0 // air
	}

	biome := BiomeAt(g.tempNoise, g.rainNoise, x, z)
	if y < surfH {
		return biome.FillerBlock
	}
	return biome.SurfaceBlock
}

// generateTrees places various tree types based on biome.
func (g *Generator) generateTrees(chunkX, chunkZ int, sections *[SectionsPerChunk][ChunkSectionSize]uint16, heightCache *[24][24]int, chunkInVill bool) {
	for lx := 2; lx < 14; lx++ {
		for lz := 2; lz < 14; lz++ {
			wx := chunkX*16 + lx
			wz := chunkZ*16 + lz

			biome := BiomeAt(g.tempNoise, g.rainNoise, wx, wz)

			if biome.TreeDensity <= 0 || (chunkInVill && g.villageGen.IsInVillage(wx, wz)) {
				continue
			}

			if !g.shouldPlaceTree(wx, wz, biome) {
				continue
			}

			surfaceY := heightCache[lx+4][lz+4]
			if surfaceY < WaterLevel || surfaceY > 240 || g.isCave(wx, surfaceY, wz) {
				continue
			}

			// Overhang check: ensure 3x3 area around trunk is on land
			overhang := false
			for dx := -1; dx <= 1; dx++ {
				for dz := -1; dz <= 1; dz++ {
					// Read natively from the expanded 24x24 [-4, 19] height cache
					h := heightCache[lx+dx+4][lz+dz+4]
					if h < WaterLevel {
						overhang = true
						break
					}
				}
				if overhang {
					break
				}
			}
			if overhang {
				continue
			}

			// Surface must be grass, snowy dirt, or sand
			surfBlock := sections[surfaceY/16][(surfaceY%16*16+lz)*16+lx] >> 4
			if surfBlock != 2 && surfBlock != 80 && surfBlock != 3 && surfBlock != 12 { // grass, snow, dirt, or sand
				continue
			}

			// Determine tree type
			treeType := 0 // 0: Oak, 1: Spruce, 2: Birch, 3: Jungle, 4: Dark Oak, 5: Cactus
			if biome == BiomeDesert {
				treeType = 5
			} else if biome == BiomeJungle {
				treeType = 3
			} else if biome == BiomeDarkForest {
				treeType = 4
			} else if biome == BiomeForest {
				// 30% chance of Birch in forests
				if (wx*31+wz*17)%10 < 3 {
					treeType = 2
				}
			} else if biome == BiomeExtremeHills || biome == BiomeSnowyTundra {
				treeType = 1
			}

			switch treeType {
			case 1: // Spruce
				g.buildSpruceTree(lx, surfaceY+1, lz, sections)
			case 2: // Birch
				g.buildGenericTree(lx, surfaceY+1, lz, 2, sections)
			case 3: // Jungle
				g.buildJungleTree(lx, surfaceY+1, lz, sections)
			case 4: // Dark Oak
				g.buildDarkOakTree(lx, surfaceY+1, lz, sections)
			case 5: // Desert (Cactus or Dead Bush)
				// 40% chance of Cactus, 60% of Dead Bush
				if (wx*13+wz*7)%10 < 4 {
					g.buildCactus(lx, surfaceY+1, lz, sections)
				} else {
					sec, sy := (surfaceY+1)/16, (surfaceY+1)%16
					sections[sec][(sy*16+lz)*16+lx] = 31 << 4 // Dead Bush
				}
			default: // Oak
				g.buildGenericTree(lx, surfaceY+1, lz, 0, sections)
			}
		}
	}
}

// buildGenericTree builds a standard 5x5 rounded canopy tree (Oak or Birch).
func (g *Generator) buildGenericTree(lx, y, lz int, meta uint16, sections *[SectionsPerChunk][ChunkSectionSize]uint16) {
	trunkTop := y + 3
	// Trunk
	for ty := y; ty <= trunkTop+1; ty++ {
		sec, sy := ty/16, ty%16
		current := sections[sec][(sy*16+lz)*16+lx] >> 4
		if current == 0 || current == 31 || current == 18 || current == 161 {
			sections[sec][(sy*16+lz)*16+lx] = 17<<4 | meta
		}
	}
	// Leaves
	leafBase := uint16(18<<4 | meta)
	for dy := -1; dy <= 0; dy++ {
		ly := trunkTop + dy
		sec, sy := ly/16, ly%16
		for dx := -2; dx <= 2; dx++ {
			for dz := -2; dz <= 2; dz++ {
				if (dx == -2 || dx == 2) && (dz == -2 || dz == 2) {
					continue
				}
				nlx, nlz := lx+dx, lz+dz
				if nlx >= 0 && nlx < 16 && nlz >= 0 && nlz < 16 {
					if sections[sec][(sy*16+nlz)*16+nlx] == 0 {
						sections[sec][(sy*16+nlz)*16+nlx] = leafBase
					}
				}
			}
		}
	}
	for dy := 1; dy <= 2; dy++ {
		ly := trunkTop + dy
		sec, sy := ly/16, ly%16
		for dx := -1; dx <= 1; dx++ {
			for dz := -1; dz <= 1; dz++ {
				if dy == 2 && dx != 0 && dz != 0 {
					continue
				}
				nlx, nlz := lx+dx, lz+dz
				if nlx >= 0 && nlx < 16 && nlz >= 0 && nlz < 16 {
					if sections[sec][(sy*16+nlz)*16+nlx] == 0 {
						sections[sec][(sy*16+nlz)*16+nlx] = leafBase
					}
				}
			}
		}
	}
}

// buildSpruceTree builds a conical spruce tree.
func (g *Generator) buildSpruceTree(lx, y, lz int, sections *[SectionsPerChunk][ChunkSectionSize]uint16) {
	height := 5 + (lx*13+lz*7)%3
	trunkTop := y + height - 1
	// Trunk
	for ty := y; ty <= trunkTop; ty++ {
		sec, sy := ty/16, ty%16
		current := sections[sec][(sy*16+lz)*16+lx] >> 4
		if current == 0 || current == 31 || current == 18 || current == 161 {
			sections[sec][(sy*16+lz)*16+lx] = 17<<4 | 1 // Spruce log
		}
	}
	// Leaves
	leafBase := uint16(18<<4 | 1) // Spruce leaves
	for dy := 2; dy <= height; dy++ {
		ly := y + dy
		sec, sy := ly/16, ly%16
		radius := 2
		if dy > height-2 {
			radius = 0
		} else if dy > height-4 {
			radius = 1
		}
		for dx := -radius; dx <= radius; dx++ {
			for dz := -radius; dz <= radius; dz++ {
				if radius > 1 && (dx == -radius || dx == radius) && (dz == -radius || dz == radius) {
					continue
				}
				nlx, nlz := lx+dx, lz+dz
				if nlx >= 0 && nlx < 16 && nlz >= 0 && nlz < 16 {
					if sections[sec][(sy*16+nlz)*16+nlx] == 0 {
						sections[sec][(sy*16+nlz)*16+nlx] = leafBase
					}
				}
			}
		}
	}
}

// buildJungleTree builds a very tall jungle tree.
func (g *Generator) buildJungleTree(lx, y, lz int, sections *[SectionsPerChunk][ChunkSectionSize]uint16) {
	height := 8 + (lx*7+lz*13)%6
	trunkTop := y + height - 1
	// Trunk
	for ty := y; ty <= trunkTop; ty++ {
		sec, sy := ty/16, ty%16
		current := sections[sec][(sy*16+lz)*16+lx] >> 4
		if current == 0 || current == 31 || current == 18 || current == 161 {
			sections[sec][(sy*16+lz)*16+lx] = 17<<4 | 3 // Jungle log
		}
	}
	// Leaves (multiple layers)
	leafBase := uint16(18<<4 | 3) // Jungle leaves
	for dy := height - 3; dy <= height; dy++ {
		radius := 2
		if dy == height {
			radius = 1
		}
		ly := y + dy
		sec, sy := ly/16, ly%16
		for dx := -radius; dx <= radius; dx++ {
			for dz := -radius; dz <= radius; dz++ {
				if (dx*dx + dz*dz) > radius*radius+1 {
					continue
				}
				nlx, nlz := lx+dx, lz+dz
				if nlx >= 0 && nlx < 16 && nlz >= 0 && nlz < 16 {
					if sections[sec][(sy*16+nlz)*16+nlx] == 0 {
						sections[sec][(sy*16+nlz)*16+nlx] = leafBase
					}
				}
			}
		}
	}
}

// buildDarkOakTree builds a 2x2 trunk tree with a broad canopy.
func (g *Generator) buildDarkOakTree(lx, y, lz int, sections *[SectionsPerChunk][ChunkSectionSize]uint16) {
	height := 6 + (lx*3+lz*5)%3
	trunkTop := y + height - 1
	// 2x2 Trunk (if space permits)
	for dx := 0; dx <= 1; dx++ {
		for dz := 0; dz <= 1; dz++ {
			nlx, nlz := lx+dx, lz+dz
			if nlx < 16 && nlz < 16 {
				for ty := y; ty <= trunkTop; ty++ {
					sec, sy := ty/16, ty%16
					current := sections[sec][(sy*16+nlz)*16+nlx] >> 4
					if current == 0 || current == 31 || current == 18 || current == 161 {
						sections[sec][(sy*16+nlz)*16+nlx] = 162<<4 | 1 // Dark Oak log
					}
				}
			}
		}
	}
	// Broad canopy
	leafBase := uint16(161<<4 | 1) // Dark Oak leaves
	for dy := height - 3; dy <= height; dy++ {
		ly := y + dy
		sec, sy := ly/16, ly%16
		radius := 3
		if dy == height {
			radius = 2
		}
		for dx := -radius + 1; dx <= radius; dx++ {
			for dz := -radius + 1; dz <= radius; dz++ {
				if (dx*dx + dz*dz) > radius*radius+2 {
					continue
				}
				nlx, nlz := lx+dx, lz+dz
				if nlx >= 0 && nlx < 16 && nlz >= 0 && nlz < 16 {
					if sections[sec][(sy*16+nlz)*16+nlx] == 0 {
						sections[sec][(sy*16+nlz)*16+nlx] = leafBase
					}
				}
			}
		}
	}
}

// buildCactus builds a 2-3 block high cactus.
func (g *Generator) buildCactus(lx, y, lz int, sections *[SectionsPerChunk][ChunkSectionSize]uint16) {
	height := 2 + (lx*7+lz*13)%2
	for ty := y; ty < y+height; ty++ {
		sec, sy := ty/16, ty%16
		sections[sec][(sy*16+lz)*16+lx] = 81 << 4 // Cactus block
	}
}

// generateBoulders places varied rock clusters.
func (g *Generator) generateBoulders(chunkX, chunkZ int, sections *[SectionsPerChunk][ChunkSectionSize]uint16, heightCache *[24][24]int, chunkInVill bool) {
	for lx := 1; lx < 15; lx++ {
		for lz := 1; lz < 15; lz++ {
			wx, wz := chunkX*16+lx, chunkZ*16+lz
			biome := BiomeAt(g.tempNoise, g.rainNoise, wx, wz)
			if biome.BoulderDensity <= 0 || (chunkInVill && g.villageGen.IsInVillage(wx, wz)) {
				continue
			}

			// Low frequency noise for "boulder fields"
			const clusterScale = 0.01
			clusterVal := g.boulderNoise.Noise2D(float64(wx)*clusterScale, float64(wz)*clusterScale)
			clusterVal = (clusterVal + 1) / 2 // [0, 1]

			// Increase/decrease density based on the macro cluster value
			// We divide by 40 because each boulder has a large radius (~4), covering a lot of area.
			effectiveDensity := (biome.BoulderDensity / 40.0) * (clusterVal * 2.0)

			// Fast pseudo-random hash for this coordinate
			hash := uint32(wx*142071 ^ wz*650021 ^ int(g.Seed+42))
			hash ^= hash >> 16
			hash *= 0x85ebca6b
			hash ^= hash >> 13
			hash *= 0xc2b2ae35
			hash ^= hash >> 16

			randVal := float64(hash) / float64(0xFFFFFFFF)

			if randVal > effectiveDensity {
				continue
			}

			y := heightCache[lx+4][lz+4]
			if y < WaterLevel {
				continue
			}

			// Footprint check for larger boulders
			overhang := false
			for dx := -2; dx <= 2; dx++ {
				for dz := -2; dz <= 2; dz++ {
					// Read natively from the expanded 24x24 [-4, 19] height cache
					h := heightCache[lx+dx+4][lz+dz+4]
					if h < WaterLevel {
						overhang = true
						break
					}
				}
				if overhang {
					break
				}
			}
			if overhang {
				continue
			}

			sec, sy := y/16, y%16
			surfID := sections[sec][(sy*16+lz)*16+lx] >> 4
			if surfID != 2 && surfID != 3 && surfID != 12 && surfID != 80 { // grass, dirt, sand, snow
				continue
			}

			// Create larger organic boulder
			hr := wx*31 + wz*17
			if hr < 0 {
				hr = -hr
			}
			baseRadius := 3.0 + float64(hr%3) // Radius 3 to 5
			for dx := -int(baseRadius) - 1; dx <= int(baseRadius)+1; dx++ {
				for dy := -1; dy <= int(baseRadius); dy++ {
					for dz := -int(baseRadius) - 1; dz <= int(baseRadius)+1; dz++ {
						// Squish the sphere down slightly for a flatter look
						distSq := float64(dx*dx)/(baseRadius*baseRadius) +
							float64(dy*dy)/((baseRadius-0.5)*(baseRadius-0.5)) +
							float64(dz*dz)/(baseRadius*baseRadius)

						// Add some noise to the edges for a natural shape
						noiseOff := float64((wx+dx*7+wz+dz*11+dy*13)%100) / 100.0 * 0.4
						if distSq+noiseOff > 1.0 {
							continue
						}

						nlx, nlz := lx+dx, lz+dz
						if nlx < 0 || nlx >= 16 || nlz < 0 || nlz >= 16 {
							continue
						}

						targetY := y + dy
						if targetY < 0 || targetY > 255 {
							continue
						}

						// Material variety matching the requested palette
						h := (wx + dx*31 + wz*dz*17 + dy*23)
						block := uint16(4 << 4) // default cobblestone
						r := h % 100
						if r < 30 {
							block = 1 << 4 // 30% stone
						} else if r < 60 {
							block = 1<<4 | 5 // 30% andesite (stone meta 5)
						} else if r < 80 {
							block = 48 << 4 // 20% mossy cobblestone
						} // Remaining 20% is cobblestone

						// Place the block if the location is air, grass, foliage, or dirt
						targetSec, targetSy := targetY/16, targetY%16
						targetID := sections[targetSec][(targetSy*16+nlz)*16+nlx] >> 4
						if targetID == 0 || targetID == 2 || targetID == 31 || targetID == 3 {
							sections[targetSec][(targetSy*16+nlz)*16+nlx] = block
						}
					}
				}
			}
		}
	}
}

func (g *Generator) GenerateInternal(chunkX, chunkZ int) ([SectionsPerChunk][ChunkSectionSize]uint16, [256]byte) {
	var sections [SectionsPerChunk][ChunkSectionSize]uint16
	var biomes [256]byte

	// 1. Terrain generation & height caching
	// ------------------------------------------------------------------
	// Oversized 24x24 height cache mapping local coords [-4, 19]
	// This ensures structures checking overhangs up to +/- 4 blocks
	// don't trigger slow-path noise evaluation.
	var heightCache [24][24]int

	// Subsampled height map: 6x6 grid mapping [-4, 20] (indices 0 to 5)
	var heightMap [6][6]float64
	for x := 0; x <= 5; x++ {
		for z := 0; z <= 5; z++ {
			wx := chunkX*16 + (x * 4) - 4
			wz := chunkZ*16 + (z * 4) - 4
			heightMap[x][z] = float64(g.SurfaceHeight(wx, wz))
		}
	}

	for lx := 0; lx < 24; lx++ {
		for lz := 0; lz < 24; lz++ {
			gridX := lx / 4
			gridZ := lz / 4
			if gridX > 4 {
				gridX = 4
			}
			if gridZ > 4 {
				gridZ = 4
			}

			u := float64(lx%4) / 4.0
			v := float64(lz%4) / 4.0

			h00 := heightMap[gridX][gridZ]
			h10 := heightMap[gridX+1][gridZ]
			h01 := heightMap[gridX][gridZ+1]
			h11 := heightMap[gridX+1][gridZ+1]

			h0 := h00 + (h10-h00)*u
			h1 := h01 + (h11-h01)*u
			surfH := int(h0 + (h1-h0)*v)

			heightCache[lx][lz] = surfH
		}
	}

	for lx := 0; lx < 16; lx++ {
		for lz := 0; lz < 16; lz++ {
			wx, wz := chunkX*16+lx, chunkZ*16+lz
			biome := BiomeAt(g.tempNoise, g.rainNoise, wx, wz)
			biomes[lz*16+lx] = biome.ID

			surfH := heightCache[lx+4][lz+4]

			// Fill from bottom up
			for y := 0; y < 256; y++ {
				sec := y / 16
				sy := y % 16
				idx := (sy*16+lz)*16 + lx

				if y == 0 {
					sections[sec][idx] = 7 << 4 // Bedrock
					continue
				}

				if y <= surfH {
					// Check for caves (if active)
					if y < surfH-2 && g.isCave(wx, y, wz) {
						// Fill water if below sea level
						if y <= WaterLevel {
							sections[sec][idx] = 8 << 4
						} else {
							sections[sec][idx] = 0 // Air
						}
						continue
					}

					if y < surfH {
						sections[sec][idx] = biome.FillerBlock
					} else {
						// Surface block logic
						// If under water, use sand/gravel instead of grass
						if y < WaterLevel {
							sections[sec][idx] = 12 << 4 // Sand underwater
						} else {
							sections[sec][idx] = biome.SurfaceBlock
						}
					}
				} else if y <= WaterLevel {
					// Water filling for lakes/ocean/rivers
					if y == WaterLevel && biome.HasSnow {
						sections[sec][idx] = 79 << 4 // Ice
					} else {
						sections[sec][idx] = 8 << 4 // Water
					}
				} else {
					// Air above everything else
					break
				}
			}
		}
	}

	// 2. Village placement
	g.villageGen.generateVillage(chunkX, chunkZ, &sections)

	// 3. Decorations
	chunkInVill := g.villageGen.ChunkInVillage(chunkX, chunkZ)
	g.generateBoulders(chunkX, chunkZ, &sections, &heightCache, chunkInVill)
	g.generateTrees(chunkX, chunkZ, &sections, &heightCache, chunkInVill)

	return sections, biomes
}

// GenerateChunkData produces the complete 1.8 chunk column data for (chunkX, chunkZ).
func (g *Generator) GenerateChunkData(chunkX, chunkZ int) ([]byte, uint16) {
	sections, biomes := g.GenerateInternal(chunkX, chunkZ)
	return SerializeSections(&sections, biomes)
}

// SerializeSections converts populated section arrays into 1.8 chunk wire format.
func SerializeSections(sections *[SectionsPerChunk][ChunkSectionSize]uint16, biomes [256]byte) ([]byte, uint16) {
	var primaryBitMask uint16
	var buf bytes.Buffer

	// First pass: determine which sections are non-empty
	for s := 0; s < SectionsPerChunk; s++ {
		empty := true
		for _, b := range sections[s] {
			if b != 0 {
				empty = false
				break
			}
		}
		if !empty {
			primaryBitMask |= 1 << uint(s)
		}
	}

	// Pre-create light buffers filled with 0xFF
	lightBuf := bytes.Repeat([]byte{0xFF}, 2048)

	// Write all block data for each active section first
	for s := 0; s < SectionsPerChunk; s++ {
		if primaryBitMask&(1<<uint(s)) == 0 {
			continue
		}
		// Manually encode uint16 array to little endian bytes
		// Avoids binary.Write reflection and allocations
		bData := make([]byte, ChunkSectionSize*2)
		for i, b := range sections[s] {
			bData[i*2] = byte(b)
			bData[i*2+1] = byte(b >> 8)
		}
		buf.Write(bData)
	}

	// Then write block light for each active section
	for s := 0; s < SectionsPerChunk; s++ {
		if primaryBitMask&(1<<uint(s)) == 0 {
			continue
		}
		buf.Write(lightBuf)
	}

	// Then write sky light for each active section
	for s := 0; s < SectionsPerChunk; s++ {
		if primaryBitMask&(1<<uint(s)) == 0 {
			continue
		}
		buf.Write(lightBuf)
	}

	// Biome data (256 bytes)
	buf.Write(biomes[:])

	return buf.Bytes(), primaryBitMask
}
