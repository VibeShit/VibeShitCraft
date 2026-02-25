package server

import (
	"bytes"
	"log"
	"math"
	"time"

	"github.com/VibeShit/VibeShitCraft/pkg/protocol"
)

// ItemEntity represents an item dropped on the ground.
type ItemEntity struct {
	EntityID   int32
	ItemID     int16
	Damage     int16
	Count      byte
	X, Y, Z    float64
	VX, VY, VZ float64 // Velocity tracking for drops
	SpawnTime  time.Time
}

// MobEntity represents a living entity (mob) in the world.
type MobEntity struct {
	EntityID   int32
	MobType    byte // Minecraft mob type ID (e.g., 50=Creeper, 90=Pig)
	X, Y, Z    float64
	VX, VY, VZ float64
	Yaw, Pitch float32
	HeadPitch  float32
	OnGround   bool
	// AIFunc is an optional AI callback invoked each tick. Can be nil.
	AIFunc func(mob *MobEntity, s *Server)
}

func (s *Server) entityPhysicsLoop() {
	ticker := time.NewTicker(50 * time.Millisecond) // 20 ticks per second
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.tickEntityPhysics()
		}
	}
}

// checkEntityCollision checks if the given AABB intersects with any solid blocks
func (s *Server) checkEntityCollision(x, y, z, width, height float64) bool {
	minX := int32(math.Floor(x - width/2))
	maxX := int32(math.Floor(x + width/2))
	minY := int32(math.Floor(y))
	maxY := int32(math.Floor(y + height))
	minZ := int32(math.Floor(z - width/2))
	maxZ := int32(math.Floor(z + width/2))

	for bx := minX; bx <= maxX; bx++ {
		for by := minY; by <= maxY; by++ {
			for bz := minZ; bz <= maxZ; bz++ {
				block := s.world.GetBlock(bx, by, bz)
				if isSolidBlock(block >> 4) {
					return true
				}
			}
		}
	}
	return false
}

// isSolidBlock returns true if the block ID represents a physical solid block.
func isSolidBlock(blockID uint16) bool {
	switch blockID {
	case 0: // Air
		return false
	case 8, 9: // Water
		return false
	case 10, 11: // Lava
		return false
	case 30: // Cobweb
		return false
	case 31, 32: // Tall grass, Dead bush
		return false
	case 37, 38, 39, 40, 175: // Flowers and mushrooms
		return false
	case 50, 75, 76: // Torches
		return false
	case 51: // Fire
		return false
	case 55: // Redstone wire
		return false
	case 59, 83, 104, 105, 115, 141, 142: // Crops/plants
		return false
	case 63, 68: // Signs
		return false
	case 65: // Ladder
		return false
	case 69, 77, 143: // Lever, buttons
		return false
	case 27, 28, 66, 157: // Rails
		return false
	case 70, 72, 147, 148: // Pressure plates
		return false
	case 106: // Vines
		return false
	case 171: // Carpet
		return false
	}
	return true
}

func (s *Server) tickEntityPhysics() {
	const itemGravity = 0.04
	const mobGravity = 0.08
	const drag = 0.98
	const groundDrag = 0.58 // 0.98 * 0.6 (slipperiness) approx 0.58

	s.mu.Lock()

	type movedItem struct {
		entityID   int32
		x, y, z    float64
		vx, vy, vz float64
	}
	var movedItems []movedItem

	for _, item := range s.entities {
		const itemWidth = 0.25
		const itemHeight = 0.25

		blockAtCenter := s.world.GetBlock(int32(math.Floor(item.X)), int32(math.Floor(item.Y)), int32(math.Floor(item.Z)))
		centerID := blockAtCenter >> 4
		inWater := centerID == 8 || centerID == 9
		inLava := centerID == 10 || centerID == 11

		// Apply fluid buoyancy and specialized drag, else apply standard gravity
		if inWater || inLava {
			item.VY -= itemGravity * 0.5 // Reduce gravity effect
			item.VX *= 0.5               // Extra horizontal drag from liquid
			item.VZ *= 0.5

			// Cap downward velocity in liquids
			if item.VY < -0.1 {
				item.VY = -0.1
			}
			// Slight float if entirely submerged (this requires checking head-level but simple approximation is VY cap)
		} else {
			item.VY -= itemGravity
		}

		// X movement: Attempt to move on X axis. If collision occurs, halt X velocity.
		if !s.checkEntityCollision(item.X+item.VX, item.Y, item.Z, itemWidth, itemHeight) {
			item.X += item.VX
		} else {
			item.VX = 0
		}

		// Y movement: Attempt to move on Y axis.
		onGround := false
		if !s.checkEntityCollision(item.X, item.Y+item.VY, item.Z, itemWidth, itemHeight) {
			item.Y += item.VY
		} else {
			// Collision on Y axis. If falling (VY < 0), we hit the ground.
			if item.VY < 0 {
				onGround = true
			}

			// Simulate item bounce, but NOT if we are in water/lava
			if inWater || inLava {
				item.VY = 0
			} else {
				item.VY *= -0.5 // Bounce multiplier
				if math.Abs(item.VY) < 0.1 {
					item.VY = 0 // Stop micro-bounces once energy is low enough
				}
				// When resting on solid ground (not liquid), snap the item to the exact block boundary
				// to prevent it from visually hovering or clipping into the floor
				if onGround && item.VY == 0 {
					item.Y = math.Floor(item.Y)
				}
			}
		}

		// Z movement: Attempt to move on Z axis. If collision occurs, halt Z velocity.
		if !s.checkEntityCollision(item.X, item.Y, item.Z+item.VZ, itemWidth, itemHeight) {
			item.Z += item.VZ
		} else {
			item.VZ = 0
		}

		// Apply friction & drag depending on if item is on the ground
		f := drag
		if onGround {
			f = groundDrag
		}
		item.VX *= f
		item.VY *= drag
		item.VZ *= f

		// Zero out tiny velocities to prevent endless micro-movement computations
		if math.Abs(item.VX) < 0.001 {
			item.VX = 0
		}
		if math.Abs(item.VY) < 0.001 {
			item.VY = 0
		}
		if math.Abs(item.VZ) < 0.001 {
			item.VZ = 0
		}

		movedItems = append(movedItems, movedItem{item.EntityID, item.X, item.Y, item.Z, item.VX, item.VY, item.VZ})
	}

	// Check for items touching cactus blocks and mark them for destruction
	var cactusDestroyed []int32
	for _, item := range s.entities {
		bx := int32(math.Floor(item.X))
		by := int32(math.Floor(item.Y))
		bz := int32(math.Floor(item.Z))
		// Check the block at the item's position and adjacent blocks
		for _, off := range [][3]int32{{0, 0, 0}, {0, 1, 0}} {
			block := s.world.GetBlock(bx+off[0], by+off[1], bz+off[2])
			if block>>4 == 81 { // Cactus
				cactusDestroyed = append(cactusDestroyed, item.EntityID)
				break
			}
		}
	}

	type movedMob struct {
		entityID   int32
		x, y, z    float64
		vx, vy, vz float64
		yaw, pitch float32
		onGround   bool
	}
	var movedMobs []movedMob

	for _, mob := range s.mobEntities {
		const mobWidth = 0.6
		const mobHeight = 1.8

		if mob.AIFunc != nil {
			mob.AIFunc(mob, s)
		}

		blockAtMob := s.world.GetBlock(int32(math.Floor(mob.X)), int32(math.Floor(mob.Y)), int32(math.Floor(mob.Z)))
		mobCenterID := blockAtMob >> 4
		mobInWater := mobCenterID == 8 || mobCenterID == 9
		mobInLava := mobCenterID == 10 || mobCenterID == 11

		if mobInWater || mobInLava {
			mob.VY += 0.05
			mob.VX *= 0.8
			mob.VY *= 0.8
			mob.VZ *= 0.8
		} else {
			mob.VY -= mobGravity
		}

		// X
		if !s.checkEntityCollision(mob.X+mob.VX, mob.Y, mob.Z, mobWidth, mobHeight) {
			mob.X += mob.VX
		} else {
			mob.VX = 0
		}

		// Y
		mob.OnGround = false
		if !s.checkEntityCollision(mob.X, mob.Y+mob.VY, mob.Z, mobWidth, mobHeight) {
			mob.Y += mob.VY
		} else {
			if mob.VY < 0 {
				mob.OnGround = true
				if !mobInWater && !mobInLava {
					mob.Y = math.Floor(mob.Y)
				}
			}
			if mobInWater || mobInLava {
				mob.VY = 0
			} else {
				mob.VY = 0
			}
		}

		// Z
		if !s.checkEntityCollision(mob.X, mob.Y, mob.Z+mob.VZ, mobWidth, mobHeight) {
			mob.Z += mob.VZ
		} else {
			mob.VZ = 0
		}

		f := drag
		if mob.OnGround {
			f = groundDrag
		}
		mob.VX *= f
		mob.VY *= drag
		mob.VZ *= f

		if math.Abs(mob.VX) < 0.001 {
			mob.VX = 0
		}
		if math.Abs(mob.VY) < 0.001 {
			mob.VY = 0
		}
		if math.Abs(mob.VZ) < 0.001 {
			mob.VZ = 0
		}

		movedMobs = append(movedMobs, movedMob{mob.EntityID, mob.X, mob.Y, mob.Z, mob.VX, mob.VY, mob.VZ, mob.Yaw, mob.Pitch, mob.OnGround})
	}

	s.mu.Unlock()

	// Destroy items that touched cactus
	for _, eid := range cactusDestroyed {
		s.mu.Lock()
		if _, exists := s.entities[eid]; exists {
			delete(s.entities, eid)
			s.mu.Unlock()
			s.broadcastDestroyEntity(eid)
		} else {
			s.mu.Unlock()
		}
	}

	for _, m := range movedItems {
		s.broadcastEntityTeleportByID(m.entityID, m.x, m.y, m.z, 0, 0, true)
		s.broadcastEntityVelocity(m.entityID, m.vx, m.vy, m.vz)
	}
	for _, m := range movedMobs {
		s.broadcastEntityTeleportByID(m.entityID, m.x, m.y, m.z, m.yaw, m.pitch, m.onGround)
		s.broadcastEntityVelocity(m.entityID, m.vx, m.vy, m.vz)
	}
}

// SpawnItem creates an item entity at the given position and broadcasts it.
func (s *Server) SpawnItem(x, y, z float64, vx, vy, vz float64, itemID int16, damage int16, count byte) {
	s.mu.Lock()
	eid := s.nextEID
	s.nextEID++

	item := &ItemEntity{
		EntityID:  eid,
		ItemID:    itemID,
		Damage:    damage,
		Count:     count,
		X:         x,
		Y:         y,
		Z:         z,
		VX:        vx,
		VY:        vy,
		VZ:        vz,
		SpawnTime: time.Now(),
	}
	s.entities[eid] = item
	s.mu.Unlock()

	s.broadcastSpawnItem(item)
}

func (s *Server) broadcastSpawnItem(item *ItemEntity) {
	// Spawn Object - 0x0E (Item Stack)
	spawnObj := protocol.MarshalPacket(0x0E, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, item.EntityID)
		protocol.WriteByte(w, 2) // Type: Item Stack
		protocol.WriteInt32(w, int32(item.X*32))
		protocol.WriteInt32(w, int32(item.Y*32))
		protocol.WriteInt32(w, int32(item.Z*32))
		protocol.WriteByte(w, 0) // Pitch
		protocol.WriteByte(w, 0) // Yaw
		// Data must be non-zero for velocity fields to be included
		protocol.WriteInt32(w, 1)
		protocol.WriteInt16(w, int16(item.VX*8000))
		protocol.WriteInt16(w, int16(item.VY*8000))
		protocol.WriteInt16(w, int16(item.VZ*8000))
	})

	// Entity Velocity - 0x12
	velocityPkt := protocol.MarshalPacket(0x12, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, item.EntityID)
		protocol.WriteInt16(w, int16(item.VX*8000))
		protocol.WriteInt16(w, int16(item.VY*8000))
		protocol.WriteInt16(w, int16(item.VZ*8000))
	})

	// Entity Metadata - 0x1C
	metadata := protocol.MarshalPacket(0x1C, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, item.EntityID)
		// Metadata for item stack (index 10, type 5: Slot)
		// Header byte: (type << 5) | (index & 0x1F)
		protocol.WriteByte(w, (5<<5)|10)
		protocol.WriteSlotData(w, item.ItemID, item.Count, item.Damage)
		protocol.WriteByte(w, 0x7F) // Terminator
	})

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.players {
		if s.shouldTrack(p, item.X, item.Y, item.Z) {
			p.mu.Lock()
			if p.Conn != nil {
				protocol.WritePacket(p.Conn, spawnObj)
				protocol.WritePacket(p.Conn, velocityPkt)
				protocol.WritePacket(p.Conn, metadata)
				p.trackedEntities[item.EntityID] = true
			}
			p.mu.Unlock()
		}
	}
}

// SpawnMob creates a mob entity at the given position and broadcasts it to all players.
func (s *Server) SpawnMob(x, y, z float64, mobType byte) {
	s.mu.Lock()
	eid := s.nextEID
	s.nextEID++

	mob := &MobEntity{
		EntityID: eid,
		MobType:  mobType,
		X:        x,
		Y:        y,
		Z:        z,
	}
	s.mobEntities[eid] = mob
	s.mu.Unlock()

	s.broadcastSpawnMob(mob)
	log.Printf("Spawned mob type %d (EID: %d) at (%.1f, %.1f, %.1f)", mobType, eid, x, y, z)
}

func (s *Server) broadcastSpawnMob(mob *MobEntity) {
	// Spawn Mob packet - 0x0F
	pkt := protocol.MarshalPacket(0x0F, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, mob.EntityID)
		protocol.WriteByte(w, mob.MobType)
		protocol.WriteInt32(w, int32(mob.X*32))
		protocol.WriteInt32(w, int32(mob.Y*32))
		protocol.WriteInt32(w, int32(mob.Z*32))
		protocol.WriteByte(w, byte(mob.Yaw*256/360))
		protocol.WriteByte(w, byte(mob.Pitch*256/360))
		protocol.WriteByte(w, byte(mob.HeadPitch*256/360))
		protocol.WriteInt16(w, int16(mob.VX*8000))
		protocol.WriteInt16(w, int16(mob.VY*8000))
		protocol.WriteInt16(w, int16(mob.VZ*8000))
		protocol.WriteByte(w, 0x7F) // Metadata terminator (no extra metadata)
	})

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.players {
		if s.shouldTrack(p, mob.X, mob.Y, mob.Z) {
			p.mu.Lock()
			if p.Conn != nil {
				protocol.WritePacket(p.Conn, pkt)
				p.trackedEntities[mob.EntityID] = true
			}
			p.mu.Unlock()
		}
	}
}

func (s *Server) spawnEntitiesForPlayer(player *Player) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, entity := range s.entities {
		if s.shouldTrack(player, entity.X, entity.Y, entity.Z) {
			s.sendItemToPlayer(player, entity)
			player.mu.Lock()
			player.trackedEntities[entity.EntityID] = true
			player.mu.Unlock()
		}
	}
}

func (s *Server) sendItemToPlayer(player *Player, item *ItemEntity) {
	// Spawn Object - 0x0E (Item Stack)
	spawnObj := protocol.MarshalPacket(0x0E, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, item.EntityID)
		protocol.WriteByte(w, 2) // Type: Item Stack
		protocol.WriteInt32(w, int32(item.X*32))
		protocol.WriteInt32(w, int32(item.Y*32))
		protocol.WriteInt32(w, int32(item.Z*32))
		protocol.WriteByte(w, 0) // Pitch
		protocol.WriteByte(w, 0) // Yaw
		// Data must be non-zero for velocity fields to be included
		protocol.WriteInt32(w, 1)
		protocol.WriteInt16(w, int16(item.VX*8000))
		protocol.WriteInt16(w, int16(item.VY*8000))
		protocol.WriteInt16(w, int16(item.VZ*8000))
	})

	// Entity Metadata - 0x1C
	metadata := protocol.MarshalPacket(0x1C, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, item.EntityID)
		// Metadata for item stack (index 10, type 5: Slot)
		// Header byte: (type << 5) | (index & 0x1F)
		protocol.WriteByte(w, (5<<5)|10)
		protocol.WriteSlotData(w, item.ItemID, item.Count, item.Damage)
		protocol.WriteByte(w, 0x7F) // Terminator
	})

	player.mu.Lock()
	if player.Conn != nil {
		protocol.WritePacket(player.Conn, spawnObj)
		protocol.WritePacket(player.Conn, metadata)
	}
	player.mu.Unlock()
}

func (s *Server) spawnMobEntitiesForPlayer(player *Player) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, mob := range s.mobEntities {
		if s.shouldTrack(player, mob.X, mob.Y, mob.Z) {
			s.sendMobToPlayer(player, mob)
			player.mu.Lock()
			player.trackedEntities[mob.EntityID] = true
			player.mu.Unlock()
		}
	}
}

func (s *Server) sendMobToPlayer(player *Player, mob *MobEntity) {
	pkt := protocol.MarshalPacket(0x0F, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, mob.EntityID)
		protocol.WriteByte(w, mob.MobType)
		protocol.WriteInt32(w, int32(mob.X*32))
		protocol.WriteInt32(w, int32(mob.Y*32))
		protocol.WriteInt32(w, int32(mob.Z*32))
		protocol.WriteByte(w, byte(mob.Yaw*256/360))
		protocol.WriteByte(w, byte(mob.Pitch*256/360))
		protocol.WriteByte(w, byte(mob.HeadPitch*256/360))
		protocol.WriteInt16(w, int16(mob.VX*8000))
		protocol.WriteInt16(w, int16(mob.VY*8000))
		protocol.WriteInt16(w, int16(mob.VZ*8000))
		protocol.WriteByte(w, 0x7F) // Metadata terminator
	})

	player.mu.Lock()
	if player.Conn != nil {
		protocol.WritePacket(player.Conn, pkt)
	}
	player.mu.Unlock()
}

func (s *Server) broadcastCollectItem(collectedID, collectorID int32) {
	pkt := protocol.MarshalPacket(0x0D, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, collectedID)
		protocol.WriteVarInt(w, collectorID)
	})

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.players {
		p.mu.Lock()
		if p.Conn != nil {
			protocol.WritePacket(p.Conn, pkt)
		}
		p.mu.Unlock()
	}
}

func (s *Server) itemPickupLoop(player *Player, stop chan struct{}) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.mu.RLock()
			if len(s.entities) == 0 {
				s.mu.RUnlock()
				continue
			}
			// Copy entities to avoid long lock
			entities := make([]*ItemEntity, 0, len(s.entities))
			for _, e := range s.entities {
				entities = append(entities, e)
			}
			s.mu.RUnlock()

			player.mu.Lock()
			px, py, pz := player.X, player.Y, player.Z
			isDead := player.IsDead
			player.mu.Unlock()

			if isDead {
				continue
			}

			for _, e := range entities {
				// Skip recently spawned items (0.75 second pickup delay)
				if time.Since(e.SpawnTime) < 750*time.Millisecond {
					continue
				}

				dx := e.X - px
				dy := e.Y - py
				dz := e.Z - pz
				distSq := dx*dx + dy*dy + dz*dz

				if distSq < 6.25 { // 2.5 blocks range
					// Try to pick up
					player.mu.Lock()
					slotIndex, ok := addItemToInventory(player, e.ItemID, e.Damage, e.Count)
					if ok {
						// Success!
						slot := player.Inventory[slotIndex]
						pkt := protocol.MarshalPacket(0x2F, func(w *bytes.Buffer) {
							protocol.WriteByte(w, 0) // Window ID 0 = player inventory
							protocol.WriteInt16(w, int16(slotIndex))
							protocol.WriteSlotData(w, slot.ItemID, slot.Count, slot.Damage)
						})
						if player.Conn != nil {
							protocol.WritePacket(player.Conn, pkt)
						}
						player.mu.Unlock()

						// Remove entity
						s.mu.Lock()
						// Re-check if it's still there (some other player might have picked it up)
						if _, exists := s.entities[e.EntityID]; exists {
							delete(s.entities, e.EntityID)
							s.mu.Unlock()

							// Broadcast collect and destroy
							s.broadcastCollectItem(e.EntityID, player.EntityID)
							s.broadcastDestroyEntity(e.EntityID)
							log.Printf("Player %s picked up item %d:%d", player.Username, e.ItemID, e.Damage)
							// Picking up an item may have filled or stacked the
							// currently selected hotbar slot; notify others so
							// they see the updated held item.
							s.broadcastHeldItem(player)
						} else {
							s.mu.Unlock()
						}
						continue
					}
					player.mu.Unlock()
				}
			}
		}
	}
}
