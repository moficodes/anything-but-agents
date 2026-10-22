// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const DefaultMaxFloors = 8

// HeroWorldState captures the hero's live position, class stats, cooldowns,
// and equipped gear inside a full client-side WorldState snapshot.
type HeroWorldState struct {
	X                float64  `json:"x"`
	Y                float64  `json:"y"`
	Facing           string   `json:"facing,omitempty"`
	HP               int      `json:"hp"`
	MaxHP            int      `json:"max_hp"`
	Armor            int      `json:"armor"`
	MoveSpeed        float64  `json:"move_speed"`
	CharacterClass   string   `json:"character_class"`
	AttackDamage     int      `json:"attack_damage"`
	AttackRange      float64  `json:"attack_range"`
	AttackCooldownMs int      `json:"attack_cooldown_ms"`
	SkillName        string   `json:"skill_name"`
	SkillCooldownMs  int      `json:"skill_cooldown_ms"`
	EquippedWeapon   string   `json:"equipped_weapon,omitempty"`
	EquippedArmor    string   `json:"equipped_armor,omitempty"`
	EquippedRelic    string   `json:"equipped_relic,omitempty"`
	Potions          int      `json:"potions"`
	Gold             int      `json:"gold"`
	Score            int      `json:"score"`
	Inventory        []string `json:"inventory,omitempty"`
}

// EnemyState captures a single enemy's live coordinates, HP, and AI state on a floor.
type EnemyState struct {
	ID        string  `json:"id"`
	Archetype string  `json:"archetype"`
	Name      string  `json:"name,omitempty"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	HP        int     `json:"hp"`
	MaxHP     int     `json:"max_hp"`
	Alive     bool    `json:"alive"`
	AIState   string  `json:"ai_state,omitempty"`
}

// ItemState captures a ground item or chest on a floor.
type ItemState struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Collected bool    `json:"collected"`
}

// InteractableState captures a lever, gate, shrine, or mutually-exclusive vault door.
type InteractableState struct {
	ID    string  `json:"id"`
	Kind  string  `json:"kind"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	State string  `json:"state"`
}

// FloorWorldState holds the live entities on a single dungeon floor.
type FloorWorldState struct {
	Enemies       []EnemyState        `json:"enemies,omitempty"`
	Items         []ItemState         `json:"items,omitempty"`
	Interactables []InteractableState `json:"interactables,omitempty"`
}

// WorldState is the complete, self-contained serializable snapshot of a player's
// 8-floor dungeon run persisted in the Agent Substrate Actor Go heap.
type WorldState struct {
	Floor     int                        `json:"floor"`
	MaxFloors int                        `json:"max_floors"`
	Hero      HeroWorldState             `json:"hero"`
	Floors    map[string]FloorWorldState `json:"floors,omitempty"`
}

func defaultHeroWorldState(charClass string) HeroWorldState {
	cls := strings.ToLower(strings.TrimSpace(charClass))
	switch cls {
	case "ranger":
		return HeroWorldState{
			X:                2,
			Y:                2,
			Facing:           "right",
			HP:               100,
			MaxHP:            100,
			Armor:            10,
			MoveSpeed:        1.15,
			CharacterClass:   "ranger",
			AttackDamage:     26,
			AttackRange:      4.5,
			AttackCooldownMs: 950,
			SkillName:        "Tumble & Snare",
			SkillCooldownMs:  5000,
			Inventory:        []string{},
		}
	case "wizard", "mage":
		if cls == "" {
			cls = "wizard"
		}
		return HeroWorldState{
			X:                2,
			Y:                2,
			Facing:           "right",
			HP:               75,
			MaxHP:            75,
			Armor:            0,
			MoveSpeed:        1.0,
			CharacterClass:   cls,
			AttackDamage:     38,
			AttackRange:      4.0,
			AttackCooldownMs: 1150,
			SkillName:        "Frost Blink",
			SkillCooldownMs:  7000,
			Inventory:        []string{},
		}
	default:
		return HeroWorldState{
			X:                2,
			Y:                2,
			Facing:           "right",
			HP:               140,
			MaxHP:            140,
			Armor:            25,
			MoveSpeed:        1.0,
			CharacterClass:   "paladin",
			AttackDamage:     22,
			AttackRange:      1.4,
			AttackCooldownMs: 550,
			SkillName:        "Aegis Shield",
			SkillCooldownMs:  6000,
			Inventory:        []string{},
		}
	}
}

func cloneWorldState(w *WorldState) *WorldState {
	if w == nil {
		return nil
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return nil
	}
	var out WorldState
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	if out.Floors == nil {
		out.Floors = map[string]FloorWorldState{}
	}
	if out.Hero.Inventory == nil {
		out.Hero.Inventory = []string{}
	}
	return &out
}

// ActorCheckpoint stores a committed milestone state (captured AFTER each
// non-fatal milestone or explicit save_world is applied) so `Revert Actor (Save-Scum)`
// can restore either the latest saved milestone or any specific earlier version (v1..vN).
type ActorCheckpoint struct {
	Version   int             `json:"version"`
	Label     string          `json:"label"`
	Floor     int             `json:"floor"`
	X         int             `json:"x"`
	Y         int             `json:"y"`
	HP        int             `json:"hp"`
	MaxHP     int             `json:"max_hp"`
	Gold      int             `json:"gold"`
	Score     int             `json:"score"`
	Inventory []string        `json:"inventory"`
	Cleared   map[string]bool `json:"cleared"`
	MonsterHP map[string]int  `json:"monster_hp"`
	BossHP    int             `json:"boss_hp"`
	World     *WorldState     `json:"world,omitempty"`
}

// ActorSession holds 100% of a player's 2D multilevel dungeon game state in
// ordinary Go heap variables. When running under Agent Substrate with
// SNAPSHOT_FIDELITY_MEMORY, suspending the actor checkpoints this entire
// heap struct to GCS, and reverting the actor rewinds it in place.
type ActorSession struct {
	mu              sync.Mutex        `json:"-"`
	BootedAt        time.Time         `json:"booted_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	HeroName        string            `json:"hero_name"`
	CharacterClass  string            `json:"character_class"`
	Floor           int               `json:"floor"`
	MaxFloors       int               `json:"max_floors"`
	X               int               `json:"x"`
	Y               int               `json:"y"`
	HP              int               `json:"hp"`
	MaxHP           int               `json:"max_hp"`
	Gold            int               `json:"gold"`
	Score           int               `json:"score"`
	Alive           bool              `json:"alive"`
	Victory         bool              `json:"victory"`
	DeathReason     string            `json:"death_reason,omitempty"`
	Inventory       []string          `json:"inventory"`
	Cleared         map[string]bool   `json:"cleared"`
	MonsterHP       map[string]int    `json:"monster_hp"`
	BossHP          int               `json:"boss_hp"`
	BossMaxHP       int               `json:"boss_max_hp"`
	Turn            int               `json:"turn"`
	MilestoneCount  int               `json:"milestone_count"`
	LastDamageDealt int               `json:"last_damage_dealt,omitempty"`
	LastDamageTaken int               `json:"last_damage_taken,omitempty"`
	LastEvent       string            `json:"last_event"`
	CurrentPodIP    string            `json:"current_pod_ip"`
	PodIPHistory    []string          `json:"pod_ip_history"`
	World           *WorldState       `json:"world,omitempty"`
	Checkpoint      *ActorCheckpoint  `json:"checkpoint,omitempty"`
	Checkpoints     []ActorCheckpoint `json:"checkpoints,omitempty"`
}

// MutateRequest describes a state milestone or full WorldState snapshot sent
// from the browser via the Gateway through atenet-router to wake and mutate
// the player's actor.
type MutateRequest struct {
	Action         string      `json:"action"`
	Label          string      `json:"label,omitempty"`
	HeroName       string      `json:"hero_name,omitempty"`
	CharacterClass string      `json:"character_class,omitempty"`
	Target         string      `json:"target,omitempty"`
	EntityID       string      `json:"entity_id,omitempty"`
	X              int         `json:"x,omitempty"`
	Y              int         `json:"y,omitempty"`
	PrevX          int         `json:"prev_x,omitempty"`
	PrevY          int         `json:"prev_y,omitempty"`
	TargetVersion  int         `json:"target_version,omitempty"`
	World          *WorldState `json:"world,omitempty"`
}

// ActorResponse is the JSON payload returned by the actor after reading or
// mutating its in-memory Go heap state.
type ActorResponse struct {
	Message     string       `json:"message"`
	EventType   string       `json:"event_type"`
	DamageDealt int          `json:"damage_dealt,omitempty"`
	DamageTaken int          `json:"damage_taken,omitempty"`
	Session     ActorSession `json:"session"`
	Persistence string       `json:"persistence"`
}

func NewActorSession() *ActorSession {
	now := time.Now().UTC()
	hero := defaultHeroWorldState("paladin")
	s := &ActorSession{
		BootedAt:       now,
		UpdatedAt:      now,
		HeroName:       "adventurer",
		CharacterClass: "paladin",
		Floor:          1,
		MaxFloors:      DefaultMaxFloors,
		X:              1,
		Y:              1,
		HP:             hero.HP,
		MaxHP:          hero.MaxHP,
		Gold:           0,
		Score:          0,
		Alive:          true,
		Victory:        false,
		Inventory:      []string{},
		Cleared:        map[string]bool{},
		MonsterHP: map[string]int{
			"f1:skeleton": 30,
			"f2:guard":    45,
			"f3:dragon":   100,
			"f8:dragon":   220,
		},
		BossHP:    100,
		BossMaxHP: 100,
		World: &WorldState{
			Floor:     1,
			MaxFloors: DefaultMaxFloors,
			Hero:      hero,
			Floors:    map[string]FloorWorldState{},
		},
		LastEvent: "Entered Floor 1: The Forgotten Crypt (100% Go heap state).",
	}
	s.saveCheckpointLocked("Entered Floor 1")
	return s
}

func (s *ActorSession) applyWorldStateLocked(w *WorldState) {
	if w == nil {
		return
	}
	cloned := cloneWorldState(w)
	if cloned == nil {
		return
	}
	baseHero := defaultHeroWorldState(firstNonEmpty(cloned.Hero.CharacterClass, s.CharacterClass))
	if cloned.Hero.CharacterClass == "" {
		cloned.Hero.CharacterClass = baseHero.CharacterClass
	}
	if cloned.Hero.MaxHP <= 0 {
		cloned.Hero.MaxHP = baseHero.MaxHP
	}
	if cloned.Hero.MoveSpeed <= 0 {
		cloned.Hero.MoveSpeed = baseHero.MoveSpeed
	}
	if cloned.Hero.AttackDamage <= 0 {
		cloned.Hero.AttackDamage = baseHero.AttackDamage
	}
	if cloned.Hero.AttackRange <= 0 {
		cloned.Hero.AttackRange = baseHero.AttackRange
	}
	if cloned.Hero.AttackCooldownMs <= 0 {
		cloned.Hero.AttackCooldownMs = baseHero.AttackCooldownMs
	}
	if cloned.Hero.SkillName == "" {
		cloned.Hero.SkillName = baseHero.SkillName
	}
	if cloned.Hero.SkillCooldownMs <= 0 {
		cloned.Hero.SkillCooldownMs = baseHero.SkillCooldownMs
	}
	if cloned.Floor <= 0 {
		cloned.Floor = max(1, s.Floor)
	}
	if cloned.MaxFloors <= 0 {
		cloned.MaxFloors = DefaultMaxFloors
	}

	s.CharacterClass = cloned.Hero.CharacterClass
	s.Floor = cloned.Floor
	s.MaxFloors = cloned.MaxFloors
	if cloned.Hero.X > 0 || cloned.Hero.Y > 0 {
		s.X = int(math.Round(cloned.Hero.X))
		s.Y = int(math.Round(cloned.Hero.Y))
	}
	s.MaxHP = cloned.Hero.MaxHP
	s.HP = max(0, min(cloned.Hero.MaxHP, cloned.Hero.HP))
	s.Gold = cloned.Hero.Gold
	s.Score = cloned.Hero.Score
	s.Alive = s.HP > 0

	inv := make([]string, len(cloned.Hero.Inventory))
	copy(inv, cloned.Hero.Inventory)
	for _, eq := range []string{cloned.Hero.EquippedWeapon, cloned.Hero.EquippedArmor, cloned.Hero.EquippedRelic} {
		if eq != "" && !slices.Contains(inv, eq) {
			inv = append(inv, eq)
		}
	}
	s.Inventory = inv
	cloned.Hero.Inventory = inv

	if s.Cleared == nil {
		s.Cleared = map[string]bool{}
	}
	if s.MonsterHP == nil {
		s.MonsterHP = map[string]int{}
	}
	for _, fState := range cloned.Floors {
		for _, en := range fState.Enemies {
			if en.ID == "" {
				continue
			}
			s.MonsterHP[en.ID] = en.HP
			s.Cleared[en.ID] = !en.Alive || en.HP <= 0
			if strings.Contains(en.ID, "dragon") && en.MaxHP > 0 {
				s.BossHP = en.HP
				s.BossMaxHP = en.MaxHP
			}
		}
		for _, it := range fState.Items {
			if it.ID != "" {
				s.Cleared[it.ID] = it.Collected
			}
		}
		for _, ia := range fState.Interactables {
			if ia.ID != "" {
				s.Cleared[ia.ID] = ia.State == "open" || ia.State == "activated"
			}
		}
	}
	s.World = cloned
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (s *ActorSession) syncWorldHeroSummaryLocked() {
	if s.World == nil {
		hero := defaultHeroWorldState(s.CharacterClass)
		s.World = &WorldState{
			Floor:     s.Floor,
			MaxFloors: max(DefaultMaxFloors, s.MaxFloors),
			Hero:      hero,
			Floors:    map[string]FloorWorldState{},
		}
	}
	s.World.Floor = s.Floor
	if s.MaxFloors > 0 {
		s.World.MaxFloors = s.MaxFloors
	}
	if s.X > 0 && s.World.Hero.X == 0 {
		s.World.Hero.X = float64(s.X)
	}
	if s.Y > 0 && s.World.Hero.Y == 0 {
		s.World.Hero.Y = float64(s.Y)
	}
	s.World.Hero.HP = s.HP
	s.World.Hero.MaxHP = s.MaxHP
	s.World.Hero.Gold = s.Gold
	s.World.Hero.Score = s.Score
	s.World.Hero.CharacterClass = s.CharacterClass
	inv := make([]string, len(s.Inventory))
	copy(inv, s.Inventory)
	s.World.Hero.Inventory = inv
}

func (s *ActorSession) saveCheckpointLocked(label string) {
	s.syncWorldHeroSummaryLocked()
	inv := make([]string, len(s.Inventory))
	copy(inv, s.Inventory)
	cleared := make(map[string]bool, len(s.Cleared))
	for k, v := range s.Cleared {
		cleared[k] = v
	}
	mhp := make(map[string]int, len(s.MonsterHP))
	for k, v := range s.MonsterHP {
		mhp[k] = v
	}
	nextVer := 1
	if len(s.Checkpoints) > 0 {
		nextVer = s.Checkpoints[len(s.Checkpoints)-1].Version + 1
	}
	cp := ActorCheckpoint{
		Version:   nextVer,
		Label:     label,
		Floor:     s.Floor,
		X:         s.X,
		Y:         s.Y,
		HP:        s.HP,
		MaxHP:     s.MaxHP,
		Gold:      s.Gold,
		Score:     s.Score,
		Inventory: inv,
		Cleared:   cleared,
		MonsterHP: mhp,
		BossHP:    s.BossHP,
		World:     cloneWorldState(s.World),
	}
	s.Checkpoints = append(s.Checkpoints, cp)
	if len(s.Checkpoints) > 25 {
		s.Checkpoints = s.Checkpoints[len(s.Checkpoints)-25:]
	}
	cpCopy := cp
	cpCopy.World = cloneWorldState(cp.World)
	s.Checkpoint = &cpCopy
}

func (s *ActorSession) restoreCheckpointLocked(targetVersion int) string {
	var chosen *ActorCheckpoint
	if targetVersion > 0 && len(s.Checkpoints) > 0 {
		for i := len(s.Checkpoints) - 1; i >= 0; i-- {
			if s.Checkpoints[i].Version == targetVersion {
				cp := s.Checkpoints[i]
				chosen = &cp
				s.Checkpoints = s.Checkpoints[:i+1]
				break
			}
		}
	}
	if chosen == nil && len(s.Checkpoints) > 0 {
		cp := s.Checkpoints[len(s.Checkpoints)-1]
		chosen = &cp
	}
	if chosen == nil {
		chosen = s.Checkpoint
	}
	if chosen == nil {
		s.Floor = 1
		s.X, s.Y = 1, 1
		s.HP = s.MaxHP
		s.Alive = true
		s.Victory = false
		s.DeathReason = ""
		return fmt.Sprintf("SAVE-SCUM REVERT: Restored Floor 1 starting checkpoint (Pos 1,1 | HP %d/%d)!", s.HP, s.MaxHP)
	}
	cp := chosen
	cpCopy := *cp
	cpCopy.World = cloneWorldState(cp.World)
	s.Checkpoint = &cpCopy

	s.Floor = cp.Floor
	s.X = cp.X
	s.Y = cp.Y
	if cp.MaxHP > 0 {
		s.MaxHP = cp.MaxHP
	}
	s.HP = max(1, cp.HP)
	s.Gold = cp.Gold
	s.Score = cp.Score
	s.Alive = true
	s.Victory = false
	s.DeathReason = ""
	s.BossHP = cp.BossHP

	s.Inventory = make([]string, len(cp.Inventory))
	copy(s.Inventory, cp.Inventory)
	s.Cleared = make(map[string]bool, len(cp.Cleared))
	for k, v := range cp.Cleared {
		s.Cleared[k] = v
	}
	s.MonsterHP = make(map[string]int, len(cp.MonsterHP))
	for k, v := range cp.MonsterHP {
		s.MonsterHP[k] = v
	}
	if cp.World != nil {
		s.World = cloneWorldState(cp.World)
		s.World.Hero.HP = s.HP
	} else {
		s.syncWorldHeroSummaryLocked()
	}
	return fmt.Sprintf("SAVE-SCUM REVERT (`kubectl ate revert` -> v%d): Restored Snapshot v%d (%s) on Floor %d (Pos %d,%d | HP %d/%d)!",
		cp.Version, cp.Version, cp.Label, s.Floor, s.X, s.Y, s.HP, s.MaxHP)
}

func (s *ActorSession) recordPodIP(ip string) {
	if ip == "" {
		ip = currentPodIP()
	}
	s.CurrentPodIP = ip
	if len(s.PodIPHistory) == 0 || s.PodIPHistory[len(s.PodIPHistory)-1] != ip {
		s.PodIPHistory = append(s.PodIPHistory, ip)
	}
}

func (s *ActorSession) Clone() *ActorSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(s)
	var out ActorSession
	_ = json.Unmarshal(raw, &out)
	if out.Cleared == nil {
		out.Cleared = map[string]bool{}
	}
	if out.MonsterHP == nil {
		out.MonsterHP = map[string]int{}
	}
	if out.Inventory == nil {
		out.Inventory = []string{}
	}
	return &out
}

func (s *ActorSession) Snapshot(podIP string) ActorResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordPodIP(podIP)
	return s.responseLocked(s.LastEvent, "STATE")
}

func (s *ActorSession) Mutate(req MutateRequest, podIP string) ActorResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.recordPodIP(podIP)
	s.UpdatedAt = time.Now().UTC()
	s.LastDamageDealt = 0
	s.LastDamageTaken = 0

	action := strings.ToLower(strings.TrimSpace(req.Action))
	if action == "" || action == "state" || action == "look" {
		return s.responseLocked(s.LastEvent, "STATE")
	}

	if action == "restore_checkpoint" {
		msg := s.restoreCheckpointLocked(req.TargetVersion)
		s.LastEvent = msg
		return s.responseLocked(msg, "REVERT")
	}

	if action == "init" {
		if req.HeroName != "" {
			s.HeroName = req.HeroName
		}
		if req.CharacterClass != "" {
			s.CharacterClass = strings.ToLower(strings.TrimSpace(req.CharacterClass))
		}
		hero := defaultHeroWorldState(s.CharacterClass)
		s.Floor = 1
		s.MaxFloors = DefaultMaxFloors
		s.X, s.Y = 1, 1
		if req.X > 0 || req.Y > 0 {
			s.X, s.Y = req.X, req.Y
			hero.X = float64(req.X)
			hero.Y = float64(req.Y)
		}
		s.HP = hero.HP
		s.MaxHP = hero.MaxHP
		s.Alive = true
		s.Victory = false
		s.DeathReason = ""
		s.Inventory = []string{}
		s.Cleared = map[string]bool{}
		s.World = &WorldState{
			Floor:     1,
			MaxFloors: DefaultMaxFloors,
			Hero:      hero,
			Floors:    map[string]FloorWorldState{},
		}
		if req.World != nil {
			s.applyWorldStateLocked(req.World)
		}
		s.Checkpoints = nil
		s.saveCheckpointLocked("Entered Floor 1")
		msg := fmt.Sprintf("Hero %s (%s, %d HP) entered Floor 1: The Forgotten Crypt.", s.HeroName, s.CharacterClass, s.MaxHP)
		s.LastEvent = msg
		return s.responseLocked(msg, "INIT")
	}

	s.Turn++
	s.MilestoneCount++

	// Full client-driven WorldState save (`save_world` or `victory`)
	if action == "save_world" || action == "victory" || (action == "quicksave" && req.World != nil) {
		if req.World != nil {
			s.applyWorldStateLocked(req.World)
		}
		if req.X > 0 || req.Y > 0 {
			s.X, s.Y = req.X, req.Y
		}
		label := strings.TrimSpace(req.Label)
		if label == "" {
			label = strings.TrimSpace(req.Target)
		}
		if label == "" {
			label = fmt.Sprintf("Floor %d World Snapshot", s.Floor)
		}
		eventType := "QUICKSAVE"
		if action == "victory" || (req.World != nil && req.World.Floor >= DefaultMaxFloors && slices.Contains(s.Inventory, "golden-chalice")) {
			if action == "victory" {
				s.Victory = true
				eventType = "VICTORY"
			}
		}
		if s.Alive {
			s.saveCheckpointLocked(label)
		}
		ver := 1
		if len(s.Checkpoints) > 0 {
			ver = s.Checkpoints[len(s.Checkpoints)-1].Version
		}
		msg := fmt.Sprintf("WORLD STATE SAVED (Snapshot v%d — %s) on Floor %d/%d (HP %d/%d, Score %d) -> Checkpointed to Go heap & GCS.",
			ver, label, s.Floor, s.MaxFloors, s.HP, s.MaxHP, s.Score)
		if s.Victory {
			msg = fmt.Sprintf("VICTORY! %s conquered all %d Floors with the Golden Chalice of State (Score: %d, Snapshot v%d, Pod IPs: %d)!",
				s.HeroName, s.MaxFloors, s.Score, ver, len(s.PodIPHistory))
		}
		s.LastEvent = msg
		return s.responseLocked(msg, eventType)
	}

	if !s.Alive {
		msg := "YOU ARE DEAD! Tap 'Revert Actor' to run `kubectl ate revert` and restore your last GCS heap snapshot."
		return s.responseLocked(msg, "DEAD")
	}
	if s.Victory {
		msg := "VICTORY! You already escaped the Dungeon with the Golden Chalice of State."
		return s.responseLocked(msg, "VICTORY")
	}

	var msg string
	eventType := "MILESTONE"
	checkpointLabel := ""

	switch action {
	case "pickup":
		item := strings.TrimSpace(req.Target)
		entityID := strings.TrimSpace(req.EntityID)
		if entityID == "" {
			entityID = fmt.Sprintf("f%d:%s", s.Floor, item)
		}
		if s.Cleared[entityID] {
			msg = fmt.Sprintf("Already collected %s.", item)
		} else {
			if req.X > 0 || req.Y > 0 {
				s.X, s.Y = req.X, req.Y
			}
			s.Cleared[entityID] = true
			if item == "gold-chest" {
				s.Gold += 100
				s.Score += 150
				msg = "Looted 100 Gold from the ancient chest! Checkpointed to Go heap."
			} else {
				if !slices.Contains(s.Inventory, item) || item == "healing-potion" {
					s.Inventory = append(s.Inventory, item)
				}
				s.Score += 100
				msg = fmt.Sprintf("Picked up %s on Floor %d! Stored in Go heap slice and committed to GCS.", item, s.Floor)
			}
			checkpointLabel = "pickup " + item
		}
		eventType = "PICKUP"

	case "drink":
		idx := slices.Index(s.Inventory, "healing-potion")
		if idx == -1 {
			msg = "No healing-potion in inventory!"
		} else {
			if req.X > 0 || req.Y > 0 {
				s.X, s.Y = req.X, req.Y
			}
			s.Inventory = slices.Delete(s.Inventory, idx, idx+1)
			s.HP = min(s.MaxHP, s.HP+50)
			s.Score += 25
			msg = fmt.Sprintf("Drank healing-potion! HP restored to %d/%d.", s.HP, s.MaxHP)
			checkpointLabel = "drink healing-potion"
		}
		eventType = "HEAL"

	case "combat":
		entityID := strings.TrimSpace(req.EntityID)
		target := strings.TrimSpace(req.Target)
		if entityID == "" {
			switch s.Floor {
			case 1:
				entityID = "f1:skeleton"
			case 2:
				entityID = "f2:guard"
			default:
				entityID = "f3:dragon"
			}
		}
		if target == "" {
			target = entityID
		}
		if s.Cleared[entityID] {
			msg = fmt.Sprintf("%s is already defeated!", target)
			eventType = "COMBAT"
			break
		}

		if req.X > 0 || req.Y > 0 {
			s.X, s.Y = req.X, req.Y
		}

		playerDmg := 30
		if slices.Contains(s.Inventory, "dragon-slayer-sword") {
			playerDmg = 55
		}

		switch entityID {
		case "f1:skeleton":
			counterDmg := 20
			s.MonsterHP[entityID] = 0
			s.Cleared[entityID] = true
			s.LastDamageDealt = playerDmg
			s.LastDamageTaken = counterDmg
			s.HP = max(0, s.HP-counterDmg)
			if s.HP == 0 {
				s.Alive = false
				s.DeathReason = fmt.Sprintf("Slain by %s's rusty bone scimitar (-%d HP)!", target, counterDmg)
				msg = fmt.Sprintf("CLASH! %s slashed you for -%d HP! YOU DIED (HP=0). Tap REVERT ACTOR to restore your last GCS snapshot!", target, counterDmg)
				eventType = "DEATH"
			} else {
				s.Gold += 50
				s.Score += 200
				msg = fmt.Sprintf("CLASH! You shattered %s (%d DMG), but its bone blade slashed you for -%d HP (Your HP: %d/%d)! Path to Crypt Key is clear.", target, playerDmg, counterDmg, s.HP, s.MaxHP)
				eventType = "COMBAT_WIN"
				checkpointLabel = "defeat " + target
			}

		case "f2:guard":
			if !slices.Contains(s.Inventory, "dragon-slayer-sword") {
				s.LastDamageDealt = 15
				s.LastDamageTaken = max(100, s.HP)
				s.HP = 0
				s.Alive = false
				s.DeathReason = "Cut down by the Shadow Guard (-100 HP — missing dragon-slayer-sword! Grab the Sword in the NW alcove first)!"
				msg = fmt.Sprintf("LETHAL COUNTER-ATTACK! Without the dragon-slayer-sword, your weapon bounced off %s and its shadow halberd dealt -100 HP! YOU DIED (HP=0). Tap REVERT ACTOR to save-scum!", target)
				eventType = "DEATH"
				break
			}
			counterDmg := 25
			s.LastDamageDealt = playerDmg
			s.LastDamageTaken = counterDmg
			s.HP = max(0, s.HP-counterDmg)
			if s.HP == 0 {
				s.Alive = false
				s.DeathReason = fmt.Sprintf("Cut down by %s's shadow halberd (-%d HP)!", target, counterDmg)
				msg = fmt.Sprintf("CLASH! %s struck you for -%d HP! YOU DIED (HP=0). Tap REVERT ACTOR to restore your last GCS snapshot!", target, counterDmg)
				eventType = "DEATH"
			} else {
				s.MonsterHP[entityID] = 0
				s.Cleared[entityID] = true
				s.Gold += 75
				s.Score += 250
				msg = fmt.Sprintf("CLASH! You vanquished %s (%d DMG), but its shadow halberd counter-attacked for -%d HP (Your HP: %d/%d)! Fire Shield vault unlocked.", target, playerDmg, counterDmg, s.HP, s.MaxHP)
				eventType = "COMBAT_WIN"
				checkpointLabel = "defeat " + target
			}

		case "f3:dragon", "f8:dragon":
			hasSword := slices.Contains(s.Inventory, "dragon-slayer-sword")
			hasShield := slices.Contains(s.Inventory, "fire-shield")

			if !hasShield && !hasSword {
				s.LastDamageDealt = 10
				s.LastDamageTaken = max(100, s.HP)
				s.HP = 0
				s.Alive = false
				s.DeathReason = "Obliterated by the Ancient Heap Dragon (-100 HP — missing both dragon-slayer-sword and fire-shield)!"
				msg = fmt.Sprintf("LETHAL BOSS WIPE! You fought %s without the Dragon-Slayer Sword or Fire Shield (-100 HP)! YOU DIED (HP=0). Tap REVERT ACTOR!", target)
				eventType = "DEATH"
				break
			}
			if !hasShield {
				s.LastDamageDealt = playerDmg
				s.LastDamageTaken = max(100, s.HP)
				s.HP = 0
				s.Alive = false
				s.DeathReason = "Incinerated by unbuffered Heap Dragon fire breath (-100 HP — missing fire-shield)!"
				msg = fmt.Sprintf("LETHAL COUNTER-ATTACK! You struck %s for %d DMG, but without the fire-shield its fire breath dealt -100 HP! YOU DIED (HP=0). Tap REVERT ACTOR to time-travel back to your last GCS snapshot!", target, playerDmg)
				eventType = "DEATH"
				break
			}
			if !hasSword {
				s.LastDamageDealt = 10
				s.LastDamageTaken = max(100, s.HP)
				s.HP = 0
				s.Alive = false
				s.DeathReason = "Crushed by the Ancient Heap Dragon (-100 HP — missing dragon-slayer-sword! Your starting weapon shattered against its scales)!"
				msg = fmt.Sprintf("LETHAL COUNTER-ATTACK! Without the dragon-slayer-sword, your blade shattered against %s's scales and its jaws dealt -100 HP! YOU DIED (HP=0). Tap REVERT ACTOR!", target)
				eventType = "DEATH"
				break
			}

			curHP := s.BossHP
			if curHP <= 0 {
				curHP = 100
			}
			curHP = max(0, curHP-playerDmg)
			s.BossHP = curHP
			s.MonsterHP[entityID] = curHP
			s.LastDamageDealt = playerDmg

			if curHP == 0 {
				counter := 10
				s.LastDamageTaken = counter
				s.HP = max(1, s.HP-counter)
				s.Cleared[entityID] = true
				s.Gold += 500
				s.Score += 1000
				msg = fmt.Sprintf("SLAYED THE %s (%d DMG)! Your fire-shield absorbed the flames (-%d HP; Your HP: %d/%d)! Grab the Golden Chalice and enter the top Portal Door!", strings.ToUpper(target), playerDmg, counter, s.HP, s.MaxHP)
				eventType = "BOSS_DEFEATED"
				checkpointLabel = "slay " + target
			} else {
				counter := 20
				s.LastDamageTaken = counter
				s.HP = max(0, s.HP-counter)
				if s.HP == 0 {
					s.Alive = false
					s.DeathReason = "Fell in battle against the Ancient Heap Dragon (-20 HP tail swipe)."
					msg = fmt.Sprintf("You struck %s (%d HP left), but its tail swipe dealt -%d HP and finished you off! YOU DIED.", target, curHP, counter)
					eventType = "DEATH"
				} else {
					s.Score += 150
					msg = fmt.Sprintf("CLASH! You struck %s for %d DMG (%d/%d Boss HP left) — Dragon counter-attacked for -%d HP (Your HP: %d/%d)! Strike again!", target, playerDmg, curHP, s.BossMaxHP, counter, s.HP, s.MaxHP)
					eventType = "COMBAT"
					checkpointLabel = "strike " + target
				}
			}

		default:
			s.Cleared[entityID] = true
			s.Score += 100
			msg = fmt.Sprintf("Defeated %s!", target)
			eventType = "COMBAT_WIN"
			checkpointLabel = "defeat " + target
		}

	case "quicksave":
		if req.X > 0 || req.Y > 0 {
			s.X, s.Y = req.X, req.Y
		}
		s.HP = s.MaxHP
		if !s.Cleared["f2:shrine_bonus"] {
			s.Cleared["f2:shrine_bonus"] = true
			s.Score += 50
		}
		label := firstNonEmpty(req.Label, "quicksave shrine")
		msg = fmt.Sprintf("SHRINE ACTIVATED on Floor %d! Restored HP to %d/%d, checkpointed Go heap to GCS, and tagged snapshot.", s.Floor, s.HP, s.MaxHP)
		eventType = "QUICKSAVE"
		checkpointLabel = label

	case "stairs":
		if s.Floor == 1 && !slices.Contains(s.Inventory, "crypt-key") {
			msg = "The Crypt stairs are locked! Collect the crypt-key on Floor 1 first."
			eventType = "BLOCKED"
		} else if s.Floor < max(DefaultMaxFloors, s.MaxFloors) {
			s.Floor++
			if req.X > 0 || req.Y > 0 {
				s.X, s.Y = req.X, req.Y
			} else if s.Floor == 2 {
				s.X, s.Y = 1, 5
			} else {
				s.X, s.Y = 7, 9
			}
			scoreKey := fmt.Sprintf("f%d:stairs_score", s.Floor-1)
			if !s.Cleared[scoreKey] {
				s.Cleared[scoreKey] = true
				s.Score += 200
			}
			msg = fmt.Sprintf("Descended to Floor %d/%d!", s.Floor, max(DefaultMaxFloors, s.MaxFloors))
			eventType = "FLOOR_CHANGE"
			checkpointLabel = fmt.Sprintf("descend to Floor %d", s.Floor)
		} else {
			msg = fmt.Sprintf("Already on the deepest floor (Floor %d).", s.Floor)
		}

	case "stairs_up":
		if s.Floor > 1 {
			s.Floor--
			if req.X > 0 || req.Y > 0 {
				s.X, s.Y = req.X, req.Y
			} else {
				s.X, s.Y = 12, 5
			}
			msg = fmt.Sprintf("Climbed the stairs back to Floor %d.", s.Floor)
			eventType = "FLOOR_CHANGE"
			checkpointLabel = fmt.Sprintf("return to Floor %d", s.Floor)
		} else {
			msg = "Already on Floor 1."
		}

	case "escape":
		if !slices.Contains(s.Inventory, "golden-chalice") {
			msg = "The Ascension Portal requires the golden-chalice! Defeat the Ancient Heap Dragon and pick up the Golden Chalice first."
			eventType = "BLOCKED"
		} else {
			if req.X > 0 || req.Y > 0 {
				s.X, s.Y = req.X, req.Y
			}
			s.Victory = true
			s.Score += 1500 + (s.HP * 5)
			msg = fmt.Sprintf("VICTORY! %s escaped the Dungeon through the Ascension Portal with the Golden Chalice of State (Final Score: %d, Pod IPs visited: %d)!", s.HeroName, s.Score, len(s.PodIPHistory))
			eventType = "VICTORY"
			checkpointLabel = "escape dungeon"
		}

	default:
		msg = fmt.Sprintf("Unknown milestone action %q.", action)
	}

	if s.Alive && checkpointLabel != "" {
		s.saveCheckpointLocked(checkpointLabel)
	} else {
		s.syncWorldHeroSummaryLocked()
	}

	s.LastEvent = msg
	return s.responseLocked(msg, eventType)
}

func (s *ActorSession) responseLocked(msg, eventType string) ActorResponse {
	s.syncWorldHeroSummaryLocked()
	inv := make([]string, len(s.Inventory))
	copy(inv, s.Inventory)
	cleared := make(map[string]bool, len(s.Cleared))
	for k, v := range s.Cleared {
		cleared[k] = v
	}
	mhp := make(map[string]int, len(s.MonsterHP))
	for k, v := range s.MonsterHP {
		mhp[k] = v
	}
	ips := make([]string, len(s.PodIPHistory))
	copy(ips, s.PodIPHistory)

	var cpCopy *ActorCheckpoint
	if s.Checkpoint != nil {
		rawCP, _ := json.Marshal(s.Checkpoint)
		var tmp ActorCheckpoint
		if json.Unmarshal(rawCP, &tmp) == nil {
			cpCopy = &tmp
		}
	}
	var cpsCopy []ActorCheckpoint
	if len(s.Checkpoints) > 0 {
		rawCPs, _ := json.Marshal(s.Checkpoints)
		_ = json.Unmarshal(rawCPs, &cpsCopy)
	}

	snap := ActorSession{
		BootedAt:        s.BootedAt,
		UpdatedAt:       s.UpdatedAt,
		HeroName:        s.HeroName,
		CharacterClass:  s.CharacterClass,
		Floor:           s.Floor,
		MaxFloors:       max(DefaultMaxFloors, s.MaxFloors),
		X:               s.X,
		Y:               s.Y,
		HP:              s.HP,
		MaxHP:           s.MaxHP,
		Gold:            s.Gold,
		Score:           s.Score,
		Alive:           s.Alive,
		Victory:         s.Victory,
		DeathReason:     s.DeathReason,
		Inventory:       inv,
		Cleared:         cleared,
		MonsterHP:       mhp,
		BossHP:          s.BossHP,
		BossMaxHP:       s.BossMaxHP,
		Turn:            s.Turn,
		MilestoneCount:  s.MilestoneCount,
		LastDamageDealt: s.LastDamageDealt,
		LastDamageTaken: s.LastDamageTaken,
		LastEvent:       s.LastEvent,
		CurrentPodIP:    s.CurrentPodIP,
		PodIPHistory:    ips,
		World:           cloneWorldState(s.World),
		Checkpoint:      cpCopy,
		Checkpoints:     cpsCopy,
	}
	return ActorResponse{
		Message:     msg,
		EventType:   eventType,
		DamageDealt: s.LastDamageDealt,
		DamageTaken: s.LastDamageTaken,
		Session:     snap,
		Persistence: "ZERO_DATABASE_PURE_PROCESS_RAM_GCS_CHECKPOINT",
	}
}

func decodeMutateRequest(r *http.Request) MutateRequest {
	var req MutateRequest
	if r.Body != nil {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 262144))
		_ = json.Unmarshal(body, &req)
	}
	if req.Action == "" {
		req.Action = r.URL.Query().Get("action")
	}
	if req.Target == "" {
		req.Target = r.URL.Query().Get("target")
	}
	if req.EntityID == "" {
		req.EntityID = r.URL.Query().Get("entity_id")
	}
	if req.HeroName == "" {
		req.HeroName = r.URL.Query().Get("hero")
	}
	return req
}

func NewActorMux(session *ActorSession) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, session.Snapshot(currentPodIP()))
	})
	mux.HandleFunc("/mutate", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, session.Mutate(decodeMutateRequest(r), currentPodIP()))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.URL.Query().Get("action") != "" {
			writeJSON(w, http.StatusOK, session.Mutate(decodeMutateRequest(r), currentPodIP()))
			return
		}
		writeJSON(w, http.StatusOK, session.Snapshot(currentPodIP()))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func currentPodIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "unknown"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return "127.0.0.1"
}
