package server
import "testing"
func TestMobWalkOffCliff(t *testing.T) {
s := New(DefaultConfig())
for x := int32(0); x <= 10; x++ {
for y := int32(0); y <= 63; y++ {
for z := int32(0); z <= 10; z++ {
// Solid blocks from X=0 to 10. X=11 is air.
s.world.SetBlock(x, y, z, 1<<4)
}
}
}
s.mu.Lock()
eid := s.nextEID
s.nextEID++
s.mobEntities[eid] = &MobEntity{
EntityID: eid, MobType: 90, X: 10.5, Y: 64.0, Z: 5.5, VX: 10.0,
}
s.mu.Unlock()

// Tick to let it move to X=11.5
s.tickEntityPhysics()
// Next tick, it should fall
s.tickEntityPhysics()

s.mu.RLock()
mob := s.mobEntities[eid]
s.mu.RUnlock()

t.Logf("Mob ended at X=%f, Y=%f, Z=%f", mob.X, mob.Y, mob.Z)
}
func TestMobFallingDown(t *testing.T) {
s := New(DefaultConfig())
s.mu.Lock()
eid := s.nextEID
s.nextEID++
mob := &MobEntity{
EntityID: eid,
MobType:  90,
X:        10.5,
Y:        100.0,
Z:        10.5,
}
s.mobEntities[eid] = mob
s.mu.Unlock()

for i := 0; i < 20; i++ {
s.tickEntityPhysics()
}

s.mu.RLock()
finalY := mob.Y
s.mu.RUnlock()

if finalY >= 100.0 {
t.Errorf("Mob did not fall! Final Y: %f", finalY)
} else {
t.Logf("Mob fell successfully to Y: %f", finalY)
}
}
