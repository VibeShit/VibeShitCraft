package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/StoreStation/VibeShitCraft/pkg/chat"
	"github.com/StoreStation/VibeShitCraft/pkg/protocol"
	"github.com/StoreStation/VibeShitCraft/pkg/world"
)

// Gamemode constants matching Minecraft protocol values.
const (
	GameModeSurvival  byte = 0
	GameModeCreative  byte = 1
	GameModeAdventure byte = 2
	GameModeSpectator byte = 3
)

// Entity metadata flags (index 0, type byte).
const (
	EntityFlagInvisible byte = 0x20
)

// DefaultSeed is used when no seed is provided (0 means random).
const DefaultSeed = 0

// Config holds server configuration.
type Config struct {
	Address         string
	MaxPlayers      int
	MOTD            string
	Seed            int64
	DefaultGameMode byte
}

// DefaultConfig returns a default server configuration.
func DefaultConfig() Config {
	return Config{
		Address:    ":25565",
		MaxPlayers: 20,
		MOTD:       "A VibeShitCraft Server",
	}
}

// ViewDistance is the radius (in chunks) around the player to keep loaded.
const ViewDistance = 7

// ChunkPos represents a chunk coordinate.
type ChunkPos struct {
	X, Z int32
}

// Server represents a Minecraft 1.8 server.
type Server struct {
	config      Config
	listener    net.Listener
	players     map[int32]*Player
	entities    map[int32]*ItemEntity
	mobEntities map[int32]*MobEntity
	mu          sync.RWMutex
	nextEID     int32
	stopCh      chan struct{}
	world       *world.World
}

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

// Slot represents an inventory slot.
type Slot struct {
	ItemID int16
	Count  byte
	Damage int16
}

// Player represents a connected player.
type Player struct {
	EntityID         int32
	Username         string
	UUID             [16]byte
	Conn             net.Conn
	State            int
	GameMode         byte
	X, Y, Z          float64
	Yaw              float32
	Pitch            float32
	OnGround         bool
	Health           float32
	IsDead           bool
	Inventory        [45]Slot
	Cursor           Slot
	ActiveSlot       int16
	loadedChunks     map[ChunkPos]bool
	lastChunkX       int32
	lastChunkZ       int32
	CraftTableGrid   [9]Slot // 3x3 crafting grid for crafting table window
	CraftTableOutput Slot    // Crafting output for crafting table window
	OpenWindowID     byte    // Currently open window ID (0 = none/player inventory)
	NoClip           bool    // True when in spectator mode (can pass through blocks)
	DragSlots        []int16 // Slots being dragged over in mode 5
	DragButton       int     // 0=left drag, 1=right drag
	mu               sync.Mutex
}

// New creates a new server with the given configuration.
func New(config Config) *Server {
	seed := config.Seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	log.Printf("World seed: %d", seed)
	return &Server{
		config:      config,
		players:     make(map[int32]*Player),
		entities:    make(map[int32]*ItemEntity),
		mobEntities: make(map[int32]*MobEntity),
		nextEID:     1,
		stopCh:      make(chan struct{}),
		world:       world.NewWorld(seed),
	}
}

// Start begins listening for connections.
func (s *Server) Start() error {
	var err error
	s.listener, err = net.Listen("tcp", s.config.Address)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.config.Address, err)
	}
	log.Printf("Server listening on %s", s.config.Address)

	go s.acceptLoop()
	go s.entityPhysicsLoop()
	return nil
}

// Stop gracefully shuts down the server.
func (s *Server) Stop() {
	close(s.stopCh)
	if s.listener != nil {
		s.listener.Close()
	}
	s.mu.RLock()
	for _, p := range s.players {
		p.Conn.Close()
	}
	s.mu.RUnlock()
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

func (s *Server) tickEntityPhysics() {
	const gravity = 0.04
	const drag = 0.98
	const groundDrag = 0.5

	s.mu.Lock()

	// Collect entities that moved so we can broadcast after releasing the lock
	type movedItem struct {
		entityID int32
		x, y, z  float64
	}
	var movedItems []movedItem

	for _, item := range s.entities {
		if item.VX == 0 && item.VY == 0 && item.VZ == 0 {
			// Check if entity is supported by ground
			blockBelow := s.world.GetBlock(int32(math.Floor(item.X)), int32(math.Floor(item.Y-0.1)), int32(math.Floor(item.Z)))
			if blockBelow>>4 != 0 {
				continue
			}
			// Not on ground and no velocity - start falling
		}

		// Apply gravity
		item.VY -= gravity

		// Apply drag
		item.VX *= drag
		item.VY *= drag
		item.VZ *= drag

		// Calculate new position
		newX := item.X + item.VX
		newY := item.Y + item.VY
		newZ := item.Z + item.VZ

		// Ground collision check
		blockAtNew := s.world.GetBlock(int32(math.Floor(newX)), int32(math.Floor(newY)), int32(math.Floor(newZ)))
		if blockAtNew>>4 != 0 {
			// Hit solid block - place on top
			newY = math.Floor(newY) + 1.0
			item.VY = 0
			item.VX *= groundDrag
			item.VZ *= groundDrag

			// Stop tiny velocities
			if math.Abs(item.VX) < 0.001 {
				item.VX = 0
			}
			if math.Abs(item.VZ) < 0.001 {
				item.VZ = 0
			}
		}

		item.X = newX
		item.Y = newY
		item.Z = newZ

		movedItems = append(movedItems, movedItem{item.EntityID, item.X, item.Y, item.Z})
	}

	// Tick mob entities: gravity, physics, and optional AI
	type movedMob struct {
		entityID   int32
		x, y, z    float64
		yaw, pitch float32
		onGround   bool
	}
	var movedMobs []movedMob

	for _, mob := range s.mobEntities {
		// Run AI if present
		if mob.AIFunc != nil {
			mob.AIFunc(mob, s)
		}

		if mob.VX == 0 && mob.VY == 0 && mob.VZ == 0 {
			blockBelow := s.world.GetBlock(int32(math.Floor(mob.X)), int32(math.Floor(mob.Y-0.1)), int32(math.Floor(mob.Z)))
			if blockBelow>>4 != 0 {
				mob.OnGround = true
				continue
			}
			mob.OnGround = false
		}

		// Apply gravity
		mob.VY -= gravity

		// Apply drag
		mob.VX *= drag
		mob.VY *= drag
		mob.VZ *= drag

		newX := mob.X + mob.VX
		newY := mob.Y + mob.VY
		newZ := mob.Z + mob.VZ

		// Ground collision
		blockAtNew := s.world.GetBlock(int32(math.Floor(newX)), int32(math.Floor(newY)), int32(math.Floor(newZ)))
		if blockAtNew>>4 != 0 {
			newY = math.Floor(newY) + 1.0
			mob.VY = 0
			mob.VX *= groundDrag
			mob.VZ *= groundDrag
			mob.OnGround = true

			if math.Abs(mob.VX) < 0.001 {
				mob.VX = 0
			}
			if math.Abs(mob.VZ) < 0.001 {
				mob.VZ = 0
			}
		} else {
			mob.OnGround = false
		}

		mob.X = newX
		mob.Y = newY
		mob.Z = newZ

		movedMobs = append(movedMobs, movedMob{mob.EntityID, mob.X, mob.Y, mob.Z, mob.Yaw, mob.Pitch, mob.OnGround})
	}

	s.mu.Unlock()

	// Broadcast entity teleport packets
	for _, m := range movedItems {
		s.broadcastEntityTeleportByID(m.entityID, m.x, m.y, m.z, 0, 0, true)
	}
	for _, m := range movedMobs {
		s.broadcastEntityTeleportByID(m.entityID, m.x, m.y, m.z, m.yaw, m.pitch, m.onGround)
	}
}

func (s *Server) broadcastEntityTeleportByID(entityID int32, x, y, z float64, yaw, pitch float32, onGround bool) {
	pkt := protocol.MarshalPacket(0x18, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, entityID)
		protocol.WriteInt32(w, int32(x*32))
		protocol.WriteInt32(w, int32(y*32))
		protocol.WriteInt32(w, int32(z*32))
		protocol.WriteByte(w, byte(yaw*256/360))
		protocol.WriteByte(w, byte(pitch*256/360))
		protocol.WriteBool(w, onGround)
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

func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
				log.Printf("Accept error: %v", err)
				continue
			}
		}
		go s.handleConnection(conn)
	}
}

func (s *Server) handleConnection(conn net.Conn) {
	defer conn.Close()

	state := protocol.StateHandshaking

	for {
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		pkt, err := protocol.ReadPacket(conn)
		if err != nil {
			return
		}

		switch state {
		case protocol.StateHandshaking:
			if pkt.ID == 0x00 {
				newState, err := s.handleHandshake(pkt)
				if err != nil {
					log.Printf("Handshake error: %v", err)
					return
				}
				state = newState
			}
		case protocol.StateStatus:
			switch pkt.ID {
			case 0x00:
				s.handleStatusRequest(conn)
			case 0x01:
				s.handlePing(conn, pkt)
				return
			}
		case protocol.StateLogin:
			if pkt.ID == 0x00 {
				player, err := s.handleLoginStart(conn, pkt)
				if err != nil {
					log.Printf("Login error: %v", err)
					return
				}
				s.handlePlay(player)
				return
			}
		}
	}
}

func (s *Server) handleHandshake(pkt *protocol.Packet) (int, error) {
	r := bytes.NewReader(pkt.Data)

	protocolVersion, _, err := protocol.ReadVarInt(r)
	if err != nil {
		return 0, err
	}
	_ = protocolVersion

	// Server address
	_, err = protocol.ReadString(r)
	if err != nil {
		return 0, err
	}

	// Server port
	_, err = protocol.ReadUint16(r)
	if err != nil {
		return 0, err
	}

	// Next state
	nextState, _, err := protocol.ReadVarInt(r)
	if err != nil {
		return 0, err
	}

	return int(nextState), nil
}

func (s *Server) handleStatusRequest(conn net.Conn) {
	response := map[string]interface{}{
		"version": map[string]interface{}{
			"name":     "1.8.9",
			"protocol": protocol.ProtocolVersion,
		},
		"players": map[string]interface{}{
			"max":    s.config.MaxPlayers,
			"online": s.playerCount(),
			"sample": []interface{}{},
		},
		"description": map[string]interface{}{
			"text": s.config.MOTD,
		},
	}

	jsonResp, err := json.Marshal(response)
	if err != nil {
		log.Printf("Failed to marshal status response: %v", err)
		return
	}
	pkt := protocol.MarshalPacket(0x00, func(w *bytes.Buffer) {
		protocol.WriteString(w, string(jsonResp))
	})
	protocol.WritePacket(conn, pkt)
}

func (s *Server) handlePing(conn net.Conn, pkt *protocol.Packet) {
	r := bytes.NewReader(pkt.Data)
	payload, err := protocol.ReadInt64(r)
	if err != nil {
		return
	}

	resp := protocol.MarshalPacket(0x01, func(w *bytes.Buffer) {
		protocol.WriteInt64(w, payload)
	})
	protocol.WritePacket(conn, resp)
}

func (s *Server) handleLoginStart(conn net.Conn, pkt *protocol.Packet) (*Player, error) {
	r := bytes.NewReader(pkt.Data)
	username, err := protocol.ReadString(r)
	if err != nil {
		return nil, err
	}

	log.Printf("Player %s is logging in", username)

	// Generate offline-mode UUID (UUID v3 based on "OfflinePlayer:" + username)
	uuid := offlineUUID(username)

	s.mu.Lock()
	eid := s.nextEID
	s.nextEID++
	s.mu.Unlock()

	// Find spawn height at (8, 8) by checking the generator
	spawnY := float64(s.world.Gen.SurfaceHeight(8, 8)) + 1.0

	player := &Player{
		EntityID: eid,
		Username: username,
		UUID:     uuid,
		Conn:     conn,
		State:    protocol.StatePlay,
		GameMode: s.config.DefaultGameMode,
		X:        8,
		Y:        spawnY,
		Z:        8,
		Yaw:      0,
		Pitch:    0,
		OnGround: true,
		Health:   20.0,
		IsDead:   false,
		NoClip:   s.config.DefaultGameMode == GameModeSpectator,
	}

	// Initialize all inventory slots as empty
	for i := range player.Inventory {
		player.Inventory[i].ItemID = -1
	}
	player.Cursor.ItemID = -1
	player.ActiveSlot = 0
	player.CraftTableOutput.ItemID = -1
	for i := range player.CraftTableGrid {
		player.CraftTableGrid[i].ItemID = -1
	}

	// Send Login Success
	loginSuccess := protocol.MarshalPacket(0x02, func(w *bytes.Buffer) {
		protocol.WriteString(w, formatUUID(uuid))
		protocol.WriteString(w, username)
	})
	if err := protocol.WritePacket(conn, loginSuccess); err != nil {
		return nil, err
	}

	return player, nil
}

func (s *Server) handlePlay(player *Player) {
	conn := player.Conn

	// Send Join Game
	joinGame := protocol.MarshalPacket(0x01, func(w *bytes.Buffer) {
		protocol.WriteInt32(w, player.EntityID)          // Entity ID
		protocol.WriteByte(w, player.GameMode)           // Gamemode
		protocol.WriteByte(w, 0)                         // Dimension: overworld
		protocol.WriteByte(w, 0)                         // Difficulty: peaceful
		protocol.WriteByte(w, byte(s.config.MaxPlayers)) // Max players
		protocol.WriteString(w, "default")               // Level type
		protocol.WriteBool(w, false)                     // Reduced debug info
	})
	protocol.WritePacket(conn, joinGame)

	// Send Spawn Position
	spawnPos := protocol.MarshalPacket(0x05, func(w *bytes.Buffer) {
		protocol.WritePosition(w, 8, int32(player.Y), 8)
	})
	protocol.WritePacket(conn, spawnPos)

	// Send Player Abilities
	s.sendPlayerAbilities(player)

	// Send Player Position and Look
	posLook := protocol.MarshalPacket(0x08, func(w *bytes.Buffer) {
		protocol.WriteFloat64(w, player.X)
		protocol.WriteFloat64(w, player.Y)
		protocol.WriteFloat64(w, player.Z)
		protocol.WriteFloat32(w, player.Yaw)
		protocol.WriteFloat32(w, player.Pitch)
		protocol.WriteByte(w, 0) // Flags (all absolute)
	})
	protocol.WritePacket(conn, posLook)

	// Send chunks around player
	s.sendSpawnChunks(player)

	// Send any block modifications to the new player
	s.sendBlockModifications(conn)

	// Register player
	s.mu.Lock()
	s.players[player.EntityID] = player
	s.mu.Unlock()

	// Broadcast join message
	s.broadcastChat(chat.Colored(player.Username+" joined the game", "yellow"))

	log.Printf("Player %s (EID: %d) joined the game", player.Username, player.EntityID)

	// Start keep-alive ticker
	stopKeepAlive := make(chan struct{})
	go s.keepAliveLoop(player, stopKeepAlive)

	// Start health regeneration loop
	stopRegen := make(chan struct{})
	go s.regenerationLoop(player, stopRegen)

	// Start item pickup loop
	stopPickup := make(chan struct{})
	go s.itemPickupLoop(player, stopPickup)

	defer func() {
		close(stopKeepAlive)
		close(stopRegen)
		close(stopPickup)
		// Remove from tab list before removing from players map
		s.broadcastPlayerListRemove(player.UUID)
		s.mu.Lock()
		delete(s.players, player.EntityID)
		s.mu.Unlock()
		s.broadcastChat(chat.Colored(player.Username+" left the game", "yellow"))
		// Despawn for other players
		s.broadcastDestroyEntity(player.EntityID)
		log.Printf("Player %s disconnected", player.Username)
	}()

	// Spawn this player for others and others for this player
	s.spawnPlayerForOthers(player)
	s.spawnOthersForPlayer(player)

	// Add self to own tab list so the player can see themselves
	selfListAdd := protocol.MarshalPacket(0x38, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, 0) // Action: Add Player
		protocol.WriteVarInt(w, 1) // Number of players
		protocol.WriteUUID(w, player.UUID)
		protocol.WriteString(w, player.Username)
		protocol.WriteVarInt(w, 0)                       // Number of properties
		protocol.WriteVarInt(w, int32(player.GameMode))  // Gamemode
		protocol.WriteVarInt(w, 0)                       // Ping
		protocol.WriteBool(w, false)                     // Has display name
	})
	player.mu.Lock()
	protocol.WritePacket(player.Conn, selfListAdd)
	player.mu.Unlock()

	// Spawn existing item entities for this player
	s.spawnEntitiesForPlayer(player)

	// Spawn existing mob entities for this player
	s.spawnMobEntitiesForPlayer(player)

	// Main packet read loop
	for {
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		pkt, err := protocol.ReadPacket(conn)
		if err != nil {
			return
		}

		s.handlePlayPacket(player, pkt)
	}
}

func (s *Server) regenerationLoop(player *Player, stop chan struct{}) {
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			player.mu.Lock()
			if player.Health < 20.0 && !player.IsDead && player.GameMode == GameModeSurvival {
				player.Health += 1.0
				if player.Health > 20.0 {
					player.Health = 20.0
				}
				player.mu.Unlock()
				s.sendHealth(player)
			} else {
				player.mu.Unlock()
			}
		}
	}
}

func (s *Server) handlePlayPacket(player *Player, pkt *protocol.Packet) {
	r := bytes.NewReader(pkt.Data)

	switch pkt.ID {
	case 0x00: // Keep Alive
		// Client responding to keep alive, ignore

	case 0x01: // Chat Message
		message, err := protocol.ReadString(r)
		if err != nil {
			return
		}
		if len(message) > 256 {
			message = message[:256]
		}
		// Route commands (messages starting with /)
		if strings.HasPrefix(message, "/") {
			s.handleCommand(player, message)
			return
		}
		chatMsg := chat.Message{
			Text: "",
			Extra: []chat.Message{
				chat.Colored("<"+player.Username+"> ", "white"),
				chat.Text(message),
			},
		}
		log.Printf("<%s> %s", player.Username, message)
		s.broadcastChat(chatMsg)

	case 0x04: // Player Position
		x, _ := protocol.ReadFloat64(r)
		y, _ := protocol.ReadFloat64(r)
		z, _ := protocol.ReadFloat64(r)
		onGround, _ := protocol.ReadBool(r)
		player.mu.Lock()
		player.X = x
		player.Y = y
		player.Z = z
		player.OnGround = onGround
		player.mu.Unlock()
		s.broadcastEntityTeleport(player)
		s.sendChunkUpdates(player)

	case 0x05: // Player Look
		yaw, _ := protocol.ReadFloat32(r)
		pitch, _ := protocol.ReadFloat32(r)
		onGround, _ := protocol.ReadBool(r)
		player.mu.Lock()
		player.Yaw = yaw
		player.Pitch = pitch
		player.OnGround = onGround
		player.mu.Unlock()
		s.broadcastEntityLook(player)

	case 0x06: // Player Position And Look
		x, _ := protocol.ReadFloat64(r)
		y, _ := protocol.ReadFloat64(r)
		z, _ := protocol.ReadFloat64(r)
		yaw, _ := protocol.ReadFloat32(r)
		pitch, _ := protocol.ReadFloat32(r)
		onGround, _ := protocol.ReadBool(r)
		player.mu.Lock()
		player.X = x
		player.Y = y
		player.Z = z
		player.Yaw = yaw
		player.Pitch = pitch
		player.OnGround = onGround
		player.mu.Unlock()
		s.broadcastEntityTeleport(player)
		s.sendChunkUpdates(player)

	case 0x03: // Player (on ground)
		// Just an update for on ground status
		onGround, _ := protocol.ReadBool(r)
		player.mu.Lock()
		player.OnGround = onGround
		player.mu.Unlock()

	case 0x09: // Held Item Change
		slot, _ := protocol.ReadInt16(r)
		player.mu.Lock()
		player.ActiveSlot = slot
		player.mu.Unlock()
		// Inform other players about the newly selected held item so that
		// they see the correct item in this player's hand.
		s.broadcastHeldItem(player)

	case 0x07: // Player Digging
		status, _ := protocol.ReadByte(r)
		x, y, z, _ := protocol.ReadPosition(r)
		_, _ = protocol.ReadByte(r) // face
		if player.GameMode == GameModeSpectator {
			return // spectators can't interact
		}
		if status == 0 && player.GameMode == GameModeCreative {
			// Creative mode: instant break on start digging
			s.handleBlockBreak(player, x, y, z)
		} else if status == 2 {
			// Survival: finished digging
			s.handleBlockBreak(player, x, y, z)
		} else if status == 0 && player.GameMode == GameModeSurvival {
			// Survival: instant-break for zero-hardness blocks (torches, flowers, etc.)
			blockState := s.world.GetBlock(x, y, z)
			if world.IsInstantBreak(blockState >> 4) {
				s.handleBlockBreak(player, x, y, z)
			}
		} else if status == 3 || status == 4 {
			// Status 3 = drop item stack (Ctrl+Q), status 4 = drop single item (Q)
			player.mu.Lock()
			slotIndex := 36 + player.ActiveSlot
			if player.Inventory[slotIndex].ItemID != -1 {
				dropItemID := player.Inventory[slotIndex].ItemID
				dropDamage := player.Inventory[slotIndex].Damage
				var dropCount byte = 1
				if status == 3 {
					// Ctrl+Q: drop entire stack
					dropCount = player.Inventory[slotIndex].Count
				}

				player.Inventory[slotIndex].Count -= dropCount
				if player.Inventory[slotIndex].Count <= 0 {
					player.Inventory[slotIndex] = Slot{ItemID: -1}
				}

				// Sync slot to client
				slot := player.Inventory[slotIndex]
				syncPkt := protocol.MarshalPacket(0x2F, func(w *bytes.Buffer) {
					protocol.WriteByte(w, 0) // Window ID 0 = player inventory
					protocol.WriteInt16(w, int16(slotIndex))
					protocol.WriteSlotData(w, slot.ItemID, slot.Count, slot.Damage)
				})
				if player.Conn != nil {
					protocol.WritePacket(player.Conn, syncPkt)
				}

				px, py, pz := player.X, player.Y, player.Z
				f1 := math.Sin(float64(player.Yaw) * math.Pi / 180.0)
				f2 := math.Cos(float64(player.Yaw) * math.Pi / 180.0)
				f3 := math.Sin(float64(player.Pitch) * math.Pi / 180.0)
				f4 := math.Cos(float64(player.Pitch) * math.Pi / 180.0)
				vx := -f1 * f4 * 0.3
				vy := -f3*0.3 + 0.1
				vz := f2 * f4 * 0.3

				player.mu.Unlock()
				s.SpawnItem(px, py+1.5, pz, vx, vy, vz, dropItemID, dropDamage, dropCount)
			} else {
				player.mu.Unlock()
			}
		}

	case 0x0A: // Animation
		// Broadcast arm swing to other players
		s.broadcastAnimation(player, 0)

	case 0x15: // Client Settings
		// Ignore for now

	case 0x13: // Player Abilities (serverbound)
		// Client sends this when toggling flying or when F3+N is pressed to
		// cycle between Creative and Spectator.
		clientFlags, _ := protocol.ReadByte(r)
		_, _ = protocol.ReadFloat32(r) // Flying speed
		_, _ = protocol.ReadFloat32(r) // Walking speed

		player.mu.Lock()
		currentMode := player.GameMode
		player.mu.Unlock()

		// Detect F3+N gamemode toggle by checking the Instant Break flag (0x08).
		// Creative mode sets 0x08; Spectator does not.
		if currentMode == GameModeCreative && (clientFlags&0x08) == 0 {
			// Client removed Instant Break flag → switching to Spectator
			s.switchGameMode(player, GameModeSpectator)
		} else if currentMode == GameModeSpectator && (clientFlags&0x08) != 0 {
			// Client set Instant Break flag → switching to Creative
			s.switchGameMode(player, GameModeCreative)
		}

	case 0x17: // Plugin Message
		// Ignore for now

	case 0x02: // Use Entity
		targetID, _, err := protocol.ReadVarInt(r)
		if err != nil {
			return
		}
		useType, _, err := protocol.ReadVarInt(r)
		if err != nil {
			return
		}
		if useType == 1 { // Attack
			s.handleAttack(player, targetID)
		}

	case 0x16: // Client Status
		actionID, _, err := protocol.ReadVarInt(r)
		if err != nil {
			return
		}
		if actionID == 0 { // Perform Respawn
			s.handleRespawn(player)
		}

	case 0x0D: // Close Window
		windowID, _ := protocol.ReadByte(r)
		player.mu.Lock()
		var dropItems []Slot
		if windowID == 0 {
			// Return items from 2x2 crafting grid to inventory
			for i := 1; i <= 4; i++ {
				if player.Inventory[i].ItemID != -1 {
					_, ok := addItemToInventory(player, player.Inventory[i].ItemID, player.Inventory[i].Damage, player.Inventory[i].Count)
					if !ok {
						dropItems = append(dropItems, player.Inventory[i])
					}
					player.Inventory[i] = Slot{ItemID: -1}
				}
			}
			player.Inventory[0] = Slot{ItemID: -1}
		} else if windowID == player.OpenWindowID {
			// Return items from crafting table grid to inventory
			for i := 0; i < 9; i++ {
				if player.CraftTableGrid[i].ItemID != -1 {
					_, ok := addItemToInventory(player, player.CraftTableGrid[i].ItemID, player.CraftTableGrid[i].Damage, player.CraftTableGrid[i].Count)
					if !ok {
						dropItems = append(dropItems, player.CraftTableGrid[i])
					}
					player.CraftTableGrid[i] = Slot{ItemID: -1}
				}
			}
			player.CraftTableOutput = Slot{ItemID: -1}
			player.OpenWindowID = 0
		}
		if player.Cursor.ItemID != -1 {
			_, ok := addItemToInventory(player, player.Cursor.ItemID, player.Cursor.Damage, player.Cursor.Count)
			if !ok {
				dropItems = append(dropItems, player.Cursor)
			}
			player.Cursor = Slot{ItemID: -1}
		}
		px, py, pz := player.X, player.Y, player.Z
		player.mu.Unlock()
		for _, item := range dropItems {
			s.SpawnItem(px, py+1.5, pz, 0, 0.2, 0, item.ItemID, item.Damage, item.Count)
		}

	case 0x08: // Block Placement
		x, y, z, _ := protocol.ReadPosition(r)
		face, _ := protocol.ReadByte(r)
		// Read held item slot data
		itemID, _, damage, _ := protocol.ReadSlotData(r)
		// Cursor position (3 bytes)
		cursorX, _ := protocol.ReadByte(r)
		cursorY, _ := protocol.ReadByte(r)
		_, _ = protocol.ReadByte(r) // cursorZ - unused

		if player.GameMode == GameModeSpectator || player.GameMode == GameModeAdventure {
			player.mu.Lock()
			slotIndex := 36 + player.ActiveSlot
			slot := player.Inventory[slotIndex]
			pkt := protocol.MarshalPacket(0x2F, func(w *bytes.Buffer) {
				protocol.WriteByte(w, 0)
				protocol.WriteInt16(w, int16(slotIndex))
				protocol.WriteSlotData(w, slot.ItemID, slot.Count, slot.Damage)
			})
			if player.Conn != nil {
				protocol.WritePacket(player.Conn, pkt)
			}
			player.mu.Unlock()
			return // spectators and adventure mode can't place blocks
		}

		// Special position (-1, -1, -1) means "use item" not placement
		if x == -1 && y == 255 && z == -1 {
			player.mu.Lock()
			slotIndex := 36 + player.ActiveSlot
			slot := player.Inventory[slotIndex]

			// Spawn eggs used in air (use item) — spawn at player's look position
			if itemID == 383 {
				px, py, pz := player.X, player.Y, player.Z
				mobType := byte(slot.Damage)
				gameMode := player.GameMode
				player.mu.Unlock()

				s.SpawnMob(px, py+1.0, pz, mobType)

				if gameMode == GameModeSurvival {
					player.mu.Lock()
					si := 36 + player.ActiveSlot
					player.Inventory[si].Count--
					if player.Inventory[si].Count <= 0 {
						player.Inventory[si] = Slot{ItemID: -1}
					}
					sl := player.Inventory[si]
					syncPkt := protocol.MarshalPacket(0x2F, func(w *bytes.Buffer) {
						protocol.WriteByte(w, 0)
						protocol.WriteInt16(w, int16(si))
						protocol.WriteSlotData(w, sl.ItemID, sl.Count, sl.Damage)
					})
					if player.Conn != nil {
						protocol.WritePacket(player.Conn, syncPkt)
					}
					player.mu.Unlock()
				}
				log.Printf("Player %s used spawn egg (mob type %d) in air", player.Username, mobType)
				return
			}

			log.Printf("Aborting USE ITEM for %d. Server thinks active slot %d (index %d) has item %d:%d qty %d", itemID, player.ActiveSlot, slotIndex, slot.ItemID, slot.Damage, slot.Count)
			pkt := protocol.MarshalPacket(0x2F, func(w *bytes.Buffer) {
				protocol.WriteByte(w, 0) // Window ID 0 = player inventory
				protocol.WriteInt16(w, int16(slotIndex))
				protocol.WriteSlotData(w, slot.ItemID, slot.Count, slot.Damage)
			})
			if player.Conn != nil {
				protocol.WritePacket(player.Conn, pkt)
			}
			player.mu.Unlock()
			return
		}

		// Check if right-clicked block is a crafting table
		clickedBlockState := s.world.GetBlock(x, y, z)
		clickedBlockID := clickedBlockState >> 4
		if clickedBlockID == 58 { // Crafting Table
			player.mu.Lock()
			player.OpenWindowID = 1
			for i := range player.CraftTableGrid {
				player.CraftTableGrid[i] = Slot{ItemID: -1}
			}
			player.CraftTableOutput = Slot{ItemID: -1}
			openPkt := protocol.MarshalPacket(0x2D, func(w *bytes.Buffer) {
				protocol.WriteByte(w, 1)                            // Window ID
				protocol.WriteString(w, "minecraft:crafting_table") // Window Type
				protocol.WriteString(w, `{"text":"Crafting"}`)      // Window Title
				protocol.WriteByte(w, 0)                            // Number of Slots
			})
			if player.Conn != nil {
				protocol.WritePacket(player.Conn, openPkt)
			}
			player.mu.Unlock()
			return
		}

		// Handle spawn egg right-click on a block
		if itemID == 383 {
			tx, ty, tz := faceOffset(x, y, z, face)
			player.mu.Lock()
			slotIndex := 36 + player.ActiveSlot
			slot := player.Inventory[slotIndex]
			mobType := byte(slot.Damage)
			gameMode := player.GameMode
			player.mu.Unlock()

			s.SpawnMob(float64(tx)+0.5, float64(ty), float64(tz)+0.5, mobType)

			if gameMode == GameModeSurvival {
				player.mu.Lock()
				player.Inventory[slotIndex].Count--
				if player.Inventory[slotIndex].Count <= 0 {
					player.Inventory[slotIndex] = Slot{ItemID: -1}
				}
				sl := player.Inventory[slotIndex]
				syncPkt := protocol.MarshalPacket(0x2F, func(w *bytes.Buffer) {
					protocol.WriteByte(w, 0)
					protocol.WriteInt16(w, int16(slotIndex))
					protocol.WriteSlotData(w, sl.ItemID, sl.Count, sl.Damage)
				})
				if player.Conn != nil {
					protocol.WritePacket(player.Conn, syncPkt)
				}
				player.mu.Unlock()
			}
			log.Printf("Player %s used spawn egg (mob type %d) at (%d, %d, %d)", player.Username, mobType, tx, ty, tz)
			return
		}

		// Determine if the item is a door and map to its block ID
		var isDoor bool
		var placedBlockID int16 = itemID

		switch itemID {
		case 324:
			placedBlockID = 64
			isDoor = true
		case 330:
			placedBlockID = 71
			isDoor = true
		case 427:
			placedBlockID = 193
			isDoor = true
		case 428:
			placedBlockID = 194
			isDoor = true
		case 429:
			placedBlockID = 195
			isDoor = true
		case 430:
			placedBlockID = 196
			isDoor = true
		case 431:
			placedBlockID = 197
			isDoor = true
		}

		// Don't place air
		if placedBlockID <= 0 || placedBlockID > 255 {
			// Abort placement, but we MUST resync the slot so the client doesn't
			// think they successfully placed it and temporarily lose the item visually.
			player.mu.Lock()
			slotIndex := 36 + player.ActiveSlot
			slot := player.Inventory[slotIndex]
			log.Printf("Aborting place for item %d (mapped to %d). Server thinks active slot %d (index %d) has item %d:%d qty %d", itemID, placedBlockID, player.ActiveSlot, slotIndex, slot.ItemID, slot.Damage, slot.Count)
			pkt := protocol.MarshalPacket(0x2F, func(w *bytes.Buffer) {
				protocol.WriteByte(w, 0) // Window ID 0 = player inventory
				protocol.WriteInt16(w, int16(slotIndex))
				protocol.WriteSlotData(w, slot.ItemID, slot.Count, slot.Damage)
			})
			if player.Conn != nil {
				protocol.WritePacket(player.Conn, pkt)
			}
			player.mu.Unlock()
			return
		}

		// Calculate target position from face
		tx, ty, tz := faceOffset(x, y, z, face)

		// Don't place in invalid y limits
		if ty < 0 || ty > 255 {
			player.mu.Lock()
			slotIndex := 36 + player.ActiveSlot
			slot := player.Inventory[slotIndex]
			pkt := protocol.MarshalPacket(0x2F, func(w *bytes.Buffer) {
				protocol.WriteByte(w, 0)
				protocol.WriteInt16(w, int16(slotIndex))
				protocol.WriteSlotData(w, slot.ItemID, slot.Count, slot.Damage)
			})
			if player.Conn != nil {
				protocol.WritePacket(player.Conn, pkt)
			}
			player.mu.Unlock()
			return
		}

		// Don't place a block inside another non-replaceable block
		existingBlock := s.world.GetBlock(tx, ty, tz)
		existingID := existingBlock >> 4
		if existingID != 0 && existingID != 8 && existingID != 9 && existingID != 10 && existingID != 11 { // not air or liquid
			player.mu.Lock()
			slotIndex := 36 + player.ActiveSlot
			slot := player.Inventory[slotIndex]
			pkt := protocol.MarshalPacket(0x2F, func(w *bytes.Buffer) {
				protocol.WriteByte(w, 0)
				protocol.WriteInt16(w, int16(slotIndex))
				protocol.WriteSlotData(w, slot.ItemID, slot.Count, slot.Damage)
			})
			if player.Conn != nil {
				protocol.WritePacket(player.Conn, pkt)
			}
			player.mu.Unlock()
			return
		}

		if isDoor {
			// Check if we can place the top half
			topBlockID := s.world.GetBlock(tx, ty+1, tz) >> 4
			if ty >= 254 || (topBlockID != 0 && topBlockID != 8 && topBlockID != 9 && topBlockID != 10 && topBlockID != 11) {
				// Cancel placement
				player.mu.Lock()
				slotIndex := 36 + player.ActiveSlot
				slot := player.Inventory[slotIndex]
				pkt := protocol.MarshalPacket(0x2F, func(w *bytes.Buffer) {
					protocol.WriteByte(w, 0)
					protocol.WriteInt16(w, int16(slotIndex))
					protocol.WriteSlotData(w, slot.ItemID, slot.Count, slot.Damage)
				})
				if player.Conn != nil {
					protocol.WritePacket(player.Conn, pkt)
				}
				player.mu.Unlock()
				return
			}
		}

		// Compute correct metadata for directional blocks
		player.mu.Lock()
		yaw := player.Yaw
		player.mu.Unlock()
		metadata := blockPlacementMeta(placedBlockID, byte(damage), face, cursorX, cursorY, yaw)

		// Set block in world
		blockState := uint16(placedBlockID)<<4 | uint16(metadata)
		s.world.SetBlock(tx, ty, tz, blockState)

		// Broadcast block change to all players
		s.broadcastBlockChange(tx, ty, tz, blockState)

		if isDoor {
			// upper half has bit 0x08 set
			topBlockState := uint16(placedBlockID)<<4 | uint16(8)
			s.world.SetBlock(tx, ty+1, tz, topBlockState)
			s.broadcastBlockChange(tx, ty+1, tz, topBlockState)
		}

		// Decrement the item stack if survival
		if player.GameMode == GameModeSurvival {
			player.mu.Lock()
			slotIndex := 36 + player.ActiveSlot
			if player.Inventory[slotIndex].ItemID == itemID && player.Inventory[slotIndex].Count > 0 {
				player.Inventory[slotIndex].Count--
				if player.Inventory[slotIndex].Count <= 0 {
					player.Inventory[slotIndex] = Slot{ItemID: -1}
				}
				// Sync the slot to the client to ensure consistency
				slot := player.Inventory[slotIndex]
				pkt := protocol.MarshalPacket(0x2F, func(w *bytes.Buffer) {
					protocol.WriteByte(w, 0) // Window ID 0 = player inventory
					protocol.WriteInt16(w, int16(slotIndex))
					protocol.WriteSlotData(w, slot.ItemID, slot.Count, slot.Damage)
				})
				if player.Conn != nil {
					protocol.WritePacket(player.Conn, pkt)
				}
			}
			player.mu.Unlock()
		}

		log.Printf("Player %s placed block %d (from item %d) at (%d, %d, %d)", player.Username, placedBlockID, itemID, tx, ty, tz)

	case 0x10: // Creative Inventory Action
		slotNum, _ := protocol.ReadInt16(r)
		itemID, count, damage, _ := protocol.ReadSlotData(r)

		if player.GameMode != GameModeCreative {
			return
		}

		// Validate slot range (0-44 for player inventory, -1 for dropping)
		if slotNum == -1 {
			// Player is dropping an item
			player.mu.Lock()
			px, py, pz := player.X, player.Y, player.Z

			// Drop base velocity calculations relative to player direction
			f1 := math.Sin(float64(player.Yaw) * math.Pi / 180.0)
			f2 := math.Cos(float64(player.Yaw) * math.Pi / 180.0)
			f3 := math.Sin(float64(player.Pitch) * math.Pi / 180.0)
			f4 := math.Cos(float64(player.Pitch) * math.Pi / 180.0)

			vx := -f1 * f4 * 0.3
			vy := -f3*0.3 + 0.1
			vz := f2 * f4 * 0.3

			player.mu.Unlock()
			if itemID != -1 {
				s.SpawnItem(px, py+1.5, pz, vx, vy, vz, itemID, damage, count)
				log.Printf("Player %s dropped item %d:%d (creative)", player.Username, itemID, damage)
			}
			return
		}
		if slotNum < 0 || slotNum > 44 {
			return
		}

		player.mu.Lock()
		if itemID == -1 {
			// Clearing slot
			player.Inventory[slotNum] = Slot{ItemID: -1}
		} else {
			player.Inventory[slotNum] = Slot{ItemID: itemID, Count: count, Damage: damage}
		}
		player.mu.Unlock()

		// Inventory contents have changed; if this touched the currently
		// active hotbar slot, broadcast the updated held item.
		s.broadcastHeldItem(player)

	case 0x0E: // Click Window
		windowID, _ := protocol.ReadByte(r)
		slotNum, _ := protocol.ReadInt16(r)
		button, _ := protocol.ReadByte(r)
		actionNum, _ := protocol.ReadInt16(r)
		mode, _ := protocol.ReadByte(r)
		// Read held item slot data
		itemID, _, _, _ := protocol.ReadSlotData(r)
		_ = itemID

		// Delegate non-player-inventory windows to separate handler
		if windowID != 0 {
			s.handleWindowClick(player, windowID, slotNum, button, actionNum, mode)
			return
		}

		player.mu.Lock()
		px, py, pz := player.X, player.Y, player.Z

		// Crafting output slot (slot 0) has special handling
		if slotNum == 0 {
			if mode == 0 && player.Inventory[0].ItemID != -1 {
				// Normal click on crafting output: take the result
				result := player.Inventory[0]
				if player.Cursor.ItemID == -1 {
					player.Cursor = result
					consumeCraftIngredients2x2(player)
				} else if player.Cursor.ItemID == result.ItemID && player.Cursor.Damage == result.Damage && int(player.Cursor.Count)+int(result.Count) <= 64 {
					player.Cursor.Count += result.Count
					consumeCraftIngredients2x2(player)
				}
			} else if mode == 1 && player.Inventory[0].ItemID != -1 {
				// Shift-click: craft all possible
				for player.Inventory[0].ItemID != -1 {
					result := player.Inventory[0]
					_, ok := addItemToInventory(player, result.ItemID, result.Damage, result.Count)
					if !ok {
						break
					}
					consumeCraftIngredients2x2(player)
					updateCraftOutput2x2(player)
				}
			}
		} else if slotNum >= 1 && slotNum < 45 {
			if mode == 0 { // Normal click
				if button == 0 { // Left click
					if player.Cursor.ItemID == player.Inventory[slotNum].ItemID && player.Cursor.Damage == player.Inventory[slotNum].Damage && player.Cursor.ItemID != -1 {
						space := 64 - player.Inventory[slotNum].Count
						if player.Cursor.Count <= space {
							player.Inventory[slotNum].Count += player.Cursor.Count
							player.Cursor = Slot{ItemID: -1}
						} else {
							player.Inventory[slotNum].Count = 64
							player.Cursor.Count -= space
						}
					} else { // Swap
						temp := player.Inventory[slotNum]
						player.Inventory[slotNum] = player.Cursor
						player.Cursor = temp
					}
				} else if button == 1 { // Right click
					if player.Cursor.ItemID == -1 && player.Inventory[slotNum].ItemID != -1 {
						half := (player.Inventory[slotNum].Count + 1) / 2
						player.Cursor = player.Inventory[slotNum]
						player.Cursor.Count = half
						player.Inventory[slotNum].Count -= half
						if player.Inventory[slotNum].Count == 0 {
							player.Inventory[slotNum] = Slot{ItemID: -1}
						}
					} else if player.Cursor.ItemID != -1 && player.Inventory[slotNum].ItemID == -1 {
						player.Inventory[slotNum] = player.Cursor
						player.Inventory[slotNum].Count = 1
						player.Cursor.Count--
						if player.Cursor.Count == 0 {
							player.Cursor = Slot{ItemID: -1}
						}
					} else if player.Cursor.ItemID == player.Inventory[slotNum].ItemID && player.Cursor.Damage == player.Inventory[slotNum].Damage {
						if player.Inventory[slotNum].Count < 64 {
							player.Inventory[slotNum].Count++
							player.Cursor.Count--
							if player.Cursor.Count == 0 {
								player.Cursor = Slot{ItemID: -1}
							}
						}
					} else { // Swap
						temp := player.Inventory[slotNum]
						player.Inventory[slotNum] = player.Cursor
						player.Cursor = temp
					}
				}
			} else if mode == 1 { // Shift-click
				if player.Inventory[slotNum].ItemID != -1 {
					item := player.Inventory[slotNum]
					moved := false
					var destStart, destEnd int
					if slotNum >= 36 && slotNum <= 44 {
						destStart, destEnd = 9, 35
					} else if slotNum >= 9 && slotNum <= 35 {
						destStart, destEnd = 36, 44
					} else if slotNum >= 5 && slotNum <= 8 {
						destStart, destEnd = 36, 44
					} else {
						destStart, destEnd = 9, 35
					}
					// First pass: try to stack onto existing matching items
					remaining := item.Count
					for i := destStart; i <= destEnd && remaining > 0; i++ {
						if player.Inventory[i].ItemID == item.ItemID && player.Inventory[i].Damage == item.Damage && player.Inventory[i].Count < 64 {
							space := 64 - player.Inventory[i].Count
							if remaining <= space {
								player.Inventory[i].Count += remaining
								remaining = 0
							} else {
								player.Inventory[i].Count = 64
								remaining -= space
							}
						}
					}
					// Second pass: put remainder in empty slots
					for i := destStart; i <= destEnd && remaining > 0; i++ {
						if player.Inventory[i].ItemID == -1 {
							player.Inventory[i] = Slot{ItemID: item.ItemID, Damage: item.Damage, Count: remaining}
							remaining = 0
						}
					}
					if remaining == 0 {
						player.Inventory[slotNum] = Slot{ItemID: -1}
						moved = true
					} else if remaining < item.Count {
						player.Inventory[slotNum].Count = remaining
					}
					// For armor slots, if not moved try main inventory as fallback
					if !moved && (slotNum >= 5 && slotNum <= 8) {
						remaining = player.Inventory[slotNum].Count
						for i := 9; i <= 35 && remaining > 0; i++ {
							if player.Inventory[i].ItemID == item.ItemID && player.Inventory[i].Damage == item.Damage && player.Inventory[i].Count < 64 {
								space := 64 - player.Inventory[i].Count
								if remaining <= space {
									player.Inventory[i].Count += remaining
									remaining = 0
								} else {
									player.Inventory[i].Count = 64
									remaining -= space
								}
							}
						}
						for i := 9; i <= 35 && remaining > 0; i++ {
							if player.Inventory[i].ItemID == -1 {
								player.Inventory[i] = Slot{ItemID: item.ItemID, Damage: item.Damage, Count: remaining}
								remaining = 0
							}
						}
						if remaining == 0 {
							player.Inventory[slotNum] = Slot{ItemID: -1}
						} else if remaining < player.Inventory[slotNum].Count {
							player.Inventory[slotNum].Count = remaining
						}
					}
				}
			} else if mode == 2 { // Number key hotkey
				// button = hotkey number (0-8), maps to hotbar slot 36+button
				hotbarSlot := int16(36) + int16(button)
				if hotbarSlot >= 36 && hotbarSlot <= 44 {
					temp := player.Inventory[slotNum]
					player.Inventory[slotNum] = player.Inventory[hotbarSlot]
					player.Inventory[hotbarSlot] = temp
				}
			}
		}

		// Mode 6 is double-click to collect matching items onto cursor
		if mode == 6 && player.Cursor.ItemID != -1 {
			for i := 1; i < 45 && player.Cursor.Count < 64; i++ {
				if player.Inventory[i].ItemID == player.Cursor.ItemID && player.Inventory[i].Damage == player.Cursor.Damage {
					space := 64 - player.Cursor.Count
					if player.Inventory[i].Count <= space {
						player.Cursor.Count += player.Inventory[i].Count
						player.Inventory[i] = Slot{ItemID: -1}
					} else {
						player.Cursor.Count = 64
						player.Inventory[i].Count -= space
					}
				}
			}
		}

		// Mode 5 is drag/paint (hold click and drag across slots)
		if mode == 5 {
			switch button {
			case 0: // Left drag start
				player.DragSlots = nil
				player.DragButton = 0
			case 4: // Right drag start
				player.DragSlots = nil
				player.DragButton = 1
			case 1: // Left drag add slot
				if slotNum >= 1 && slotNum < 45 {
					player.DragSlots = append(player.DragSlots, slotNum)
				}
			case 5: // Right drag add slot
				if slotNum >= 1 && slotNum < 45 {
					player.DragSlots = append(player.DragSlots, slotNum)
				}
			case 2: // Left drag end - distribute evenly
				if player.Cursor.ItemID != -1 && len(player.DragSlots) > 0 {
					perSlot := player.Cursor.Count / byte(len(player.DragSlots))
					if perSlot < 1 {
						perSlot = 1
					}
					for _, ds := range player.DragSlots {
						if player.Cursor.Count <= 0 {
							break
						}
						if player.Inventory[ds].ItemID == -1 {
							give := perSlot
							if give > player.Cursor.Count {
								give = player.Cursor.Count
							}
							player.Inventory[ds] = Slot{ItemID: player.Cursor.ItemID, Damage: player.Cursor.Damage, Count: give}
							player.Cursor.Count -= give
						} else if player.Inventory[ds].ItemID == player.Cursor.ItemID && player.Inventory[ds].Damage == player.Cursor.Damage {
							space := 64 - player.Inventory[ds].Count
							give := perSlot
							if give > space {
								give = space
							}
							if give > player.Cursor.Count {
								give = player.Cursor.Count
							}
							player.Inventory[ds].Count += give
							player.Cursor.Count -= give
						}
					}
					if player.Cursor.Count <= 0 {
						player.Cursor = Slot{ItemID: -1}
					}
				}
				player.DragSlots = nil
			case 6: // Right drag end - place one per slot
				if player.Cursor.ItemID != -1 && len(player.DragSlots) > 0 {
					for _, ds := range player.DragSlots {
						if player.Cursor.Count <= 0 {
							break
						}
						if player.Inventory[ds].ItemID == -1 {
							player.Inventory[ds] = Slot{ItemID: player.Cursor.ItemID, Damage: player.Cursor.Damage, Count: 1}
							player.Cursor.Count--
						} else if player.Inventory[ds].ItemID == player.Cursor.ItemID && player.Inventory[ds].Damage == player.Cursor.Damage && player.Inventory[ds].Count < 64 {
							player.Inventory[ds].Count++
							player.Cursor.Count--
						}
					}
					if player.Cursor.Count <= 0 {
						player.Cursor = Slot{ItemID: -1}
					}
				}
				player.DragSlots = nil
			}
		}

		// Mode 4 is drop from window
		if mode == 4 && player.GameMode != GameModeSpectator {
			if slotNum == -999 { // Drop from cursor
				if player.Cursor.ItemID != -1 {
					// Save item data BEFORE modifying cursor
					vitemID := player.Cursor.ItemID
					vdamage := player.Cursor.Damage
					dropCount := player.Cursor.Count
					if button == 0 { // Left click drops 1
						dropCount = 1
						player.Cursor.Count--
						if player.Cursor.Count <= 0 {
							player.Cursor = Slot{ItemID: -1}
						}
					} else {
						player.Cursor = Slot{ItemID: -1}
					}

					f1 := math.Sin(float64(player.Yaw) * math.Pi / 180.0)
					f2 := math.Cos(float64(player.Yaw) * math.Pi / 180.0)
					f3 := math.Sin(float64(player.Pitch) * math.Pi / 180.0)
					f4 := math.Cos(float64(player.Pitch) * math.Pi / 180.0)

					vx := -f1 * f4 * 0.3
					vy := -f3*0.3 + 0.1
					vz := f2 * f4 * 0.3

					player.mu.Unlock() // unlock to spawn
					s.SpawnItem(px, py+1.5, pz, vx, vy, vz, vitemID, vdamage, dropCount)
					player.mu.Lock()
				}
			} else if slotNum >= 1 && slotNum < 45 {
				if player.Inventory[slotNum].ItemID != -1 {
					// Save item data BEFORE modifying slot
					dropItemID := player.Inventory[slotNum].ItemID
					dropDamage := player.Inventory[slotNum].Damage
					dropCount := player.Inventory[slotNum].Count
					if button == 0 { // Q drops 1
						dropCount = 1
						player.Inventory[slotNum].Count--
						if player.Inventory[slotNum].Count <= 0 {
							player.Inventory[slotNum] = Slot{ItemID: -1}
						}
					} else { // Ctrl+Q drops all
						player.Inventory[slotNum] = Slot{ItemID: -1}
					}

					f1 := math.Sin(float64(player.Yaw) * math.Pi / 180.0)
					f2 := math.Cos(float64(player.Yaw) * math.Pi / 180.0)
					f3 := math.Sin(float64(player.Pitch) * math.Pi / 180.0)
					f4 := math.Cos(float64(player.Pitch) * math.Pi / 180.0)

					vx := -f1 * f4 * 0.3
					vy := -f3*0.3 + 0.1
					vz := f2 * f4 * 0.3

					player.mu.Unlock() // unlock to spawn
					s.SpawnItem(px, py+1.5, pz, vx, vy, vz, dropItemID, dropDamage, dropCount)
					player.mu.Lock()
				}
			}
		} else if slotNum == -999 && mode == 0 && player.Cursor.ItemID != -1 { // Clicked outside with cursor
			dropCount := player.Cursor.Count
			if button == 0 {
				dropCount = player.Cursor.Count
			} else {
				dropCount = 1
			} // Left drops all, right drops 1

			dropDamage := player.Cursor.Damage
			dropItemID := player.Cursor.ItemID

			player.Cursor.Count -= dropCount
			if player.Cursor.Count <= 0 {
				player.Cursor = Slot{ItemID: -1}
			}

			f1 := math.Sin(float64(player.Yaw) * math.Pi / 180.0)
			f2 := math.Cos(float64(player.Yaw) * math.Pi / 180.0)
			f3 := math.Sin(float64(player.Pitch) * math.Pi / 180.0)
			f4 := math.Cos(float64(player.Pitch) * math.Pi / 180.0)

			vx := -f1 * f4 * 0.3
			vy := -f3*0.3 + 0.1
			vz := f2 * f4 * 0.3

			player.mu.Unlock()
			s.SpawnItem(px, py+1.5, pz, vx, vy, vz, dropItemID, dropDamage, dropCount)
			player.mu.Lock()
		}

		// Update crafting output based on current grid contents
		updateCraftOutput2x2(player)

		// Acknowledge action
		confirmPkt := protocol.MarshalPacket(0x32, func(w *bytes.Buffer) {
			protocol.WriteByte(w, 0) // window ID
			protocol.WriteInt16(w, actionNum)
			protocol.WriteBool(w, true) // accepted
		})

		// Always send a full WindowItems sync and SetSlot for cursor to prevent ANY duplication/desyncs!
		syncPkt := protocol.MarshalPacket(0x30, func(w *bytes.Buffer) {
			protocol.WriteByte(w, 0)   // Window ID
			protocol.WriteInt16(w, 45) // Count
			for i := 0; i < 45; i++ {
				slot := player.Inventory[i]
				protocol.WriteSlotData(w, slot.ItemID, slot.Count, slot.Damage)
			}
		})
		cursorPkt := protocol.MarshalPacket(0x2F, func(w *bytes.Buffer) {
			protocol.WriteByte(w, 0xff) // Cursor
			protocol.WriteInt16(w, -1)
			protocol.WriteSlotData(w, player.Cursor.ItemID, player.Cursor.Count, player.Cursor.Damage)
		})

		if player.Conn != nil {
			protocol.WritePacket(player.Conn, confirmPkt)
			protocol.WritePacket(player.Conn, syncPkt)
			protocol.WritePacket(player.Conn, cursorPkt)
		}

		player.mu.Unlock()
		// After any inventory manipulation, ensure other players see the
		// correct held item for this player.
		s.broadcastHeldItem(player)
	}
}

// handleWindowClick handles Click Window packets for non-player-inventory windows (e.g. crafting table).
func (s *Server) handleWindowClick(player *Player, windowID byte, slotNum int16, button byte, actionNum int16, mode byte) {
	player.mu.Lock()

	if windowID != player.OpenWindowID {
		player.mu.Unlock()
		return
	}

	px, py, pz := player.X, player.Y, player.Z
	totalSlots := int16(46) // 1 output + 9 grid + 27 main + 9 hotbar

	// Slot accessors: translate window slot to storage
	getSlot := func(n int16) Slot {
		switch {
		case n == 0:
			return player.CraftTableOutput
		case n >= 1 && n <= 9:
			return player.CraftTableGrid[n-1]
		case n >= 10 && n <= 45:
			return player.Inventory[n-1]
		}
		return Slot{ItemID: -1}
	}
	setSlot := func(n int16, sl Slot) {
		switch {
		case n == 0:
			player.CraftTableOutput = sl
		case n >= 1 && n <= 9:
			player.CraftTableGrid[n-1] = sl
		case n >= 10 && n <= 45:
			player.Inventory[n-1] = sl
		}
	}

	// Handle crafting output (slot 0) specially
	if slotNum == 0 {
		if mode == 0 && player.CraftTableOutput.ItemID != -1 {
			result := player.CraftTableOutput
			if player.Cursor.ItemID == -1 {
				player.Cursor = result
				consumeCraftIngredients3x3(player)
			} else if player.Cursor.ItemID == result.ItemID && player.Cursor.Damage == result.Damage && int(player.Cursor.Count)+int(result.Count) <= 64 {
				player.Cursor.Count += result.Count
				consumeCraftIngredients3x3(player)
			}
		} else if mode == 1 && player.CraftTableOutput.ItemID != -1 {
			// Shift-click: craft all possible
			for player.CraftTableOutput.ItemID != -1 {
				result := player.CraftTableOutput
				_, ok := addItemToInventory(player, result.ItemID, result.Damage, result.Count)
				if !ok {
					break
				}
				consumeCraftIngredients3x3(player)
				updateCraftOutput3x3(player)
			}
		}
	} else if slotNum >= 1 && slotNum < totalSlots {
		if mode == 0 { // Normal click
			sl := getSlot(slotNum)
			if button == 0 { // Left click
				if player.Cursor.ItemID == sl.ItemID && player.Cursor.Damage == sl.Damage && player.Cursor.ItemID != -1 {
					space := 64 - sl.Count
					if player.Cursor.Count <= space {
						sl.Count += player.Cursor.Count
						player.Cursor = Slot{ItemID: -1}
					} else {
						sl.Count = 64
						player.Cursor.Count -= space
					}
					setSlot(slotNum, sl)
				} else {
					setSlot(slotNum, player.Cursor)
					player.Cursor = sl
				}
			} else if button == 1 { // Right click
				if player.Cursor.ItemID == -1 && sl.ItemID != -1 {
					half := (sl.Count + 1) / 2
					player.Cursor = sl
					player.Cursor.Count = half
					sl.Count -= half
					if sl.Count == 0 {
						sl = Slot{ItemID: -1}
					}
					setSlot(slotNum, sl)
				} else if player.Cursor.ItemID != -1 && sl.ItemID == -1 {
					sl = player.Cursor
					sl.Count = 1
					setSlot(slotNum, sl)
					player.Cursor.Count--
					if player.Cursor.Count == 0 {
						player.Cursor = Slot{ItemID: -1}
					}
				} else if player.Cursor.ItemID == sl.ItemID && player.Cursor.Damage == sl.Damage {
					if sl.Count < 64 {
						sl.Count++
						setSlot(slotNum, sl)
						player.Cursor.Count--
						if player.Cursor.Count == 0 {
							player.Cursor = Slot{ItemID: -1}
						}
					}
				} else {
					setSlot(slotNum, player.Cursor)
					player.Cursor = sl
				}
			}
		} else if mode == 1 { // Shift-click
			sl := getSlot(slotNum)
			if sl.ItemID != -1 {
				var destStart, destEnd int16
				if slotNum >= 1 && slotNum <= 9 {
					destStart, destEnd = 10, 45
				} else if slotNum >= 10 && slotNum <= 36 {
					destStart, destEnd = 37, 45
				} else if slotNum >= 37 && slotNum <= 45 {
					destStart, destEnd = 10, 36
				} else {
					destStart, destEnd = 10, 45
				}
				remaining := sl.Count
				for i := destStart; i <= destEnd && remaining > 0; i++ {
					ds := getSlot(i)
					if ds.ItemID == sl.ItemID && ds.Damage == sl.Damage && ds.Count < 64 {
						space := 64 - ds.Count
						if remaining <= space {
							ds.Count += remaining
							remaining = 0
						} else {
							ds.Count = 64
							remaining -= space
						}
						setSlot(i, ds)
					}
				}
				for i := destStart; i <= destEnd && remaining > 0; i++ {
					ds := getSlot(i)
					if ds.ItemID == -1 {
						setSlot(i, Slot{ItemID: sl.ItemID, Damage: sl.Damage, Count: remaining})
						remaining = 0
					}
				}
				if remaining == 0 {
					setSlot(slotNum, Slot{ItemID: -1})
				} else if remaining < sl.Count {
					sl.Count = remaining
					setSlot(slotNum, sl)
				}
			}
		} else if mode == 2 { // Number key hotkey
			hotbarWinSlot := int16(37) + int16(button)
			if hotbarWinSlot >= 37 && hotbarWinSlot <= 45 {
				temp := getSlot(slotNum)
				setSlot(slotNum, getSlot(hotbarWinSlot))
				setSlot(hotbarWinSlot, temp)
			}
		}
	}

	// Mode 6: double-click collect
	if mode == 6 && player.Cursor.ItemID != -1 {
		for i := int16(1); i < totalSlots && player.Cursor.Count < 64; i++ {
			sl := getSlot(i)
			if sl.ItemID == player.Cursor.ItemID && sl.Damage == player.Cursor.Damage {
				space := 64 - player.Cursor.Count
				if sl.Count <= space {
					player.Cursor.Count += sl.Count
					setSlot(i, Slot{ItemID: -1})
				} else {
					player.Cursor.Count = 64
					sl.Count -= space
					setSlot(i, sl)
				}
			}
		}
	}

	// Mode 5: drag/paint
	if mode == 5 {
		switch button {
		case 0:
			player.DragSlots = nil
			player.DragButton = 0
		case 4:
			player.DragSlots = nil
			player.DragButton = 1
		case 1:
			if slotNum >= 1 && slotNum < totalSlots {
				player.DragSlots = append(player.DragSlots, slotNum)
			}
		case 5:
			if slotNum >= 1 && slotNum < totalSlots {
				player.DragSlots = append(player.DragSlots, slotNum)
			}
		case 2: // Left drag end
			if player.Cursor.ItemID != -1 && len(player.DragSlots) > 0 {
				perSlot := player.Cursor.Count / byte(len(player.DragSlots))
				if perSlot < 1 {
					perSlot = 1
				}
				for _, ds := range player.DragSlots {
					if player.Cursor.Count <= 0 {
						break
					}
					dsl := getSlot(ds)
					if dsl.ItemID == -1 {
						give := perSlot
						if give > player.Cursor.Count {
							give = player.Cursor.Count
						}
						setSlot(ds, Slot{ItemID: player.Cursor.ItemID, Damage: player.Cursor.Damage, Count: give})
						player.Cursor.Count -= give
					} else if dsl.ItemID == player.Cursor.ItemID && dsl.Damage == player.Cursor.Damage {
						space := 64 - dsl.Count
						give := perSlot
						if give > space {
							give = space
						}
						if give > player.Cursor.Count {
							give = player.Cursor.Count
						}
						dsl.Count += give
						setSlot(ds, dsl)
						player.Cursor.Count -= give
					}
				}
				if player.Cursor.Count <= 0 {
					player.Cursor = Slot{ItemID: -1}
				}
			}
			player.DragSlots = nil
		case 6: // Right drag end
			if player.Cursor.ItemID != -1 && len(player.DragSlots) > 0 {
				for _, ds := range player.DragSlots {
					if player.Cursor.Count <= 0 {
						break
					}
					dsl := getSlot(ds)
					if dsl.ItemID == -1 {
						setSlot(ds, Slot{ItemID: player.Cursor.ItemID, Damage: player.Cursor.Damage, Count: 1})
						player.Cursor.Count--
					} else if dsl.ItemID == player.Cursor.ItemID && dsl.Damage == player.Cursor.Damage && dsl.Count < 64 {
						dsl.Count++
						setSlot(ds, dsl)
						player.Cursor.Count--
					}
				}
				if player.Cursor.Count <= 0 {
					player.Cursor = Slot{ItemID: -1}
				}
			}
			player.DragSlots = nil
		}
	}

	// Mode 4: drop
	if mode == 4 && player.GameMode != GameModeSpectator {
		if slotNum == -999 {
			if player.Cursor.ItemID != -1 {
				vitemID := player.Cursor.ItemID
				vdamage := player.Cursor.Damage
				dropCount := player.Cursor.Count
				if button == 0 {
					dropCount = 1
					player.Cursor.Count--
					if player.Cursor.Count <= 0 {
						player.Cursor = Slot{ItemID: -1}
					}
				} else {
					player.Cursor = Slot{ItemID: -1}
				}
				f1 := math.Sin(float64(player.Yaw) * math.Pi / 180.0)
				f2 := math.Cos(float64(player.Yaw) * math.Pi / 180.0)
				f3 := math.Sin(float64(player.Pitch) * math.Pi / 180.0)
				f4 := math.Cos(float64(player.Pitch) * math.Pi / 180.0)
				vx := -f1 * f4 * 0.3
				vy := -f3*0.3 + 0.1
				vz := f2 * f4 * 0.3
				player.mu.Unlock()
				s.SpawnItem(px, py+1.5, pz, vx, vy, vz, vitemID, vdamage, dropCount)
				player.mu.Lock()
			}
		} else if slotNum >= 1 && slotNum < totalSlots {
			sl := getSlot(slotNum)
			if sl.ItemID != -1 {
				dropItemID := sl.ItemID
				dropDamage := sl.Damage
				dropCount := sl.Count
				if button == 0 {
					dropCount = 1
					sl.Count--
					if sl.Count <= 0 {
						sl = Slot{ItemID: -1}
					}
					setSlot(slotNum, sl)
				} else {
					setSlot(slotNum, Slot{ItemID: -1})
				}
				f1 := math.Sin(float64(player.Yaw) * math.Pi / 180.0)
				f2 := math.Cos(float64(player.Yaw) * math.Pi / 180.0)
				f3 := math.Sin(float64(player.Pitch) * math.Pi / 180.0)
				f4 := math.Cos(float64(player.Pitch) * math.Pi / 180.0)
				vx := -f1 * f4 * 0.3
				vy := -f3*0.3 + 0.1
				vz := f2 * f4 * 0.3
				player.mu.Unlock()
				s.SpawnItem(px, py+1.5, pz, vx, vy, vz, dropItemID, dropDamage, dropCount)
				player.mu.Lock()
			}
		}
	} else if slotNum == -999 && mode == 0 && player.Cursor.ItemID != -1 {
		dropCount := player.Cursor.Count
		if button == 1 {
			dropCount = 1
		}
		dropItemID := player.Cursor.ItemID
		dropDamage := player.Cursor.Damage
		player.Cursor.Count -= dropCount
		if player.Cursor.Count <= 0 {
			player.Cursor = Slot{ItemID: -1}
		}
		f1 := math.Sin(float64(player.Yaw) * math.Pi / 180.0)
		f2 := math.Cos(float64(player.Yaw) * math.Pi / 180.0)
		f3 := math.Sin(float64(player.Pitch) * math.Pi / 180.0)
		f4 := math.Cos(float64(player.Pitch) * math.Pi / 180.0)
		vx := -f1 * f4 * 0.3
		vy := -f3*0.3 + 0.1
		vz := f2 * f4 * 0.3
		player.mu.Unlock()
		s.SpawnItem(px, py+1.5, pz, vx, vy, vz, dropItemID, dropDamage, dropCount)
		player.mu.Lock()
	}

	// Update crafting output
	updateCraftOutput3x3(player)

	// Acknowledge action
	confirmPkt := protocol.MarshalPacket(0x32, func(w *bytes.Buffer) {
		protocol.WriteByte(w, windowID)
		protocol.WriteInt16(w, actionNum)
		protocol.WriteBool(w, true)
	})

	// Send full window items sync
	syncPkt := protocol.MarshalPacket(0x30, func(w *bytes.Buffer) {
		protocol.WriteByte(w, windowID)
		protocol.WriteInt16(w, totalSlots)
		// Slot 0: crafting output
		protocol.WriteSlotData(w, player.CraftTableOutput.ItemID, player.CraftTableOutput.Count, player.CraftTableOutput.Damage)
		// Slots 1-9: crafting grid
		for i := 0; i < 9; i++ {
			sl := player.CraftTableGrid[i]
			protocol.WriteSlotData(w, sl.ItemID, sl.Count, sl.Damage)
		}
		// Slots 10-36: main inventory (player slots 9-35)
		for i := 9; i <= 35; i++ {
			sl := player.Inventory[i]
			protocol.WriteSlotData(w, sl.ItemID, sl.Count, sl.Damage)
		}
		// Slots 37-45: hotbar (player slots 36-44)
		for i := 36; i <= 44; i++ {
			sl := player.Inventory[i]
			protocol.WriteSlotData(w, sl.ItemID, sl.Count, sl.Damage)
		}
	})
	cursorPkt := protocol.MarshalPacket(0x2F, func(w *bytes.Buffer) {
		protocol.WriteByte(w, 0xff)
		protocol.WriteInt16(w, -1)
		protocol.WriteSlotData(w, player.Cursor.ItemID, player.Cursor.Count, player.Cursor.Damage)
	})

	if player.Conn != nil {
		protocol.WritePacket(player.Conn, confirmPkt)
		protocol.WritePacket(player.Conn, syncPkt)
		protocol.WritePacket(player.Conn, cursorPkt)
	}

	player.mu.Unlock()
	s.broadcastHeldItem(player)
}

// broadcastHeldItem sends an Entity Equipment packet (0x04) to all other
// players so they see the correct item in the given player's hand.
func (s *Server) broadcastHeldItem(player *Player) {
	player.mu.Lock()
	entityID := player.EntityID
	activeSlot := int(player.ActiveSlot)
	slotIndex := 36 + activeSlot
	if activeSlot < 0 || slotIndex < 0 || slotIndex >= len(player.Inventory) {
		player.mu.Unlock()
		return
	}
	slot := player.Inventory[slotIndex]
	player.mu.Unlock()

	pkt := protocol.MarshalPacket(0x04, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, entityID)
		// Slot 0 = item in hand in Minecraft 1.8
		protocol.WriteInt16(w, 0)
		protocol.WriteSlotData(w, slot.ItemID, slot.Count, slot.Damage)
	})

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, other := range s.players {
		if other.EntityID == entityID {
			continue
		}
		other.mu.Lock()
		if other.Conn != nil {
			protocol.WritePacket(other.Conn, pkt)
		}
		other.mu.Unlock()
	}
}

func (s *Server) keepAliveLoop(player *Player, stop chan struct{}) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			keepAliveID := rand.Int31()
			pkt := protocol.MarshalPacket(0x00, func(w *bytes.Buffer) {
				protocol.WriteVarInt(w, keepAliveID)
			})
			player.mu.Lock()
			err := protocol.WritePacket(player.Conn, pkt)
			player.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (s *Server) sendSpawnChunks(player *Player) {
	playerChunkX := int32(int(player.X) >> 4)
	playerChunkZ := int32(int(player.Z) >> 4)

	player.mu.Lock()
	player.loadedChunks = make(map[ChunkPos]bool)
	player.lastChunkX = playerChunkX
	player.lastChunkZ = playerChunkZ
	player.mu.Unlock()

	// Send chunks in a square around the player's chunk
	for cx := playerChunkX - ViewDistance; cx <= playerChunkX+ViewDistance; cx++ {
		for cz := playerChunkZ - ViewDistance; cz <= playerChunkZ+ViewDistance; cz++ {
			s.sendChunkColumn(player, cx, cz)
		}
	}
}

// sendChunkColumn generates and sends a single chunk column to a player.
func (s *Server) sendChunkColumn(player *Player, cx, cz int32) {
	pos := ChunkPos{cx, cz}
	player.mu.Lock()
	if player.loadedChunks[pos] {
		player.mu.Unlock()
		return
	}
	player.loadedChunks[pos] = true
	player.mu.Unlock()

	chunkData, primaryBitMask := s.world.GetChunkData(int32(cx), int32(cz))
	pkt := protocol.MarshalPacket(0x21, func(w *bytes.Buffer) {
		protocol.WriteInt32(w, cx)                     // Chunk X
		protocol.WriteInt32(w, cz)                     // Chunk Z
		protocol.WriteBool(w, true)                    // Ground-up continuous
		protocol.WriteUint16(w, primaryBitMask)        // Primary bit mask
		protocol.WriteVarInt(w, int32(len(chunkData))) // Size
		w.Write(chunkData)                             // Data
	})
	player.mu.Lock()
	protocol.WritePacket(player.Conn, pkt)
	player.mu.Unlock()
}

// sendChunkUpdates streams new chunks to the player when they cross chunk boundaries
// and unloads chunks that are too far away.
func (s *Server) sendChunkUpdates(player *Player) {
	player.mu.Lock()
	currentChunkX := int32(int(player.X) >> 4)
	currentChunkZ := int32(int(player.Z) >> 4)

	if currentChunkX == player.lastChunkX && currentChunkZ == player.lastChunkZ {
		player.mu.Unlock()
		return
	}

	player.lastChunkX = currentChunkX
	player.lastChunkZ = currentChunkZ
	player.mu.Unlock()

	// Send new chunks that are now in range
	for cx := currentChunkX - ViewDistance; cx <= currentChunkX+ViewDistance; cx++ {
		for cz := currentChunkZ - ViewDistance; cz <= currentChunkZ+ViewDistance; cz++ {
			s.sendChunkColumn(player, cx, cz)
		}
	}

	// Unload chunks that are now out of range
	player.mu.Lock()
	var toUnload []ChunkPos
	for pos := range player.loadedChunks {
		dx := pos.X - currentChunkX
		dz := pos.Z - currentChunkZ
		if dx < -ViewDistance || dx > ViewDistance || dz < -ViewDistance || dz > ViewDistance {
			toUnload = append(toUnload, pos)
		}
	}
	for _, pos := range toUnload {
		delete(player.loadedChunks, pos)
	}
	player.mu.Unlock()

	// Send unload packets (empty chunk with primary bit mask 0)
	for _, pos := range toUnload {
		pkt := protocol.MarshalPacket(0x21, func(w *bytes.Buffer) {
			protocol.WriteInt32(w, pos.X)
			protocol.WriteInt32(w, pos.Z)
			protocol.WriteBool(w, true) // Ground-up continuous
			protocol.WriteUint16(w, 0)  // Primary bit mask: 0 = unload
			protocol.WriteVarInt(w, 0)  // Size: 0
		})
		player.mu.Lock()
		protocol.WritePacket(player.Conn, pkt)
		player.mu.Unlock()
	}
}

func (s *Server) sendBlockModifications(conn net.Conn) {
	modifications := s.world.GetModifications()
	for pos, blockState := range modifications {
		pkt := protocol.MarshalPacket(0x23, func(w *bytes.Buffer) {
			protocol.WritePosition(w, pos.X, pos.Y, pos.Z)
			protocol.WriteVarInt(w, int32(blockState))
		})
		protocol.WritePacket(conn, pkt)
	}
}

func (s *Server) broadcastChat(msg chat.Message) {
	jsonMsg := msg.String()
	pkt := protocol.MarshalPacket(0x02, func(w *bytes.Buffer) {
		protocol.WriteString(w, jsonMsg)
		protocol.WriteByte(w, 0) // Position: chat
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

func (s *Server) spawnPlayerForOthers(player *Player) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, other := range s.players {
		if other.EntityID == player.EntityID {
			continue
		}
		s.sendSpawnPlayer(other, player)
	}
}

func (s *Server) spawnOthersForPlayer(player *Player) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, other := range s.players {
		if other.EntityID == player.EntityID {
			continue
		}
		s.sendSpawnPlayer(player, other)
	}
}

func (s *Server) spawnEntitiesForPlayer(player *Player) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, entity := range s.entities {
		s.sendItemToPlayer(player, entity)
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
		protocol.WriteByte(w, 0)  // Pitch
		protocol.WriteByte(w, 0)  // Yaw
		protocol.WriteInt32(w, 0) // Data (needs to be non-zero for some objects, but 0 often means no velocity)
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
	protocol.WritePacket(player.Conn, spawnObj)
	protocol.WritePacket(player.Conn, metadata)
	player.mu.Unlock()
}

func (s *Server) sendSpawnPlayer(viewer *Player, target *Player) {
	target.mu.Lock()
	x := target.X
	y := target.Y
	z := target.Z
	yaw := target.Yaw
	pitch := target.Pitch
	targetGameMode := target.GameMode
	// Determine the item ID currently held in the target's hand so the viewer
	// immediately sees the correct held item model.
	currentItemID := int16(0)
	activeSlot := int(target.ActiveSlot)
	slotIndex := 36 + activeSlot
	if activeSlot >= 0 && slotIndex >= 0 && slotIndex < len(target.Inventory) {
		held := target.Inventory[slotIndex]
		if held.ItemID > 0 {
			currentItemID = held.ItemID
		}
	}
	target.mu.Unlock()

	// Player List Item (Add Player) - 0x38
	playerListAdd := protocol.MarshalPacket(0x38, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, 0) // Action: Add Player
		protocol.WriteVarInt(w, 1) // Number of players
		protocol.WriteUUID(w, target.UUID)
		protocol.WriteString(w, target.Username)
		protocol.WriteVarInt(w, 0)                      // Number of properties
		protocol.WriteVarInt(w, int32(target.GameMode)) // Gamemode
		protocol.WriteVarInt(w, 0)                      // Ping
		protocol.WriteBool(w, false)                    // Has display name
	})
	viewer.mu.Lock()
	protocol.WritePacket(viewer.Conn, playerListAdd)
	viewer.mu.Unlock()

	// Entity flags: set invisible for spectators
	var entityFlags byte
	if targetGameMode == GameModeSpectator {
		entityFlags = EntityFlagInvisible
	}

	// Spawn Player - 0x0C
	spawnPlayer := protocol.MarshalPacket(0x0C, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, target.EntityID)
		protocol.WriteUUID(w, target.UUID)
		protocol.WriteInt32(w, int32(x*32)) // Fixed-point X
		protocol.WriteInt32(w, int32(y*32)) // Fixed-point Y
		protocol.WriteInt32(w, int32(z*32)) // Fixed-point Z
		protocol.WriteByte(w, byte(yaw*256/360))
		protocol.WriteByte(w, byte(pitch*256/360))
		// Current item in hand (ID, not slot index)
		protocol.WriteInt16(w, currentItemID)
		// Minimal entity metadata so clients always receive a non-empty
		// DataWatcher list for spawned players:
		// Index 0, type 0 (byte) = entity flags
		protocol.WriteByte(w, 0x00)        // header: (type 0 << 5) | index 0
		protocol.WriteByte(w, entityFlags) // flags (EntityFlagInvisible for spectators)
		protocol.WriteByte(w, 0x7F)        // Metadata terminator
	})
	viewer.mu.Lock()
	protocol.WritePacket(viewer.Conn, spawnPlayer)
	viewer.mu.Unlock()
}

func (s *Server) broadcastEntityTeleport(player *Player) {
	player.mu.Lock()
	x := player.X
	y := player.Y
	z := player.Z
	yaw := player.Yaw
	pitch := player.Pitch
	onGround := player.OnGround
	player.mu.Unlock()

	pkt := protocol.MarshalPacket(0x18, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, player.EntityID)
		protocol.WriteInt32(w, int32(x*32))
		protocol.WriteInt32(w, int32(y*32))
		protocol.WriteInt32(w, int32(z*32))
		protocol.WriteByte(w, byte(yaw*256/360))
		protocol.WriteByte(w, byte(pitch*256/360))
		protocol.WriteBool(w, onGround)
	})

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, other := range s.players {
		if other.EntityID == player.EntityID {
			continue
		}
		other.mu.Lock()
		protocol.WritePacket(other.Conn, pkt)
		other.mu.Unlock()
	}
}

func (s *Server) broadcastEntityLook(player *Player) {
	player.mu.Lock()
	yaw := player.Yaw
	pitch := player.Pitch
	onGround := player.OnGround
	player.mu.Unlock()

	pkt := protocol.MarshalPacket(0x16, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, player.EntityID)
		protocol.WriteByte(w, byte(yaw*256/360))
		protocol.WriteByte(w, byte(pitch*256/360))
		protocol.WriteBool(w, onGround)
	})

	headRotation := protocol.MarshalPacket(0x19, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, player.EntityID)
		protocol.WriteByte(w, byte(yaw*256/360))
	})

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, other := range s.players {
		if other.EntityID == player.EntityID {
			continue
		}
		other.mu.Lock()
		protocol.WritePacket(other.Conn, pkt)
		protocol.WritePacket(other.Conn, headRotation)
		other.mu.Unlock()
	}
}

func (s *Server) broadcastAnimation(player *Player, animationID byte) {
	pkt := protocol.MarshalPacket(0x0B, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, player.EntityID)
		protocol.WriteByte(w, animationID)
	})

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, other := range s.players {
		if other.EntityID == player.EntityID {
			continue
		}
		other.mu.Lock()
		protocol.WritePacket(other.Conn, pkt)
		other.mu.Unlock()
	}
}

func (s *Server) broadcastDestroyEntity(entityID int32) {
	pkt := protocol.MarshalPacket(0x13, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, 1) // Count
		protocol.WriteVarInt(w, entityID)
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

func (s *Server) handleBlockBreak(player *Player, x, y, z int32) {
	blockState := s.world.GetBlock(x, y, z)
	blockID := blockState >> 4

	// Don't break air or bedrock
	if blockID == 0 || blockID == 7 {
		return
	}

	// Broadcast block break effect *before* changing the block state so that
	// the client still sees the correct block at this position when rendering
	// particles/sound. Sending the effect after turning the block into air
	// can cause client-side crashes for certain blocks (like stairs) that
	// expect specific properties on the block state.
	s.broadcastBlockBreakEffect(player, x, y, z, blockState)

	// In creative mode, don't give items on break
	var giveItem bool
	var itemID int16
	var damage int16
	var count byte

	if player.GameMode != GameModeCreative {
		giveItem = true
		itemID, damage, count = world.BlockToItemID(blockState)
	} else {
		log.Printf("Player %s broke block %d at (%d, %d, %d) (creative)", player.Username, blockID, x, y, z)
	}

	// Handle multi-block structures (doors, double plants)
	metadata := int16(blockState & 0x0F)
	isUpperHalf := metadata&0x08 != 0

	var otherY int32
	if isUpperHalf {
		otherY = y - 1
	} else {
		otherY = y + 1
	}

	// Check if the other block should also be broken
	// Doors: 64, 71, 193-197
	// Double Plants: 175
	isDoor := blockID == 64 || blockID == 71 || (blockID >= 193 && blockID <= 197)
	isDoublePlant := blockID == 175

	if isDoor || isDoublePlant {
		otherState := s.world.GetBlock(x, otherY, z)
		otherID := otherState >> 4

		// For doors, if the first part didn't give an item (e.g. upper half broken), try the other half.
		// Since we changed BlockToItemID to always return an item, this might not be needed, but it's a safe fallback.
		if isDoor && giveItem && itemID < 0 && otherID == blockID {
			itemID, damage, count = world.BlockToItemID(otherState)
		}

		if otherID == blockID {
			// Break the other half too
			s.world.SetBlock(x, otherY, z, 0)
			s.broadcastBlockChange(x, otherY, z, 0)
		}
	}

	// Set block to air in world state
	s.world.SetBlock(x, y, z, 0)

	// Broadcast block change (air) to all players
	s.broadcastBlockChange(x, y, z, 0)

	// In creative mode, don't give items on break, we already logged it at the top
	if !giveItem {
		return
	}

	// Give item to player by spawning it on the ground
	if itemID < 0 {
		return
	}

	// Spawn item at the center of the broken block with random velocity
	vx := (rand.Float64()*0.2 - 0.1)
	vy := 0.2
	vz := (rand.Float64()*0.2 - 0.1)
	s.SpawnItem(float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, vx, vy, vz, itemID, damage, count)

	log.Printf("Player %s broke block %d at (%d, %d, %d), spawned item %d:%d (count: %d)", player.Username, blockID, x, y, z, itemID, damage, count)
}

func (s *Server) broadcastBlockChange(x, y, z int32, blockState uint16) {
	pkt := protocol.MarshalPacket(0x23, func(w *bytes.Buffer) {
		protocol.WritePosition(w, x, y, z)
		protocol.WriteVarInt(w, int32(blockState))
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

func (s *Server) broadcastBlockBreakEffect(breaker *Player, x, y, z int32, blockState uint16) {
	blockID := blockState >> 4
	metadata := blockState & 0x0F
	effectData := int32(blockID) | (int32(metadata) << 12)

	pkt := protocol.MarshalPacket(0x28, func(w *bytes.Buffer) {
		protocol.WriteInt32(w, 2001) // Effect ID: block break
		protocol.WritePosition(w, x, y, z)
		protocol.WriteInt32(w, effectData)
		protocol.WriteBool(w, false) // Disable relative volume
	})

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.players {
		if p.EntityID == breaker.EntityID {
			continue // Breaking player already sees the effect client-side
		}
		p.mu.Lock()
		if p.Conn != nil {
			protocol.WritePacket(p.Conn, pkt)
		}
		p.mu.Unlock()
	}
}

// addItemToInventory finds a suitable slot and adds the item to the player's inventory.
// Returns the slot index and true if successful, or -1 and false if inventory is full.
// Must be called with player.mu held.
func addItemToInventory(player *Player, itemID int16, damage int16, count byte) (int, bool) {
	// Try to stack in hotbar (slots 36-44)
	for i := 36; i <= 44; i++ {
		if player.Inventory[i].ItemID == itemID && player.Inventory[i].Damage == damage && player.Inventory[i].Count+count <= 64 {
			player.Inventory[i].Count += count
			return i, true
		}
	}
	// Try to stack in main inventory (slots 9-35)
	for i := 9; i <= 35; i++ {
		if player.Inventory[i].ItemID == itemID && player.Inventory[i].Damage == damage && player.Inventory[i].Count+count <= 64 {
			player.Inventory[i].Count += count
			return i, true
		}
	}
	// Try empty slot in hotbar
	for i := 36; i <= 44; i++ {
		if player.Inventory[i].ItemID == -1 {
			player.Inventory[i] = Slot{ItemID: itemID, Damage: damage, Count: count}
			return i, true
		}
	}
	// Try empty slot in main inventory
	for i := 9; i <= 35; i++ {
		if player.Inventory[i].ItemID == -1 {
			player.Inventory[i] = Slot{ItemID: itemID, Damage: damage, Count: count}
			return i, true
		}
	}
	return -1, false
}

func (s *Server) playerCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.players)
}

func offlineUUID(username string) [16]byte {
	// Simple offline UUID generation (MD5 of "OfflinePlayer:" + username)
	// For simplicity, we'll use a deterministic hash
	data := []byte("OfflinePlayer:" + username)
	var uuid [16]byte

	// Simple hash
	h := uint64(0)
	for i, b := range data {
		h ^= uint64(b) << (uint(i%8) * 8)
	}
	uuid[0] = byte(h)
	uuid[1] = byte(h >> 8)
	uuid[2] = byte(h >> 16)
	uuid[3] = byte(h >> 24)
	uuid[4] = byte(h >> 32)
	uuid[5] = byte(h >> 40)
	uuid[6] = byte(h >> 48)
	uuid[7] = byte(h >> 56)

	// Second pass
	h2 := uint64(0x5555555555555555)
	for i, b := range data {
		h2 = h2*31 + uint64(b) + uint64(i)
	}
	uuid[8] = byte(h2)
	uuid[9] = byte(h2 >> 8)
	uuid[10] = byte(h2 >> 16)
	uuid[11] = byte(h2 >> 24)
	uuid[12] = byte(h2 >> 32)
	uuid[13] = byte(h2 >> 40)
	uuid[14] = byte(h2 >> 48)
	uuid[15] = byte(h2 >> 56)

	// Set version to 3 (name-based)
	uuid[6] = (uuid[6] & 0x0F) | 0x30
	// Set variant to RFC 4122
	uuid[8] = (uuid[8] & 0x3F) | 0x80

	return uuid
}

func formatUUID(uuid [16]byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16])
}

// sendPlayerAbilities sends the Player Abilities packet (0x39) based on the player's current gamemode.
func (s *Server) sendPlayerAbilities(player *Player) {
	var flags byte
	switch player.GameMode {
	case GameModeCreative:
		flags = 0x0D // Invulnerable (0x01) | Allow Flying (0x04) | Instant Break (0x08)
	case GameModeSpectator:
		flags = 0x07 // Invulnerable (0x01) | Flying (0x02) | Allow Flying (0x04)
	default:
		flags = 0x00
	}
	abilities := protocol.MarshalPacket(0x39, func(w *bytes.Buffer) {
		protocol.WriteByte(w, flags)
		protocol.WriteFloat32(w, 0.05) // Flying speed
		protocol.WriteFloat32(w, 0.1)  // Walking speed (FOV modifier)
	})
	player.mu.Lock()
	protocol.WritePacket(player.Conn, abilities)
	player.mu.Unlock()
}

// broadcastPlayerListGamemode sends a Player List Item (action=1, Update Gamemode)
// to all players, updating the target player's gamemode in the tab list.
func (s *Server) broadcastPlayerListGamemode(player *Player) {
	player.mu.Lock()
	gameMode := player.GameMode
	uuid := player.UUID
	player.mu.Unlock()

	pkt := protocol.MarshalPacket(0x38, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, 1) // Action: Update Gamemode
		protocol.WriteVarInt(w, 1) // Number of players
		protocol.WriteUUID(w, uuid)
		protocol.WriteVarInt(w, int32(gameMode)) // New gamemode
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

// broadcastPlayerListRemove sends a Player List Item (action=4, Remove Player)
// to all connected players, removing the target player from the tab list.
func (s *Server) broadcastPlayerListRemove(uuid [16]byte) {
	pkt := protocol.MarshalPacket(0x38, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, 4) // Action: Remove Player
		protocol.WriteVarInt(w, 1) // Number of players
		protocol.WriteUUID(w, uuid)
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

// sendChatToPlayer sends a chat message to a single player.
func (s *Server) sendChatToPlayer(player *Player, msg chat.Message) {
	jsonMsg := msg.String()
	pkt := protocol.MarshalPacket(0x02, func(w *bytes.Buffer) {
		protocol.WriteString(w, jsonMsg)
		protocol.WriteByte(w, 0) // Position: chat
	})
	player.mu.Lock()
	protocol.WritePacket(player.Conn, pkt)
	player.mu.Unlock()
}

func (s *Server) handleAttack(attacker *Player, targetID int32) {
	// Spectators cannot attack
	attacker.mu.Lock()
	if attacker.GameMode == GameModeSpectator {
		attacker.mu.Unlock()
		return
	}
	attacker.mu.Unlock()

	s.mu.RLock()
	target, ok := s.players[targetID]
	s.mu.RUnlock()
	if !ok {
		return
	}

	target.mu.Lock()
	if target.IsDead || target.GameMode == GameModeCreative || target.GameMode == GameModeSpectator {
		target.mu.Unlock()
		return
	}

	// Apply damage
	damage := float32(2.0) // 1 heart
	target.Health -= damage
	if target.Health <= 0 {
		target.Health = 0
		target.IsDead = true
	}
	isDead := target.IsDead

	// Calculate knockback
	attackerX, attackerZ := attacker.X, attacker.Z
	targetX, targetZ := target.X, target.Z
	target.mu.Unlock()

	// Broadcast damage animation (1 = take damage)
	s.broadcastAnimation(target, 1)
	// Broadcast hurt status (2 = hurt)
	s.broadcastEntityStatus(target.EntityID, 2)

	// Apply knockback if not dead
	if !isDead {
		dx := targetX - attackerX
		dz := targetZ - attackerZ
		dist := math.Sqrt(dx*dx + dz*dz)

		if dist > 0 {
			// Normalize and scale
			vx := (dx / dist) * 0.4
			vz := (dz / dist) * 0.4
			vy := 0.4 // Small upward pop

			s.sendEntityVelocity(target, vx, vy, vz)
		}
	}

	// Update health for the target player
	s.sendHealth(target)

	if isDead {
		// Broadcast dead status (3 = dead)
		s.broadcastEntityStatus(target.EntityID, 3)
		// Broadcast death message
		s.broadcastChat(chat.Colored(target.Username+" was slain by "+attacker.Username, "red"))
		log.Printf("Player %s was slain by %s", target.Username, attacker.Username)
	}
}

func (s *Server) sendEntityVelocity(player *Player, vx, vy, vz float64) {
	log.Printf("Sending velocity to %s: %f, %f, %f", player.Username, vx, vy, vz)
	// Entity Velocity packet (0x12)
	// Velocity is in units of 1/8000 blocks per tick.
	pkt := protocol.MarshalPacket(0x12, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, player.EntityID)
		protocol.WriteInt16(w, int16(vx*8000))
		protocol.WriteInt16(w, int16(vy*8000))
		protocol.WriteInt16(w, int16(vz*8000))
	})

	player.mu.Lock()
	protocol.WritePacket(player.Conn, pkt)
	player.mu.Unlock()
}

func (s *Server) handleRespawn(player *Player) {
	player.mu.Lock()
	if !player.IsDead {
		player.mu.Unlock()
		return
	}

	// Reset health and state
	player.Health = 20.0
	player.IsDead = false

	// Reset position to spawn (8, spawnY, 8)
	spawnY := float64(s.world.Gen.SurfaceHeight(8, 8)) + 1.0
	player.X = 8
	player.Y = spawnY
	player.Z = 8
	player.mu.Unlock()

	// 0x07 Respawn packet
	respawnPkt := protocol.MarshalPacket(0x07, func(w *bytes.Buffer) {
		protocol.WriteInt32(w, 0) // Overworld
		protocol.WriteByte(w, 0)  // Peaceful difficulty
		protocol.WriteByte(w, player.GameMode)
		protocol.WriteString(w, "default")
	})
	player.mu.Lock()
	protocol.WritePacket(player.Conn, respawnPkt)
	player.mu.Unlock()

	// Send Position
	posLook := protocol.MarshalPacket(0x08, func(w *bytes.Buffer) {
		protocol.WriteFloat64(w, player.X)
		protocol.WriteFloat64(w, player.Y)
		protocol.WriteFloat64(w, player.Z)
		protocol.WriteFloat32(w, 0)
		protocol.WriteFloat32(w, 0)
		protocol.WriteByte(w, 0) // Flags
	})
	player.mu.Lock()
	protocol.WritePacket(player.Conn, posLook)
	player.mu.Unlock()

	// Update health
	s.sendHealth(player)

	// Re-spawn for others
	s.broadcastDestroyEntity(player.EntityID)
	s.spawnPlayerForOthers(player)

	log.Printf("Player %s respawned", player.Username)
}

func (s *Server) sendHealth(player *Player) {
	player.mu.Lock()
	health := player.Health
	player.mu.Unlock()

	pkt := protocol.MarshalPacket(0x06, func(w *bytes.Buffer) {
		protocol.WriteFloat32(w, health)
		protocol.WriteVarInt(w, 20)   // Food
		protocol.WriteFloat32(w, 5.0) // Food Saturation
	})
	player.mu.Lock()
	protocol.WritePacket(player.Conn, pkt)
	player.mu.Unlock()
}

func (s *Server) broadcastEntityStatus(entityID int32, status byte) {
	pkt := protocol.MarshalPacket(0x1A, func(w *bytes.Buffer) {
		protocol.WriteInt32(w, entityID)
		protocol.WriteByte(w, status)
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

// handleCommand dispatches a /-prefixed command from a player.
func (s *Server) handleCommand(player *Player, message string) {
	parts := strings.Fields(message)
	if len(parts) == 0 {
		return
	}
	cmd := strings.ToLower(parts[0])
	log.Printf("Player %s issued command: %s", player.Username, message)

	switch cmd {
	case "/gamemode", "/gm":
		s.handleGamemodeCommand(player, parts[1:])
	case "/tp", "/teleport":
		s.handleTpCommand(player, parts[1:])
	default:
		s.sendChatToPlayer(player, chat.Colored("Unknown command: "+cmd, "red"))
	}
}

// handleGamemodeCommand handles the /gamemode command.
// Usage: /gamemode <survival|creative|adventure|spectator|0|1|2|3>
func (s *Server) handleGamemodeCommand(player *Player, args []string) {
	if len(args) < 1 {
		s.sendChatToPlayer(player, chat.Colored("Usage: /gamemode <survival|creative|adventure|spectator|0|1|2|3>", "red"))
		return
	}

	var mode byte
	switch strings.ToLower(args[0]) {
	case "survival", "s", "0":
		mode = GameModeSurvival
	case "creative", "c", "1":
		mode = GameModeCreative
	case "adventure", "a", "2":
		mode = GameModeAdventure
	case "spectator", "sp", "3":
		mode = GameModeSpectator
	default:
		s.sendChatToPlayer(player, chat.Colored("Unknown gamemode: "+args[0], "red"))
		return
	}

	player.mu.Lock()
	player.GameMode = mode
	player.NoClip = mode == GameModeSpectator
	player.mu.Unlock()

	// Send Change Game State packet (reason=3 = change game mode)
	changeGameState := protocol.MarshalPacket(0x2B, func(w *bytes.Buffer) {
		protocol.WriteByte(w, 3)                // Reason: change game mode
		protocol.WriteFloat32(w, float32(mode)) // Value: new game mode
	})
	player.mu.Lock()
	protocol.WritePacket(player.Conn, changeGameState)
	player.mu.Unlock()

	// Send updated abilities
	s.sendPlayerAbilities(player)

	// Update gamemode in player list for all clients
	s.broadcastPlayerListGamemode(player)

	// Update entity visibility (spectators are invisible)
	s.broadcastEntityFlags(player)

	// Feedback
	modeName := GameModeName(mode)
	s.sendChatToPlayer(player, chat.Colored("Game mode set to "+modeName, "gray"))
	log.Printf("Player %s game mode changed to %s", player.Username, modeName)
}

// switchGameMode changes a player's gamemode, sending all necessary packets
// to the player and broadcasting updates to other players.
func (s *Server) switchGameMode(player *Player, mode byte) {
	player.mu.Lock()
	player.GameMode = mode
	player.NoClip = mode == GameModeSpectator
	player.mu.Unlock()

	// Send Change Game State packet (reason=3 = change game mode)
	changeGameState := protocol.MarshalPacket(0x2B, func(w *bytes.Buffer) {
		protocol.WriteByte(w, 3)                // Reason: change game mode
		protocol.WriteFloat32(w, float32(mode)) // Value: new game mode
	})
	player.mu.Lock()
	protocol.WritePacket(player.Conn, changeGameState)
	player.mu.Unlock()

	// Send updated abilities
	s.sendPlayerAbilities(player)

	// Update gamemode in player list for all clients
	s.broadcastPlayerListGamemode(player)

	// Update entity visibility (spectators are invisible)
	s.broadcastEntityFlags(player)

	modeName := GameModeName(mode)
	log.Printf("Player %s game mode changed to %s", player.Username, modeName)
}

// broadcastEntityFlags sends an Entity Metadata packet (0x1C) to all players
// with updated entity flags (index 0) for the given player.
// In spectator mode, the invisible flag (0x20) is set so the player appears
// as a transparent head to other spectators and is invisible to non-spectators.
func (s *Server) broadcastEntityFlags(player *Player) {
	player.mu.Lock()
	var flags byte
	if player.GameMode == GameModeSpectator {
		flags = EntityFlagInvisible
	}
	entityID := player.EntityID
	player.mu.Unlock()

	pkt := protocol.MarshalPacket(0x1C, func(w *bytes.Buffer) {
		protocol.WriteVarInt(w, entityID)
		protocol.WriteByte(w, 0x00) // header: (type 0 << 5) | index 0 = entity flags
		protocol.WriteByte(w, flags)
		protocol.WriteByte(w, 0x7F) // Metadata terminator
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

// handleTpCommand handles the /tp command.
// Usage: /tp <x> <y> <z> — teleport to coordinates
// Usage: /tp <player>    — teleport to another player
func (s *Server) handleTpCommand(player *Player, args []string) {
	if len(args) == 3 {
		// /tp <x> <y> <z>
		x, err1 := strconv.ParseFloat(args[0], 64)
		y, err2 := strconv.ParseFloat(args[1], 64)
		z, err3 := strconv.ParseFloat(args[2], 64)
		if err1 != nil || err2 != nil || err3 != nil {
			s.sendChatToPlayer(player, chat.Colored("Invalid coordinates. Usage: /tp <x> <y> <z>", "red"))
			return
		}
		s.teleportPlayer(player, x, y, z)
		s.sendChatToPlayer(player, chat.Colored(fmt.Sprintf("Teleported to %.1f, %.1f, %.1f", x, y, z), "gray"))
		log.Printf("Player %s teleported to %.1f, %.1f, %.1f", player.Username, x, y, z)
	} else if len(args) == 1 {
		// /tp <player>
		targetName := args[0]
		s.mu.RLock()
		var target *Player
		for _, p := range s.players {
			if strings.EqualFold(p.Username, targetName) {
				target = p
				break
			}
		}
		s.mu.RUnlock()

		if target == nil {
			s.sendChatToPlayer(player, chat.Colored("Player not found: "+targetName, "red"))
			return
		}

		target.mu.Lock()
		tx, ty, tz := target.X, target.Y, target.Z
		target.mu.Unlock()

		s.teleportPlayer(player, tx, ty, tz)
		s.sendChatToPlayer(player, chat.Colored("Teleported to "+target.Username, "gray"))
		log.Printf("Player %s teleported to %s (%.1f, %.1f, %.1f)", player.Username, target.Username, tx, ty, tz)
	} else {
		s.sendChatToPlayer(player, chat.Colored("Usage: /tp <x> <y> <z> or /tp <player>", "red"))
	}
}

// teleportPlayer moves a player to the given coordinates and syncs the change.
func (s *Server) teleportPlayer(player *Player, x, y, z float64) {
	player.mu.Lock()
	player.X = x
	player.Y = y
	player.Z = z
	player.mu.Unlock()

	// Send Player Position And Look to the teleported player
	posLook := protocol.MarshalPacket(0x08, func(w *bytes.Buffer) {
		protocol.WriteFloat64(w, x)
		protocol.WriteFloat64(w, y)
		protocol.WriteFloat64(w, z)
		protocol.WriteFloat32(w, player.Yaw)
		protocol.WriteFloat32(w, player.Pitch)
		protocol.WriteByte(w, 0) // Flags (all absolute)
	})
	player.mu.Lock()
	protocol.WritePacket(player.Conn, posLook)
	player.mu.Unlock()

	// Broadcast teleport to other players
	s.broadcastEntityTeleport(player)

	// Load/unload chunks around new position
	s.sendChunkUpdates(player)
}

// GameModeName returns the display name for a gamemode.
func GameModeName(mode byte) string {
	switch mode {
	case GameModeSurvival:
		return "Survival"
	case GameModeCreative:
		return "Creative"
	case GameModeAdventure:
		return "Adventure"
	case GameModeSpectator:
		return "Spectator"
	default:
		return fmt.Sprintf("Unknown(%d)", mode)
	}
}

// faceOffset returns the target block position when placing against a face.
// Face values: 0=bottom, 1=top, 2=north(-Z), 3=south(+Z), 4=west(-X), 5=east(+X)
func faceOffset(x, y, z int32, face byte) (int32, int32, int32) {
	switch face {
	case 0:
		return x, y - 1, z
	case 1:
		return x, y + 1, z
	case 2:
		return x, y, z - 1
	case 3:
		return x, y, z + 1
	case 4:
		return x - 1, y, z
	case 5:
		return x + 1, y, z
	default:
		return x, y + 1, z
	}
}

// yawToDirection converts a player yaw angle to a cardinal direction index.
// Returns: 0=south, 1=west, 2=north, 3=east (matches vanilla Minecraft).
func yawToDirection(yaw float32) int {
	return int(math.Floor(float64(yaw)*4.0/360.0+0.5)) & 3
}

// blockPlacementMeta computes the block metadata for a placed block based on its
// type, the item damage value, the face clicked, cursor position, and player yaw.
// For directional blocks (stairs, torches, levers, etc.) the metadata encodes
// orientation derived from placement context. For other blocks the item damage
// value is used directly as metadata (e.g. wool colour, wood type).
func blockPlacementMeta(blockID int16, itemDamage byte, face byte, cursorX byte, cursorY byte, yaw float32) byte {
	dir := yawToDirection(yaw)

	switch blockID {
	// --- Door ---
	case 64, 71, 193, 194, 195, 196, 197:
		switch dir {
		case 0:
			return 1 // South => 1
		case 1:
			return 2 // West => 2
		case 2:
			return 3 // North => 3
		case 3:
			return 0 // East => 0
		default:
			return 0
		}

	// --- Stairs ---
	case 53, 67, 108, 109, 114, 128, 134, 135, 136, 156, 163, 164, 180:
		// Bits 0-1: direction the stair ascends toward, based on player yaw.
		// Bit 2: set when upside-down (placed on bottom face or upper-half of a side face).
		var meta byte
		switch dir {
		case 0:
			meta = 2 // south
		case 1:
			meta = 1 // west
		case 2:
			meta = 3 // north
		case 3:
			meta = 0 // east
		}
		if face == 0 || (face != 1 && cursorY >= 8) {
			meta |= 4
		}
		return meta

	// --- Torch / Redstone Torch ---
	case 50, 75, 76:
		switch face {
		case 1:
			return 5 // floor
		case 2:
			return 4 // attached to block south, pointing north
		case 3:
			return 3 // attached to block north, pointing south
		case 4:
			return 2 // attached to block east, pointing west
		case 5:
			return 1 // attached to block west, pointing east
		default:
			return 5
		}

	// --- Lever ---
	case 69:
		switch face {
		case 0: // ceiling
			if dir == 0 || dir == 2 {
				return 7 // N-S axis
			}
			return 0 // E-W axis
		case 1: // floor
			if dir == 0 || dir == 2 {
				return 5 // N-S axis
			}
			return 6 // E-W axis
		case 2:
			return 4
		case 3:
			return 3
		case 4:
			return 2
		case 5:
			return 1
		default:
			return 5
		}

	// --- Ladder / Wall Sign ---
	case 65, 68:
		switch face {
		case 2:
			return 2
		case 3:
			return 3
		case 4:
			return 4
		case 5:
			return 5
		default:
			return 2
		}

	// --- Button (stone / wood) ---
	case 77, 143:
		switch face {
		case 0:
			return 0 // ceiling
		case 1:
			return 5 // floor
		case 2:
			return 4
		case 3:
			return 3
		case 4:
			return 2
		case 5:
			return 1
		default:
			return 5
		}

	// --- Furnace / Dispenser / Dropper ---
	case 61, 23, 158:
		// Front faces toward the player (opposite of look direction).
		switch dir {
		case 0:
			return 2 // north
		case 1:
			return 5 // east
		case 2:
			return 3 // south
		case 3:
			return 4 // west
		default:
			return 2
		}

	// --- Chest / Trapped Chest / Ender Chest ---
	case 54, 146, 130:
		switch dir {
		case 0:
			return 2
		case 1:
			return 5
		case 2:
			return 3
		case 3:
			return 4
		default:
			return 2
		}

	// --- Pumpkin / Jack-o-Lantern ---
	case 86, 91:
		// Carved face faces toward the player.
		return byte((dir + 2) & 3)

	// --- Log / Log2 ---
	case 17, 162:
		woodType := itemDamage & 0x03
		switch face {
		case 2, 3:
			return woodType | 8 // Z axis
		case 4, 5:
			return woodType | 4 // X axis
		default:
			return woodType // Y axis (face 0, 1, or default)
		}

	// --- Slab / Wooden Slab ---
	case 44, 126:
		slabType := itemDamage & 0x07
		if face == 0 || (face != 1 && cursorY >= 8) {
			slabType |= 8 // upper slab
		}
		return slabType

	// --- Standing Sign ---
	case 63:
		return byte(int(math.Floor(float64(yaw+180.0)*16.0/360.0+0.5)) & 15)

	// --- Hopper ---
	case 154:
		switch face {
		case 2:
			return 2
		case 3:
			return 3
		case 4:
			return 4
		case 5:
			return 5
		default:
			return 0 // output down
		}

	// --- Anvil ---
	case 145:
		return byte(dir & 3)

	// --- Redstone Repeater / Comparator ---
	case 93, 149:
		return byte(dir)

	default:
		// Non-directional blocks: use item damage as metadata (colour, variant, etc.)
		return itemDamage & 0x0F
	}
}

// ParseGameMode parses a gamemode string into its byte value.
// Returns the mode and true on success, or 0 and false on failure.
func ParseGameMode(s string) (byte, bool) {
	switch strings.ToLower(s) {
	case "survival", "s", "0":
		return GameModeSurvival, true
	case "creative", "c", "1":
		return GameModeCreative, true
	case "adventure", "a", "2":
		return GameModeAdventure, true
	case "spectator", "sp", "3":
		return GameModeSpectator, true
	default:
		return 0, false
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
		protocol.WriteByte(w, 0)  // Pitch
		protocol.WriteByte(w, 0)  // Yaw
		protocol.WriteInt32(w, 0) // Data for thrower (0 usually)
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
		p.mu.Lock()
		if p.Conn != nil {
			protocol.WritePacket(p.Conn, spawnObj)
			protocol.WritePacket(p.Conn, velocityPkt)
			protocol.WritePacket(p.Conn, metadata)
		}
		p.mu.Unlock()
	}
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
	ticker := time.NewTicker(500 * time.Millisecond)
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
				// Skip recently spawned items (1 second pickup delay)
				if time.Since(e.SpawnTime) < 1*time.Second {
					continue
				}

				dx := e.X - px
				dy := e.Y - py
				dz := e.Z - pz
				distSq := dx*dx + dy*dy + dz*dz

				if distSq < 4.0 { // 2.0 blocks range
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
		p.mu.Lock()
		if p.Conn != nil {
			protocol.WritePacket(p.Conn, pkt)
		}
		p.mu.Unlock()
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

func (s *Server) spawnMobEntitiesForPlayer(player *Player) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, mob := range s.mobEntities {
		s.sendMobToPlayer(player, mob)
	}
}
