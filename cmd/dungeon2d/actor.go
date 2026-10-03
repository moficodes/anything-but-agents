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
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// ActorCheckpoint stores the last committed milestone state (captured AFTER
// each non-fatal milestone is applied) so `Revert Actor (Save-Scum)` restores
// the exact state right after the last saved milestone.
type ActorCheckpoint struct {
	Label     string          `json:"label"`
	Floor     int             `json:"floor"`
	X         int             `json:"x"`
	Y         int             `json:"y"`
	HP        int             `json:"hp"`
	Gold      int             `json:"gold"`
	Score     int             `json:"score"`
	Inventory []string        `json:"inventory"`
	Cleared   map[string]bool `json:"cleared"`
	MonsterHP map[string]int  `json:"monster_hp"`
	BossHP    int             `json:"boss_hp"`
}

// ActorSession holds 100% of a player's 2D multilevel dungeon game state in
// ordinary Go heap variables. When running under Agent Substrate with
// SNAPSHOT_CONTENT_SCOPE_FULL, suspending the actor checkpoints this entire
// heap struct to GCS, and reverting the actor rewinds it in place.
type ActorSession struct {
	mu              sync.Mutex       `json:"-"`
	BootedAt        time.Time        `json:"booted_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	HeroName        string           `json:"hero_name"`
	CharacterClass  string           `json:"character_class"`
	Floor           int              `json:"floor"`
	X               int              `json:"x"`
	Y               int              `json:"y"`
	HP              int              `json:"hp"`
	MaxHP           int              `json:"max_hp"`
	Gold            int              `json:"gold"`
	Score           int              `json:"score"`
	Alive           bool             `json:"alive"`
	Victory         bool             `json:"victory"`
	DeathReason     string           `json:"death_reason,omitempty"`
	Inventory       []string         `json:"inventory"`
	Cleared         map[string]bool  `json:"cleared"`
	MonsterHP       map[string]int   `json:"monster_hp"`
	BossHP          int              `json:"boss_hp"`
	BossMaxHP       int              `json:"boss_max_hp"`
	Turn            int              `json:"turn"`
	MilestoneCount  int              `json:"milestone_count"`
	LastDamageDealt int              `json:"last_damage_dealt,omitempty"`
	LastDamageTaken int              `json:"last_damage_taken,omitempty"`
	LastEvent       string           `json:"last_event"`
	CurrentPodIP    string           `json:"current_pod_ip"`
	PodIPHistory    []string         `json:"pod_ip_history"`
	Checkpoint      *ActorCheckpoint `json:"checkpoint,omitempty"`
}

// MutateRequest describes a state milestone sent from the browser via the
// Gateway through atenet-router to wake and mutate the player's actor.
type MutateRequest struct {
	Action         string `json:"action"`
	HeroName       string `json:"hero_name,omitempty"`
	CharacterClass string `json:"character_class,omitempty"`
	Target         string `json:"target,omitempty"`
	EntityID       string `json:"entity_id,omitempty"`
	X              int    `json:"x,omitempty"`
	Y              int    `json:"y,omitempty"`
	PrevX          int    `json:"prev_x,omitempty"`
	PrevY          int    `json:"prev_y,omitempty"`
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
	s := &ActorSession{
		BootedAt:       now,
		UpdatedAt:      now,
		HeroName:       "adventurer",
		CharacterClass: "paladin",
		Floor:          1,
		X:              1,
		Y:              1,
		HP:             100,
		MaxHP:          100,
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
		},
		BossHP:    100,
		BossMaxHP: 100,
		LastEvent: "Entered Floor 1: The Forgotten Crypt (100% Go heap state).",
	}
	s.saveCheckpointLocked("entered Floor 1")
	return s
}

func (s *ActorSession) saveCheckpointLocked(label string) {
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
	s.Checkpoint = &ActorCheckpoint{
		Label:     label,
		Floor:     s.Floor,
		X:         s.X,
		Y:         s.Y,
		HP:        s.HP,
		Gold:      s.Gold,
		Score:     s.Score,
		Inventory: inv,
		Cleared:   cleared,
		MonsterHP: mhp,
		BossHP:    s.BossHP,
	}
}

func (s *ActorSession) restoreCheckpointLocked() string {
	if s.Checkpoint == nil {
		s.Floor = 1
		s.X, s.Y = 1, 1
		s.HP = s.MaxHP
		s.Alive = true
		s.Victory = false
		s.DeathReason = ""
		return "SAVE-SCUM REVERT: Restored Floor 1 starting checkpoint (Pos 1,1 | HP 100/100)!"
	}
	cp := s.Checkpoint
	s.Floor = cp.Floor
	s.X = cp.X
	s.Y = cp.Y
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
	return fmt.Sprintf("SAVE-SCUM REVERT (`kubectl ate revert`): Restored saved state after %q on Floor %d (Pos %d,%d | HP %d/%d)!",
		cp.Label, s.Floor, s.X, s.Y, s.HP, s.MaxHP)
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
		msg := s.restoreCheckpointLocked()
		s.LastEvent = msg
		return s.responseLocked(msg, "REVERT")
	}

	if action == "init" {
		if req.HeroName != "" {
			s.HeroName = req.HeroName
		}
		if req.CharacterClass != "" {
			s.CharacterClass = req.CharacterClass
		}
		s.Floor = 1
		s.X, s.Y = 1, 1
		if req.X > 0 || req.Y > 0 {
			s.X, s.Y = req.X, req.Y
		}
		s.saveCheckpointLocked("entered Floor 1")
		msg := fmt.Sprintf("Hero %s (%s) entered Floor 1: The Forgotten Crypt.", s.HeroName, s.CharacterClass)
		s.LastEvent = msg
		return s.responseLocked(msg, "INIT")
	}

	s.Turn++
	s.MilestoneCount++

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

		case "f3:dragon":
			hasSword := slices.Contains(s.Inventory, "dragon-slayer-sword")
			hasShield := slices.Contains(s.Inventory, "fire-shield")

			if !hasShield && !hasSword {
				s.LastDamageDealt = 10
				s.LastDamageTaken = max(100, s.HP)
				s.HP = 0
				s.Alive = false
				s.DeathReason = "Obliterated by the Ancient Heap Dragon (-100 HP — missing both dragon-slayer-sword and fire-shield from Floor 2)!"
				msg = fmt.Sprintf("LETHAL BOSS WIPE! You fought %s without the Dragon-Slayer Sword or Fire Shield (-100 HP)! YOU DIED (HP=0). Tap REVERT ACTOR, then take the Up-Stairs back to Floor 2!", target)
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
				msg = fmt.Sprintf("LETHAL COUNTER-ATTACK! Without the dragon-slayer-sword, your blade shattered against %s's scales and its jaws dealt -100 HP! YOU DIED (HP=0). Tap REVERT ACTOR, then take the Up-Stairs to Floor 2!", target)
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
		msg = fmt.Sprintf("SHRINE ACTIVATED on Floor %d! Restored HP to %d/%d, checkpointed Go heap to GCS, and tagged snapshot.", s.Floor, s.HP, s.MaxHP)
		eventType = "QUICKSAVE"
		checkpointLabel = "quicksave shrine"

	case "stairs":
		if s.Floor == 1 {
			if !slices.Contains(s.Inventory, "crypt-key") {
				msg = "The Crypt stairs are locked! Collect the crypt-key on Floor 1 first."
				eventType = "BLOCKED"
			} else {
				s.Floor = 2
				s.X, s.Y = 1, 5
				if !s.Cleared["f1:stairs_score"] {
					s.Cleared["f1:stairs_score"] = true
					s.Score += 200
				}
				msg = "Descended to Floor 2: The Abandoned Armory! Explore the alcoves for the Dragon-Slayer Sword & Fire Shield, or rush ahead at your own peril!"
				eventType = "FLOOR_CHANGE"
				checkpointLabel = "descend to Floor 2"
			}
		} else if s.Floor == 2 {
			s.Floor = 3
			s.X, s.Y = 7, 9
			if !s.Cleared["f2:stairs_score"] {
				s.Cleared["f2:stairs_score"] = true
				s.Score += 300
			}
			msg = "Descended to Floor 3: Cavern of the Ancient Heap Dragon! (Use the Up-Stairs at the bottom-right anytime to return to Floor 2.)"
			eventType = "FLOOR_CHANGE"
			checkpointLabel = "descend to Floor 3"
		} else {
			msg = "Already on the deepest floor (Floor 3)."
		}

	case "stairs_up":
		if s.Floor == 3 {
			s.Floor = 2
			s.X, s.Y = 12, 5
			msg = "Climbed the stairs back to Floor 2: The Abandoned Armory! Grab the Dragon-Slayer Sword (NW), Fire Shield (NE), or heal at the Shrine!"
			eventType = "FLOOR_CHANGE"
			checkpointLabel = "return to Floor 2"
		} else if s.Floor == 2 {
			s.Floor = 1
			s.X, s.Y = 12, 9
			msg = "Climbed the stairs back to Floor 1: The Forgotten Crypt."
			eventType = "FLOOR_CHANGE"
			checkpointLabel = "return to Floor 1"
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
			msg = fmt.Sprintf("VICTORY! %s escaped the 3-Floor Dungeon through the Ascension Portal with the Golden Chalice of State (Final Score: %d, Pod IPs visited: %d)!", s.HeroName, s.Score, len(s.PodIPHistory))
			eventType = "VICTORY"
			checkpointLabel = "escape dungeon"
		}

	default:
		msg = fmt.Sprintf("Unknown milestone action %q.", action)
	}

	if s.Alive && checkpointLabel != "" {
		s.saveCheckpointLocked(checkpointLabel)
	}

	s.LastEvent = msg
	return s.responseLocked(msg, eventType)
}

func (s *ActorSession) responseLocked(msg, eventType string) ActorResponse {
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

	snap := ActorSession{
		BootedAt:        s.BootedAt,
		UpdatedAt:       s.UpdatedAt,
		HeroName:        s.HeroName,
		CharacterClass:  s.CharacterClass,
		Floor:           s.Floor,
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
		Checkpoint:      cpCopy,
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
		var req MutateRequest
		if r.Body != nil {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 16384))
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
		writeJSON(w, http.StatusOK, session.Mutate(req, currentPodIP()))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.URL.Query().Get("action") != "" {
			var req MutateRequest
			if r.Body != nil {
				body, _ := io.ReadAll(io.LimitReader(r.Body, 16384))
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
			writeJSON(w, http.StatusOK, session.Mutate(req, currentPodIP()))
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
